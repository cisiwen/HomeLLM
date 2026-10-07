package home

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/wweng/homellm/protocol"
)

const (
	gatherTimeout     = 10 * time.Second
	imageWaitTimeout  = 30 * time.Second
	connectionTimeout = 30 * time.Second
)

// session is one phone's WebRTC peer connection. After the answer is sent, nothing in here
// touches the signalling server: all traffic is on the data channels, directly between the
// two devices (or via TURN if a direct path couldn't be found).
type session struct {
	id     string
	userID string
	agent  *Agent
	log    *slog.Logger
	pc     *webrtc.PeerConnection
	images *imageStore

	sendMu sync.Mutex
	chat   *webrtc.DataChannel
	files  *webrtc.DataChannel

	mu       sync.Mutex
	inflight map[string]context.CancelFunc
	closed   bool
}

// newSession answers a phone's offer. It returns the signed answer to relay back.
func newSession(a *Agent, id, userID string, offer protocol.Negotiation) (*session, protocol.Negotiation, error) {
	s := &session{
		id:       id,
		userID:   userID,
		agent:    a,
		log:      slog.With("session", id[:min(8, len(id))], "user", userID),
		images:   newImageStore(),
		inflight: map[string]context.CancelFunc{},
	}
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{ICEServers: a.cfg.ICEServers})
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
	// A phone that gets an answer but never connects shouldn't leave a peer connection behind.
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
		// Send whatever candidates were found; a slow STUN server shouldn't block everything.
		s.log.Warn("ICE gathering timed out; answering with the candidates found so far")
	}

	sdp := pc.LocalDescription().SDP
	sig := ed25519.Sign(a.id.PrivateKey, protocol.AnswerSigningBytes(offer.SDP, sdp))
	return s, protocol.Negotiation{Type: "answer", SDP: sdp, Sig: base64.StdEncoding.EncodeToString(sig)}, nil
}

// logPath records whether the connection is direct or relayed through TURN.
func (s *session) logPath() {
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

func (s *session) onDataChannel(dc *webrtc.DataChannel) {
	switch dc.Label() {
	case protocol.ChannelChat:
		s.sendMu.Lock()
		s.chat = dc
		s.sendMu.Unlock()
		dc.OnMessage(func(m webrtc.DataChannelMessage) {
			if m.IsString {
				s.onChat(m.Data)
			}
		})
	case protocol.ChannelFiles:
		s.sendMu.Lock()
		s.files = dc
		s.sendMu.Unlock()
		rx := &fileReceiver{store: s.images, replyFn: func(p protocol.Peer) { s.send(s.files, p) }}
		// Pion delivers one channel's messages sequentially, so rx needs no locking.
		dc.OnMessage(func(m webrtc.DataChannelMessage) {
			if m.IsString {
				rx.onText(m.Data)
			} else {
				rx.onBinary(m.Data)
			}
		})
	default:
		s.log.Warn("ignoring unknown data channel", "label", dc.Label())
	}
}

func (s *session) send(dc *webrtc.DataChannel, p protocol.Peer) {
	if dc == nil {
		return
	}
	data, _ := json.Marshal(p)
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if err := dc.SendText(string(data)); err != nil {
		s.log.Debug("data channel send failed", "err", err)
	}
}

func (s *session) onChat(data []byte) {
	var m protocol.Peer
	if err := json.Unmarshal(data, &m); err != nil {
		return
	}
	switch m.Type {
	case protocol.PeerHello:
		b := s.agent.cfg.Backend
		s.send(s.chat, protocol.Peer{Type: protocol.PeerHello, Name: s.agent.id.Name, Model: b.DefaultModel(), Vision: b.Vision()})
	case protocol.PeerRequest:
		if m.ID == "" {
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			cancel()
			return
		}
		if _, dup := s.inflight[m.ID]; dup {
			s.mu.Unlock()
			cancel()
			return
		}
		s.inflight[m.ID] = cancel
		s.mu.Unlock()
		go s.runRequest(ctx, m)
	case protocol.PeerCancel:
		s.mu.Lock()
		cancel := s.inflight[m.ID]
		s.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	}
}

func (s *session) runRequest(ctx context.Context, m protocol.Peer) {
	defer func() {
		s.mu.Lock()
		if c := s.inflight[m.ID]; c != nil {
			c()
			delete(s.inflight, m.ID)
		}
		s.mu.Unlock()
	}()
	fail := func(err error) {
		if errors.Is(err, context.Canceled) {
			s.send(s.chat, protocol.Peer{Type: protocol.PeerDone, ID: m.ID})
			return
		}
		s.log.Warn("request failed", "id", m.ID, "err", err)
		s.send(s.chat, protocol.Peer{Type: protocol.PeerError, ID: m.ID, Message: err.Error()})
	}

	msgs, err := s.resolveImages(ctx, m.Messages)
	if err != nil {
		fail(err)
		return
	}
	if n := s.agent.cfg.ContextTokens; n > 0 {
		// Every request carries the whole conversation; keep a quarter of the context free
		// for the reply.
		if trimmed := trimHistory(msgs, n*3/4); len(trimmed) < len(msgs) {
			s.log.Info("trimmed history to fit the context", "id", m.ID, "dropped", len(msgs)-len(trimmed))
			msgs = trimmed
		}
	}

	// One GPU only handles a few generations at once; queue the rest.
	select {
	case s.agent.slots <- struct{}{}:
		defer func() { <-s.agent.slots }()
	case <-ctx.Done():
		fail(ctx.Err())
		return
	}

	s.log.Info("generating", "id", m.ID, "model", m.Model, "messages", len(msgs))
	start := time.Now()
	err = s.agent.cfg.Backend.Stream(ctx, m.Model, msgs, func(delta string) error {
		s.send(s.chat, protocol.Peer{Type: protocol.PeerChunk, ID: m.ID, Delta: delta})
		return nil
	})
	if err != nil {
		fail(err)
		return
	}
	s.log.Info("done", "id", m.ID, "took", time.Since(start).Round(time.Millisecond))
	s.send(s.chat, protocol.Peer{Type: protocol.PeerDone, ID: m.ID})
}

func (s *session) resolveImages(ctx context.Context, in []protocol.ChatMessage) ([]LLMMessage, error) {
	wctx, cancel := context.WithTimeout(ctx, imageWaitTimeout)
	defer cancel()
	out := make([]LLMMessage, 0, len(in))
	for _, cm := range in {
		lm := LLMMessage{Role: cm.Role, Content: cm.Content}
		for _, id := range cm.ImageIDs {
			img, err := s.images.Wait(wctx, id)
			if err != nil {
				return nil, err
			}
			lm.Images = append(lm.Images, img)
		}
		out = append(out, lm)
	}
	return out, nil
}

func (s *session) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	for _, cancel := range s.inflight {
		cancel()
	}
	s.mu.Unlock()
	s.pc.Close()
	s.agent.removeSession(s)
}
