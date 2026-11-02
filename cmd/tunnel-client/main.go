// Command tunnel-client runs on the machine you work from. It pairs with a PC running
// tunnel-home and then listens on a local port, forwarding each connection to the same port
// on that PC over a direct, encrypted WebRTC data channel.
//
//	tunnel-client pair  -qr '<blob from tunnel-home -pair>' <auth flags>
//	tunnel-client serve -listen 127.0.0.1:8000 -port 8000  <auth flags>
//
// Auth flags: against the dev signalling server, -token <any string> is the user ID. Against
// the real one, sign in with Cognito: -cognito-pool, -cognito-client, -cognito-region and
// -username, with the password in $TUNNEL_PASSWORD (or typed at a prompt).
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/pion/webrtc/v4"
	"golang.org/x/term"

	"github.com/wweng/homellm/internal/tunnel"
	"github.com/wweng/homellm/protocol"
)

func main() {
	if len(os.Args) < 2 || (os.Args[1] != "pair" && os.Args[1] != "serve") {
		fmt.Fprintln(os.Stderr, "usage: tunnel-client pair|serve [flags]   (-h after a command lists its flags)")
		os.Exit(2)
	}
	cmd := os.Args[1]
	homeDir, _ := os.UserHomeDir()

	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	var (
		dir           = fs.String("dir", filepath.Join(homeDir, ".homellm-tunnel-client"), "directory the pairing is kept in")
		token         = fs.String("token", "", "ID token or user ID for the dev signalling server")
		cognitoPool   = fs.String("cognito-pool", "", "Cognito user pool ID")
		cognitoClient = fs.String("cognito-client", "", "Cognito app client ID")
		cognitoRegion = fs.String("cognito-region", "", "Cognito region (default: from the pool ID)")
		username      = fs.String("username", "", "Cognito username (email)")
		stun          = fs.String("stun", "stun:stun.l.google.com:19302", "comma-separated STUN URLs")
		turn          = fs.String("turn", "", "optional TURN URL, e.g. turn:turn.example.com:3478")
		turnUser      = fs.String("turn-user", "", "TURN username")
		turnPass      = fs.String("turn-pass", "", "TURN credential")
		qrJSON        = fs.String("qr", "", "pair: the pairing blob printed by tunnel-home -pair")
		listen        = fs.String("listen", "127.0.0.1:8000", "serve: local address to listen on")
		port          = fs.Int("port", 8000, "serve: the PC's localhost port to forward to")
	)
	fs.Parse(os.Args[2:])

	tokenFn, err := tokenSource(*token, *cognitoPool, *cognitoClient, *cognitoRegion, *username)
	if err != nil {
		fatal("auth", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pairingPath := filepath.Join(*dir, "pairing.json")

	switch cmd {
	case "pair":
		var qr protocol.PairingQR
		if err := json.Unmarshal([]byte(*qrJSON), &qr); err != nil || qr.DeviceID == "" {
			fatal("bad -qr", errors.New("expected the JSON printed by tunnel-home -pair"))
		}
		pctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		fmt.Println("Waiting for approval on the PC...")
		name, err := tunnel.Pair(pctx, qr, tokenFn)
		if err != nil {
			fatal("pairing", err)
		}
		qr.Code = "" // single use; don't keep it
		data, _ := json.Marshal(qr)
		if err := os.MkdirAll(*dir, 0o700); err != nil {
			fatal("creating -dir", err)
		}
		if err := os.WriteFile(pairingPath, data, 0o600); err != nil {
			fatal("saving pairing", err)
		}
		fmt.Printf("Paired with %q. Now run: tunnel-client serve\n", name)

	case "serve":
		data, err := os.ReadFile(pairingPath)
		if err != nil {
			fatal("reading pairing (run `tunnel-client pair` first)", err)
		}
		var qr protocol.PairingQR
		if err := json.Unmarshal(data, &qr); err != nil {
			fatal("bad pairing file", err)
		}
		var ice []webrtc.ICEServer
		if *stun != "" {
			ice = append(ice, webrtc.ICEServer{URLs: strings.Split(*stun, ",")})
		}
		if *turn != "" {
			ice = append(ice, webrtc.ICEServer{URLs: []string{*turn}, Username: *turnUser, Credential: *turnPass})
		}
		if err := tunnel.NewClient(qr, tokenFn, ice).Listen(ctx, *listen, *port); err != nil {
			fatal("serving", err)
		}
	}
}

// tokenSource returns the function that supplies a fresh ID token on every negotiation.
func tokenSource(token, pool, client, region, username string) (tunnel.TokenFunc, error) {
	if token != "" {
		return func(context.Context) (string, error) { return token, nil }, nil
	}
	if pool == "" || client == "" || username == "" {
		return nil, errors.New("give -token (dev server) or -cognito-pool, -cognito-client and -username")
	}
	if region == "" {
		region, _, _ = strings.Cut(pool, "_")
	}
	password := os.Getenv("TUNNEL_PASSWORD")
	if password == "" {
		fmt.Fprint(os.Stderr, "Cognito password: ")
		var b []byte
		var err error
		if term.IsTerminal(int(os.Stdin.Fd())) {
			b, err = term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(os.Stderr)
		} else {
			var line string
			line, err = bufio.NewReader(os.Stdin).ReadString('\n')
			b = []byte(strings.TrimSpace(line))
		}
		if err != nil || len(b) == 0 {
			return nil, errors.New("no password given")
		}
		password = string(b)
	}
	return func(ctx context.Context) (string, error) {
		return cognitoIDToken(ctx, region, pool, client, username, password)
	}, nil
}

func fatal(msg string, err error) {
	slog.Error(msg, "err", err)
	os.Exit(1)
}
