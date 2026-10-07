package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mdp/qrterminal/v3"

	"github.com/wweng/homellm/internal/home"
	"github.com/wweng/homellm/protocol"
)

// When running as a background service there's no console to type "p" or "y" into, so the
// service also listens on a localhost-only control port, and `homellm pair` (run in any
// terminal on the same PC) uses it to fetch a QR and answer the approval prompt.
//
// Anyone who can run programs on this PC can pair a phone this way - the same trust as
// typing into the server's own console.

const defaultControlAddr = "127.0.0.1:8091"

type approval struct {
	ID     string `json:"request_id"`
	UserID string `json:"user_id"`
	answer chan bool
}

// pairDesk hands pairing approvals to an attached `homellm pair` client.
type pairDesk struct {
	newQR func(context.Context) (protocol.PairingQR, error)

	mu       sync.Mutex
	attached time.Time // when a client last asked for a QR or polled
	waiting  map[string]*approval
	pending  chan *approval
}

func newPairDesk(newQR func(context.Context) (protocol.PairingQR, error)) *pairDesk {
	return &pairDesk{newQR: newQR, waiting: map[string]*approval{}, pending: make(chan *approval)}
}

// active reports whether a `homellm pair` client is currently showing a QR.
func (d *pairDesk) active() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return time.Since(d.attached) < home.PairingTTL+time.Minute
}

// ask forwards one approval to the attached client and waits for its answer.
func (d *pairDesk) ask(ctx context.Context, userID string) bool {
	a := &approval{ID: randomID(), UserID: userID, answer: make(chan bool, 1)}
	d.mu.Lock()
	d.waiting[a.ID] = a
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.waiting, a.ID)
		d.mu.Unlock()
	}()
	select {
	case d.pending <- a:
	case <-ctx.Done():
		return false
	}
	select {
	case ok := <-a.answer:
		return ok
	case <-ctx.Done():
		return false
	}
}

func (d *pairDesk) touch() {
	d.mu.Lock()
	d.attached = time.Now()
	d.mu.Unlock()
}

func (d *pairDesk) serve(ctx context.Context, addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /pair", func(w http.ResponseWriter, r *http.Request) {
		qr, err := d.newQR(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		d.touch()
		json.NewEncoder(w).Encode(qr)
	})
	// Long poll: returns the next approval request, or 204 if none arrived in time.
	mux.HandleFunc("GET /pair/next", func(w http.ResponseWriter, r *http.Request) {
		d.touch()
		select {
		case a := <-d.pending:
			json.NewEncoder(w).Encode(a)
		case <-time.After(50 * time.Second):
			w.WriteHeader(http.StatusNoContent)
		case <-r.Context().Done():
		}
	})
	mux.HandleFunc("POST /pair/answer", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ID       string `json:"request_id"`
			Approved bool   `json:"approved"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		d.mu.Lock()
		a := d.waiting[body.ID]
		d.mu.Unlock()
		if a == nil {
			http.Error(w, "no such request (it may have timed out)", http.StatusNotFound)
			return
		}
		a.answer <- body.Approved
	})

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		slog.Warn("pairing control port unavailable; use the console instead", "addr", addr, "err", err)
		return
	}
	srv := &http.Server{Handler: mux}
	go func() { <-ctx.Done(); srv.Close() }()
	srv.Serve(ln)
}

// runPairClient implements `homellm pair`: show a QR from the running service, then handle
// the approval prompt for whoever scans it.
func runPairClient(args []string) {
	fs := flag.NewFlagSet("pair", flag.ExitOnError)
	addr := fs.String("control", defaultControlAddr, "the running server's control address")
	fs.Parse(args)
	base := "http://" + *addr

	resp, err := http.Post(base+"/pair", "application/json", nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Cannot reach the HomeLLM server - is it running?", err)
		os.Exit(1)
	}
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "The server couldn't create a pairing code:", strings.TrimSpace(buf.String()))
		os.Exit(1)
	}
	var qr protocol.PairingQR
	json.Unmarshal(buf.Bytes(), &qr)
	data, _ := json.Marshal(qr)
	fmt.Println("Scan with the app to pair (valid for", home.PairingTTL.String()+", single use):")
	qrterminal.GenerateHalfBlock(string(data), qrterminal.L, os.Stdout)
	fmt.Println("Or paste this into the app:\n" + string(data))
	fmt.Println("\nWaiting for a phone to scan it...")

	stdin := bufio.NewReader(os.Stdin)
	deadline := time.Now().Add(home.PairingTTL + 30*time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/pair/next")
		if err != nil {
			fmt.Fprintln(os.Stderr, "Lost contact with the server:", err)
			os.Exit(1)
		}
		if resp.StatusCode == http.StatusNoContent {
			resp.Body.Close()
			continue
		}
		var a approval
		json.NewDecoder(resp.Body).Decode(&a)
		resp.Body.Close()

		fmt.Printf("\nPhone user %q wants to pair with this server. Approve? [y/N] ", a.UserID)
		line, _ := stdin.ReadString('\n')
		ok := strings.EqualFold(strings.TrimSpace(line), "y")
		body, _ := json.Marshal(map[string]any{"request_id": a.ID, "approved": ok})
		r, err := http.Post(base+"/pair/answer", "application/json", bytes.NewReader(body))
		if err != nil || r.StatusCode != http.StatusOK {
			fmt.Println("Too late - the request timed out. Run `homellm pair` again.")
			os.Exit(1)
		}
		r.Body.Close()
		if ok {
			fmt.Println("Approved. The phone is paired.")
		} else {
			fmt.Println("Rejected.")
		}
		return
	}
	fmt.Println("The pairing code expired. Run `homellm pair` again.")
}

func randomID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}
