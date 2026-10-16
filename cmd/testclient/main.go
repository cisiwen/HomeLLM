// Command testclient plays the phone's role, so the home server can be tested without a phone:
// it pairs using the QR JSON, connects over WebRTC, optionally sends an image, and streams a
// reply.
//
//	testclient -qr-file qr.json -user alice -prompt "hello" -image cat.jpg
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/pion/webrtc/v4"

	"github.com/wweng/homellm/protocol"
)

func main() {
	var (
		qrJSON      = flag.String("qr", "", "pairing QR JSON")
		qrFile      = flag.String("qr-file", "", "file containing the pairing QR JSON")
		user        = flag.String("user", "alice", "ID token; the dev signalling server accepts any string as the user ID")
		skipPair    = flag.Bool("skip-pair", false, "already paired: only use the QR's signal URL, device ID and key")
		prompt      = flag.String("prompt", "Hello from the test client", "message to send")
		image       = flag.String("image", "", "optional image file to attach")
		cancelAfter = flag.Duration("cancel-after", 0, "send cancel after this long (tests cancellation)")
		imagine     = flag.String("imagine", "", "generate an image from this prompt instead of chatting")
		enhance     = flag.Bool("enhance", false, "with -imagine: let the LLM rewrite the prompt first")
		out         = flag.String("out", "image.jpg", "with -imagine/-animate: where to save the result")
		animate     = flag.String("animate", "", "with -image: animate it, with this motion prompt")
		timeout     = flag.Duration("timeout", 2*time.Minute, "overall timeout")
	)
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	opts := imagineOpts{prompt: *imagine, enhance: *enhance, out: *out, animate: *animate}
	if err := run(ctx, *qrJSON, *qrFile, *user, *skipPair, *prompt, *image, *cancelAfter, opts); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
}

type imagineOpts struct {
	prompt  string
	enhance bool
	out     string
	animate string
}

func run(ctx context.Context, qrJSON, qrFile, user string, skipPair bool, prompt, image string, cancelAfter time.Duration, im imagineOpts) error {
	if qrFile != "" {
		b, err := os.ReadFile(qrFile)
		if err != nil {
			return err
		}
		qrJSON = string(b)
	}
	var qr protocol.PairingQR
	if err := json.Unmarshal([]byte(qrJSON), &qr); err != nil {
		return fmt.Errorf("bad QR JSON: %w", err)
	}
	pub, err := base64.StdEncoding.DecodeString(qr.Pub)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return errors.New("bad public key in QR")
	}

	// 1. Signalling.
	u, err := url.Parse(qr.Signal)
	if err != nil {
		return err
	}
	q := u.Query()
	q.Set("role", "phone")
	u.RawQuery = q.Encode()
	ws, _, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + user}},
	})
	if err != nil {
		return fmt.Errorf("signalling: %w", err)
	}
	defer ws.CloseNow()
	read := func() (protocol.Signal, error) {
		var s protocol.Signal
		err := wsjson.Read(ctx, ws, &s)
		return s, err
	}

	// 2. Pairing.
	if !skipPair {
		if err := wsjson.Write(ctx, ws, protocol.Signal{Action: protocol.ActionPair, DeviceID: qr.DeviceID, Code: qr.Code}); err != nil {
			return err
		}
		res, err := read()
		if err != nil {
			return err
		}
		if res.Action != protocol.ActionPairResult || !res.OK {
			return fmt.Errorf("pairing failed: %s %s", res.Action, res.Message)
		}
		fmt.Printf("paired with %q\n", res.Name)
	}

	// 3. WebRTC offer.
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}},
	})
	if err != nil {
		return err
	}
	defer pc.Close()
	chat, err := pc.CreateDataChannel(protocol.ChannelChat, nil)
	if err != nil {
		return err
	}
	files, err := pc.CreateDataChannel(protocol.ChannelFiles, nil)
	if err != nil {
		return err
	}
	chatOpen, filesOpen := make(chan struct{}), make(chan struct{})
	chat.OnOpen(func() { close(chatOpen) })
	files.OnOpen(func() { close(filesOpen) })
	incoming := make(chan protocol.Peer, 256)
	chat.OnMessage(func(m webrtc.DataChannelMessage) {
		var p protocol.Peer
		if json.Unmarshal(m.Data, &p) == nil {
			incoming <- p
		}
	})
	// Files from the home server (generated images): file_start, binary chunks, file_end.
	// Pion delivers one channel's messages in order, so this needs no locking.
	var fileBuf []byte
	received := make(chan []byte, 4)
	files.OnMessage(func(m webrtc.DataChannelMessage) {
		if !m.IsString {
			fileBuf = append(fileBuf, m.Data...)
			return
		}
		var p protocol.Peer
		if json.Unmarshal(m.Data, &p) != nil {
			return
		}
		switch p.Type {
		case protocol.PeerFileStart:
			fileBuf = make([]byte, 0, p.Size)
		case protocol.PeerFileEnd:
			received <- fileBuf
			fileBuf = nil
		default:
			incoming <- p
		}
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		return err
	}
	<-gathered
	offerSDP := pc.LocalDescription().SDP
	session := randomHex(16)
	payload, _ := json.Marshal(protocol.Negotiation{Type: "offer", SDP: offerSDP})
	if err := wsjson.Write(ctx, ws, protocol.Signal{Action: protocol.ActionSignal, DeviceID: qr.DeviceID, Session: session, Payload: payload}); err != nil {
		return err
	}

	// 4. Verified answer.
	var answer protocol.Negotiation
	for {
		s, err := read()
		if err != nil {
			return err
		}
		if s.Action == protocol.ActionError {
			return fmt.Errorf("signalling error: %s", s.Message)
		}
		if s.Action == protocol.ActionSignal && s.Session == session {
			if err := json.Unmarshal(s.Payload, &answer); err != nil {
				return err
			}
			break
		}
	}
	sig, _ := base64.StdEncoding.DecodeString(answer.Sig)
	if !ed25519.Verify(pub, protocol.AnswerSigningBytes(offerSDP, answer.SDP), sig) {
		return errors.New("answer signature INVALID - refusing to connect")
	}
	fmt.Println("answer signature verified")
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.SDP}); err != nil {
		return err
	}
	// From here on the signalling connection isn't needed at all.
	ws.Close(websocket.StatusNormalClosure, "connected")

	select {
	case <-chatOpen:
	case <-ctx.Done():
		return errors.New("data channel never opened")
	}
	if pair, err := pc.SCTP().Transport().ICETransport().GetSelectedCandidatePair(); err == nil && pair != nil {
		fmt.Printf("connected: local=%s remote=%s\n", pair.Local.Typ, pair.Remote.Typ)
	}

	send := func(dc *webrtc.DataChannel, p protocol.Peer) error {
		b, _ := json.Marshal(p)
		return dc.SendText(string(b))
	}
	if err := send(chat, protocol.Peer{Type: protocol.PeerHello}); err != nil {
		return err
	}
	hello, err := next(ctx, incoming)
	if err != nil {
		return err
	}
	fmt.Printf("home server: name=%q model=%q vision=%v images=%v\n", hello.Name, hello.Model, hello.Vision, hello.Images)
	if im.prompt != "" {
		<-filesOpen
		return runImagine(ctx, im, func(p protocol.Peer) error { return send(chat, p) }, incoming, received)
	}

	// 5. Optional image, chunked with flow control.
	var imageIDs []string
	if image != "" {
		data, err := os.ReadFile(image)
		if err != nil {
			return err
		}
		<-filesOpen
		id := "img-" + randomHex(4)
		sum := sha256.Sum256(data)
		mt := mime.TypeByExtension(strings.ToLower(filepath.Ext(image)))
		if err := send(files, protocol.Peer{Type: protocol.PeerFileStart, ID: id, MIME: mt, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}); err != nil {
			return err
		}
		for off := 0; off < len(data); off += protocol.FileChunkSize {
			for files.BufferedAmount() > 1<<20 {
				time.Sleep(5 * time.Millisecond)
			}
			if err := files.Send(data[off:min(off+protocol.FileChunkSize, len(data))]); err != nil {
				return err
			}
		}
		if err := send(files, protocol.Peer{Type: protocol.PeerFileEnd, ID: id}); err != nil {
			return err
		}
		imageIDs = append(imageIDs, id)
		fmt.Printf("sent image %s (%d bytes, %s)\n", id, len(data), mt)
	}

	if im.animate != "" {
		if len(imageIDs) == 0 {
			return errors.New("-animate needs -image")
		}
		id := "vid-" + randomHex(4)
		if err := send(chat, protocol.Peer{Type: protocol.PeerAnimate, ID: id, ImageID: imageIDs[0], Prompt: im.animate}); err != nil {
			return err
		}
		return waitResult(ctx, im.out, protocol.PeerVideoDone, incoming, received)
	}

	// 6. Request and streamed reply.
	reqID := "req-" + randomHex(4)
	if err := send(chat, protocol.Peer{Type: protocol.PeerRequest, ID: reqID, Messages: []protocol.ChatMessage{{Role: "user", Content: prompt, ImageIDs: imageIDs}}}); err != nil {
		return err
	}
	if cancelAfter > 0 {
		time.AfterFunc(cancelAfter, func() {
			fmt.Print(" <cancel> ")
			send(chat, protocol.Peer{Type: protocol.PeerCancel, ID: reqID})
		})
	}
	start := time.Now()
	for {
		p, err := next(ctx, incoming)
		if err != nil {
			return err
		}
		switch p.Type {
		case protocol.PeerChunk:
			fmt.Print(p.Delta)
		case protocol.PeerDone:
			fmt.Printf("\n[done in %s]\n", time.Since(start).Round(time.Millisecond))
			return nil
		case protocol.PeerError, protocol.PeerFileError:
			return fmt.Errorf("home server error: %s", p.Message)
		}
	}
}

func next(ctx context.Context, ch <-chan protocol.Peer) (protocol.Peer, error) {
	select {
	case p := <-ch:
		return p, nil
	case <-ctx.Done():
		return protocol.Peer{}, ctx.Err()
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// runImagine asks for an image and saves it.
func runImagine(ctx context.Context, im imagineOpts, send func(protocol.Peer) error, incoming <-chan protocol.Peer, received <-chan []byte) error {
	id := "img-" + randomHex(4)
	if err := send(protocol.Peer{Type: protocol.PeerImagine, ID: id, Prompt: im.prompt, Enhance: im.enhance}); err != nil {
		return err
	}
	return waitResult(ctx, im.out, protocol.PeerImageDone, incoming, received)
}

// waitResult prints progress until a generated image or clip (doneType) and its file have
// both arrived, then saves the file.
func waitResult(ctx context.Context, out, doneType string, incoming <-chan protocol.Peer, received <-chan []byte) error {
	start := time.Now()
	var done *protocol.Peer
	var data []byte
	for done == nil || data == nil {
		select {
		case b := <-received:
			data = b
			continue
		case <-ctx.Done():
			return ctx.Err()
		case p := <-incoming:
			switch p.Type {
			case protocol.PeerImagePrompt:
				fmt.Printf("enhanced prompt: %s\n", p.Prompt)
			case protocol.PeerImageProgress:
				if p.Step == 0 {
					fmt.Printf("[%s] loading model...\n", time.Since(start).Round(100*time.Millisecond))
				} else {
					fmt.Printf("[%s] step %d/%d\n", time.Since(start).Round(100*time.Millisecond), p.Step, p.Steps)
				}
			case doneType:
				done = &p
			case protocol.PeerError, protocol.PeerFileError:
				return fmt.Errorf("home server error: %s", p.Message)
			}
		}
	}
	if err := os.WriteFile(out, data, 0o644); err != nil {
		return err
	}
	fmt.Printf("[done in %s] %dx%d seed=%d, %d bytes -> %s\n", time.Since(start).Round(time.Millisecond), done.Width, done.Height, done.Seed, len(data), out)
	return nil
}
