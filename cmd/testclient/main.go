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
		timeout     = flag.Duration("timeout", 2*time.Minute, "overall timeout")
	)
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	if err := run(ctx, *qrJSON, *qrFile, *user, *skipPair, *prompt, *image, *cancelAfter); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, qrJSON, qrFile, user string, skipPair bool, prompt, image string, cancelAfter time.Duration) error {
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
	files.OnMessage(func(m webrtc.DataChannelMessage) {
		var p protocol.Peer
		if json.Unmarshal(m.Data, &p) == nil {
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
	fmt.Printf("home server: name=%q model=%q vision=%v\n", hello.Name, hello.Model, hello.Vision)

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
