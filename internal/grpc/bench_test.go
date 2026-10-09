package grpc

import (
	"testing"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"

	"github.com/taha2samy/hypergate/internal/config"
	"github.com/taha2samy/hypergate/internal/engine"
	"github.com/taha2samy/hypergate/internal/memory"
	"github.com/taha2samy/hypergate/internal/router"
)

// benchServer is a server with an empty default chain and the engine's default
// pool settings.
func benchServer(b *testing.B) *Server {
	b.Helper()
	registry := engine.NewChainRegistry()
	cfg := &config.Config{Router: config.RouterConfig{DefaultChain: "c"}}
	registry.Swap(engine.NewSnapshot(cfg, map[string]engine.Chain{"c": {}}), nil)
	return &Server{
		pool:     memory.NewContextPool(64, 65536),
		router:   router.NewEngineRouter(),
		registry: registry,
		executor: engine.NewChainExecutor(),
	}
}

func benchProcess(b *testing.B, msgs ...*extprocv3.ProcessingRequest) {
	s := benchServer(b)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		stream := &fakeStream{}
		for pb.Next() {
			stream.in = append(stream.in[:0], msgs...)
			stream.out = stream.out[:0]
			if err := s.Process(stream); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkProcess_Headers is one request with headers only.
func BenchmarkProcess_Headers(b *testing.B) {
	benchProcess(b, reqHeaders("/api/orders?id=1", true, "user-agent", "bench", "x-request-id", "abc"))
}

// BenchmarkProcess_Body16K is one request with a 16 KiB body.
func BenchmarkProcess_Body16K(b *testing.B) {
	benchProcess(b, reqHeaders("/api/orders", false, "content-type", "application/json"),
		&extprocv3.ProcessingRequest{Request: &extprocv3.ProcessingRequest_RequestBody{
			RequestBody: &extprocv3.HttpBody{Body: make([]byte, 16<<10), EndOfStream: true},
		}})
}
