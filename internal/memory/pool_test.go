package memory_test

import (
	"testing"

	"github.com/taha/myprog/internal/memory"
)

func TestContextPool_PreallocBodyBuffer(t *testing.T) {
	defaultPool := memory.NewContextPool(64, 0) // Should default to 65536
	ctx1 := defaultPool.Acquire()
	if cap(ctx1.RawBodyBuffer) != 65536 {
		t.Errorf("Expected RawBodyBuffer capacity to be 65536, got %d", cap(ctx1.RawBodyBuffer))
	}
	ctx1.RawBodyBuffer = append(ctx1.RawBodyBuffer, []byte("hello")...)
	if len(ctx1.RawBodyBuffer) != 5 {
		t.Errorf("Expected RawBodyBuffer length to be 5, got %d", len(ctx1.RawBodyBuffer))
	}

	defaultPool.Release(ctx1)

	ctx2 := defaultPool.Acquire()
	if len(ctx2.RawBodyBuffer) != 0 {
		t.Errorf("Expected RawBodyBuffer length to be reset to 0, got %d", len(ctx2.RawBodyBuffer))
	}
	if cap(ctx2.RawBodyBuffer) != 65536 {
		t.Errorf("Expected RawBodyBuffer capacity to remain 65536 after reset, got %d", cap(ctx2.RawBodyBuffer))
	}
	defaultPool.Release(ctx2)

	customPool := memory.NewContextPool(32, 1024)
	ctxCustom := customPool.Acquire()
	if cap(ctxCustom.RawBodyBuffer) != 1024 {
		t.Errorf("Expected custom RawBodyBuffer capacity to be 1024, got %d", cap(ctxCustom.RawBodyBuffer))
	}
	customPool.Release(ctxCustom)
}
