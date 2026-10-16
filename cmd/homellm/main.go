// Command homellm runs the home server. It connects out to the signalling server (no open
// ports needed), prints a pairing QR for phones to scan, and serves LLM requests to paired
// phones over direct WebRTC data channels.
//
// Console commands while running: "p" + Enter prints a new pairing QR; pairing requests are
// approved by answering "y". When it runs as a background service with no console,
// `homellm pair` in any terminal on the same PC does the same (see pairdesk.go).
// `homellm imagine <prompt>`, `homellm animate -image x.jpg <prompt>` and `homellm music -clip
// x.mp4 <tags>` check this PC's image, video and music setup (see imagine.go).
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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mdp/qrterminal/v3"
	"github.com/pion/webrtc/v4"

	"github.com/wweng/homellm/internal/home"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "pair" {
		runPairClient(os.Args[2:])
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "imagine" {
		runImagineCheck(os.Args[2:])
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "animate" {
		runAnimateCheck(os.Args[2:])
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "music" {
		runMusicCheck(os.Args[2:])
		return
	}
	homeDir, _ := os.UserHomeDir()
	var (
		signalURL     = flag.String("signal", "ws://localhost:8090/ws", "signalling WebSocket URL this server connects to")
		publicSignal  = flag.String("public-signal", "", "signalling URL phones should use, put in the QR (default: -signal)")
		dir           = flag.String("dir", filepath.Join(homeDir, ".homellm"), "directory for this server's identity")
		name          = flag.String("name", "", "display name shown to phones (default: hostname)")
		llm           = flag.String("llm", "ollama", "LLM backend: ollama (any OpenAI-compatible server) or mock")
		llmURL        = flag.String("llm-url", "http://127.0.0.1:11434", "base URL of the OpenAI-compatible LLM server")
		model         = flag.String("model", "qwen2.5vl:7b", "default model")
		vision        = flag.Bool("vision", true, "the default model accepts images")
		images        = flag.String("images", "", "image generation backend: comfyui or mock (empty = off)")
		comfyURL      = flag.String("comfy-url", "http://127.0.0.1:8188", "base URL of the ComfyUI server")
		imageModel    = flag.String("image-model", "flux1-schnell-fp8.safetensors", "ComfyUI checkpoint for image generation")
		imageSteps    = flag.Int("image-steps", 4, "sampling steps per image (FLUX schnell is made for 4)")
		videos        = flag.String("videos", "", "image-to-video backend: comfyui (Wan 2.2) or mock (empty = off)")
		sound         = flag.String("sound", "", "add-sound backend: foley (HunyuanVideo-Foley service) or mock (empty = off)")
		foleyURL      = flag.String("foley-url", "http://127.0.0.1:8190", "base URL of the Foley service (deploy/foley)")
		music         = flag.String("music", "", "background-music backend: comfyui (ACE-Step 1.5) or mock (empty = off); needs -ffmpeg")
		ffmpegPath    = flag.String("ffmpeg", "ffmpeg", "ffmpeg, for mixing music under clips (ffprobe must be next to it)")
		stun          = flag.String("stun", "stun:stun.l.google.com:19302", "comma-separated STUN URLs")
		turn          = flag.String("turn", "", "optional TURN URL for networks where a direct path fails, e.g. turn:turn.example.com:3478")
		turnUser      = flag.String("turn-user", "", "TURN username")
		turnPass      = flag.String("turn-pass", "", "TURN credential")
		maxConcurrent = flag.Int("max-concurrent", 1, "generations run at once; others queue")
		contextTokens = flag.Int("context", 4096, "the model's context size in tokens (Ollama's default is 4096); older messages are dropped to fit, 0 = never")
		autoApprove   = flag.Bool("auto-approve", false, "approve every pairing without asking (testing only)")
		pairAtStart   = flag.Bool("pair", false, "print a pairing QR as soon as signalling connects")
		qrFile        = flag.String("qr-file", "", "also write each pairing QR's JSON to this file (for scripts/tests)")
		controlAddr   = flag.String("control", defaultControlAddr, "localhost address `homellm pair` uses to reach this server (empty = off)")
		logFile       = flag.String("log", "", "append all output to this file instead of the console (for running as a service)")
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

	id, err := home.LoadOrCreateIdentity(*dir, *name)
	if err != nil {
		fatal("loading identity", err)
	}

	var backend home.Backend
	switch *llm {
	case "ollama":
		backend = &home.Ollama{BaseURL: *llmURL, Model: *model, HasVision: *vision}
	case "mock":
		backend = home.Mock{}
	default:
		fatal("unknown -llm", fmt.Errorf("%q", *llm))
	}

	var imageBackend home.ImageBackend
	switch *images {
	case "":
	case "comfyui":
		imageBackend = &home.ComfyUI{BaseURL: *comfyURL, Checkpoint: *imageModel, Steps: *imageSteps}
	case "mock":
		imageBackend = home.MockImage{}
	default:
		fatal("unknown -images", fmt.Errorf("%q", *images))
	}

	var videoBackend home.VideoBackend
	switch *videos {
	case "":
	case "comfyui":
		videoBackend = &home.ComfyUI{BaseURL: *comfyURL, Video: home.DefaultWanVideo}
	case "mock":
		videoBackend = home.MockVideo{}
	default:
		fatal("unknown -videos", fmt.Errorf("%q", *videos))
	}

	var soundBackend home.SoundBackend
	switch *sound {
	case "":
	case "foley":
		soundBackend = &home.Foley{BaseURL: *foleyURL}
	case "mock":
		soundBackend = home.MockSound{}
	default:
		fatal("unknown -sound", fmt.Errorf("%q", *sound))
	}

	var musicBackend home.MusicBackend
	switch *music {
	case "":
	case "comfyui":
		musicBackend = &home.ComfyUI{BaseURL: *comfyURL}
	case "mock":
		musicBackend = home.MockMusic{}
	default:
		fatal("unknown -music", fmt.Errorf("%q", *music))
	}
	var mixer *home.Mixer
	if musicBackend != nil {
		mixer = &home.Mixer{FFmpeg: *ffmpegPath}
	}

	var ice []webrtc.ICEServer
	if *stun != "" {
		ice = append(ice, webrtc.ICEServer{URLs: strings.Split(*stun, ",")})
	}
	if *turn != "" {
		ice = append(ice, webrtc.ICEServer{URLs: []string{*turn}, Username: *turnUser, Credential: *turnPass})
	}

	con := newConsole()
	var desk *pairDesk
	agent := home.NewAgent(home.Config{
		SignalURL:       *signalURL,
		PublicSignalURL: *publicSignal,
		ICEServers:      ice,
		Backend:         backend,
		Images:          imageBackend,
		Videos:          videoBackend,
		Sound:           soundBackend,
		Music:           musicBackend,
		Mixer:           mixer,
		MaxConcurrent:   *maxConcurrent,
		ContextTokens:   *contextTokens,
		Approve: func(ctx context.Context, userID string) bool {
			if *autoApprove {
				return true
			}
			if desk != nil && desk.active() {
				return desk.ask(ctx, userID)
			}
			return con.ask(ctx, fmt.Sprintf("\nPhone user %q wants to pair with this server. Approve? [y/N] ", userID))
		},
	}, id)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *controlAddr != "" {
		desk = newPairDesk(agent.NewPairingQR)
		go desk.serve(ctx, *controlAddr)
	}

	showQR := func() {
		qr, err := agent.NewPairingQR(ctx)
		if err != nil {
			slog.Error("cannot create pairing code", "err", err)
			return
		}
		data, _ := json.Marshal(qr)
		fmt.Println("\nScan with the app to pair (valid for", home.PairingTTL.String()+", single use):")
		qrterminal.GenerateHalfBlock(string(data), qrterminal.L, os.Stdout)
		fmt.Println("Or paste this into the app:\n" + string(data) + "\n")
		if *qrFile != "" {
			if err := os.WriteFile(*qrFile, data, 0o600); err != nil {
				slog.Error("writing -qr-file", "err", err)
			}
		}
	}

	go con.run(func(line string) {
		switch strings.TrimSpace(line) {
		case "p":
			showQR()
		case "":
		default:
			fmt.Println(`commands: "p" = new pairing QR`)
		}
	})

	if *pairAtStart {
		go func() {
			for !agent.Connected() {
				if !sleepCtx(ctx, 200*time.Millisecond) {
					return
				}
			}
			showQR()
		}()
	}

	slog.Info("home server starting", "name", id.Name, "device", id.DeviceID, "llm", *llm, "model", backend.DefaultModel(), "images", *images, "videos", *videos, "sound", *sound, "music", *music)
	fmt.Println(`Type "p" + Enter to show a pairing QR.`)
	if err := agent.Run(ctx); err != nil && ctx.Err() == nil {
		fatal("agent stopped", err)
	}
}

// console multiplexes stdin between commands and yes/no pairing prompts.
type console struct {
	askMu   sync.Mutex // one prompt at a time
	mu      sync.Mutex
	waiting chan string
}

func newConsole() *console { return &console{} }

func (c *console) run(onCommand func(string)) {
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		c.mu.Lock()
		w := c.waiting
		c.waiting = nil
		c.mu.Unlock()
		if w != nil {
			w <- sc.Text()
			continue
		}
		onCommand(sc.Text())
	}
	// stdin closed (e.g. running as a service): pending and future prompts are rejected.
}

func (c *console) ask(ctx context.Context, prompt string) bool {
	c.askMu.Lock()
	defer c.askMu.Unlock()
	ch := make(chan string, 1)
	c.mu.Lock()
	c.waiting = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.waiting == ch {
			c.waiting = nil
		}
		c.mu.Unlock()
	}()
	fmt.Print(prompt)
	select {
	case ans := <-ch:
		return strings.EqualFold(strings.TrimSpace(ans), "y")
	case <-ctx.Done():
		fmt.Println("\n(timed out - rejected)")
		return false
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func fatal(msg string, err error) {
	slog.Error(msg, "err", err)
	os.Exit(1)
}
