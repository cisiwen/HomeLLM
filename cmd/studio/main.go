// Command studio is HomeLLM Studio: a desktop playground for the home PC's ComfyUI video and
// image models (Wan 2.2 image-to-video and text-to-video, FLUX schnell), for learning how the
// settings change the result.
//
// It runs on your Mac or laptop, opens an SSH tunnel to the home PC (ComfyUI and Ollama only
// listen on the PC's loopback), serves the web UI on 127.0.0.1 and opens it in a browser
// window. Finished runs are saved with their settings under -dir, so they can be compared later.
//
//	go run ./cmd/studio -host user@192.168.1.120
//	go run ./cmd/studio -comfy http://127.0.0.1:8188   # on the PC itself: no tunnel
package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

func main() {
	host := flag.String("host", envOr("HOMELLM_STUDIO_HOST", "cisiwen@192.168.1.120"), "SSH destination of the home PC")
	comfyURL := flag.String("comfy", "", "ComfyUI base URL to use directly instead of an SSH tunnel")
	ollamaURL := flag.String("ollama", "", "Ollama base URL to use directly (with -comfy)")
	controlURL := flag.String("control", "", "HomeLLM control URL to use directly (with -comfy), e.g. http://127.0.0.1:8091")
	addr := flag.String("addr", "127.0.0.1:8777", "where the studio UI listens")
	home, _ := os.UserHomeDir()
	dir := flag.String("dir", filepath.Join(home, "HomeLLM Studio"), "where finished runs are saved")
	noOpen := flag.Bool("no-open", false, "don't open a browser window")
	flag.Parse()

	uiURL := "http://" + *addr + "/"
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		// Most likely the studio is already running: just show it.
		if ping(uiURL + "api/status") {
			log.Printf("studio already running at %s", uiURL)
			if !*noOpen {
				openWindow(uiURL)
			}
			return
		}
		log.Fatalf("listening on %s: %v", *addr, err)
	}

	runsDir := filepath.Join(*dir, "runs")
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	s := &studio{runsDir: runsDir, host: *host}
	if *comfyURL != "" {
		s.comfy = mustURL(*comfyURL)
		s.ollama = mustURL(*ollamaURL)
		s.control = mustURL(*controlURL)
		s.setTunnel("direct")
	} else {
		cp, op, hp := freePort(), freePort(), freePort()
		s.comfy = mustURL(fmt.Sprintf("http://127.0.0.1:%d", cp))
		s.ollama = mustURL(fmt.Sprintf("http://127.0.0.1:%d", op))
		s.control = mustURL(fmt.Sprintf("http://127.0.0.1:%d", hp))
		go s.keepTunnel(ctx, cp, op, hp)
	}

	mux := http.NewServeMux()
	web, _ := fs.Sub(webFS, "web")
	mux.Handle("/", http.FileServer(http.FS(web)))
	mux.Handle("/comfy/", s.proxy("/comfy", func() *url.URL { return s.comfy }))
	mux.Handle("/ollama/", s.proxy("/ollama", func() *url.URL { return s.ollama }))
	mux.Handle("/runs/", http.StripPrefix("/runs/", http.FileServer(http.Dir(runsDir))))
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("POST /api/pair-code", s.pairCode)
	mux.HandleFunc("GET /api/runs", s.listRuns)
	mux.HandleFunc("POST /api/runs", s.saveRun)
	mux.HandleFunc("DELETE /api/runs/{id}", s.deleteRun)
	mux.HandleFunc("POST /api/reveal", s.reveal)
	mux.HandleFunc("POST /api/quit", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
		go func() { time.Sleep(200 * time.Millisecond); stop() }()
	})

	srv := &http.Server{Handler: mux}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	log.Printf("HomeLLM Studio at %s (runs saved in %s)", uiURL, runsDir)
	if !*noOpen {
		go openWindow(uiURL)
	}
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	s.mu.Lock()
	if s.sshCmd != nil && s.sshCmd.Process != nil {
		s.sshCmd.Process.Kill()
	}
	s.mu.Unlock()
}

type studio struct {
	runsDir       string
	host          string
	comfy, ollama *url.URL
	control       *url.URL // HomeLLM's localhost control port (pairing codes)

	mu     sync.Mutex
	tunnel string // "connecting", "up", "down: …", "direct"
	sshCmd *exec.Cmd
}

func (s *studio) setTunnel(state string) {
	s.mu.Lock()
	s.tunnel = state
	s.mu.Unlock()
}

// keepTunnel runs `ssh -N -L …` to the PC and restarts it whenever it exits.
func (s *studio) keepTunnel(ctx context.Context, comfyPort, ollamaPort, controlPort int) {
	for ctx.Err() == nil {
		s.setTunnel("connecting")
		cmd := exec.CommandContext(ctx, "ssh", "-N",
			"-o", "BatchMode=yes", "-o", "ExitOnForwardFailure=yes",
			"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", "-o", "ConnectTimeout=10",
			"-L", fmt.Sprintf("127.0.0.1:%d:127.0.0.1:8188", comfyPort),
			"-L", fmt.Sprintf("127.0.0.1:%d:127.0.0.1:11434", ollamaPort),
			"-L", fmt.Sprintf("127.0.0.1:%d:127.0.0.1:8091", controlPort),
			s.host)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			s.setTunnel("down: " + err.Error())
		} else {
			s.mu.Lock()
			s.sshCmd = cmd
			s.mu.Unlock()
			// ssh prints nothing once the forwards are up, so wait for the port to answer.
			go func() {
				for i := 0; i < 40 && ctx.Err() == nil; i++ {
					if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", comfyPort), time.Second); err == nil {
						c.Close()
						s.setTunnel("up")
						return
					}
					time.Sleep(500 * time.Millisecond)
				}
			}()
			err := cmd.Wait()
			msg := strings.TrimSpace(stderr.String())
			if msg == "" && err != nil {
				msg = err.Error()
			}
			s.setTunnel("down: " + msg)
			if ctx.Err() == nil {
				log.Printf("ssh tunnel to %s ended: %s", s.host, msg)
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
		}
	}
}

// proxy forwards /prefix/... to a backend, WebSockets included. The Origin header is dropped:
// ComfyUI refuses requests whose Origin doesn't match its own host.
func (s *studio) proxy(prefix string, target func() *url.URL) http.Handler {
	rp := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			t := target()
			r.SetURL(t)
			r.Out.URL.Path = strings.TrimPrefix(r.In.URL.Path, prefix)
			r.Out.URL.RawPath = ""
			r.Out.Header.Del("Origin")
			r.Out.Header.Del("Referer")
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "home PC not reachable: "+err.Error(), http.StatusBadGateway)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if target() == nil {
			http.Error(w, "not configured", http.StatusServiceUnavailable)
			return
		}
		rp.ServeHTTP(w, r)
	})
}

// pairCode asks HomeLLM on the PC for a one-time code to pair a browser with (homellm
// code). Only this one control call is passed through, not the approval endpoints.
func (s *studio) pairCode(w http.ResponseWriter, r *http.Request) {
	if s.control == nil {
		http.Error(w, "no HomeLLM control address (-control)", http.StatusServiceUnavailable)
		return
	}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, s.control.String()+"/pair/code", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "HomeLLM on the PC isn't answering: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, io.LimitReader(resp.Body, 1<<16))
}

func (s *studio) status(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	tunnel := s.tunnel
	s.mu.Unlock()
	writeJSON(w, map[string]any{"host": s.host, "tunnel": tunnel, "runsDir": s.runsDir})
}

var idRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,80}$`)

// saveRun copies a finished ComfyUI job's outputs (and its start image) into runs/<id>/ next
// to a meta.json holding the settings the UI sent.
func (s *studio) saveRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID         string         `json:"id"`
		PromptID   string         `json:"promptId"`
		StartImage string         `json:"startImage"` // a name in ComfyUI's input folder
		Meta       map[string]any `json:"meta"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&req); err != nil || !idRe.MatchString(req.ID) || req.PromptID == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()

	var history map[string]struct {
		Outputs map[string]map[string]json.RawMessage `json:"outputs"`
	}
	if err := s.comfyJSON(ctx, "/history/"+url.PathEscape(req.PromptID), &history); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	dir := filepath.Join(s.runsDir, req.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type file struct {
		Node string `json:"node"`
		Name string `json:"name"`
		Kind string `json:"kind"` // video or image
	}
	var files []file
	nodes := make([]string, 0)
	for n := range history[req.PromptID].Outputs {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	for _, node := range nodes {
		for _, raw := range history[req.PromptID].Outputs[node] {
			var list []struct{ Filename, Subfolder, Type string }
			if json.Unmarshal(raw, &list) != nil {
				continue
			}
			for i, f := range list {
				if f.Filename == "" {
					continue
				}
				ext := strings.ToLower(filepath.Ext(f.Filename))
				kind := "image"
				if ext == ".mp4" || ext == ".webm" || ext == ".mov" {
					kind = "video"
				}
				name := fmt.Sprintf("%s-%d%s", node, i, ext)
				if err := s.comfyFile(ctx, f.Filename, f.Subfolder, f.Type, filepath.Join(dir, name)); err != nil {
					http.Error(w, err.Error(), http.StatusBadGateway)
					return
				}
				files = append(files, file{Node: node, Name: name, Kind: kind})
			}
		}
	}
	meta := req.Meta
	if meta == nil {
		meta = map[string]any{}
	}
	if req.StartImage != "" {
		sub, name := "", req.StartImage
		if i := strings.LastIndex(name, "/"); i >= 0 {
			sub, name = name[:i], name[i+1:]
		}
		dst := "start" + strings.ToLower(filepath.Ext(name))
		if err := s.comfyFile(ctx, name, sub, "input", filepath.Join(dir, dst)); err == nil {
			meta["startFile"] = dst
		}
	}
	meta["id"] = req.ID
	meta["promptId"] = req.PromptID
	meta["files"] = files
	meta["savedAt"] = time.Now().Format(time.RFC3339)
	data, _ := json.MarshalIndent(meta, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), data, 0o644); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func (s *studio) listRuns(w http.ResponseWriter, r *http.Request) {
	entries, _ := os.ReadDir(s.runsDir)
	runs := []json.RawMessage{}
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if !e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.runsDir, e.Name(), "meta.json"))
		if err == nil && json.Valid(data) {
			runs = append(runs, data)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(runs)
}

func (s *studio) deleteRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idRe.MatchString(id) {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := os.RemoveAll(filepath.Join(s.runsDir, id)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// reveal shows the runs folder (or one run) in Finder / Explorer.
func (s *studio) reveal(w http.ResponseWriter, r *http.Request) {
	path := s.runsDir
	if id := r.URL.Query().Get("id"); idRe.MatchString(id) {
		path = filepath.Join(s.runsDir, id)
	}
	switch runtime.GOOS {
	case "darwin":
		exec.Command("open", path).Start()
	case "windows":
		exec.Command("explorer", path).Start()
	default:
		exec.Command("xdg-open", path).Start()
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *studio) comfyJSON(ctx context.Context, path string, out any) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.comfy.String()+path, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ComfyUI %s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (s *studio) comfyFile(ctx context.Context, name, sub, typ, dst string) error {
	q := url.Values{"filename": {name}, "subfolder": {sub}, "type": {typ}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.comfy.String()+"/view?"+q.Encode(), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetching %s: %s", name, resp.Status)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// openWindow opens the UI, as a chromeless app window when Chrome or Edge is installed.
func openWindow(u string) {
	if runtime.GOOS == "darwin" {
		for _, app := range []string{"Google Chrome", "Microsoft Edge", "Brave Browser"} {
			if _, err := os.Stat("/Applications/" + app + ".app"); err == nil {
				if exec.Command("open", "-na", app, "--args", "--app="+u, "--window-size=1500,950").Run() == nil {
					return
				}
			}
		}
		exec.Command("open", u).Run()
		return
	}
	if runtime.GOOS == "windows" {
		exec.Command("cmd", "/c", "start", "", u).Run()
		return
	}
	exec.Command("xdg-open", u).Run()
}

func ping(u string) bool {
	c := http.Client{Timeout: time.Second}
	resp, err := c.Get(u)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func freePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func mustURL(s string) *url.URL {
	if s == "" {
		return nil
	}
	u, err := url.Parse(s)
	if err != nil {
		log.Fatalf("bad URL %q: %v", s, err)
	}
	return u
}

func envOr(key, def string) string {
	if v := os.Getenv(key); key != "" && v != "" {
		return v
	}
	return def
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
