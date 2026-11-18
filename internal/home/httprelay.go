package home

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/wweng/homellm/protocol"
)

// httpRelay serves one session's ChannelHTTP: it forwards the client's requests to the web
// apps the owner allowed (Config.Apps) and streams the responses back (see
// protocol/httprelay.go).
type httpRelay struct {
	dc   *webrtc.DataChannel
	apps map[string]*url.URL
	log  *slog.Logger

	mu     sync.Mutex
	reqs   map[uint32]*relayReq
	closed bool
}

type relayReq struct {
	cancel context.CancelFunc
	body   chan []byte // request body chunks for the upstream request; nil without a body
	once   sync.Once   // closes body

	creditMu sync.Mutex
	credit   int64
	more     chan struct{} // signalled when credit grows
}

const (
	relayChunk       = 16 * 1024
	relayMaxRequests = 64
	relayMaxBuffered = 1 << 20
	// relayBodyQueue bounds request-body chunks waiting for the app to read them.
	relayBodyQueue = 256
)

// relayClient talks to apps on this PC. No overall timeout: some requests legitimately take
// minutes (a page polling a long job, an LLM call), and cancelling is the client's call.
var relayClient = &http.Client{
	Transport: &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 16, IdleConnTimeout: time.Minute},
	// Redirects go back to the browser, which asks again through the relay.
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// hopHeaders are per-connection headers that must not be forwarded.
var hopHeaders = map[string]bool{
	"connection": true, "keep-alive": true, "proxy-authenticate": true, "proxy-authorization": true,
	"proxy-connection": true, "te": true, "trailer": true, "transfer-encoding": true, "upgrade": true,
	"host": true,
}

func newHTTPRelay(dc *webrtc.DataChannel, apps map[string]*url.URL, log *slog.Logger) *httpRelay {
	r := &httpRelay{dc: dc, apps: apps, log: log, reqs: map[uint32]*relayReq{}}
	dc.OnMessage(func(m webrtc.DataChannelMessage) {
		if m.IsString {
			r.onText(m.Data)
		} else {
			r.onBinary(m.Data)
		}
	})
	return r
}

func (r *httpRelay) sendMsg(m protocol.HTTPMsg) {
	data, _ := json.Marshal(m)
	r.dc.SendText(string(data))
}

func (r *httpRelay) onText(data []byte) {
	var m protocol.HTTPMsg
	if json.Unmarshal(data, &m) != nil {
		return
	}
	switch m.Type {
	case protocol.HTTPReq:
		r.start(m)
	case protocol.HTTPReqEnd:
		if q := r.get(m.N); q != nil && q.body != nil {
			q.once.Do(func() { close(q.body) })
		}
	case protocol.HTTPCredit:
		if q := r.get(m.N); q != nil && m.Credit > 0 {
			q.creditMu.Lock()
			q.credit += m.Credit
			q.creditMu.Unlock()
			select {
			case q.more <- struct{}{}:
			default:
			}
		}
	case protocol.HTTPCancel:
		if q := r.take(m.N); q != nil {
			q.cancel()
		}
	}
}

func (r *httpRelay) onBinary(frame []byte) {
	n, data, ok := protocol.ParseHTTPFrame(frame)
	if !ok {
		return
	}
	q := r.get(n)
	if q == nil || q.body == nil {
		return
	}
	// Pion reuses nothing here, but copy anyway: the chunk outlives this callback.
	chunk := append([]byte(nil), data...)
	defer func() { recover() }() // body already closed by a racing cancel
	q.body <- chunk
}

func (r *httpRelay) get(n uint32) *relayReq {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reqs[n]
}

func (r *httpRelay) take(n uint32) *relayReq {
	r.mu.Lock()
	defer r.mu.Unlock()
	q := r.reqs[n]
	delete(r.reqs, n)
	return q
}

func (r *httpRelay) fail(n uint32, msg string) {
	r.sendMsg(protocol.HTTPMsg{Type: protocol.HTTPResError, N: n, Message: msg})
}

func (r *httpRelay) start(m protocol.HTTPMsg) {
	base, ok := r.apps[m.App]
	if !ok {
		r.fail(m.N, "no such app on this PC: "+m.App)
		return
	}
	switch m.Method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		r.fail(m.N, "method not allowed")
		return
	}
	if !strings.HasPrefix(m.Path, "/") || strings.HasPrefix(m.Path, "//") {
		r.fail(m.N, "bad path")
		return
	}
	target, err := base.Parse(strings.TrimSuffix(base.Path, "/") + m.Path)
	if err != nil || target.Host != base.Host {
		r.fail(m.N, "bad path")
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	q := &relayReq{cancel: cancel, more: make(chan struct{}, 1)}
	if m.Body {
		q.body = make(chan []byte, relayBodyQueue)
	}
	r.mu.Lock()
	if r.closed || len(r.reqs) >= relayMaxRequests || r.reqs[m.N] != nil {
		r.mu.Unlock()
		cancel()
		r.fail(m.N, "too many requests")
		return
	}
	r.reqs[m.N] = q
	r.mu.Unlock()

	var body io.Reader
	if q.body != nil {
		body = &chanReader{ctx: ctx, ch: q.body}
	}
	req, err := http.NewRequestWithContext(ctx, m.Method, target.String(), body)
	if err != nil {
		r.take(m.N)
		cancel()
		r.fail(m.N, err.Error())
		return
	}
	for k, v := range m.Headers {
		if !hopHeaders[strings.ToLower(k)] {
			req.Header.Set(k, v)
		}
	}
	if cl := req.Header.Get("Content-Length"); cl != "" && body != nil {
		req.ContentLength, _ = strconv.ParseInt(cl, 10, 64)
	}
	go r.forward(m.N, q, req)
}

func (r *httpRelay) forward(n uint32, q *relayReq, req *http.Request) {
	defer func() {
		r.take(n)
		q.cancel()
	}()
	resp, err := relayClient.Do(req)
	if err != nil {
		if req.Context().Err() == nil {
			r.fail(n, "the app on the PC isn't answering: "+err.Error())
		}
		return
	}
	defer resp.Body.Close()
	headers := map[string]string{}
	for k, v := range resp.Header {
		if !hopHeaders[strings.ToLower(k)] && len(v) > 0 {
			headers[k] = strings.Join(v, ", ")
		}
	}
	r.sendMsg(protocol.HTTPMsg{Type: protocol.HTTPRes, N: n, Status: resp.StatusCode, Headers: headers})

	buf := make([]byte, relayChunk)
	for {
		k, err := resp.Body.Read(buf)
		for chunk := buf[:k]; len(chunk) > 0; {
			got := r.waitToSend(req.Context(), q, int64(len(chunk)))
			if got == 0 {
				return
			}
			if r.dc.Send(protocol.HTTPFrame(n, chunk[:got])) != nil {
				return
			}
			chunk = chunk[got:]
		}
		if err == io.EOF {
			r.sendMsg(protocol.HTTPMsg{Type: protocol.HTTPResEnd, N: n})
			return
		}
		if err != nil {
			if req.Context().Err() == nil {
				r.fail(n, err.Error())
			}
			return
		}
	}
}

// waitToSend waits until the client has granted some credit and the channel's send buffer
// has room, then takes up to want bytes of it. It returns 0 once the request is cancelled.
func (r *httpRelay) waitToSend(ctx context.Context, q *relayReq, want int64) int64 {
	var got int64
	for {
		q.creditMu.Lock()
		got = min(q.credit, want)
		q.credit -= got
		q.creditMu.Unlock()
		if got > 0 {
			break
		}
		select {
		case <-q.more:
		case <-ctx.Done():
			return 0
		}
	}
	for r.dc.BufferedAmount() > relayMaxBuffered {
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(5 * time.Millisecond):
		}
	}
	return got
}

func (r *httpRelay) close() {
	r.mu.Lock()
	r.closed = true
	reqs := r.reqs
	r.reqs = map[uint32]*relayReq{}
	r.mu.Unlock()
	for _, q := range reqs {
		q.cancel()
	}
}

// chanReader turns the request-body chunks into the upstream request's body.
type chanReader struct {
	ctx context.Context
	ch  <-chan []byte
	cur []byte
}

func (c *chanReader) Read(p []byte) (int, error) {
	for len(c.cur) == 0 {
		select {
		case b, ok := <-c.ch:
			if !ok {
				return 0, io.EOF
			}
			c.cur = b
		case <-c.ctx.Done():
			return 0, errors.New("request cancelled")
		}
	}
	k := copy(p, c.cur)
	c.cur = c.cur[k:]
	return k, nil
}

// ParseApps reads -app flags ("name=http://127.0.0.1:8000"). Only loopback URLs are allowed:
// the relay is for apps on this PC, not a way into the rest of the network.
func ParseApps(specs []string) (map[string]*url.URL, error) {
	apps := map[string]*url.URL{}
	for _, s := range specs {
		name, raw, ok := strings.Cut(s, "=")
		if !ok || name == "" {
			return nil, errors.New("-app wants name=url, e.g. dubbing=http://127.0.0.1:8000")
		}
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "http" {
			return nil, errors.New("-app " + name + ": want an http:// URL")
		}
		host := u.Hostname()
		if host != "127.0.0.1" && host != "localhost" && host != "::1" {
			return nil, errors.New("-app " + name + ": only apps on this PC (127.0.0.1) can be relayed")
		}
		apps[name] = u
	}
	return apps, nil
}
