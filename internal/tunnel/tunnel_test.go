package tunnel

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wweng/homellm/internal/home"
	"github.com/wweng/homellm/internal/signaldev"
)

// echoServer accepts connections on a random localhost port and echoes everything back.
func echoServer(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestForwardsAllowedPortOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	srv, err := signaldev.New("")
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(srv)
	defer hs.Close()
	signalURL := "ws" + strings.TrimPrefix(hs.URL, "http")

	allowed, blocked := echoServer(t), echoServer(t)
	id, err := home.LoadOrCreateIdentity(t.TempDir(), "test-pc")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHome(HomeConfig{
		SignalURL: signalURL,
		Ports:     []int{allowed},
		Approve:   func(context.Context, string) bool { return true },
	}, id)
	go h.Run(ctx)
	for !h.Connected() {
		time.Sleep(20 * time.Millisecond)
	}

	qr, err := h.NewPairingQR(ctx)
	if err != nil {
		t.Fatal(err)
	}
	token := func(context.Context) (string, error) { return "alice", nil }
	if _, err := Pair(ctx, qr, token); err != nil {
		t.Fatalf("pair: %v", err)
	}

	c := NewClient(qr, token, nil)
	listen := func(port int) string {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		ln.Close()
		go c.Listen(ctx, addr, port)
		time.Sleep(100 * time.Millisecond)
		return addr
	}

	// Several concurrent connections share one peer connection; payload larger than one
	// data channel message checks chunking.
	addr := listen(allowed)
	payload := bytes.Repeat([]byte("windows-mcp "), 100_000) // ~1.2 MB
	errs := make(chan error, 3)
	for range 3 {
		go func() {
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				errs <- err
				return
			}
			defer conn.Close()
			go func() { conn.Write(payload) }()
			got := make([]byte, len(payload))
			if _, err := io.ReadFull(conn, got); err != nil {
				errs <- err
				return
			}
			if !bytes.Equal(got, payload) {
				errs <- io.ErrUnexpectedEOF
				return
			}
			errs <- nil
		}()
	}
	for range 3 {
		if err := <-errs; err != nil {
			t.Fatalf("echo through tunnel: %v", err)
		}
	}

	// A port that isn't on the allow-list is refused: the connection is closed with no data.
	addr = listen(blocked)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte("hello"))
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := conn.Read(make([]byte, 16)); n != 0 || err == nil {
		t.Fatalf("blocked port: read %d bytes, err %v; want closed connection", n, err)
	}
}
