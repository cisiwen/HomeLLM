// Command tunnel-home runs on the PC whose localhost services (e.g. windows-mcp on port 8000)
// should be reachable from anywhere. It connects out to the signalling server (no open ports
// needed) and forwards paired clients' data channels to an allow-list of localhost ports.
//
// Pair a client once, in a terminal on the PC, with the service stopped (two copies with the
// same identity would replace each other on signalling):
//
//	tunnel-home -pair -signal wss://<signalling-host>/prod
//
// It prints a pairing blob to pass to `tunnel-client pair`, then asks you to approve the
// client. After that, run it without -pair (e.g. as a scheduled task, see
// deploy/install-tunnel-windows.ps1) and approvals are refused, since there is no console.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/wweng/homellm/internal/home"
	"github.com/wweng/homellm/internal/tunnel"
)

func main() {
	homeDir, _ := os.UserHomeDir()
	var (
		signalURL    = flag.String("signal", "ws://localhost:8090/ws", "signalling WebSocket URL this server connects to")
		publicSignal = flag.String("public-signal", "", "signalling URL clients should use, put in the pairing blob (default: -signal)")
		dir          = flag.String("dir", filepath.Join(homeDir, ".homellm-tunnel"), "directory for this tunnel's identity (separate from HomeLLM's)")
		name         = flag.String("name", "", "display name (default: hostname)")
		ports        = flag.String("ports", "8000", "comma-separated localhost ports clients may reach; nothing else is forwarded")
		stun         = flag.String("stun", "stun:stun.l.google.com:19302", "comma-separated STUN URLs")
		turn         = flag.String("turn", "", "optional TURN URL for networks where a direct path fails, e.g. turn:turn.example.com:3478")
		turnUser     = flag.String("turn-user", "", "TURN username")
		turnPass     = flag.String("turn-pass", "", "TURN credential")
		pair         = flag.Bool("pair", false, "print a pairing blob once signalling connects, and ask here to approve the client")
		pairFile     = flag.String("pair-file", "", "with -pair: also write the blob to this file")
		logFile      = flag.String("log", "", "append all output to this file instead of the console (for running as a service)")
	)
	flag.Parse()

	if *logFile != "" {
		f, err := os.OpenFile(*logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			fatal("opening -log", err)
		}
		os.Stdout, os.Stderr = f, f
		slog.SetDefault(slog.New(slog.NewTextHandler(f, nil)))
	}

	var allowed []int
	for _, p := range strings.Split(*ports, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n < 1 || n > 65535 {
			fatal("bad -ports", fmt.Errorf("%q", p))
		}
		allowed = append(allowed, n)
	}

	id, err := home.LoadOrCreateIdentity(*dir, *name)
	if err != nil {
		fatal("loading identity", err)
	}

	var ice []webrtc.ICEServer
	if *stun != "" {
		ice = append(ice, webrtc.ICEServer{URLs: strings.Split(*stun, ",")})
	}
	if *turn != "" {
		ice = append(ice, webrtc.ICEServer{URLs: []string{*turn}, Username: *turnUser, Credential: *turnPass})
	}

	// Approvals are answered on the console, and only while -pair is on: a running service
	// never approves anyone, so a leaked pairing code can't be used without someone at the PC.
	lines := make(chan string)
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines) // stdin closed (running as a service): approvals are refused
	}()
	h := tunnel.NewHome(tunnel.HomeConfig{
		SignalURL:       *signalURL,
		PublicSignalURL: *publicSignal,
		ICEServers:      ice,
		Ports:           allowed,
		Approve: func(ctx context.Context, userID string) bool {
			if !*pair {
				return false
			}
			fmt.Printf("\nUser %q wants to pair with this PC. Approve? [y/N] ", userID)
			select {
			case ans := <-lines:
				return strings.EqualFold(strings.TrimSpace(ans), "y")
			case <-ctx.Done():
				fmt.Println("\n(timed out - rejected)")
				return false
			}
		},
	}, id)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *pair {
		go func() {
			for !h.Connected() {
				select {
				case <-ctx.Done():
					return
				case <-time.After(200 * time.Millisecond):
				}
			}
			qr, err := h.NewPairingQR(ctx)
			if err != nil {
				slog.Error("cannot create pairing code", "err", err)
				return
			}
			data, _ := json.Marshal(qr)
			fmt.Printf("\nOn the client, run (valid for %s, single use):\n  tunnel-client pair -qr '%s'\n", home.PairingTTL, data)
			if *pairFile != "" {
				if err := os.WriteFile(*pairFile, data, 0o600); err != nil {
					slog.Error("writing -pair-file", "err", err)
				}
			}
		}()
	}

	slog.Info("tunnel home starting", "name", id.Name, "device", id.DeviceID, "ports", allowed)
	if err := h.Run(ctx); err != nil && ctx.Err() == nil {
		fatal("tunnel stopped", err)
	}
}

func fatal(msg string, err error) {
	slog.Error(msg, "err", err)
	os.Exit(1)
}
