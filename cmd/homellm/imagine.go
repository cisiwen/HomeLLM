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

// runAnimateCheck is `homellm animate [flags] -image start.jpg <motion prompt>`: makes one
// clip through ComfyUI's Wan 2.2 image-to-video, to check and time this PC's video setup.
func runAnimateCheck(args []string) {
	fs := flag.NewFlagSet("animate", flag.ExitOnError)
	comfyURL := fs.String("comfy-url", "http://127.0.0.1:8188", "base URL of the ComfyUI server")
	imagePath := fs.String("image", "", "start image (JPEG or PNG)")
	frames := fs.Int("frames", 81, "frames at 16 fps (4n+1; 81 = about 5 s)")
	seed := fs.Int64("seed", 1, "seed")
	out := fs.String("out", "homellm-animate.mp4", "where to save the clip")
	fs.Parse(args)
	prompt := strings.Join(fs.Args(), " ")
	if prompt == "" || *imagePath == "" {
		fmt.Fprintln(os.Stderr, "usage: homellm animate [flags] -image start.jpg <motion prompt>")
		os.Exit(2)
	}
	data, err := os.ReadFile(*imagePath)
	if err != nil {
		fatal("reading -image", err)
	}
	mime := "image/jpeg"
	if strings.HasSuffix(strings.ToLower(*imagePath), ".png") {
		mime = "image/png"
	}
	img := home.Image{MIME: mime, Data: data}
	req := home.VideoRequest{Image: img, Prompt: prompt, Frames: home.NormalizeFrames(*frames), FPS: 16, Seed: *seed}
	req.Width, req.Height = home.VideoSize(img)

	c := &home.ComfyUI{BaseURL: *comfyURL}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	start := time.Now()
	fmt.Printf("animating %dx%d, %d frames\n", req.Width, req.Height, req.Frames)
	clip, err := c.Animate(ctx, req, func(step, total int) {
		fmt.Printf("[%s] step %d/%d\n", time.Since(start).Round(time.Second), step, total)
	})
	if err != nil {
		fatal("animating", err)
	}
	if err := os.WriteFile(*out, clip.Data, 0o644); err != nil {
		fatal("saving", err)
	}
	fmt.Printf("done in %s: %d bytes -> %s\n", time.Since(start).Round(time.Second), len(clip.Data), *out)
}

// runMusicCheck is `homellm music [flags] -clip in.mp4 <style tags>`: composes a background
// track with ComfyUI's ACE-Step 1.5 and mixes it under the clip, to check and time a PC.
func runMusicCheck(args []string) {
	fs := flag.NewFlagSet("music", flag.ExitOnError)
	comfyURL := fs.String("comfy-url", "http://127.0.0.1:8188", "base URL of the ComfyUI server")
	clipPath := fs.String("clip", "", "the clip (MP4) to put music under")
	ffmpeg := fs.String("ffmpeg", "ffmpeg", "ffmpeg (ffprobe must be next to it)")
	bpm := fs.Int("bpm", 90, "tempo")
	key := fs.String("key", "C major", "key")
	seed := fs.Int64("seed", 1, "seed")
	out := fs.String("out", "homellm-music.mp4", "where to save the clip with music")
	fs.Parse(args)
	tags := strings.Join(fs.Args(), " ")
	if tags == "" || *clipPath == "" {
		fmt.Fprintln(os.Stderr, "usage: homellm music [flags] -clip in.mp4 <style tags>")
		os.Exit(2)
	}
	data, err := os.ReadFile(*clipPath)
	if err != nil {
		fatal("reading -clip", err)
	}
	clip := home.Video{MIME: "video/mp4", Data: data}
	mixer := home.Mixer{FFmpeg: *ffmpeg}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	seconds, err := mixer.Duration(ctx, clip)
	if err != nil {
		fatal("probing -clip", err)
	}
	start := time.Now()
	c := &home.ComfyUI{BaseURL: *comfyURL}
	track, err := c.Compose(ctx, home.MusicRequest{Tags: tags, BPM: *bpm, Key: *key, Seconds: seconds + 2, Seed: *seed})
	if err != nil {
		fatal("composing", err)
	}
	fmt.Printf("composed %.1fs of music in %s\n", seconds+2, time.Since(start).Round(100*time.Millisecond))
	mixed, err := mixer.Mix(ctx, clip, track)
	if err != nil {
		fatal("mixing", err)
	}
	if err := os.WriteFile(*out, mixed.Data, 0o644); err != nil {
		fatal("saving", err)
	}
	fmt.Printf("done in %s: %d bytes -> %s\n", time.Since(start).Round(100*time.Millisecond), len(mixed.Data), *out)
}
