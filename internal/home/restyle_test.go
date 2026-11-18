package home

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"strings"
	"testing"
	"time"
)

func pngImage(t *testing.T, w, h int) Image {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return Image{MIME: "image/png", Data: buf.Bytes()}
}

func TestComfyUIRestyle(t *testing.T) {
	f, srv := newFakeComfy(t)
	c := &ComfyUI{BaseURL: srv.URL}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var steps []int
	img, err := c.Restyle(ctx, RestyleRequest{Image: pngImage(t, 40, 30), Prompt: RestylePrompt(""), Seed: 7}, func(step, _ int) {
		steps = append(steps, step)
	})
	if err != nil {
		t.Fatal(err)
	}
	if img.MIME != "image/jpeg" {
		t.Fatalf("got %s, want image/jpeg", img.MIME)
	}
	if w, h := ImageSize(img); w != 64 || h != 32 {
		t.Errorf("ImageSize = %dx%d, want the generated 64x32", w, h)
	}
	if len(steps) != 4 {
		t.Errorf("progress steps = %v, want 4", steps)
	}

	f.mu.Lock()
	wf, _ := json.Marshal(f.queued)
	f.mu.Unlock()
	for _, want := range []string{
		`"unet_name":"flux1-dev-kontext_fp8_scaled.safetensors"`, `"image":"homellm-start.png"`,
		`"FluxKontextImageScale"`, `"ReferenceLatent"`, `"guidance":2.5`, `"steps":20`, `"seed":7`, `cartoon`,
	} {
		if !strings.Contains(string(wf), want) {
			t.Errorf("workflow is missing %s: %s", want, wf)
		}
	}
}

func TestRestylePrompt(t *testing.T) {
	if p := RestylePrompt("  "); !strings.Contains(p, "cartoon") || !strings.Contains(p, "same people") {
		t.Errorf("empty style: %q", p)
	}
	if p := RestylePrompt("a watercolour painting."); !strings.HasPrefix(p, "Redraw this photo in the style of a watercolour painting. Keep") {
		t.Errorf("watercolour: %q", p)
	}
}

func TestMockRestyleKeepsSize(t *testing.T) {
	out, err := MockRestyle{}.Restyle(context.Background(), RestyleRequest{Image: pngImage(t, 50, 20)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if w, h := ImageSize(out); w != 50 || h != 20 {
		t.Errorf("size = %dx%d, want 50x20", w, h)
	}
}
