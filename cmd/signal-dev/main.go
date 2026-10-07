// Command signal-dev runs the local development signalling server. See package signaldev for
// why it must not be exposed to the internet.
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"

	"github.com/wweng/homellm/internal/signaldev"
)

func main() {
	addr := flag.String("addr", ":8090", "listen address")
	state := flag.String("state", "signal-dev-state.json", "file to persist pairings in (empty = memory only)")
	flag.Parse()

	srv, err := signaldev.New(*state)
	if err != nil {
		slog.Error("loading state", "err", err)
		os.Exit(1)
	}
	mux := http.NewServeMux()
	mux.Handle("/ws", srv)
	slog.Warn("DEV signalling server: phone tokens are NOT verified - do not expose publicly", "addr", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}
