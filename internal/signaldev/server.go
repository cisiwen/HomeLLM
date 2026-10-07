// Package signaldev is a small signalling server for local development. It implements the
// whole signalling protocol (pairing, device lists, offer/answer relay) so the home server and
// phone can be tested end to end without any cloud backend.
//
// It is NOT safe to expose publicly: phone identity is taken from an unverified idToken, and a
// device's secret is trusted on first use. A production backend (e.g. API Gateway WebSocket +
// Lambda + DynamoDB, alongside lambda-videochat) must verify the Cognito token and register
// devices properly; the message protocol stays the same.
package signaldev

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/wweng/homellm/protocol"
)

const pairApprovalTimeout = 90 * time.Second

// Server holds all signalling state. Pairings and devices are optionally persisted to a JSON
// file so restarting the dev server doesn't force re-pairing; codes and connections are not.
type Server struct {
	statePath string

	mu      sync.Mutex
	devices map[string]*device          // device_id -> device
	access  map[string]map[string]bool  // device_id -> user_id -> paired
	codes   map[string]*pairCode        // sha256(code) -> code
	homes   map[string]*conn            // device_id -> connected home server
	phones  map[string]*phoneSession    // session -> phone connection that owns it
	pending map[string]*pendingApproval // request_id -> waiting pairing
}

type device struct {
	SecretHash string `json:"secret_hash"`
	Name       string `json:"name"`
}

type pairCode struct {
	deviceID  string
	expiresAt time.Time
}

type phoneSession struct {
	c        *conn
	deviceID string
}

type pendingApproval struct {
	phone    *conn
	deviceID string
	userID   string
	timer    *time.Timer
}

type persisted struct {
	Devices map[string]*device         `json:"devices"`
	Access  map[string]map[string]bool `json:"access"`
}

// conn is one WebSocket client: a home server (deviceID set) or a phone (userID set).
type conn struct {
	ws       *websocket.Conn
	deviceID string
	instance string // home only: the connecting process's instance ID
	userID   string
}

func (c *conn) send(ctx context.Context, s protocol.Signal) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := wsjson.Write(ctx, c.ws, s); err != nil {
		slog.Debug("signal write failed", "action", s.Action, "err", err)
	}
}

// New creates a server, loading persisted pairings from statePath if it is non-empty.
func New(statePath string) (*Server, error) {
	s := &Server{
		statePath: statePath,
		devices:   map[string]*device{},
		access:    map[string]map[string]bool{},
		codes:     map[string]*pairCode{},
		homes:     map[string]*conn{},
		phones:    map[string]*phoneSession{},
		pending:   map[string]*pendingApproval{},
	}
	if statePath == "" {
		return s, nil
	}
	data, err := os.ReadFile(statePath)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var p persisted
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	if p.Devices != nil {
		s.devices = p.Devices
	}
	if p.Access != nil {
		s.access = p.Access
	}
	return s, nil
}

// saveLocked writes pairings to disk. Caller holds s.mu.
func (s *Server) saveLocked() {
	if s.statePath == "" {
		return
	}
	data, _ := json.MarshalIndent(persisted{Devices: s.devices, Access: s.access}, "", "  ")
	if err := os.WriteFile(s.statePath, data, 0o600); err != nil {
		slog.Warn("saving signalling state failed", "err", err)
	}
}

// ServeHTTP accepts the signalling WebSocket. Query parameters:
//
//	home:  role=home&device_id=...&name=...&instance=...  + X-Device-Secret header
//	phone: role=phone                                     + Authorization: Bearer <idToken>
//
// (device_secret / idToken query parameters are still accepted from older clients.)
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	c := &conn{}
	switch q.Get("role") {
	case "home":
		secret := r.Header.Get(protocol.HeaderDeviceSecret)
		if secret == "" {
			secret = q.Get("device_secret")
		}
		if err := s.authenticateHome(q.Get("device_id"), secret, q.Get("name")); err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		c.deviceID = q.Get("device_id")
		c.instance = q.Get("instance")
	case "phone":
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" {
			token = q.Get("idToken")
		}
		c.userID = userFromToken(token)
		if c.userID == "" {
			http.Error(w, "missing idToken", http.StatusUnauthorized)
			return
		}
	default:
		http.Error(w, "role must be home or phone", http.StatusBadRequest)
		return
	}

	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	c.ws = ws
	ws.SetReadLimit(1 << 20) // SDPs are a few KB
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defer ws.CloseNow()

	s.attach(c)
	defer s.detach(c)
	go keepAlive(ctx, ws)

	slog.Info("connected", "device", c.deviceID, "user", c.userID)
	for {
		var msg protocol.Signal
		if err := wsjson.Read(ctx, ws, &msg); err != nil {
			slog.Info("disconnected", "device", c.deviceID, "user", c.userID, "err", websocket.CloseStatus(err))
			return
		}
		if msg.Action == protocol.ActionPing {
			c.send(ctx, protocol.Signal{Action: protocol.ActionPong})
			continue
		}
		if c.deviceID != "" {
			s.handleHome(ctx, c, msg)
		} else {
			s.handlePhone(ctx, c, msg)
		}
	}
}

// keepAlive pings so dead connections (a home PC that lost power) are noticed and dropped.
func keepAlive(ctx context.Context, ws *websocket.Conn) {
	t := time.NewTicker(25 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := ws.Ping(pctx)
			cancel()
			if err != nil {
				ws.CloseNow()
				return
			}
		}
	}
}

func (s *Server) authenticateHome(deviceID, secret, name string) error {
	if deviceID == "" || len(secret) < 16 {
		return errors.New("device_id and device_secret required")
	}
	h := hashHex(secret)
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[deviceID]
	if !ok {
		// Trust on first use. A real backend registers devices through an authenticated call.
		s.devices[deviceID] = &device{SecretHash: h, Name: name}
		s.saveLocked()
		return nil
	}
	if subtle.ConstantTimeCompare([]byte(d.SecretHash), []byte(h)) != 1 {
		return errors.New("bad device secret")
	}
	if name != "" && name != d.Name {
		d.Name = name
		s.saveLocked()
	}
	return nil
}

func (s *Server) attach(c *conn) {
	if c.deviceID == "" {
		return
	}
	s.mu.Lock()
	old := s.homes[c.deviceID]
	s.homes[c.deviceID] = c
	s.mu.Unlock()
	if old != nil {
		// A reconnecting home server replaces its stale connection. Say who replaced it, so a
		// server can tell its own reconnect from a second copy running (see ActionReplaced).
		old.send(context.Background(), protocol.Signal{Action: protocol.ActionReplaced, Instance: c.instance, Message: "replaced by a newer connection"})
		old.ws.Close(websocket.StatusNormalClosure, "replaced by a newer connection")
	}
}

func (s *Server) detach(c *conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.deviceID != "" && s.homes[c.deviceID] == c {
		delete(s.homes, c.deviceID)
	}
	for id, ps := range s.phones {
		if ps.c == c {
			delete(s.phones, id)
		}
	}
	for id, p := range s.pending {
		if p.phone == c {
			p.timer.Stop()
			delete(s.pending, id)
		}
	}
}

func (s *Server) handleHome(ctx context.Context, c *conn, msg protocol.Signal) {
	switch msg.Action {
	case protocol.ActionPairOpen:
		if len(msg.Code) < 16 {
			c.send(ctx, errorSignal("pairing code too short", ""))
			return
		}
		expires := time.Unix(msg.ExpiresAt, 0)
		if limit := time.Now().Add(15 * time.Minute); expires.After(limit) {
			expires = limit
		}
		s.mu.Lock()
		s.codes[hashHex(msg.Code)] = &pairCode{deviceID: c.deviceID, expiresAt: expires}
		s.mu.Unlock()

	case protocol.ActionPairResponse:
		s.mu.Lock()
		p := s.pending[msg.RequestID]
		if p != nil && p.deviceID == c.deviceID {
			delete(s.pending, msg.RequestID)
			p.timer.Stop()
			if msg.Approved {
				if s.access[p.deviceID] == nil {
					s.access[p.deviceID] = map[string]bool{}
				}
				s.access[p.deviceID][p.userID] = true
				s.saveLocked()
			}
		}
		name := ""
		if d := s.devices[c.deviceID]; d != nil {
			name = d.Name
		}
		s.mu.Unlock()
		if p == nil || p.deviceID != c.deviceID {
			return
		}
		res := protocol.Signal{Action: protocol.ActionPairResult, DeviceID: c.deviceID, Name: name, OK: msg.Approved}
		if !msg.Approved {
			res.Message = "pairing was rejected on the home server"
		}
		p.phone.send(ctx, res)

	case protocol.ActionSignal:
		s.mu.Lock()
		ps := s.phones[msg.Session]
		s.mu.Unlock()
		if ps == nil || ps.deviceID != c.deviceID {
			c.send(ctx, errorSignal("unknown session", msg.Session))
			return
		}
		ps.c.send(ctx, protocol.Signal{Action: protocol.ActionSignal, DeviceID: c.deviceID, Session: msg.Session, Payload: msg.Payload})
	}
}

func (s *Server) handlePhone(ctx context.Context, c *conn, msg protocol.Signal) {
	switch msg.Action {
	case protocol.ActionPair:
		s.startPairing(ctx, c, msg)

	case protocol.ActionDevices:
		s.mu.Lock()
		var list []protocol.Device
		for id, users := range s.access {
			if users[c.userID] {
				name := ""
				if d := s.devices[id]; d != nil {
					name = d.Name
				}
				list = append(list, protocol.Device{DeviceID: id, Name: name, Online: s.homes[id] != nil})
			}
		}
		s.mu.Unlock()
		c.send(ctx, protocol.Signal{Action: protocol.ActionDevices, Devices: list})

	case protocol.ActionSignal:
		if msg.Session == "" {
			c.send(ctx, errorSignal("session required", ""))
			return
		}
		s.mu.Lock()
		allowed := s.access[msg.DeviceID][c.userID]
		home := s.homes[msg.DeviceID]
		owner := s.phones[msg.Session]
		if allowed && home != nil && (owner == nil || owner.c == c) {
			s.phones[msg.Session] = &phoneSession{c: c, deviceID: msg.DeviceID}
		}
		s.mu.Unlock()
		switch {
		case !allowed:
			// Same reply as "no such device", so device IDs can't be probed.
			c.send(ctx, errorSignal("device not found", msg.Session))
		case home == nil:
			c.send(ctx, errorSignal("home server is offline", msg.Session))
		case owner != nil && owner.c != c:
			c.send(ctx, errorSignal("session belongs to another connection", msg.Session))
		default:
			home.send(ctx, protocol.Signal{Action: protocol.ActionSignal, Session: msg.Session, UserID: c.userID, Payload: msg.Payload})
		}
	}
}

func (s *Server) startPairing(ctx context.Context, c *conn, msg protocol.Signal) {
	fail := func(m string) {
		c.send(ctx, protocol.Signal{Action: protocol.ActionPairResult, DeviceID: msg.DeviceID, Message: m})
	}
	key := hashHex(msg.Code)
	s.mu.Lock()
	pc := s.codes[key]
	// Single use: consumed by the first attempt, right or wrong.
	delete(s.codes, key)
	home := s.homes[msg.DeviceID]
	s.mu.Unlock()

	if pc == nil || pc.deviceID != msg.DeviceID || time.Now().After(pc.expiresAt) {
		fail("invalid or expired pairing code")
		return
	}
	if home == nil {
		fail("home server is offline")
		return
	}

	reqID := randomID()
	p := &pendingApproval{phone: c, deviceID: msg.DeviceID, userID: c.userID}
	p.timer = time.AfterFunc(pairApprovalTimeout, func() {
		s.mu.Lock()
		_, still := s.pending[reqID]
		delete(s.pending, reqID)
		s.mu.Unlock()
		if still {
			c.send(context.Background(), protocol.Signal{Action: protocol.ActionPairResult, DeviceID: msg.DeviceID, Message: "pairing was not approved in time"})
		}
	})
	s.mu.Lock()
	s.pending[reqID] = p
	s.mu.Unlock()
	home.send(ctx, protocol.Signal{Action: protocol.ActionPairRequest, RequestID: reqID, UserID: c.userID})
}

func errorSignal(message, session string) protocol.Signal {
	return protocol.Signal{Action: protocol.ActionError, Message: message, Session: session}
}

// userFromToken derives the phone's user ID. For a JWT (the app's Cognito id token) it reads
// the "sub" claim WITHOUT verifying the signature - acceptable only because this is a local dev
// server. Any other non-empty string is used as the user ID as-is, which is handy for testing.
func userFromToken(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) == 3 {
		if payload, err := base64.RawURLEncoding.DecodeString(parts[1]); err == nil {
			var claims struct {
				Sub string `json:"sub"`
			}
			if json.Unmarshal(payload, &claims) == nil && claims.Sub != "" {
				return claims.Sub
			}
		}
	}
	return token
}

func hashHex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func randomID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
