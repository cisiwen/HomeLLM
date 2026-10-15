package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/wweng/homellm/internal/home"
)

// runImagineCheck is `homellm imagine [flags] <prompt>`: generates one image through ComfyUI
// directly, the same way a phone's request would, to check this PC's image setup without a
// phone. It doesn't free the LLM first - unload it yourself if the GPU is short of memory.
func runImagineCheck(args []string) {
	fs := flag.NewFlagSet("imagine", flag.ExitOnError)
	comfyURL := fs.String("comfy-url", "http://127.0.0.1:8188", "base URL of the ComfyUI server")
	model := fs.String("image-model", "flux1-schnell-fp8.safetensors", "ComfyUI checkpoint")
	steps := fs.Int("image-steps", 4, "sampling steps")
	width := fs.Int("width", 1024, "image width")
	height := fs.Int("height", 1024, "image height")
	seed := fs.Int64("seed", 1, "seed")
	out := fs.String("out", "homellm-imagine.jpg", "where to save the image")
	fs.Parse(args)
	prompt := strings.Join(fs.Args(), " ")
	if prompt == "" {
		fmt.Fprintln(os.Stderr, "usage: homellm imagine [flags] <prompt>")
		os.Exit(2)
	}

	c := &home.ComfyUI{BaseURL: *comfyURL, Checkpoint: *model, Steps: *steps}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	start := time.Now()
	img, err := c.Generate(ctx, home.ImageRequest{Prompt: prompt, Width: *width, Height: *height, Seed: *seed}, func(step, total int) {
		fmt.Printf("[%s] step %d/%d\n", time.Since(start).Round(100*time.Millisecond), step, total)
	})
	if err != nil {
		fatal("generating", err)
	}
	if err := os.WriteFile(*out, img.Data, 0o644); err != nil {
		fatal("saving", err)
	}
	fmt.Printf("done in %s: %d bytes -> %s\n", time.Since(start).Round(100*time.Millisecond), len(img.Data), *out)
}
