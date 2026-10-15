package home

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestGPUArbiterSwitchesOnlyBetweenModes(t *testing.T) {
	var mu sync.Mutex
	var switches []string
	g := newGPUArbiter(func(_ context.Context, mode string) error {
		mu.Lock()
		switches = append(switches, mode)
		mu.Unlock()
		return nil
	})
	ctx := context.Background()

	r1, _ := g.acquire(ctx, gpuLLM)
	r2, _ := g.acquire(ctx, gpuLLM) // same mode: runs alongside, no switch
	r1()
	r2()
	r3, _ := g.acquire(ctx, gpuImage)
	r3()
	r4, _ := g.acquire(ctx, gpuImage)
	r4()

	mu.Lock()
	defer mu.Unlock()
	if want := []string{gpuLLM, gpuImage}; len(switches) != 2 || switches[0] != want[0] || switches[1] != want[1] {
		t.Fatalf("switches = %v, want %v", switches, want)
	}
}

func TestGPUArbiterOtherModeWaits(t *testing.T) {
	g := newGPUArbiter(nil)
	ctx := context.Background()
	release, _ := g.acquire(ctx, gpuLLM)

	got := make(chan struct{})
	go func() {
		r, err := g.acquire(ctx, gpuImage)
		if err == nil {
			r()
		}
		close(got)
	}()
	select {
	case <-got:
		t.Fatal("image job started while an LLM job held the GPU")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("image job never started after the LLM job finished")
	}
}

func TestGPUArbiterWaitHonoursCancel(t *testing.T) {
	g := newGPUArbiter(nil)
	release, _ := g.acquire(context.Background(), gpuLLM)
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := g.acquire(ctx, gpuImage); err == nil {
		t.Fatal("acquire succeeded despite the other mode holding the GPU")
	}
}
