package memory

import (
	"sync"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"

	"github.com/taha2samy/hypergate/internal/engine"
)

type ContextPool struct {
	pool *sync.Pool
}

func NewContextPool(initialHeaderCap int, preallocBodyBytes int) *ContextPool {
	if initialHeaderCap <= 0 {
		initialHeaderCap = 64
	}
	if preallocBodyBytes <= 0 {
		preallocBodyBytes = 65536
	}
	sliceCap := initialHeaderCap / 2
	if sliceCap <= 0 {
		sliceCap = 10
	}

	return &ContextPool{
		pool: &sync.Pool{
			New: func() interface{} {
				return &engine.RequestContext{
					Headers:                 make(map[string]string, initialHeaderCap),
					ResponseHeaders:         make(map[string]string, initialHeaderCap),
					HeadersToAdd:            make([]engine.Header, 0, sliceCap),
					ResponseHeadersToAdd:    make([]engine.Header, 0, sliceCap),
					HeadersToRemove:         make([]string, 0, sliceCap),
					ResponseHeadersToRemove: make([]string, 0, sliceCap),
					UpstreamShadow:          make(map[string]string, initialHeaderCap),
					DownstreamShadow:        make(map[string]string, initialHeaderCap),
					SetHeaderOptions:        make([]*corev3.HeaderValueOption, 0, sliceCap),
					RawBodyBuffer:           make([]byte, 0, preallocBodyBytes),
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
