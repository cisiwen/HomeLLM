// Package home is the home server: it stays connected to the signalling server, handles QR
// pairing, and answers each phone's WebRTC offer with a peer connection that serves LLM
// requests over data channels.
package home

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/pion/webrtc/v4"

	"github.com/wweng/homellm/protocol"
)

// PairingTTL is how long a pairing QR code stays valid.
const PairingTTL = 5 * time.Minute

// Config configures an Agent.
type Config struct {
	// SignalURL is the signalling WebSocket this server connects to, e.g. ws://localhost:8090/ws.
	SignalURL string
	// PublicSignalURL is the signalling URL phones should use, put into the pairing QR. It
	// differs from SignalURL when this server reaches signalling over localhost. Defaults to
	// SignalURL.
	PublicSignalURL string
	ICEServers      []webrtc.ICEServer
	Backend         Backend
	// Images generates images from prompts; nil turns image generation off. It shares the
	// GPU with Backend (see gpuArbiter).
	Images ImageBackend
	// MaxConcurrent is how many generations run at once across all phones; others queue.
	MaxConcurrent int
	// ContextTokens is the model's context size. Requests drop their oldest messages to fit,
	// leaving room for the reply. 0 disables trimming.
	ContextTokens int
	// Approve asks the owner whether userID may pair. It must return within ctx's deadline.
	Approve func(ctx context.Context, userID string) bool
}

// Agent is a running home server.
type Agent struct {
	cfg   Config
	id    *Identity
	slots chan struct{}
	// imageSlots runs one image job at a time; gpu decides whether the LLM or the image
	// model has the GPU.
	imageSlots chan struct{}
	gpu        *gpuArbiter

	// instance identifies this process, so a "replaced" message can tell this process's own
	// reconnect apart from a second copy of the server running with the same identity.
	instance string

	mu       sync.Mutex
	ws       *websocket.Conn // current signalling connection, nil while reconnecting
	sessions map[string]*session
}

// pingInterval is how often the agent pings signalling; a link with no pong for three
// intervals is treated as dead.
const pingInterval = time.Minute

var errDuplicate = errors.New("replaced by another instance")

// NewAgent creates an agent; call Run to start it.
func NewAgent(cfg Config, id *Identity) *Agent {
	if cfg.PublicSignalURL == "" {
		cfg.PublicSignalURL = cfg.SignalURL
	}
	if cfg.MaxConcurrent < 1 {
		cfg.MaxConcurrent = 1
	}
	a := &Agent{cfg: cfg, id: id, instance: randomHex(8), slots: make(chan struct{}, cfg.MaxConcurrent),
		imageSlots: make(chan struct{}, 1), sessions: map[string]*session{}}
	a.gpu = newGPUArbiter(a.freeGPUFor)
	return a
}

// Run keeps the signalling connection up until ctx is cancelled, reconnecting with backoff.
// Existing peer connections are unaffected by signalling dropping: they're direct.
func (a *Agent) Run(ctx context.Context) error {
	delay := time.Second
	for {
		started := time.Now()
		err := a.runOnce(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errDuplicate) {
			// Another copy of this server is running with the same identity. Reconnecting
			// would just kick it off in turn, forever, so let the newer one win.
			return errors.New("another HomeLLM server with this identity (" + a.id.DeviceID + ") connected; stopping this one")
		}
		if time.Since(started) > time.Minute {
			delay = time.Second // it was a healthy connection; reconnect quickly
		}
		wait := delay + time.Duration(rand.Int64N(int64(delay)))
		slog.Warn("signalling disconnected; reconnecting", "err", err, "in", wait.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		delay = min(delay*2, 30*time.Second)
	}
}

func (a *Agent) runOnce(ctx context.Context) error {
	u, err := url.Parse(a.cfg.SignalURL)
	if err != nil {
		return err
	}
	q := u.Query()
	q.Set("role", "home")
	q.Set("device_id", a.id.DeviceID)
	q.Set("name", a.id.Name)
	q.Set("instance", a.instance)
	u.RawQuery = q.Encode()

	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	ws, _, err := websocket.Dial(dctx, u.String(), &websocket.DialOptions{
		HTTPHeader: http.Header{protocol.HeaderDeviceSecret: {a.id.DeviceSecret}},
	})
	cancel()
	if err != nil {
		return err
	}
	defer ws.CloseNow()
	ws.SetReadLimit(1 << 20)

	a.mu.Lock()
	a.ws = ws
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		if a.ws == ws {
			a.ws = nil
		}
		a.mu.Unlock()
	}()
	slog.Info("connected to signalling", "url", a.cfg.SignalURL, "device", a.id.DeviceID, "name", a.id.Name)

	cctx, stop := context.WithCancel(ctx)
	defer stop()
	var lastPong atomic.Int64
	lastPong.Store(time.Now().UnixNano())
	go func() {
		// Keeps API Gateway from closing an idle connection, and notices a silently dead link
		// (e.g. the router rebooted) instead of waiting forever.
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
				a.send(cctx, protocol.Signal{Action: protocol.ActionPing})
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
			if msg.Instance != a.instance {
				return errDuplicate
			}
			return errors.New("signalling switched to this server's own newer connection")
		default:
			a.handle(cctx, msg)
		}
	}
}

func (a *Agent) send(ctx context.Context, s protocol.Signal) error {
	a.mu.Lock()
	ws := a.ws
	a.mu.Unlock()
	if ws == nil {
		return errors.New("not connected to signalling")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return wsjson.Write(ctx, ws, s)
}

func (a *Agent) handle(ctx context.Context, msg protocol.Signal) {
	switch msg.Action {
	case protocol.ActionPairRequest:
		go func() {
			actx, cancel := context.WithTimeout(ctx, 80*time.Second)
			defer cancel()
			ok := a.cfg.Approve != nil && a.cfg.Approve(actx, msg.UserID)
			slog.Info("pairing", "user", msg.UserID, "approved", ok)
			a.send(ctx, protocol.Signal{Action: protocol.ActionPairResponse, RequestID: msg.RequestID, Approved: ok})
		}()

	case protocol.ActionSignal:
		var neg protocol.Negotiation
		if err := json.Unmarshal(msg.Payload, &neg); err != nil || neg.Type != "offer" || msg.Session == "" {
			slog.Warn("ignoring malformed signal", "session", msg.Session)
			return
		}
		// Gathering ICE candidates takes up to a few seconds; don't block the read loop.
		go a.answer(ctx, msg.Session, msg.UserID, neg)

	case protocol.ActionError:
		slog.Warn("signalling error", "message", msg.Message, "session", msg.Session)
	}
}

func (a *Agent) answer(ctx context.Context, sessionID, userID string, offer protocol.Negotiation) {
	a.mu.Lock()
	old := a.sessions[sessionID]
	a.mu.Unlock()
	if old != nil {
		// The phone is renegotiating (e.g. after switching networks); start fresh.
		old.close()
	}

	s, answer, err := newSession(a, sessionID, userID, offer)
	if err != nil {
		slog.Warn("answering offer failed", "session", sessionID, "err", err)
		return
	}
	a.mu.Lock()
	a.sessions[sessionID] = s
	a.mu.Unlock()

	payload, _ := json.Marshal(answer)
	if err := a.send(ctx, protocol.Signal{Action: protocol.ActionSignal, Session: sessionID, Payload: payload}); err != nil {
		slog.Warn("sending answer failed", "session", sessionID, "err", err)
		s.close()
	}
}

func (a *Agent) removeSession(s *session) {
	a.mu.Lock()
	defer a.mu.Unlock()
	// Only if it's still the current one: a replaced session's late close mustn't remove its
	// replacement.
	if a.sessions[s.id] == s {
		delete(a.sessions, s.id)
	}
}

// NewPairingQR registers a fresh one-time pairing code and returns the QR payload to show.
func (a *Agent) NewPairingQR(ctx context.Context) (protocol.PairingQR, error) {
	code := randomHex(16) // 128 bits: unguessable within its lifetime
	err := a.send(ctx, protocol.Signal{Action: protocol.ActionPairOpen, Code: code, ExpiresAt: time.Now().Add(PairingTTL).Unix()})
	if err != nil {
		return protocol.PairingQR{}, err
	}
	return protocol.PairingQR{
		V:        1,
		Signal:   a.cfg.PublicSignalURL,
		DeviceID: a.id.DeviceID,
		Name:     a.id.Name,
		Code:     code,
		Pub:      a.id.PublicKeyBase64(),
	}, nil
}

// Connected reports whether the signalling connection is currently up.
func (a *Agent) Connected() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ws != nil
}

// freeGPUFor unloads whichever model would be in the way of mode.
func (a *Agent) freeGPUFor(ctx context.Context, mode string) error {
	switch mode {
	case gpuImage:
		if u, ok := a.cfg.Backend.(Unloader); ok {
			return u.Unload(ctx)
		}
	case gpuLLM:
		if a.cfg.Images != nil {
			return a.cfg.Images.Free(ctx)
		}
	}
	return nil
}
