package home

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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
	// fileOutMu keeps one outgoing file in flight at a time - the files channel's binary
	// chunks carry no header, so files can't interleave.
	fileOutMu sync.Mutex

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
		s.send(s.chat, protocol.Peer{Type: protocol.PeerHello, Name: s.agent.id.Name, Model: b.DefaultModel(), Vision: b.Vision(),
			Images: s.agent.cfg.Images != nil})
	case protocol.PeerRequest:
		if ctx, ok := s.track(m.ID); ok {
			go s.runRequest(ctx, m)
		}
	case protocol.PeerImagine:
		if ctx, ok := s.track(m.ID); ok {
			go s.runImagine(ctx, m)
		}
	case protocol.PeerCancel:
		s.mu.Lock()
		cancel := s.inflight[m.ID]
		s.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	}
}

// track registers a new request so cancel and close can stop it. ok is false for an empty or
// duplicate id, or once the session is closing.
func (s *session) track(id string) (ctx context.Context, ok bool) {
	if id == "" {
		return nil, false
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.inflight[id]; s.closed || dup {
		cancel()
		return nil, false
	}
	s.inflight[id] = cancel
	return ctx, true
}

func (s *session) untrack(id string) {
	s.mu.Lock()
	if c := s.inflight[id]; c != nil {
		c()
		delete(s.inflight, id)
	}
	s.mu.Unlock()
}

// failRequest reports a failed request - or, when the phone cancelled it, just ends it.
func (s *session) failRequest(id string, err error) {
	if errors.Is(err, context.Canceled) {
		s.send(s.chat, protocol.Peer{Type: protocol.PeerDone, ID: id})
		return
	}
	s.log.Warn("request failed", "id", id, "err", err)
	s.send(s.chat, protocol.Peer{Type: protocol.PeerError, ID: id, Message: err.Error()})
}

func (s *session) runRequest(ctx context.Context, m protocol.Peer) {
	defer s.untrack(m.ID)
	fail := func(err error) { s.failRequest(m.ID, err) }

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
	release, err := s.agent.gpu.acquire(ctx, gpuLLM)
	if err != nil {
		fail(err)
		return
	}
	defer release()

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

// maxPromptRunes caps an image prompt; FLUX's text encoder only reads the first ~512 tokens.
const maxPromptRunes = 2000

// enhanceInstructions has the LLM turn a short idea into a prompt FLUX does well with. FLUX
// follows English best, so this also translates a prompt written in another language.
const enhanceInstructions = "You write prompts for a text-to-image model. Rewrite the user's idea as one " +
	"detailed English prompt: the subject, setting, composition, lighting, colours and style. Keep " +
	"everything the user asked for and don't add people, text or logos they didn't ask for. Reply " +
	"with only the prompt, under 80 words, no quotes or preamble."

func (s *session) runImagine(ctx context.Context, m protocol.Peer) {
	defer s.untrack(m.ID)
	fail := func(err error) { s.failRequest(m.ID, err) }
	images := s.agent.cfg.Images
	if images == nil {
		fail(errors.New("this home server can't generate images"))
		return
	}
	prompt := []rune(strings.TrimSpace(m.Prompt))
	if len(prompt) == 0 {
		fail(errors.New("the image prompt is empty"))
		return
	}
	req := ImageRequest{Prompt: string(prompt[:min(len(prompt), maxPromptRunes)]), Seed: m.Seed}
	req.Width, req.Height = normalizeImageSize(m.Width, m.Height)
	if req.Seed <= 0 {
		req.Seed = randomSeed()
	}

	// One image at a time: a second job would only queue inside the image server anyway.
	select {
	case s.agent.imageSlots <- struct{}{}:
		defer func() { <-s.agent.imageSlots }()
	case <-ctx.Done():
		fail(ctx.Err())
		return
	}

	if m.Enhance {
		enhanced, err := s.enhancePrompt(ctx, req.Prompt)
		switch {
		case ctx.Err() != nil:
			fail(ctx.Err())
			return
		case err != nil:
			s.log.Warn("prompt enhancement failed; using the prompt as given", "id", m.ID, "err", err)
		case enhanced != "":
			req.Prompt = enhanced
			s.send(s.chat, protocol.Peer{Type: protocol.PeerImagePrompt, ID: m.ID, Prompt: enhanced})
		}
	}

	release, err := s.agent.gpu.acquire(ctx, gpuImage)
	if err != nil {
		fail(err)
		return
	}
	// Step 0 tells the phone the job has the GPU and the model is loading.
	s.send(s.chat, protocol.Peer{Type: protocol.PeerImageProgress, ID: m.ID, Step: 0})
	s.log.Info("generating image", "id", m.ID, "size", fmt.Sprintf("%dx%d", req.Width, req.Height), "seed", req.Seed)
	start := time.Now()
	img, err := images.Generate(ctx, req, func(step, steps int) {
		s.send(s.chat, protocol.Peer{Type: protocol.PeerImageProgress, ID: m.ID, Step: step, Steps: steps})
	})
	release()
	if err != nil {
		fail(err)
		return
	}
	s.log.Info("image done", "id", m.ID, "took", time.Since(start).Round(time.Millisecond), "bytes", len(img.Data))

	fileID := m.ID + "-image"
	if err := s.sendFile(ctx, fileID, img); err != nil {
		fail(err)
		return
	}
	s.send(s.chat, protocol.Peer{Type: protocol.PeerImageDone, ID: m.ID, FileID: fileID, Width: req.Width, Height: req.Height, Seed: req.Seed})
}

// enhancePrompt asks the LLM for a detailed version of a short image idea.
func (s *session) enhancePrompt(ctx context.Context, idea string) (string, error) {
	select {
	case s.agent.slots <- struct{}{}:
		defer func() { <-s.agent.slots }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	release, err := s.agent.gpu.acquire(ctx, gpuLLM)
	if err != nil {
		return "", err
	}
	defer release()
	var out strings.Builder
	err = s.agent.cfg.Backend.Stream(ctx, "", []LLMMessage{
		{Role: "system", Content: enhanceInstructions},
		{Role: "user", Content: idea},
	}, func(delta string) error {
		out.WriteString(delta)
		return nil
	})
	if err != nil {
		return "", err
	}
	text := strings.Trim(strings.TrimSpace(out.String()), "\"'“”")
	return string([]rune(text)[:min(len([]rune(text)), maxPromptRunes)]), nil
}

// sendFile sends an image to the phone on the files channel: file_start, binary chunks,
// file_end - the same framing the phone uses for photos it sends.
func (s *session) sendFile(ctx context.Context, id string, img Image) error {
	s.sendMu.Lock()
	dc := s.files
	s.sendMu.Unlock()
	if dc == nil {
		return errors.New("the phone hasn't opened its files channel")
	}
	s.fileOutMu.Lock()
	defer s.fileOutMu.Unlock()

	sum := sha256.Sum256(img.Data)
	s.send(dc, protocol.Peer{Type: protocol.PeerFileStart, ID: id, MIME: img.MIME, Size: int64(len(img.Data)), SHA256: hex.EncodeToString(sum[:])})
	for off := 0; off < len(img.Data); off += protocol.FileChunkSize {
		// Don't queue the whole file in SCTP's send buffer at once.
		for dc.BufferedAmount() > maxBufferedFileBytes {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(10 * time.Millisecond):
			}
		}
		chunk := img.Data[off:min(off+protocol.FileChunkSize, len(img.Data))]
		s.sendMu.Lock()
		err := dc.Send(chunk)
		s.sendMu.Unlock()
		if err != nil {
			return fmt.Errorf("sending the image: %w", err)
		}
	}
	s.send(dc, protocol.Peer{Type: protocol.PeerFileEnd, ID: id})
	return nil
}

// maxBufferedFileBytes is how much of an outgoing file may wait in the data channel's buffer.
const maxBufferedFileBytes = 1 << 20
