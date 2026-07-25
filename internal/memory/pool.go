package memory

import (
	"sync"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"

	"github.com/taha/myprog/internal/engine"
)

type ContextPool struct {
	pool *sync.Pool
}

func NewContextPool(initialCap int) *ContextPool {
	if initialCap <= 0 {
		initialCap = 64
	}
	sliceCap := initialCap / 2
	if sliceCap <= 0 {
		sliceCap = 10
	}

	return &ContextPool{
		pool: &sync.Pool{
			New: func() interface{} {
				return &engine.RequestContext{
					Headers:              make(map[string]string, initialCap),
					HeadersToAdd:         make([]engine.Header, 0, sliceCap),
					ResponseHeadersToAdd: make([]engine.Header, 0, sliceCap),
					HeadersToRemove:      make([]string, 0, sliceCap),
					UpstreamShadow:       make(map[string]string, initialCap),
					DownstreamShadow:     make(map[string]string, initialCap),
					SetHeaderOptions:     make([]*corev3.HeaderValueOption, 0, sliceCap),
				}
			},
		},
	}
}

func (cp *ContextPool) Prewarm(count int) {
	if count <= 0 {
		return
	}
	objs := make([]*engine.RequestContext, count)
	for i := 0; i < count; i++ {
		objs[i] = cp.Acquire()
	}
	for i := 0; i < count; i++ {
		cp.Release(objs[i])
	}
}

func (cp *ContextPool) Acquire() *engine.RequestContext {
	return cp.pool.Get().(*engine.RequestContext)
}

func (cp *ContextPool) Release(ctx *engine.RequestContext) {
	ctx.Reset()
	cp.pool.Put(ctx)
}
