package tunnel

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/pion/webrtc/v4"

	"github.com/wweng/homellm/protocol"
)

const channelOpenTimeout = 15 * time.Second

// TokenFunc returns a fresh ID token for the signalling server (a Cognito ID token in
// production; any string on the dev server, where it becomes the user ID).
type TokenFunc func(ctx context.Context) (string, error)

// dialSignal connects to the signalling server as a client.
func dialSignal(ctx context.Context, signalURL string, token TokenFunc) (*websocket.Conn, error) {
	tok, err := token(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting token: %w", err)
	}
	u, err := url.Parse(signalURL)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("role", "phone")
	u.RawQuery = q.Encode()
	ws, _, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + tok}},
	})
	if err != nil {
		return nil, fmt.Errorf("signalling: %w", err)
	}
	ws.SetReadLimit(1 << 20)
	return ws, nil
}

// Pair redeems the pairing blob printed by `tunnel-home -pair`. It returns once the owner of
// the PC has approved (or rejected) it.
func Pair(ctx context.Context, qr protocol.PairingQR, token TokenFunc) (name string, err error) {
	ws, err := dialSignal(ctx, qr.Signal, token)
	if err != nil {
		return "", err
	}
	defer ws.CloseNow()
	if err := wsjson.Write(ctx, ws, protocol.Signal{Action: protocol.ActionPair, DeviceID: qr.DeviceID, Code: qr.Code}); err != nil {
		return "", err
	}
	for {
		var res protocol.Signal
		if err := wsjson.Read(ctx, ws, &res); err != nil {
			return "", err
		}
		switch res.Action {
		case protocol.ActionPairResult:
			if !res.OK {
				return "", fmt.Errorf("pairing failed: %s", res.Message)
			}
			return res.Name, nil
		case protocol.ActionError:
			return "", fmt.Errorf("pairing failed: %s", res.Message)
		}
	}
}

// Client is the Mac side: one peer connection to the paired PC, opened on demand and shared by
// every forwarded TCP connection.
type Client struct {
	qr    protocol.PairingQR
	token TokenFunc
	ice   []webrtc.ICEServer

	mu sync.Mutex
	pc *webrtc.PeerConnection
}

// NewClient creates a client for an already-paired PC.
func NewClient(qr protocol.PairingQR, token TokenFunc, ice []webrtc.ICEServer) *Client {
	return &Client{qr: qr, token: token, ice: ice}
}

// Listen accepts TCP connections on addr and forwards each to the PC's localhost port.
func (c *Client) Listen(ctx context.Context, addr string, port int) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	go func() { <-ctx.Done(); ln.Close() }()
	slog.Info("forwarding", "listen", ln.Addr().String(), "to", c.qr.Name, "port", port)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go func() {
			if err := c.forward(ctx, conn, port); err != nil {
				slog.Warn("forwarding failed", "err", err)
				conn.Close()
			}
		}()
	}
}

// forward opens a data channel for conn and pipes it. On error the caller closes conn.
func (c *Client) forward(ctx context.Context, conn net.Conn, port int) error {
	pc, err := c.peer(ctx)
	if err != nil {
		return err
	}
	dc, err := pc.CreateDataChannel(ChannelPrefix+strconv.Itoa(port), nil)
	if err != nil {
		return err
	}
	open := make(chan error, 1)
	dc.OnOpen(func() {
		raw, err := dc.Detach()
		if err != nil {
			open <- err
			return
		}
		open <- nil
		pipe(raw, dc, conn)
	})
	select {
	case err := <-open:
		return err
	case <-time.After(channelOpenTimeout):
		dc.Close()
		return errors.New("data channel did not open")
	case <-ctx.Done():
		dc.Close()
		return ctx.Err()
	}
}

// peer returns the connected peer connection, negotiating a new one if there isn't one.
func (c *Client) peer(ctx context.Context) (*webrtc.PeerConnection, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pc != nil {
		if c.pc.ConnectionState() == webrtc.PeerConnectionStateConnected {
			return c.pc, nil
		}
		c.pc.Close()
		c.pc = nil
	}
	pc, err := c.negotiate(ctx)
	if err != nil {
		return nil, err
	}
	c.pc = pc
	return pc, nil
}

// negotiate sets up a peer connection through signalling and checks the PC's signature on the
// answer against the key pinned at pairing.
func (c *Client) negotiate(ctx context.Context) (*webrtc.PeerConnection, error) {
	pub, err := base64.StdEncoding.DecodeString(c.qr.Pub)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return nil, errors.New("bad public key in pairing")
	}
	ws, err := dialSignal(ctx, c.qr.Signal, c.token)
	if err != nil {
		return nil, err
	}
	defer ws.CloseNow()

	pc, err := newAPI().NewPeerConnection(webrtc.Configuration{ICEServers: c.ice})
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			pc.Close()
		}
	}()
	ctl, err := pc.CreateDataChannel(controlChannel, nil)
	if err != nil {
		return nil, err
	}
	ctlOpen := make(chan struct{})
	ctl.OnOpen(func() { close(ctlOpen) })

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return nil, err
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		return nil, err
	}
	select {
	case <-gathered:
	case <-time.After(gatherTimeout):
		slog.Warn("ICE gathering timed out; offering the candidates found so far")
	}
	offerSDP := pc.LocalDescription().SDP
	session := randomHex(16)
	payload, _ := json.Marshal(protocol.Negotiation{Type: "offer", SDP: offerSDP})
	if err := wsjson.Write(ctx, ws, protocol.Signal{Action: protocol.ActionSignal, DeviceID: c.qr.DeviceID, Session: session, Payload: payload}); err != nil {
		return nil, err
	}

	var answer protocol.Negotiation
	for {
		var s protocol.Signal
		if err := wsjson.Read(ctx, ws, &s); err != nil {
			return nil, err
		}
		if s.Action == protocol.ActionError {
			return nil, fmt.Errorf("signalling error: %s", s.Message)
		}
		if s.Action == protocol.ActionSignal && s.Session == session {
			if err := json.Unmarshal(s.Payload, &answer); err != nil {
				return nil, err
			}
			break
		}
	}
	sig, _ := base64.StdEncoding.DecodeString(answer.Sig)
	if !ed25519.Verify(pub, protocol.AnswerSigningBytes(offerSDP, answer.SDP), sig) {
		return nil, errors.New("answer signature INVALID - refusing to connect")
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.SDP}); err != nil {
		return nil, err
	}
	// Signalling isn't needed from here on.
	ws.Close(websocket.StatusNormalClosure, "connected")

	select {
	case <-ctlOpen:
	case <-time.After(channelOpenTimeout + gatherTimeout):
		return nil, errors.New("peer connection did not open (a firewall may be blocking UDP; try -turn)")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if pair, err := pc.SCTP().Transport().ICETransport().GetSelectedCandidatePair(); err == nil && pair != nil {
		path := "direct"
		if pair.Local.Typ == webrtc.ICECandidateTypeRelay || pair.Remote.Typ == webrtc.ICECandidateTypeRelay {
			path = "relayed via TURN"
		}
		slog.Info("connected", "to", c.qr.Name, "path", path, "local", pair.Local.Typ.String(), "remote", pair.Remote.Typ.String())
	}
	ok = true
	return pc, nil
}
