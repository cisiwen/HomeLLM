package tunnel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/pion/webrtc/v4"

	"github.com/wweng/homellm/internal/home"
	"github.com/wweng/homellm/protocol"
)

const (
	gatherTimeout     = 10 * time.Second
	connectionTimeout = 30 * time.Second
	dialTimeout       = 5 * time.Second
	pingInterval      = time.Minute
	// maxChannels bounds forwarded connections per paired client.
	maxChannels = 128
)

var errDuplicate = errors.New("replaced by another instance")

// HomeConfig configures a Home.
type HomeConfig struct {
	// SignalURL is the signalling WebSocket this server connects to.
	SignalURL string
	// PublicSignalURL is the URL clients use, put in the pairing blob. Defaults to SignalURL.
	PublicSignalURL string
	ICEServers      []webrtc.ICEServer
	// Ports is the allow-list of localhost ports a client may reach. Nothing else is dialed.
	Ports []int
	// Approve asks the owner whether userID may pair. It must return within ctx's deadline.
	Approve func(ctx context.Context, userID string) bool
}

// Home is the PC side: it holds the signalling connection, answers pairing, and forwards
// each client data channel to a localhost port.
type Home struct {
	cfg      HomeConfig
	id       *home.Identity
	instance string

	mu       sync.Mutex
	ws       *websocket.Conn // nil while reconnecting
	sessions map[string]*homeSession
}

// NewHome creates a Home; call Run to start it.
func NewHome(cfg HomeConfig, id *home.Identity) *Home {
	if cfg.PublicSignalURL == "" {
		cfg.PublicSignalURL = cfg.SignalURL
	}
	return &Home{cfg: cfg, id: id, instance: randomHex(8), sessions: map[string]*homeSession{}}
}

// Run keeps the signalling connection up until ctx is cancelled, reconnecting with backoff.
// Established peer connections are direct, so they survive signalling dropping.
func (h *Home) Run(ctx context.Context) error {
	delay := time.Second
	for {
		started := time.Now()
		err := h.runOnce(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errDuplicate) {
			return errors.New("another tunnel-home with this identity (" + h.id.DeviceID + ") connected; stopping this one")
		}
		if time.Since(started) > time.Minute {
			delay = time.Second
		}
		wait := delay + time.Duration(mrand.Int64N(int64(delay)))
		slog.Warn("signalling disconnected; reconnecting", "err", err, "in", wait.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		delay = min(delay*2, 30*time.Second)
	}
}

func (h *Home) runOnce(ctx context.Context) error {
	u, err := url.Parse(h.cfg.SignalURL)
	if err != nil {
		return err
	}
	q := u.Query()
	q.Set("role", "home")
	q.Set("device_id", h.id.DeviceID)
	q.Set("name", h.id.Name)
	q.Set("instance", h.instance)
	u.RawQuery = q.Encode()

	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	ws, _, err := websocket.Dial(dctx, u.String(), &websocket.DialOptions{
		HTTPHeader: http.Header{protocol.HeaderDeviceSecret: {h.id.DeviceSecret}},
	})
	cancel()
	if err != nil {
		return err
	}
	defer ws.CloseNow()
	ws.SetReadLimit(1 << 20)

	h.mu.Lock()
	h.ws = ws
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		if h.ws == ws {
			h.ws = nil
		}
		h.mu.Unlock()
	}()
	slog.Info("connected to signalling", "url", h.cfg.SignalURL, "device", h.id.DeviceID, "name", h.id.Name, "ports", h.cfg.Ports)

	cctx, stop := context.WithCancel(ctx)
	defer stop()
	var lastPong atomic.Int64
	lastPong.Store(time.Now().UnixNano())
	go func() {
		t := time.NewTicker(pingInterval)
		defer t.Stop()
		for {
			select {
			case <-cctx.Done():
				return
			case <-t.C:
				if time.Since(time.Unix(0, lastPong.Load())) > 3*pingInterval {
					slog.Warn("no pong from signalling; reconnecting")
					ws.CloseNow()
					return
				}
				h.send(cctx, protocol.Signal{Action: protocol.ActionPing})
			}
		}
	}()

	for {
		var msg protocol.Signal
		if err := wsjson.Read(cctx, ws, &msg); err != nil {
			return err
		}
		switch msg.Action {
		case protocol.ActionPong:
			lastPong.Store(time.Now().UnixNano())
		case protocol.ActionReplaced:
			if msg.Instance != h.instance {
				return errDuplicate
			}
			return errors.New("signalling switched to this server's own newer connection")
		default:
			h.handle(cctx, msg)
		}
	}
}

func (h *Home) send(ctx context.Context, s protocol.Signal) error {
	h.mu.Lock()
	ws := h.ws
	h.mu.Unlock()
	if ws == nil {
		return errors.New("not connected to signalling")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return wsjson.Write(ctx, ws, s)
}

func (h *Home) handle(ctx context.Context, msg protocol.Signal) {
	switch msg.Action {
	case protocol.ActionPairRequest:
		go func() {
			actx, cancel := context.WithTimeout(ctx, 80*time.Second)
			defer cancel()
			ok := h.cfg.Approve != nil && h.cfg.Approve(actx, msg.UserID)
			slog.Info("pairing", "user", msg.UserID, "approved", ok)
			h.send(ctx, protocol.Signal{Action: protocol.ActionPairResponse, RequestID: msg.RequestID, Approved: ok})
		}()

	case protocol.ActionSignal:
		var neg protocol.Negotiation
		if err := json.Unmarshal(msg.Payload, &neg); err != nil || neg.Type != "offer" || msg.Session == "" {
			slog.Warn("ignoring malformed signal", "session", msg.Session)
			return
		}
		go h.answer(ctx, msg.Session, msg.UserID, neg)

	case protocol.ActionError:
		slog.Warn("signalling error", "message", msg.Message, "session", msg.Session)
	}
}

func (h *Home) answer(ctx context.Context, sessionID, userID string, offer protocol.Negotiation) {
	h.mu.Lock()
	old := h.sessions[sessionID]
	h.mu.Unlock()
	if old != nil {
		old.close()
	}

	s, answer, err := h.newSession(sessionID, userID, offer)
	if err != nil {
		slog.Warn("answering offer failed", "session", sessionID, "err", err)
		return
	}
	h.mu.Lock()
	h.sessions[sessionID] = s
	h.mu.Unlock()

	payload, _ := json.Marshal(answer)
	if err := h.send(ctx, protocol.Signal{Action: protocol.ActionSignal, Session: sessionID, Payload: payload}); err != nil {
		slog.Warn("sending answer failed", "session", sessionID, "err", err)
		s.close()
	}
}

func (h *Home) removeSession(s *homeSession) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sessions[s.id] == s {
		delete(h.sessions, s.id)
	}
}

// NewPairingQR registers a fresh one-time pairing code and returns the blob to give the client.
func (h *Home) NewPairingQR(ctx context.Context) (protocol.PairingQR, error) {
	code := randomHex(16) // 128 bits: unguessable within its lifetime
	err := h.send(ctx, protocol.Signal{Action: protocol.ActionPairOpen, Code: code, ExpiresAt: time.Now().Add(home.PairingTTL).Unix()})
	if err != nil {
		return protocol.PairingQR{}, err
	}
	return protocol.PairingQR{
		V:        1,
		Signal:   h.cfg.PublicSignalURL,
		DeviceID: h.id.DeviceID,
		Name:     h.id.Name,
		Code:     code,
		Pub:      h.id.PublicKeyBase64(),
	}, nil
}

// Connected reports whether the signalling connection is currently up.
func (h *Home) Connected() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ws != nil
}

// homeSession is one client's peer connection.
type homeSession struct {
	id     string
	userID string
	home   *Home
	log    *slog.Logger
	pc     *webrtc.PeerConnection

	channels atomic.Int32
	closed   atomic.Bool
}

func (h *Home) newSession(id, userID string, offer protocol.Negotiation) (*homeSession, protocol.Negotiation, error) {
	s := &homeSession{id: id, userID: userID, home: h, log: slog.With("session", id[:min(8, len(id))], "user", userID)}
	pc, err := newAPI().NewPeerConnection(webrtc.Configuration{ICEServers: h.cfg.ICEServers})
	if err != nil {
		return nil, protocol.Negotiation{}, err
	}
	s.pc = pc
	pc.OnDataChannel(s.onDataChannel)
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		s.log.Info("peer connection", "state", st.String())
		switch st {
		case webrtc.PeerConnectionStateConnected:
			s.logPath()
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			go s.close() // not inline: closing from inside a Pion callback can deadlock
		}
	})
	time.AfterFunc(connectionTimeout, func() {
		if pc.ConnectionState() != webrtc.PeerConnectionStateConnected {
			s.close()
		}
	})

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer.SDP}); err != nil {
		s.close()
		return nil, protocol.Negotiation{}, fmt.Errorf("bad offer: %w", err)
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		s.close()
		return nil, protocol.Negotiation{}, err
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		s.close()
		return nil, protocol.Negotiation{}, err
	}
	select {
	case <-gathered:
	case <-time.After(gatherTimeout):
		s.log.Warn("ICE gathering timed out; answering with the candidates found so far")
	}

	sdp := pc.LocalDescription().SDP
	sig := ed25519.Sign(h.id.PrivateKey, protocol.AnswerSigningBytes(offer.SDP, sdp))
	return s, protocol.Negotiation{Type: "answer", SDP: sdp, Sig: base64.StdEncoding.EncodeToString(sig)}, nil
}

// logPath records whether the connection is direct or relayed through TURN.
func (s *homeSession) logPath() {
	sctp := s.pc.SCTP()
	if sctp == nil {
		return
	}
	pair, err := sctp.Transport().ICETransport().GetSelectedCandidatePair()
	if err != nil || pair == nil {
		return
	}
	path := "direct"
	if pair.Local.Typ == webrtc.ICECandidateTypeRelay || pair.Remote.Typ == webrtc.ICECandidateTypeRelay {
		path = "relayed via TURN"
	}
	s.log.Info("connected", "path", path, "local", pair.Local.Typ.String(), "remote", pair.Remote.Typ.String())
}

func (s *homeSession) onDataChannel(dc *webrtc.DataChannel) {
	label := dc.Label()
	if label == controlChannel {
		return
	}
	port, ok := s.home.allowedPort(label)
	if !ok {
		s.log.Warn("refusing data channel", "label", label)
		dc.Close()
		return
	}
	if s.channels.Add(1) > maxChannels {
		s.channels.Add(-1)
		s.log.Warn("too many open channels; refusing", "label", label)
		dc.Close()
		return
	}
	dc.OnOpen(func() {
		defer s.channels.Add(-1)
		raw, err := dc.Detach()
		if err != nil {
			s.log.Warn("detach failed", "err", err)
			return
		}
		c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), dialTimeout)
		if err != nil {
			s.log.Warn("dialing local port failed", "port", port, "err", err)
			raw.Close()
			return
		}
		pipe(raw, dc, c)
	})
}

func (s *homeSession) close() {
	if !s.closed.CompareAndSwap(false, true) {
		return
	}
	s.pc.Close()
	s.home.removeSession(s)
}

// allowedPort parses a "tcp:<port>" label and checks it against the allow-list.
func (h *Home) allowedPort(label string) (int, bool) {
	p, ok := strings.CutPrefix(label, ChannelPrefix)
	if !ok {
		return 0, false
	}
	port, err := strconv.Atoi(p)
	if err != nil || !slices.Contains(h.cfg.Ports, port) {
		return 0, false
	}
	return port, true
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
