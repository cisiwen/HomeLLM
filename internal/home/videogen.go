package home

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // DecodeConfig for the start image
	_ "image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"time"
)

// VideoRequest animates a start image. Width/Height/Frames are already validated.
type VideoRequest struct {
	Image         Image
	Prompt        string
	Width, Height int
	Frames, FPS   int
	Seed          int64
}

// Video is an encoded clip.
type Video struct {
	MIME string
	Data []byte
}

// VideoBackend turns an image and a motion prompt into a short clip.
type VideoBackend interface {
	Animate(ctx context.Context, req VideoRequest, onProgress func(step, steps int)) (Video, error)
	Free(ctx context.Context) error
}

// Wan 2.2's native output: 16 fps, and frame counts of 4n+1 (81 = about 5 s).
const (
	videoFPS          = 16
	defaultFrames     = 81
	maxFrames         = 121
	maxVideoFileBytes = 64 << 20
)

// VideoSize picks Wan's 480p size closest to the start image's shape (the image is
// centre-cropped to it), in multiples of 16.
func VideoSize(img Image) (int, int) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(img.Data))
	if err != nil || cfg.Width == 0 || cfg.Height == 0 {
		return 832, 480
	}
	r := float64(cfg.Width) / float64(cfg.Height)
	switch {
	case r > 1.25:
		return 832, 480
	case r < 0.8:
		return 480, 832
	default:
		return 640, 640
	}
}

// NormalizeFrames clamps a requested clip length to 4n+1 frames.
func NormalizeFrames(n int) int {
	if n <= 0 {
		return defaultFrames
	}
	n = min(max(n, 17), maxFrames)
	return (n-1)/4*4 + 1
}

// WanVideo is the ComfyUI files for Wan 2.2 image-to-video with the lightx2v 4-step LoRAs:
// two 14B experts, the high-noise one for the first steps and the low-noise one for the rest.
type WanVideo struct {
	HighModel, LowModel string
	HighLoRA, LowLoRA   string
	TextEncoder, VAE    string
	Steps               int // 4 with the lightx2v LoRAs; the first half runs on HighModel
}

// DefaultWanVideo names Comfy-Org's Wan 2.2 repackaged files.
var DefaultWanVideo = WanVideo{
	HighModel:   "wan2.2_i2v_high_noise_14B_fp8_scaled.safetensors",
	LowModel:    "wan2.2_i2v_low_noise_14B_fp8_scaled.safetensors",
	HighLoRA:    "wan2.2_i2v_lightx2v_4steps_lora_v1_high_noise.safetensors",
	LowLoRA:     "wan2.2_i2v_lightx2v_4steps_lora_v1_low_noise.safetensors",
	TextEncoder: "umt5_xxl_fp8_e4m3fn_scaled.safetensors",
	VAE:         "wan_2.1_vae.safetensors",
	Steps:       4,
}

// wanNegative is the negative prompt from Wan's own examples (it reads Chinese best). With
// the 4-step LoRAs cfg is 1, so it hardly matters, but it's harmless to keep.
const wanNegative = "色调艳丽，过曝，静态，细节模糊不清，字幕，风格，作品，画作，画面，静止，整体发灰，最差质量，低质量，" +
	"JPEG压缩残留，丑陋的，残缺的，多余的手指，画得不好的手部，画得不好的脸部，畸形的，毁容的，形态畸形的肢体，" +
	"手指融合，静止不动的画面，杂乱的背景，三条腿，背景人很多，倒着走"

// videoWorkflow is ComfyUI's Wan 2.2 14B image-to-video template with the lightx2v LoRAs:
// the high-noise expert does steps [0, split), the low-noise one [split, steps).
func (w WanVideo) videoWorkflow(req VideoRequest, uploaded string) map[string]any {
	steps := w.Steps
	if steps <= 0 {
		steps = 4
	}
	split := steps / 2
	node := func(class string, inputs map[string]any) map[string]any {
		return map[string]any{"class_type": class, "inputs": inputs}
	}
	return map[string]any{
		"unet_high": node("UNETLoader", map[string]any{"unet_name": w.HighModel, "weight_dtype": "default"}),
		"unet_low":  node("UNETLoader", map[string]any{"unet_name": w.LowModel, "weight_dtype": "default"}),
		"lora_high": node("LoraLoaderModelOnly", map[string]any{"lora_name": w.HighLoRA, "strength_model": 1.0, "model": []any{"unet_high", 0}}),
		"lora_low":  node("LoraLoaderModelOnly", map[string]any{"lora_name": w.LowLoRA, "strength_model": 1.0, "model": []any{"unet_low", 0}}),
		"ms_high":   node("ModelSamplingSD3", map[string]any{"shift": 5.0, "model": []any{"lora_high", 0}}),
		"ms_low":    node("ModelSamplingSD3", map[string]any{"shift": 5.0, "model": []any{"lora_low", 0}}),
		"clip":      node("CLIPLoader", map[string]any{"clip_name": w.TextEncoder, "type": "wan", "device": "default"}),
		"pos":       node("CLIPTextEncode", map[string]any{"text": req.Prompt, "clip": []any{"clip", 0}}),
		"neg":       node("CLIPTextEncode", map[string]any{"text": wanNegative, "clip": []any{"clip", 0}}),
		"vae":       node("VAELoader", map[string]any{"vae_name": w.VAE}),
		"start":     node("LoadImage", map[string]any{"image": uploaded}),
		"i2v": node("WanImageToVideo", map[string]any{
			"positive": []any{"pos", 0}, "negative": []any{"neg", 0}, "vae": []any{"vae", 0}, "start_image": []any{"start", 0},
			"width": req.Width, "height": req.Height, "length": req.Frames, "batch_size": 1,
		}),
		"sample_high": node("KSamplerAdvanced", map[string]any{
			"add_noise": "enable", "noise_seed": req.Seed, "steps": steps, "cfg": 1.0, "sampler_name": "euler", "scheduler": "simple",
			"start_at_step": 0, "end_at_step": split, "return_with_leftover_noise": "enable",
			"model": []any{"ms_high", 0}, "positive": []any{"i2v", 0}, "negative": []any{"i2v", 1}, "latent_image": []any{"i2v", 2},
		}),
		"sample_low": node("KSamplerAdvanced", map[string]any{
			"add_noise": "disable", "noise_seed": req.Seed, "steps": steps, "cfg": 1.0, "sampler_name": "euler", "scheduler": "simple",
			"start_at_step": split, "end_at_step": 10000, "return_with_leftover_noise": "disable",
			"model": []any{"ms_low", 0}, "positive": []any{"i2v", 0}, "negative": []any{"i2v", 1}, "latent_image": []any{"sample_high", 0},
		}),
		"decode": node("VAEDecode", map[string]any{"samples": []any{"sample_low", 0}, "vae": []any{"vae", 0}}),
		"video":  node("CreateVideo", map[string]any{"images": []any{"decode", 0}, "fps": float64(req.FPS)}),
		"out":    node("SaveVideo", map[string]any{"video": []any{"video", 0}, "filename_prefix": "homellm/video", "format": "mp4", "codec": "h264"}),
	}
}

// Animate runs Wan 2.2 image-to-video on ComfyUI. Progress is reported as overall sampling
// steps: the two experts each report their own half, so they're stitched into one count.
func (c *ComfyUI) Animate(ctx context.Context, req VideoRequest, onProgress func(step, steps int)) (Video, error) {
	w := c.Video
	if w.HighModel == "" {
		w = DefaultWanVideo
	}
	uploaded, err := c.uploadImage(ctx, req.Image)
	if err != nil {
		return Video{}, err
	}
	steps := w.Steps
	if steps <= 0 {
		steps = 4
	}
	split := steps / 2
	data, err := c.run(ctx, w.videoWorkflow(req, uploaded), func(node string, value, _ int) {
		if onProgress == nil {
			return
		}
		switch node {
		case "sample_high":
			onProgress(value, steps)
		case "sample_low":
			onProgress(split+value, steps)
		}
	}, maxVideoFileBytes)
	if err != nil {
		return Video{}, err
	}
	return Video{MIME: "video/mp4", Data: data}, nil
}

// uploadImage puts the start image into ComfyUI's input folder (overwriting one fixed name,
// so start images don't pile up there) and returns the name LoadImage should use.
func (c *ComfyUI) uploadImage(ctx context.Context, img Image) (string, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	ext := ".jpg"
	if img.MIME == "image/png" {
		ext = ".png"
	}
	part, err := mw.CreateFormFile("image", "homellm-start"+ext)
	if err != nil {
		return "", err
	}
	part.Write(img.Data)
	mw.WriteField("overwrite", "true")
	mw.Close()
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url("/upload/image"), &body)
	if err != nil {
		return "", err
	}
	hreq.Header.Set("Content-Type", mw.FormDataContentType())
	var out struct {
		Name      string `json:"name"`
		Subfolder string `json:"subfolder"`
	}
	if err := c.do(hreq, &out); err != nil {
		return "", fmt.Errorf("uploading the start image: %w", err)
	}
	if out.Subfolder != "" {
		return out.Subfolder + "/" + out.Name, nil
	}
	return out.Name, nil
}

// fetchOutput downloads the job's first output file of any kind (images, videos, gifs),
// waiting for it to show up in ComfyUI's history.
func (c *ComfyUI) fetchOutput(ctx context.Context, promptID string, maxBytes int64) ([]byte, error) {
	deadline := time.Now().Add(historyWait)
	var f comfyFile
	for {
		var history map[string]struct {
			Outputs map[string]map[string]json.RawMessage `json:"outputs"`
		}
		if err := c.getJSON(ctx, "/history/"+url.PathEscape(promptID), &history); err != nil {
			return nil, err
		}
		found := false
		for _, list := range history[promptID].Outputs["out"] {
			var files []comfyFile
			if json.Unmarshal(list, &files) == nil && len(files) > 0 && files[0].Filename != "" {
				f, found = files[0], true
				break
			}
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			return nil, errors.New("the generator finished without an output")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	q := url.Values{"filename": {f.Filename}, "subfolder": {f.Subfolder}, "type": {f.Type}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url("/view?"+q.Encode()), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching the output: %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("the output is larger than %d MB", maxBytes>>20)
	}
	return data, nil
}

// MockVideo returns a tiny fake clip, stepping like a real sampler, for tests.
type MockVideo struct{}

func (MockVideo) Free(context.Context) error { return nil }

func (MockVideo) Animate(ctx context.Context, req VideoRequest, onProgress func(step, steps int)) (Video, error) {
	for i := 1; i <= 4; i++ {
		select {
		case <-ctx.Done():
			return Video{}, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
		if onProgress != nil {
			onProgress(i, 4)
		}
	}
	// Not a playable MP4 - just the right magic bytes and the request echoed, for tests.
	return Video{MIME: "video/mp4", Data: append([]byte("\x00\x00\x00\x18ftypmp42"), []byte(req.Prompt)...)}, nil
}
