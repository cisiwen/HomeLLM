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
	// videoJobs are clip renders, which outlive the session (see runAnimate): close doesn't
	// cancel them, only an explicit cancel does.
	videoJobs map[string]context.CancelFunc
	closed    bool
}

// newSession answers a phone's offer. It returns the signed answer to relay back.
func newSession(a *Agent, id, userID string, offer protocol.Negotiation) (*session, protocol.Negotiation, error) {
	s := &session{
		id:        id,
		userID:    userID,
		agent:     a,
		log:       slog.With("session", id[:min(8, len(id))], "user", userID),
		images:    newImageStore(),
		inflight:  map[string]context.CancelFunc{},
		videoJobs: map[string]context.CancelFunc{},
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
			Images: s.agent.cfg.Images != nil, Videos: s.agent.cfg.Videos != nil, Sound: s.agent.cfg.Sound != nil,
			Music: s.agent.canMakeMusic()})
		go s.deliverHeld()
	case protocol.PeerRequest:
		if ctx, ok := s.track(m.ID); ok {
			go s.runRequest(ctx, m)
		}
	case protocol.PeerImagine:
		if ctx, ok := s.track(m.ID); ok {
			go s.runImagine(ctx, m)
		}
	case protocol.PeerAnimate:
		if ctx, ok := s.trackVideo(m.ID); ok {
			go s.runAnimate(ctx, m)
		}
	case protocol.PeerAddSound:
		if ctx, ok := s.trackVideo(m.ID); ok {
			go s.runAddSound(ctx, m)
		}
	case protocol.PeerAddMusic:
		if ctx, ok := s.trackVideo(m.ID); ok {
			go s.runAddMusic(ctx, m)
		}
	case protocol.PeerCancel:
		s.mu.Lock()
		cancel := s.inflight[m.ID]
		if cancel == nil {
			cancel = s.videoJobs[m.ID]
		}
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
	return s.rewrite(ctx, enhanceInstructions, idea)
}

// rewrite runs one short instruction-following request on the LLM (prompt rewriting).
func (s *session) rewrite(ctx context.Context, instructions, text string) (string, error) {
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
		{Role: "system", Content: instructions},
		{Role: "user", Content: text},
	}, func(delta string) error {
		out.WriteString(delta)
		return nil
	})
	if err != nil {
		return "", err
	}
	result := []rune(strings.Trim(strings.TrimSpace(out.String()), "\"'“”"))
	return string(result[:min(len(result), maxPromptRunes)]), nil
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

// videoJobTimeout bounds one clip render, which otherwise runs on after its phone leaves.
const videoJobTimeout = 30 * time.Minute

// trackVideo registers a clip render. Its context isn't tied to the session: a render takes
// minutes, long enough for a phone to lock its screen or lose signal.
func (s *session) trackVideo(id string) (context.Context, bool) {
	if id == "" {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), videoJobTimeout)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.videoJobs[id]; s.closed || dup {
		cancel()
		return nil, false
	}
	s.videoJobs[id] = cancel
	return ctx, true
}

func (s *session) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *session) runAnimate(ctx context.Context, m protocol.Peer) {
	defer func() {
		s.mu.Lock()
		if c := s.videoJobs[m.ID]; c != nil {
			c()
			delete(s.videoJobs, m.ID)
		}
		s.mu.Unlock()
	}()
	fail := func(err error) { s.failRequest(m.ID, err) }
	videos := s.agent.cfg.Videos
	if videos == nil {
		fail(errors.New("this home server can't make videos"))
		return
	}
	prompt := []rune(strings.TrimSpace(m.Prompt))
	if len(prompt) == 0 {
		fail(errors.New("describe the motion you want"))
		return
	}
	img, err := s.images.Wait(ctx, m.ImageID)
	if err != nil {
		fail(err)
		return
	}
	req := VideoRequest{
		Image: img, Prompt: string(prompt[:min(len(prompt), maxPromptRunes)]),
		Frames: NormalizeFrames(m.Frames), FPS: videoFPS, Seed: m.Seed,
	}
	req.Width, req.Height = VideoSize(img)
	if req.Seed <= 0 {
		req.Seed = randomSeed()
	}
	withSound := m.Sound && s.agent.cfg.Sound != nil
	withMusic := m.Music && s.agent.canMakeMusic()

	select {
	case s.agent.imageSlots <- struct{}{}:
		defer func() { <-s.agent.imageSlots }()
	case <-ctx.Done():
		fail(ctx.Err())
		return
	}
	// The sound description comes from the motion prompt alone, so work it out before the clip:
	// the LLM then loads once, before Wan, instead of between Wan and Foley.
	soundPrompt := ""
	if withSound {
		soundPrompt = s.describeSound(ctx, req.Prompt)
	}
	var music MusicRequest
	if withMusic {
		music = s.musicBrief(ctx, req.Prompt, float64(req.Frames)/float64(req.FPS), req.Seed)
	}

	release, err := s.agent.gpu.acquire(ctx, gpuImage)
	if err != nil {
		fail(err)
		return
	}
	s.send(s.chat, protocol.Peer{Type: protocol.PeerImageProgress, ID: m.ID, Step: 0})
	s.log.Info("animating", "id", m.ID, "size", fmt.Sprintf("%dx%d", req.Width, req.Height), "frames", req.Frames, "seed", req.Seed)
	start := time.Now()
	clip, err := videos.Animate(ctx, req, func(step, steps int) {
		s.send(s.chat, protocol.Peer{Type: protocol.PeerImageProgress, ID: m.ID, Step: step, Steps: steps})
	})
	release()
	if err != nil {
		fail(err)
		return
	}
	s.log.Info("clip done", "id", m.ID, "took", time.Since(start).Round(time.Second), "bytes", len(clip.Data))

	done := protocol.Peer{
		Type: protocol.PeerVideoDone, ID: m.ID, FileID: m.ID + "-video",
		Width: req.Width, Height: req.Height, Frames: req.Frames, FPS: req.FPS, Seed: req.Seed,
	}
	// Music before sound: ACE-Step runs in ComfyUI, which still has the GPU after Wan.
	var track Audio
	var notes []string
	if withMusic {
		var err error
		if track, err = s.composeMusic(ctx, m.ID, music); err != nil {
			if ctx.Err() != nil {
				fail(ctx.Err())
				return
			}
			s.log.Warn("composing music failed", "id", m.ID, "err", err)
			notes = append(notes, "couldn't add music: "+err.Error())
			withMusic = false
		}
	}
	if withSound {
		// A failed soundtrack shouldn't cost the clip: send it silent, saying why.
		if voiced, err := s.addSound(ctx, m.ID, clip, soundPrompt, req.Seed); err != nil {
			if ctx.Err() != nil {
				fail(ctx.Err())
				return
			}
			s.log.Warn("adding sound failed; sending the clip without it", "id", m.ID, "err", err)
			notes = append(notes, "couldn't add sound: "+err.Error())
		} else {
			clip = voiced
			done.Prompt = soundPrompt
		}
	}
	if withMusic {
		if mixed, err := s.agent.cfg.Mixer.Mix(ctx, clip, track); err != nil {
			s.log.Warn("mixing music failed", "id", m.ID, "err", err)
			notes = append(notes, "couldn't add music: "+err.Error())
		} else {
			clip = mixed
			done.Prompt = strings.TrimPrefix(done.Prompt+" · music: "+music.Tags, " · ")
		}
	}
	done.Message = strings.Join(notes, "; ")
	if s.isClosed() || s.deliverClip(context.Background(), done, clip) != nil {
		s.log.Info("phone has gone; holding the clip for its next visit", "id", m.ID)
		s.agent.hold(s.userID, heldClip{done: done, video: clip, expires: time.Now().Add(heldClipTTL)})
	}
}

// deliverClip sends a finished clip and its video_done.
func (s *session) deliverClip(ctx context.Context, done protocol.Peer, clip Video) error {
	if err := s.sendFile(ctx, done.FileID, Image{MIME: clip.MIME, Data: clip.Data}); err != nil {
		return err
	}
	s.send(s.chat, done)
	return nil
}

// deliverHeld sends clips that finished while this user's phone was away.
func (s *session) deliverHeld() {
	for _, c := range s.agent.takeHeld(s.userID) {
		s.log.Info("delivering a clip that finished while the phone was away", "id", c.done.ID)
		if err := s.deliverClip(context.Background(), c.done, c.video); err != nil {
			s.agent.hold(s.userID, c)
			return
		}
	}
}

// describeSound has the LLM turn a clip description into a sound description, falling back to
// the description itself.
func (s *session) describeSound(ctx context.Context, clipDescription string) string {
	text, err := s.rewrite(ctx, soundInstructions, clipDescription)
	if err != nil || text == "" {
		if ctx.Err() == nil {
			s.log.Warn("sound description failed; using the clip description", "err", err)
		}
		return clipDescription
	}
	return text
}

// addSound runs the sound model on a finished clip, reporting the stage to the phone.
func (s *session) addSound(ctx context.Context, id string, clip Video, prompt string, seed int64) (Video, error) {
	release, err := s.agent.gpu.acquire(ctx, gpuSound)
	if err != nil {
		return Video{}, err
	}
	defer release()
	s.send(s.chat, protocol.Peer{Type: protocol.PeerImageProgress, ID: id, Stage: "sound"})
	s.log.Info("adding sound", "id", id, "prompt", prompt)
	start := time.Now()
	out, err := s.agent.cfg.Sound.AddSound(ctx, clip, prompt, seed)
	if err == nil {
		s.log.Info("sound added", "id", id, "took", time.Since(start).Round(time.Second), "bytes", len(out.Data))
	}
	return out, err
}

// runAddSound gives an existing clip (sent back by the phone) a soundtrack.
func (s *session) runAddSound(ctx context.Context, m protocol.Peer) {
	defer func() {
		s.mu.Lock()
		if c := s.videoJobs[m.ID]; c != nil {
			c()
			delete(s.videoJobs, m.ID)
		}
		s.mu.Unlock()
	}()
	fail := func(err error) { s.failRequest(m.ID, err) }
	if s.agent.cfg.Sound == nil {
		fail(errors.New("this home server can't add sound"))
		return
	}
	file, err := s.images.Wait(ctx, m.ImageID)
	if err != nil {
		fail(err)
		return
	}
	if file.MIME != "video/mp4" {
		fail(errors.New("send the clip as an MP4"))
		return
	}
	prompt := strings.TrimSpace(m.Prompt)
	// The sound model's text encoder only reads English: have the LLM rewrite anything else.
	if prompt != "" && (m.Enhance || !isASCII(prompt)) {
		prompt = s.describeSound(ctx, prompt)
	}
	select {
	case s.agent.imageSlots <- struct{}{}:
		defer func() { <-s.agent.imageSlots }()
	case <-ctx.Done():
		fail(ctx.Err())
		return
	}
	seed := m.Seed
	if seed <= 0 {
		seed = randomSeed()
	}
	voiced, err := s.addSound(ctx, m.ID, Video{MIME: file.MIME, Data: file.Data}, prompt, seed)
	if err != nil {
		fail(err)
		return
	}
	done := protocol.Peer{Type: protocol.PeerVideoDone, ID: m.ID, FileID: m.ID + "-video", Prompt: prompt, Seed: seed}
	if s.isClosed() || s.deliverClip(context.Background(), done, voiced) != nil {
		s.agent.hold(s.userID, heldClip{done: done, video: voiced, expires: time.Now().Add(heldClipTTL)})
	}
}

func isASCII(s string) bool {
	for _, r := range s {
		if r > 127 {
			return false
		}
	}
	return true
}

// musicBrief has the LLM pick a style, tempo and key for a clip, for a track a little longer
// than the clip itself (the mixer trims and fades it).
func (s *session) musicBrief(ctx context.Context, clipDescription string, clipSeconds float64, seed int64) MusicRequest {
	reply, err := s.rewrite(ctx, musicInstructions, clipDescription)
	if err != nil && ctx.Err() == nil {
		s.log.Warn("music brief failed; using a default style", "err", err)
	}
	tags, bpm, key := parseMusicBrief(reply)
	if clipSeconds <= 0 {
		clipSeconds = 5
	}
	return MusicRequest{Tags: tags, BPM: bpm, Key: key, Seconds: clipSeconds + 2, Seed: seed}
}

// composeMusic runs the music model (in ComfyUI), reporting the stage to the phone.
func (s *session) composeMusic(ctx context.Context, id string, req MusicRequest) (Audio, error) {
	release, err := s.agent.gpu.acquire(ctx, gpuImage)
	if err != nil {
		return Audio{}, err
	}
	defer release()
	s.send(s.chat, protocol.Peer{Type: protocol.PeerImageProgress, ID: id, Stage: "music"})
	s.log.Info("composing music", "id", id, "tags", req.Tags, "bpm", req.BPM, "key", req.Key)
	start := time.Now()
	track, err := s.agent.cfg.Music.Compose(ctx, req)
	if err == nil {
		s.log.Info("music composed", "id", id, "took", time.Since(start).Round(time.Second), "bytes", len(track.Data))
	}
	return track, err
}

// runAddMusic lays background music under an existing clip (sent back by the phone).
func (s *session) runAddMusic(ctx context.Context, m protocol.Peer) {
	defer func() {
		s.mu.Lock()
		if c := s.videoJobs[m.ID]; c != nil {
			c()
			delete(s.videoJobs, m.ID)
		}
		s.mu.Unlock()
	}()
	fail := func(err error) { s.failRequest(m.ID, err) }
	if !s.agent.canMakeMusic() {
		fail(errors.New("this home server can't add music"))
		return
	}
	file, err := s.images.Wait(ctx, m.ImageID)
	if err != nil {
		fail(err)
		return
	}
	if file.MIME != "video/mp4" {
		fail(errors.New("send the clip as an MP4"))
		return
	}
	clip := Video{MIME: file.MIME, Data: file.Data}
	seconds, err := s.agent.cfg.Mixer.Duration(ctx, clip)
	if err != nil {
		fail(err)
		return
	}
	seed := m.Seed
	if seed <= 0 {
		seed = randomSeed()
	}
	description := strings.TrimSpace(m.Prompt)
	if description == "" {
		description = "a short video clip"
	}
	brief := s.musicBrief(ctx, description, seconds, seed)
	select {
	case s.agent.imageSlots <- struct{}{}:
		defer func() { <-s.agent.imageSlots }()
	case <-ctx.Done():
		fail(ctx.Err())
		return
	}
	track, err := s.composeMusic(ctx, m.ID, brief)
	if err != nil {
		fail(err)
		return
	}
	mixed, err := s.agent.cfg.Mixer.Mix(ctx, clip, track)
	if err != nil {
		fail(err)
		return
	}
	done := protocol.Peer{Type: protocol.PeerVideoDone, ID: m.ID, FileID: m.ID + "-video", Prompt: "music: " + brief.Tags, Seed: seed}
	if s.isClosed() || s.deliverClip(context.Background(), done, mixed) != nil {
		s.agent.hold(s.userID, heldClip{done: done, video: mixed, expires: time.Now().Add(heldClipTTL)})
	}
}
