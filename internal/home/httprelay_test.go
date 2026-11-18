package home

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/wweng/homellm/protocol"
)

// relayPair connects two in-process peers and puts an httpRelay on the answerer's end of a
// ChannelHTTP channel; it returns the client end.
func relayPair(t *testing.T, apps map[string]*url.URL) *relayClientEnd {
	t.Helper()
	offerer, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	answerer, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { offerer.Close(); answerer.Close() })

	var relay *httpRelay
	answerer.OnDataChannel(func(dc *webrtc.DataChannel) {
		relay = newHTTPRelay(dc, apps, slog.Default())
	})
	c := &relayClientEnd{t: t, msgs: map[uint32][]protocol.HTTPMsg{}, bodies: map[uint32]*bytes.Buffer{}}
	dc, err := offerer.CreateDataChannel(protocol.ChannelHTTP, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.dc = dc
	open := make(chan struct{})
	dc.OnOpen(func() { close(open) })
	dc.OnMessage(c.onMessage)

	offer, _ := offerer.CreateOffer(nil)
	gather := webrtc.GatheringCompletePromise(offerer)
	offerer.SetLocalDescription(offer)
	<-gather
	answerer.SetRemoteDescription(*offerer.LocalDescription())
	answer, _ := answerer.CreateAnswer(nil)
	gather = webrtc.GatheringCompletePromise(answerer)
	answerer.SetLocalDescription(answer)
	<-gather
	offerer.SetRemoteDescription(*answerer.LocalDescription())
	select {
	case <-open:
	case <-time.After(10 * time.Second):
		t.Fatal("data channel never opened")
	}
	t.Cleanup(func() {
		if relay != nil {
			relay.close()
		}
	})
	return c
}

type relayClientEnd struct {
	t  *testing.T
	dc *webrtc.DataChannel

	mu     sync.Mutex
	msgs   map[uint32][]protocol.HTTPMsg
	bodies map[uint32]*bytes.Buffer
}

func (c *relayClientEnd) onMessage(m webrtc.DataChannelMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if m.IsString {
		var h protocol.HTTPMsg
		json.Unmarshal(m.Data, &h)
		c.msgs[h.N] = append(c.msgs[h.N], h)
		return
	}
	n, data, _ := protocol.ParseHTTPFrame(m.Data)
	if c.bodies[n] == nil {
		c.bodies[n] = &bytes.Buffer{}
	}
	c.bodies[n].Write(data)
}

func (c *relayClientEnd) send(m protocol.HTTPMsg) {
	data, _ := json.Marshal(m)
	c.dc.SendText(string(data))
}

// waitFor waits until request n got a message of type typ (or any end, if typ is "").
func (c *relayClientEnd) waitFor(n uint32, types ...string) protocol.HTTPMsg {
	c.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		for _, m := range c.msgs[n] {
			for _, typ := range types {
				if m.Type == typ {
					c.mu.Unlock()
					return m
				}
			}
		}
		c.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	c.t.Fatalf("request %d: no %v", n, types)
	return protocol.HTTPMsg{}
}

func (c *relayClientEnd) body(n uint32) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.bodies[n] == nil {
		return nil
	}
	return append([]byte(nil), c.bodies[n].Bytes()...)
}

func TestHTTPRelay(t *testing.T) {
	file := make([]byte, 300*1024)
	rand.Read(file)
	var uploaded []byte
	var mu sync.Mutex
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/media/clip.mp4":
			http.ServeContent(w, r, "clip.mp4", time.Time{}, bytes.NewReader(file))
		case "/api/upload":
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			uploaded = b
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"ok":true,"q":"` + r.URL.Query().Get("x") + `"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer app.Close()
	base, _ := url.Parse(app.URL)
	c := relayPair(t, map[string]*url.URL{"dubbing": base})

	// A whole file, granted credit in steps: nothing flows beyond what was granted.
	c.send(protocol.HTTPMsg{Type: protocol.HTTPReq, N: 1, App: "dubbing", Method: "GET", Path: "/media/clip.mp4"})
	res := c.waitFor(1, protocol.HTTPRes)
	if res.Status != 200 || res.Headers["Content-Length"] != "307200" {
		t.Fatalf("res = %+v", res)
	}
	c.send(protocol.HTTPMsg{Type: protocol.HTTPCredit, N: 1, Credit: 64 * 1024})
	time.Sleep(300 * time.Millisecond)
	if got := len(c.body(1)); got != 64*1024 {
		t.Fatalf("got %d bytes with 64 KiB of credit", got)
	}
	c.send(protocol.HTTPMsg{Type: protocol.HTTPCredit, N: 1, Credit: 1 << 20})
	c.waitFor(1, protocol.HTTPResEnd)
	if !bytes.Equal(c.body(1), file) {
		t.Fatal("body differs")
	}

	// A range, as a video element asks for when seeking.
	c.send(protocol.HTTPMsg{Type: protocol.HTTPReq, N: 2, App: "dubbing", Method: "GET", Path: "/media/clip.mp4",
		Headers: map[string]string{"Range": "bytes=1000-1999"}})
	c.send(protocol.HTTPMsg{Type: protocol.HTTPCredit, N: 2, Credit: 1 << 20})
	res = c.waitFor(2, protocol.HTTPRes)
	c.waitFor(2, protocol.HTTPResEnd)
	if res.Status != 206 || res.Headers["Content-Range"] != "bytes 1000-1999/307200" || !bytes.Equal(c.body(2), file[1000:2000]) {
		t.Fatalf("range: %+v, %d bytes", res, len(c.body(2)))
	}

	// An upload in body frames.
	payload := bytes.Repeat([]byte("abcdefgh"), 10000)
	c.send(protocol.HTTPMsg{Type: protocol.HTTPReq, N: 3, App: "dubbing", Method: "POST", Path: "/api/upload?x=1", Body: true,
		Headers: map[string]string{"Content-Type": "application/octet-stream", "Content-Length": "80000"}})
	for off := 0; off < len(payload); off += 16 * 1024 {
		c.dc.Send(protocol.HTTPFrame(3, payload[off:min(off+16*1024, len(payload))]))
	}
	c.send(protocol.HTTPMsg{Type: protocol.HTTPReqEnd, N: 3})
	c.send(protocol.HTTPMsg{Type: protocol.HTTPCredit, N: 3, Credit: 1 << 20})
	c.waitFor(3, protocol.HTTPResEnd)
	mu.Lock()
	ok := bytes.Equal(uploaded, payload)
	mu.Unlock()
	if !ok || !strings.Contains(string(c.body(3)), `"q":"1"`) {
		t.Fatalf("upload: ok=%v reply=%s", ok, c.body(3))
	}

	// Cancelling stops the stream.
	c.send(protocol.HTTPMsg{Type: protocol.HTTPReq, N: 4, App: "dubbing", Method: "GET", Path: "/media/clip.mp4"})
	c.waitFor(4, protocol.HTTPRes)
	c.send(protocol.HTTPMsg{Type: protocol.HTTPCredit, N: 4, Credit: 16 * 1024})
	time.Sleep(100 * time.Millisecond)
	c.send(protocol.HTTPMsg{Type: protocol.HTTPCancel, N: 4})
	c.send(protocol.HTTPMsg{Type: protocol.HTTPCredit, N: 4, Credit: 1 << 20})
	time.Sleep(300 * time.Millisecond)
	if got := len(c.body(4)); got > 16*1024 {
		t.Fatalf("%d bytes arrived after cancelling", got)
	}

	// Only listed apps, only paths on them.
	c.send(protocol.HTTPMsg{Type: protocol.HTTPReq, N: 5, App: "router", Method: "GET", Path: "/"})
	if m := c.waitFor(5, protocol.HTTPResError); !strings.Contains(m.Message, "no such app") {
		t.Fatalf("unknown app: %+v", m)
	}
	c.send(protocol.HTTPMsg{Type: protocol.HTTPReq, N: 6, App: "dubbing", Method: "GET", Path: "//evil.example/x"})
	c.waitFor(6, protocol.HTTPResError)
}

func TestParseApps(t *testing.T) {
	if _, err := ParseApps([]string{"dubbing=http://127.0.0.1:8000"}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"x=http://192.168.1.1/", "x=https://127.0.0.1/", "nourl", "=http://127.0.0.1/"} {
		if _, err := ParseApps([]string{bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
