package home

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// ImageRequest is one text-to-image job. Width and Height are already validated.
type ImageRequest struct {
	Prompt        string
	Width, Height int
	Seed          int64
}

// ImageBackend turns a prompt into an image. onProgress reports sampling steps as they finish.
type ImageBackend interface {
	Generate(ctx context.Context, req ImageRequest, onProgress func(step, steps int)) (Image, error)
	// Free unloads the image model from GPU memory, so the LLM can load.
	Free(ctx context.Context) error
}

// Image sizes accepted from phones: multiples of 64 within these bounds (FLUX works in 16 px
// latent patches, but 64 keeps every model happy), defaulting to a square.
const (
	minImageSide     = 256
	maxImageSide     = 1536
	defaultImageSide = 1024
	jpegQuality      = 92
)

// normalizeImageSize clamps a requested size to what the image model handles.
func normalizeImageSize(w, h int) (int, int) {
	fix := func(v int) int {
		if v <= 0 {
			return defaultImageSide
		}
		v = min(max(v, minImageSide), maxImageSide)
		return v / 64 * 64
	}
	return fix(w), fix(h)
}

// randomSeed picks a seed when the phone didn't ask for one; reported back so a good result
// can be reproduced.
func randomSeed() int64 {
	n, err := rand.Int(rand.Reader, big.NewInt(1<<48))
	if err != nil {
		return time.Now().UnixNano() & (1<<48 - 1)
	}
	return n.Int64()
}

// ComfyUI drives a local ComfyUI server (https://github.com/Comfy-Org/ComfyUI) through its
// HTTP + WebSocket API with a fixed FLUX schnell workflow. ComfyUI does the GPU work; this
// only queues the job, follows its progress and fetches the result.
type ComfyUI struct {
	BaseURL    string // e.g. http://127.0.0.1:8188
	Checkpoint string // an all-in-one checkpoint in ComfyUI's models/checkpoints
	Steps      int    // FLUX schnell is distilled for 4
	// Video is the image-to-video model's files; zero means DefaultWanVideo.
	Video WanVideo
	// MusicCheckpoint is the ACE-Step 1.5 checkpoint; empty means DefaultMusicCheckpoint.
	MusicCheckpoint string
	Client          *http.Client
}

func (c *ComfyUI) client() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return http.DefaultClient
}

func (c *ComfyUI) url(path string) string { return strings.TrimRight(c.BaseURL, "/") + path }

// workflow is ComfyUI's API-format graph for the official FLUX schnell fp8 checkpoint
// example: checkpoint -> text encode -> 4 euler steps at cfg 1 -> VAE decode. PreviewImage
// (not SaveImage) keeps results in ComfyUI's temp folder, which it clears on restart, so
// generated images don't pile up on the PC.
func (c *ComfyUI) workflow(req ImageRequest) map[string]any {
	steps := c.Steps
	if steps <= 0 {
		steps = 4
	}
	node := func(class string, inputs map[string]any) map[string]any {
		return map[string]any{"class_type": class, "inputs": inputs}
	}
	return map[string]any{
		"ckpt": node("CheckpointLoaderSimple", map[string]any{"ckpt_name": c.Checkpoint}),
		"pos":  node("CLIPTextEncode", map[string]any{"text": req.Prompt, "clip": []any{"ckpt", 1}}),
		"neg":  node("CLIPTextEncode", map[string]any{"text": "", "clip": []any{"ckpt", 1}}),
		"size": node("EmptySD3LatentImage", map[string]any{"width": req.Width, "height": req.Height, "batch_size": 1}),
		"sample": node("KSampler", map[string]any{
			"seed": req.Seed, "steps": steps, "cfg": 1.0, "sampler_name": "euler", "scheduler": "simple", "denoise": 1.0,
			"model": []any{"ckpt", 0}, "positive": []any{"pos", 0}, "negative": []any{"neg", 0}, "latent_image": []any{"size", 0},
		}),
		"decode": node("VAEDecode", map[string]any{"samples": []any{"sample", 0}, "vae": []any{"ckpt", 2}}),
		"out":    node("PreviewImage", map[string]any{"images": []any{"decode", 0}}),
	}
}

func (c *ComfyUI) Generate(ctx context.Context, req ImageRequest, onProgress func(step, steps int)) (Image, error) {
	data, err := c.run(ctx, c.workflow(req), func(_ string, value, max int) {
		if onProgress != nil {
			onProgress(value, max)
		}
	}, MaxFileSize)
	if err != nil {
		return Image{}, err
	}
	return toJPEG(data)
}

// run queues a workflow, follows its progress (reported per node) and returns the bytes of
// its "out" node's first output file.
func (c *ComfyUI) run(ctx context.Context, workflow map[string]any, onProgress func(node string, value, max int), maxBytes int64) ([]byte, error) {
	clientID := randomHex(8)

	// Listen before queueing, so no progress or completion event can be missed.
	wsURL := strings.Replace(c.url("/ws?clientId="+clientID), "http", "ws", 1)
	ws, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPClient: c.client()})
	if err != nil {
		return nil, fmt.Errorf("image generator not reachable (is ComfyUI running?): %w", err)
	}
	defer ws.CloseNow()
	ws.SetReadLimit(64 << 20) // ComfyUI also sends binary preview frames

	body, _ := json.Marshal(map[string]any{"prompt": workflow, "client_id": clientID})
	var queued struct {
		PromptID string          `json:"prompt_id"`
		Error    json.RawMessage `json:"error"`
	}
	if err := c.postJSON(ctx, "/prompt", body, &queued); err != nil {
		return nil, err
	}
	if queued.PromptID == "" {
		return nil, fmt.Errorf("image generator rejected the job: %s", string(queued.Error))
	}

	done := make(chan error, 1)
	go func() { done <- c.follow(ctx, ws, queued.PromptID, onProgress) }()
	select {
	case err = <-done:
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err != nil {
		if ctx.Err() != nil {
			c.cancel(queued.PromptID)
		}
		return nil, err
	}
	return c.fetchOutput(ctx, queued.PromptID, maxBytes)
}

// follow reads ComfyUI's event stream until this prompt finishes.
func (c *ComfyUI) follow(ctx context.Context, ws *websocket.Conn, promptID string, onProgress func(node string, value, max int)) error {
	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			return fmt.Errorf("lost the image generator's progress stream: %w", err)
		}
		if typ != websocket.MessageText {
			continue
		}
		var ev struct {
			Type string `json:"type"`
			Data struct {
				PromptID         string  `json:"prompt_id"`
				Node             *string `json:"node"`
				ProgressNode     string  `json:"node_id"`
				Value            int     `json:"value"`
				Max              int     `json:"max"`
				ExceptionMessage string  `json:"exception_message"`
			} `json:"data"`
		}
		if json.Unmarshal(data, &ev) != nil || ev.Data.PromptID != promptID {
			continue
		}
		switch ev.Type {
		case "progress":
			if onProgress != nil {
				node := ev.Data.ProgressNode
				if node == "" && ev.Data.Node != nil {
					node = *ev.Data.Node
				}
				onProgress(node, ev.Data.Value, ev.Data.Max)
			}
		case "execution_error":
			return fmt.Errorf("image generation failed: %s", strings.TrimSpace(ev.Data.ExceptionMessage))
		case "execution_interrupted":
			return errors.New("image generation was interrupted")
		case "execution_success":
			return nil
		case "executing":
			if ev.Data.Node == nil { // older servers signal the end this way
				return nil
			}
		}
	}
}

// historyWait is how long fetchResult keeps looking for a finished job in ComfyUI's history.
// ComfyUI announces execution_success a moment before it records the job there.
const historyWait = 10 * time.Second

type comfyFile struct {
	Filename  string `json:"filename"`
	Subfolder string `json:"subfolder"`
	Type      string `json:"type"`
}

// cancel drops a queued job, or stops it if it's the one running. Best effort, and on a
// fresh context: the job's own was just cancelled.
func (c *ComfyUI) cancel(promptID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	body, _ := json.Marshal(map[string]any{"delete": []string{promptID}})
	_ = c.postJSON(ctx, "/queue", body, nil)
	body, _ = json.Marshal(map[string]any{"prompt_id": promptID})
	_ = c.postJSON(ctx, "/interrupt", body, nil)
}

func (c *ComfyUI) Free(ctx context.Context) error {
	body, _ := json.Marshal(map[string]any{"unload_models": true, "free_memory": true})
	return c.postJSON(ctx, "/free", body, nil)
}

func (c *ComfyUI) postJSON(ctx context.Context, path string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(path), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, out)
}

func (c *ComfyUI) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(path), nil)
	if err != nil {
		return err
	}
	return c.do(req, out)
}

func (c *ComfyUI) do(req *http.Request, out any) error {
	resp, err := c.client().Do(req)
	if err != nil {
		return fmt.Errorf("image generator not reachable (is ComfyUI running?): %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("image generator returned %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// toJPEG re-encodes ComfyUI's PNG output as JPEG - about a quarter of the size to send to the
// phone, with no visible difference at this quality.
func toJPEG(data []byte) (Image, error) {
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return Image{}, fmt.Errorf("decoding the generated image: %w", err)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return Image{}, err
	}
	return Image{MIME: "image/jpeg", Data: buf.Bytes()}, nil
}

// MockImage paints a gradient seeded by the prompt, stepping like a real sampler. It lets the
// image path be tested without a GPU.
type MockImage struct{}

func (MockImage) Free(context.Context) error { return nil }

func (MockImage) Generate(ctx context.Context, req ImageRequest, onProgress func(step, steps int)) (Image, error) {
	const steps = 4
	for i := 1; i <= steps; i++ {
		select {
		case <-ctx.Done():
			return Image{}, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
		if onProgress != nil {
			onProgress(i, steps)
		}
	}
	h := []byte(hex.EncodeToString([]byte(req.Prompt + "x")))
	img := image.NewRGBA(image.Rect(0, 0, req.Width, req.Height))
	for y := 0; y < req.Height; y++ {
		for x := 0; x < req.Width; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 255 / req.Width), uint8(y * 255 / req.Height), h[0] * 2, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return Image{}, err
	}
	return Image{MIME: "image/jpeg", Data: buf.Bytes()}, nil
}
