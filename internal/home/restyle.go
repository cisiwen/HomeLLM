package home

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"strings"
	"time"
)

// RestyleRequest redraws a photo in another style ("turn this into a cartoon"). Prompt is the
// full edit instruction (see RestylePrompt).
type RestyleRequest struct {
	Image  Image
	Prompt string
	Seed   int64
}

// RestyleBackend edits a photo by instruction, keeping its people and composition.
type RestyleBackend interface {
	Restyle(ctx context.Context, req RestyleRequest, onProgress func(step, steps int)) (Image, error)
	Free(ctx context.Context) error
}

// defaultRestyle is what an empty style asks for: the cartoon look.
const defaultRestyle = "a cartoon illustration with bold clean outlines, flat vibrant colours and simple cel shading"

// restyleKeep tells the edit model what must survive; FLUX Kontext otherwise tends to swap
// faces for generic ones.
const restyleKeep = "Keep the same people, faces, expressions, poses, clothing, composition and background."

// RestylePrompt turns a short style ("watercolour", "Pixar-like 3D", or nothing for a cartoon)
// into an edit instruction Kontext follows well.
func RestylePrompt(style string) string {
	style = strings.TrimRight(strings.TrimSpace(style), ".")
	if style == "" {
		style = defaultRestyle
	}
	return "Redraw this photo in the style of " + style + ". " + restyleKeep
}

// FluxKontext is ComfyUI's files for FLUX.1 Kontext [dev], an instruction-following image
// editor. It's a 12B model separate from the FLUX schnell checkpoint used for text-to-image.
type FluxKontext struct {
	Model    string // models/diffusion_models
	CLIP, T5 string // models/text_encoders
	VAE      string // models/vae
	Steps    int
	Guidance float64
}

// DefaultFluxKontext names Comfy-Org's repackaged Kontext files, with the settings from
// ComfyUI's own Kontext template.
var DefaultFluxKontext = FluxKontext{
	Model:    "flux1-dev-kontext_fp8_scaled.safetensors",
	CLIP:     "clip_l.safetensors",
	T5:       "t5xxl_fp8_e4m3fn_scaled.safetensors",
	VAE:      "ae.safetensors",
	Steps:    20,
	Guidance: 2.5,
}

// restyleWorkflow is ComfyUI's FLUX.1 Kontext [dev] template: the photo is scaled to a size
// Kontext was trained on, encoded, and attached to the instruction as a reference latent;
// sampling starts from that latent at full denoise, so the size follows the photo.
func (k FluxKontext) restyleWorkflow(req RestyleRequest, uploaded string) map[string]any {
	steps := k.Steps
	if steps <= 0 {
		steps = DefaultFluxKontext.Steps
	}
	guidance := k.Guidance
	if guidance <= 0 {
		guidance = DefaultFluxKontext.Guidance
	}
	node := func(class string, inputs map[string]any) map[string]any {
		return map[string]any{"class_type": class, "inputs": inputs}
	}
	return map[string]any{
		"unet":  node("UNETLoader", map[string]any{"unet_name": k.Model, "weight_dtype": "default"}),
		"clip":  node("DualCLIPLoader", map[string]any{"clip_name1": k.CLIP, "clip_name2": k.T5, "type": "flux", "device": "default"}),
		"vae":   node("VAELoader", map[string]any{"vae_name": k.VAE}),
		"start": node("LoadImage", map[string]any{"image": uploaded}),
		"scale": node("FluxKontextImageScale", map[string]any{"image": []any{"start", 0}}),
		"enc":   node("VAEEncode", map[string]any{"pixels": []any{"scale", 0}, "vae": []any{"vae", 0}}),
		"pos":   node("CLIPTextEncode", map[string]any{"text": req.Prompt, "clip": []any{"clip", 0}}),
		"ref":   node("ReferenceLatent", map[string]any{"conditioning": []any{"pos", 0}, "latent": []any{"enc", 0}}),
		"guide": node("FluxGuidance", map[string]any{"conditioning": []any{"ref", 0}, "guidance": guidance}),
		"neg":   node("ConditioningZeroOut", map[string]any{"conditioning": []any{"pos", 0}}),
		"sample": node("KSampler", map[string]any{
			"seed": req.Seed, "steps": steps, "cfg": 1.0, "sampler_name": "euler", "scheduler": "simple", "denoise": 1.0,
			"model": []any{"unet", 0}, "positive": []any{"guide", 0}, "negative": []any{"neg", 0}, "latent_image": []any{"enc", 0},
		}),
		"decode": node("VAEDecode", map[string]any{"samples": []any{"sample", 0}, "vae": []any{"vae", 0}}),
		"out":    node("PreviewImage", map[string]any{"images": []any{"decode", 0}}),
	}
}

// Restyle runs FLUX.1 Kontext on ComfyUI.
func (c *ComfyUI) Restyle(ctx context.Context, req RestyleRequest, onProgress func(step, steps int)) (Image, error) {
	k := c.Kontext
	if k.Model == "" {
		k = DefaultFluxKontext
	}
	uploaded, err := c.uploadImage(ctx, req.Image)
	if err != nil {
		return Image{}, err
	}
	data, err := c.run(ctx, k.restyleWorkflow(req, uploaded), func(_ string, value, max int) {
		if onProgress != nil {
			onProgress(value, max)
		}
	}, MaxFileSize)
	if err != nil {
		return Image{}, err
	}
	return toJPEG(data)
}

// ImageSize reads an encoded image's dimensions, or 0, 0 if it can't.
func ImageSize(img Image) (int, int) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(img.Data))
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}

// MockRestyle posterizes the photo - a crude cartoon - stepping like a real sampler, for tests.
type MockRestyle struct{}

func (MockRestyle) Free(context.Context) error { return nil }

func (MockRestyle) Restyle(ctx context.Context, req RestyleRequest, onProgress func(step, steps int)) (Image, error) {
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
	src, _, err := image.Decode(bytes.NewReader(req.Image.Data))
	if err != nil {
		return Image{}, err
	}
	b := src.Bounds()
	out := image.NewRGBA(b)
	poster := func(v uint32) uint8 { return uint8(v>>8) / 64 * 64 }
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := src.At(x, y).RGBA()
			out.Set(x, y, color.RGBA{poster(r), poster(g), poster(bl), 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, out, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return Image{}, err
	}
	return Image{MIME: "image/jpeg", Data: buf.Bytes()}, nil
}
