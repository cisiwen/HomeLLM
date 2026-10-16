package home

import (
	"context"
	"log/slog"
	"sync"
)

// GPU modes: which kind of model currently owns the GPU.
const (
	gpuLLM   = "llm"
	gpuImage = "image" // ComfyUI: images and clips
	gpuSound = "sound" // the Foley service
)

// gpuArbiter shares one GPU between the LLM and the image model, which don't fit in its
// memory together. Any number of jobs of the current mode run at once; a job of the other
// mode waits for them to finish, then switchTo frees the old model before it starts. An
// unknown mode (at startup) counts as needing a switch, so whatever an earlier run left
// loaded gets freed too.
type gpuArbiter struct {
	switchTo func(ctx context.Context, mode string) error

	mu      sync.Mutex
	mode    string
	active  int
	changed chan struct{} // closed (and replaced) whenever active drops to 0

	// switching is held for the duration of a mode switch, so same-mode jobs that arrive
	// meanwhile wait for the old model to be gone instead of loading alongside it.
	switching sync.Mutex
}

func newGPUArbiter(switchTo func(ctx context.Context, mode string) error) *gpuArbiter {
	return &gpuArbiter{switchTo: switchTo, changed: make(chan struct{})}
}

// acquire waits until a job of this mode may use the GPU. Call release when done.
func (g *gpuArbiter) acquire(ctx context.Context, mode string) (release func(), err error) {
	for {
		g.mu.Lock()
		if g.active == 0 || g.mode == mode {
			needSwitch := g.mode != mode
			g.mode = mode
			g.active++
			g.mu.Unlock()

			g.switching.Lock()
			if needSwitch && g.switchTo != nil {
				// A failed switch isn't fatal: the job may still fit, and if it doesn't, its
				// own error says so more usefully than this would.
				if err := g.switchTo(ctx, mode); err != nil {
					slog.Warn("freeing the GPU for the next job failed", "mode", mode, "err", err)
				}
			}
			g.switching.Unlock()
			return g.release, nil
		}
		ch := g.changed
		g.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (g *gpuArbiter) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.active--
	if g.active == 0 {
		close(g.changed)
		g.changed = make(chan struct{})
	}
}
