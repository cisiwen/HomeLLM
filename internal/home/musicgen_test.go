package home

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseMusicBrief(t *testing.T) {
	cases := []struct {
		reply string
		tags  string
		bpm   int
		key   string
	}{
		{`{"tags": "lofi hip hop, mellow, rhodes, vinyl crackle", "bpm": 82, "key": "A minor"}`, "lofi hip hop, mellow, rhodes, vinyl crackle", 82, "A minor"},
		{"Sure!\n```json\n{\"tags\":\"epic orchestral\",\"bpm\":\"128\",\"key\":\"D major\"}\n```", "epic orchestral", 128, "D major"},
		{`{"tags": "x", "bpm": 900, "key": "H dorian"}`, "x", 90, "C major"}, // out of range -> defaults
		{"no json at all", "calm cinematic ambient, soft piano, warm pads", 90, "C major"},
	}
	for _, c := range cases {
		tags, bpm, key := parseMusicBrief(c.reply)
		if tags != c.tags || bpm != c.bpm || key != c.key {
			t.Errorf("parseMusicBrief(%q) = %q, %d, %q; want %q, %d, %q", c.reply, tags, bpm, key, c.tags, c.bpm, c.key)
		}
	}
}

func TestMusicWorkflowIsInstrumental(t *testing.T) {
	c := &ComfyUI{}
	wf, _ := json.Marshal(c.musicWorkflow(MusicRequest{Tags: "calm piano", BPM: 80, Key: "A minor", Seconds: 7, Seed: 5}))
	for _, want := range []string{`"lyrics":"[Instrumental]"`, `"tags":"calm piano"`, `"bpm":80`, `"keyscale":"A minor"`, `"duration":7`, `"ckpt_name":"` + DefaultMusicCheckpoint + `"`, `"steps":8`} {
		if !strings.Contains(string(wf), want) {
			t.Errorf("music workflow is missing %s", want)
		}
	}
}

// TestMixerWithFFmpeg mixes a tone under a clip with and without its own audio track. Skipped
// where ffmpeg isn't installed.
func TestMixerWithFFmpeg(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		if out, err := exec.Command(ffmpeg, append([]string{"-v", "error", "-y"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg %v: %v %s", args, err, out)
		}
	}
	silent, voiced, tone := filepath.Join(dir, "silent.mp4"), filepath.Join(dir, "voiced.mp4"), filepath.Join(dir, "tone.flac")
	run("-f", "lavfi", "-i", "color=c=blue:s=160x96:d=3:r=16", "-c:v", "libx264", "-pix_fmt", "yuv420p", silent)
	run("-i", silent, "-f", "lavfi", "-i", "anoisesrc=d=3:a=0.1", "-c:v", "copy", "-c:a", "aac", "-shortest", voiced)
	run("-f", "lavfi", "-i", "sine=frequency=440:duration=5", tone)
	music, _ := os.ReadFile(tone)

	m := Mixer{FFmpeg: ffmpeg}
	for _, name := range []string{silent, voiced} {
		data, _ := os.ReadFile(name)
		out, err := m.Mix(context.Background(), Video{MIME: "video/mp4", Data: data}, Audio{MIME: "audio/flac", Data: music})
		if err != nil {
			t.Fatalf("Mix(%s): %v", filepath.Base(name), err)
		}
		mixed := filepath.Join(dir, "mixed-"+filepath.Base(name))
		os.WriteFile(mixed, out.Data, 0o600)
		seconds, hasAudio, err := m.probe(context.Background(), mixed)
		if err != nil || !hasAudio || seconds < 2.8 || seconds > 3.3 {
			t.Errorf("Mix(%s) -> %.2fs, audio=%v, err=%v; want ~3s with audio", filepath.Base(name), seconds, hasAudio, err)
		}
	}
}
