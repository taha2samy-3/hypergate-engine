package memory_test

import (
	"testing"

	"github.com/taha2samy/hypergate/internal/memory"
)

func TestContextPool_ReleaseDropsBodiesAndKeepsMaps(t *testing.T) {
	pool := memory.NewContextPool(64)
	ctx := pool.Acquire()
	body := make([]byte, 1<<20)
	ctx.RequestBody = body
	ctx.ResponseBodyBytes = body
	ctx.Headers["x"] = "1"
	pool.Release(ctx)

	again := pool.Acquire()
	if again.RequestBody != nil || again.ResponseBodyBytes != nil {
		t.Fatal("a released context must not keep the previous request's body alive")
	}
	if len(again.Headers) != 0 || again.Headers == nil {
		t.Fatalf("headers must be cleared but kept allocated, got %v", again.Headers)
	}
	pool.Release(again)
}

func TestContextPool_NoAllocationsOnceWarm(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector makes sync.Pool drop objects on purpose")
	}
	pool := memory.NewContextPool(64)
	pool.Prewarm(8)
	allocs := testing.AllocsPerRun(1000, func() {
		ctx := pool.Acquire()
		ctx.Headers[":path"] = "/"
		pool.Release(ctx)
	})
	if allocs != 0 {
		t.Fatalf("Acquire/Release allocated %.1f times per run", allocs)
	}
}
