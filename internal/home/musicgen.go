package home

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// MusicRequest is one instrumental background track.
type MusicRequest struct {
	Tags    string  // style, mood and instruments, e.g. "calm ambient, soft piano, warm pads"
	BPM     int     // tempo
	Key     string  // e.g. "A minor"
	Seconds float64 // length; a little longer than the clip, which trims it
	Seed    int64
}

// Audio is an encoded audio track.
type Audio struct {
	MIME string
	Data []byte
}

// MusicBackend composes a background track.
type MusicBackend interface {
	Compose(ctx context.Context, req MusicRequest) (Audio, error)
	Free(ctx context.Context) error
}

// DefaultMusicCheckpoint is Comfy-Org's all-in-one ACE-Step 1.5 turbo (MIT; trained on
// licensed music, and its output may be used commercially).
const DefaultMusicCheckpoint = "ace_step_1.5_turbo_aio.safetensors"

// musicWorkflow is ComfyUI's ACE-Step 1.5 checkpoint template, made instrumental: 8 turbo
// steps at cfg 1, the negative a zeroed copy of the positive.
func (c *ComfyUI) musicWorkflow(req MusicRequest) map[string]any {
	ckpt := c.MusicCheckpoint
	if ckpt == "" {
		ckpt = DefaultMusicCheckpoint
	}
	node := func(class string, inputs map[string]any) map[string]any {
		return map[string]any{"class_type": class, "inputs": inputs}
	}
	return map[string]any{
		"ckpt": node("CheckpointLoaderSimple", map[string]any{"ckpt_name": ckpt}),
		"text": node("TextEncodeAceStepAudio1.5", map[string]any{
			"clip": []any{"ckpt", 1}, "tags": req.Tags, "lyrics": "[Instrumental]", "seed": req.Seed,
			"bpm": req.BPM, "duration": req.Seconds, "timesignature": "4", "language": "en", "keyscale": req.Key,
			"generate_audio_codes": true, "cfg_scale": 2.0, "temperature": 0.85, "top_p": 0.9, "top_k": 0, "min_p": 0.0,
		}),
		"neg":    node("ConditioningZeroOut", map[string]any{"conditioning": []any{"text", 0}}),
		"latent": node("EmptyAceStep1.5LatentAudio", map[string]any{"seconds": req.Seconds, "batch_size": 1}),
		"shift":  node("ModelSamplingAuraFlow", map[string]any{"shift": 3.0, "model": []any{"ckpt", 0}}),
		"sample": node("KSampler", map[string]any{"seed": req.Seed, "steps": 8, "cfg": 1.0, "sampler_name": "euler", "scheduler": "simple", "denoise": 1.0, "model": []any{"shift", 0}, "positive": []any{"text", 0}, "negative": []any{"neg", 0}, "latent_image": []any{"latent", 0}}),
		"decode": node("VAEDecodeAudio", map[string]any{"samples": []any{"sample", 0}, "vae": []any{"ckpt", 2}}),
		"out":    node("SaveAudioAdvanced", map[string]any{"audio": []any{"decode", 0}, "filename_prefix": "homellm/music", "format": "flac"}),
	}
}

// Compose runs ACE-Step 1.5 on ComfyUI. Progress isn't reported: 8 steps take seconds.
func (c *ComfyUI) Compose(ctx context.Context, req MusicRequest) (Audio, error) {
	data, err := c.run(ctx, c.musicWorkflow(req), nil, MaxFileSize)
	if err != nil {
		return Audio{}, err
	}
	return Audio{MIME: "audio/flac", Data: data}, nil
}

// musicInstructions has the LLM pick a fitting instrumental style for a clip.
// It gives an example rather than describing the tags' format: given "under 20 words", models
// copied the phrase into the tags themselves.
const musicInstructions = "You choose instrumental background music for a short video clip. From " +
	"the user's description of the clip, pick a genre, a mood and two to four instruments, and " +
	"reply with only a JSON object like this example: {\"tags\": \"lofi hip hop, mellow, " +
	"electric piano, soft drums\", \"bpm\": 80, \"key\": \"A minor\"}. Tags are short English " +
	"comma-separated words. No vocals, no singing, no lyrics. Tempo between 60 and 160. No preamble."

// Tags that must never reach the music model: word-count phrases copied from instructions,
// and anything asking for a voice in what is always an instrumental track.
var (
	wordCountTag = regexp.MustCompile(`(?i)\b(under|less than|at most|max(imum)?|up to)\s+\d+\s+words?\b`)
	vocalTag     = regexp.MustCompile(`(?i)\b(vocals?|voices?|singing|singer|choir|lyrics?|rap(ping)?|humming)\b`)
)

// cleanMusicTags drops unwanted tags from a comma-separated list.
func cleanMusicTags(tags string) string {
	var kept []string
	for _, t := range strings.Split(tags, ",") {
		t = strings.TrimSpace(t)
		if t == "" || wordCountTag.MatchString(t) || vocalTag.MatchString(t) {
			continue
		}
		kept = append(kept, t)
	}
	return strings.Join(kept, ", ")
}

var keyPattern = regexp.MustCompile(`^[A-G][#b]? (major|minor)$`)

// parseMusicBrief reads the LLM's JSON reply, falling back to safe defaults for anything
// missing or out of range.
func parseMusicBrief(reply string) (tags string, bpm int, key string) {
	tags, bpm, key = "calm cinematic ambient, soft piano, warm pads", 90, "C major"
	if i, j := strings.Index(reply, "{"), strings.LastIndex(reply, "}"); i >= 0 && j > i {
		var b struct {
			Tags string          `json:"tags"`
			BPM  json.RawMessage `json:"bpm"`
			Key  string          `json:"key"`
		}
		if json.Unmarshal([]byte(reply[i:j+1]), &b) == nil {
			if t := cleanMusicTags(b.Tags); t != "" {
				tags = t
			}
			if n, err := strconv.Atoi(strings.Trim(string(b.BPM), `" `)); err == nil && n >= 50 && n <= 200 {
				bpm = n
			}
			if k := strings.TrimSpace(b.Key); keyPattern.MatchString(k) {
				key = k
			}
		}
	}
	return tags, bpm, key
}

// Mixer lays a music track under a clip with ffmpeg: below the clip's own soundtrack (Foley)
// when it has one, or as its only sound.
type Mixer struct {
	FFmpeg string // path to ffmpeg; ffprobe is looked for next to it
}

// musicVolume is how loud music sits under a soundtrack - background, not the main event.
const musicVolume = 0.35

func (m Mixer) ffprobe() string {
	dir, name := filepath.Split(m.FFmpeg)
	return filepath.Join(dir, strings.Replace(name, "ffmpeg", "ffprobe", 1))
}

// Mix returns the clip with music, faded in and out over the clip's own length.
func (m Mixer) Mix(ctx context.Context, clip Video, music Audio) (Video, error) {
	if m.FFmpeg == "" {
		return Video{}, errors.New("no ffmpeg configured for mixing music")
	}
	dir, err := os.MkdirTemp("", "homellm-mix-")
	if err != nil {
		return Video{}, err
	}
	defer os.RemoveAll(dir)
	in, track, out := filepath.Join(dir, "in.mp4"), filepath.Join(dir, "music.flac"), filepath.Join(dir, "out.mp4")
	if err := os.WriteFile(in, clip.Data, 0o600); err != nil {
		return Video{}, err
	}
	if err := os.WriteFile(track, music.Data, 0o600); err != nil {
		return Video{}, err
	}

	length, hasAudio, err := m.probe(ctx, in)
	if err != nil {
		return Video{}, err
	}

	fadeOut := max(length-1.5, 0)
	music1 := fmt.Sprintf("[1:a]atrim=0:%.2f,afade=t=in:d=0.5,afade=t=out:st=%.2f:d=1.5", length, fadeOut)
	var filter string
	if hasAudio {
		filter = fmt.Sprintf("%s,volume=%.2f[m];[0:a][m]amix=inputs=2:duration=first:normalize=0[a]", music1, musicVolume)
	} else {
		filter = music1 + "[a]"
	}
	cmd := exec.CommandContext(ctx, m.FFmpeg, "-v", "error", "-y", "-i", in, "-i", track,
		"-filter_complex", filter, "-map", "0:v:0", "-map", "[a]", "-c:v", "copy", "-c:a", "aac", "-b:a", "192k",
		"-t", fmt.Sprintf("%.3f", length), out)
	if msg, err := cmd.CombinedOutput(); err != nil {
		return Video{}, fmt.Errorf("mixing music: %v: %s", err, strings.TrimSpace(string(msg)))
	}
	data, err := os.ReadFile(out)
	if err != nil {
		return Video{}, err
	}
	return Video{MIME: "video/mp4", Data: data}, nil
}

// probe returns a clip file's length and whether it has an audio track.
func (m Mixer) probe(ctx context.Context, path string) (seconds float64, hasAudio bool, err error) {
	out, err := exec.CommandContext(ctx, m.ffprobe(), "-v", "error", "-show_entries", "format=duration:stream=codec_type", "-of", "json", path).Output()
	if err != nil {
		return 0, false, fmt.Errorf("probing the clip: %w", err)
	}
	var info struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
		Streams []struct {
			CodecType string `json:"codec_type"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return 0, false, fmt.Errorf("probing the clip: %w", err)
	}
	seconds, _ = strconv.ParseFloat(info.Format.Duration, 64)
	if seconds <= 0 {
		seconds = 5
	}
	for _, s := range info.Streams {
		hasAudio = hasAudio || s.CodecType == "audio"
	}
	return seconds, hasAudio, nil
}

// Duration returns a clip's length in seconds.
func (m Mixer) Duration(ctx context.Context, clip Video) (float64, error) {
	f, err := os.CreateTemp("", "homellm-probe-*.mp4")
	if err != nil {
		return 0, err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(clip.Data)
	f.Close()
	if err != nil {
		return 0, err
	}
	seconds, _, err := m.probe(ctx, f.Name())
	return seconds, err
}

// MockMusic returns a few bytes of "audio", for tests.
type MockMusic struct{}

func (MockMusic) Free(context.Context) error { return nil }

func (MockMusic) Compose(ctx context.Context, req MusicRequest) (Audio, error) {
	select {
	case <-ctx.Done():
		return Audio{}, ctx.Err()
	case <-time.After(20 * time.Millisecond):
	}
	return Audio{MIME: "audio/flac", Data: []byte("fLaC" + req.Tags)}, nil
}
