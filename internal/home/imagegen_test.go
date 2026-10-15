package home

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeComfy is just enough of ComfyUI's API for ComfyUI.Generate: a WebSocket that reports
// progress once a prompt is queued, /prompt, /history and /view.
type fakeComfy struct {
	mu       sync.Mutex
	queued   map[string]any // the last workflow received
	freed    bool
	promptCh chan string
}

func newFakeComfy(t *testing.T) (*fakeComfy, *httptest.Server) {
	f := &fakeComfy{promptCh: make(chan string, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		pid := <-f.promptCh
		send := func(v any) {
			data, _ := json.Marshal(v)
			_ = c.Write(r.Context(), websocket.MessageText, data)
		}
		// An event for someone else's job must be ignored.
		send(map[string]any{"type": "progress", "data": map[string]any{"prompt_id": "other", "value": 9, "max": 9}})
		for i := 1; i <= 4; i++ {
			send(map[string]any{"type": "progress", "data": map[string]any{"prompt_id": pid, "value": i, "max": 4}})
		}
		send(map[string]any{"type": "execution_success", "data": map[string]any{"prompt_id": pid}})
		time.Sleep(100 * time.Millisecond)
	})
	mux.HandleFunc("/prompt", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Prompt map[string]any `json:"prompt"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.queued = body.Prompt
		f.mu.Unlock()
		f.promptCh <- "p1"
		_ = json.NewEncoder(w).Encode(map[string]any{"prompt_id": "p1"})
	})
	historyReads := 0
	mux.HandleFunc("/history/p1", func(w http.ResponseWriter, r *http.Request) {
		// Like the real ComfyUI, the job isn't in its history yet right after
		// execution_success.
		if historyReads++; historyReads == 1 {
			_, _ = w.Write([]byte("{}"))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"p1": map[string]any{"outputs": map[string]any{
			"out": map[string]any{"images": []any{map[string]any{"filename": "a.png", "subfolder": "", "type": "temp"}}},
		}}})
	})
	mux.HandleFunc("/view", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("filename") != "a.png" || r.URL.Query().Get("type") != "temp" {
			http.NotFound(w, r)
			return
		}
		_ = png.Encode(w, image.NewRGBA(image.Rect(0, 0, 64, 32)))
	})
	mux.HandleFunc("/free", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.freed = true
		f.mu.Unlock()
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return f, srv
}

func TestComfyUIGenerate(t *testing.T) {
	f, srv := newFakeComfy(t)
	c := &ComfyUI{BaseURL: srv.URL, Checkpoint: "flux1-schnell-fp8.safetensors", Steps: 4}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var steps []int
	img, err := c.Generate(ctx, ImageRequest{Prompt: "a red fox", Width: 768, Height: 512, Seed: 42}, func(step, max int) {
		steps = append(steps, step)
	})
	if err != nil {
		t.Fatal(err)
	}
	if img.MIME != "image/jpeg" || !bytes.HasPrefix(img.Data, []byte{0xFF, 0xD8}) {
		t.Fatalf("got %s (%d bytes), want a JPEG", img.MIME, len(img.Data))
	}
	if len(steps) != 4 || steps[3] != 4 {
		t.Fatalf("progress steps = %v, want 1..4 for our job only", steps)
	}

	f.mu.Lock()
	wf, _ := json.Marshal(f.queued)
	f.mu.Unlock()
	for _, want := range []string{`"text":"a red fox"`, `"width":768`, `"height":512`, `"seed":42`, `"steps":4`, `"ckpt_name":"flux1-schnell-fp8.safetensors"`, `"PreviewImage"`} {
		if !strings.Contains(string(wf), want) {
			t.Errorf("workflow is missing %s: %s", want, wf)
		}
	}

	if err := c.Free(ctx); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.freed {
		t.Error("Free didn't call /free")
	}
}

func TestComfyUIUnreachable(t *testing.T) {
	c := &ComfyUI{BaseURL: "http://127.0.0.1:1", Checkpoint: "x"}
	_, err := c.Generate(context.Background(), ImageRequest{Prompt: "x", Width: 256, Height: 256}, nil)
	if err == nil || !strings.Contains(err.Error(), "ComfyUI") {
		t.Fatalf("err = %v, want one naming ComfyUI", err)
	}
}

func TestNormalizeImageSize(t *testing.T) {
	cases := []struct{ w, h, ww, wh int }{
		{0, 0, 1024, 1024},
		{100, 5000, 256, 1536},
		{1000, 700, 960, 640},
	}
	for _, c := range cases {
		if w, h := normalizeImageSize(c.w, c.h); w != c.ww || h != c.wh {
			t.Errorf("normalizeImageSize(%d, %d) = %d, %d; want %d, %d", c.w, c.h, w, h, c.ww, c.wh)
		}
	}
}
