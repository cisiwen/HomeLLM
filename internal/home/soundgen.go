package home

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SoundBackend adds a matching soundtrack to a silent clip.
type SoundBackend interface {
	AddSound(ctx context.Context, clip Video, prompt string, seed int64) (Video, error)
	Free(ctx context.Context) error
}

// Foley talks to deploy/foley/foley_server.py, a local HunyuanVideo-Foley service on the same
// PC. Clips are passed as files in Dir rather than uploaded.
type Foley struct {
	BaseURL string // e.g. http://127.0.0.1:8190
	Dir     string // scratch directory for clips; os.TempDir() when empty
	Client  *http.Client
}

// foleyTimeout bounds one soundtrack: model load plus 50 sampling steps.
const foleyTimeout = 10 * time.Minute

func (f *Foley) client() *http.Client {
	if f.Client != nil {
		return f.Client
	}
	return &http.Client{Timeout: foleyTimeout}
}

func (f *Foley) post(ctx context.Context, path string, body any, out any) error {
	data, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(f.BaseURL, "/")+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.client().Do(req)
	if err != nil {
		return fmt.Errorf("sound generator not reachable (is the Foley service running?): %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return fmt.Errorf("adding sound failed: %s", e.Error)
		}
		return fmt.Errorf("sound generator returned %s", resp.Status)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func (f *Foley) AddSound(ctx context.Context, clip Video, prompt string, seed int64) (Video, error) {
	dir, err := os.MkdirTemp(f.Dir, "homellm-sound-")
	if err != nil {
		return Video{}, err
	}
	defer os.RemoveAll(dir)
	in := filepath.Join(dir, "in.mp4")
	out := filepath.Join(dir, "out.mp4")
	if err := os.WriteFile(in, clip.Data, 0o600); err != nil {
		return Video{}, err
	}
	err = f.post(ctx, "/generate", map[string]any{
		// The service seeds NumPy, which only takes 32-bit seeds.
		"video": in, "out": out, "prompt": prompt, "seed": seed & 0xffffffff,
		// Hunyuan's own default negative prompt, as in its gradio app.
		"neg_prompt": "noisy, harsh",
	}, nil)
	if err != nil {
		return Video{}, err
	}
	data, err := os.ReadFile(out)
	if err != nil {
		return Video{}, fmt.Errorf("reading the clip with sound: %w", err)
	}
	return Video{MIME: "video/mp4", Data: data}, nil
}

func (f *Foley) Free(ctx context.Context) error {
	return f.post(ctx, "/free", map[string]any{}, nil)
}

// soundInstructions has the LLM turn a clip's description into what it would sound like - the
// text prompt HunyuanVideo-Foley works best with (it also watches the clip itself).
const soundInstructions = "You describe sound for a short silent video clip, to guide a model that " +
	"generates its soundtrack. From the user's description of the clip, list what it would " +
	"sound like: ambience, sound effects and their sources, in English, as one comma-separated " +
	"line under 30 words. No music unless asked, no speech, no quotes or preamble."

// MockSound returns the clip unchanged, for tests.
type MockSound struct{}

func (MockSound) Free(context.Context) error { return nil }

func (MockSound) AddSound(ctx context.Context, clip Video, prompt string, _ int64) (Video, error) {
	select {
	case <-ctx.Done():
		return Video{}, ctx.Err()
	case <-time.After(50 * time.Millisecond):
	}
	return Video{MIME: clip.MIME, Data: append(append([]byte{}, clip.Data...), []byte(" +sound:"+prompt)...)}, nil
}
