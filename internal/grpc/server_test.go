package grpc

import (
	"context"
	"io"
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"

	"github.com/taha2samy/hypergate/internal/config"
	"github.com/taha2samy/hypergate/internal/engine"
	"github.com/taha2samy/hypergate/internal/filters/deny"
	"github.com/taha2samy/hypergate/internal/filters/header_modifier"
	"github.com/taha2samy/hypergate/internal/memory"
	"github.com/taha2samy/hypergate/internal/router"
)

// fakeStream replays a fixed sequence of ext_proc messages and records responses.
type fakeStream struct {
	grpc.ServerStream
	in  []*extprocv3.ProcessingRequest
	out []*extprocv3.ProcessingResponse
}

func (f *fakeStream) Context() context.Context { return context.Background() }

func (f *fakeStream) Recv() (*extprocv3.ProcessingRequest, error) {
	if len(f.in) == 0 {
		return nil, io.EOF
	}
	req := f.in[0]
	f.in = f.in[1:]
	return req, nil
}

func (f *fakeStream) Send(resp *extprocv3.ProcessingResponse) error {
	f.out = append(f.out, resp)
	return nil
}

func headers(kv ...string) *corev3.HeaderMap {
	m := &corev3.HeaderMap{}
	for i := 0; i+1 < len(kv); i += 2 {
		m.Headers = append(m.Headers, &corev3.HeaderValue{Key: kv[i], RawValue: []byte(kv[i+1])})
	}
	return m
}

func reqHeaders(path string, endOfStream bool, kv ...string) *extprocv3.ProcessingRequest {
	return &extprocv3.ProcessingRequest{Request: &extprocv3.ProcessingRequest_RequestHeaders{
		RequestHeaders: &extprocv3.HttpHeaders{
			Headers:     headers(append([]string{":path", path, ":method", "GET"}, kv...)...),
			EndOfStream: endOfStream,
		},
	}}
}

func respHeaders(kv ...string) *extprocv3.ProcessingRequest {
	return &extprocv3.ProcessingRequest{Request: &extprocv3.ProcessingRequest_ResponseHeaders{
		ResponseHeaders: &extprocv3.HttpHeaders{Headers: headers(append([]string{":status", "200"}, kv...)...)},
	}}
}

func reqBody(body string) *extprocv3.ProcessingRequest {
	return &extprocv3.ProcessingRequest{Request: &extprocv3.ProcessingRequest_RequestBody{
		RequestBody: &extprocv3.HttpBody{Body: []byte(body), EndOfStream: true},
	}}
}

func newTestServer(cfg *config.Config, chains map[string]engine.Chain) *Server {
	registry := engine.NewChainRegistry()
	registry.Swap(engine.NewSnapshot(cfg, chains), nil)
	return &Server{
		pool:     memory.NewContextPool(16, 1024),
		router:   router.NewEngineRouter(),
		registry: registry,
		executor: engine.NewChainExecutor(),
	}
}

func run(t *testing.T, s *Server, msgs ...*extprocv3.ProcessingRequest) []*extprocv3.ProcessingResponse {
	t.Helper()
	stream := &fakeStream{in: msgs}
	if err := s.Process(stream); err != nil {
		t.Fatalf("Process returned error: %v", err)
	}
	return stream.out
}

func TestProcess_MissingChainFailsClosed(t *testing.T) {
	cfg := &config.Config{Router: config.RouterConfig{DefaultChain: "gone"}}
	s := newTestServer(cfg, map[string]engine.Chain{})

	out := run(t, s, reqHeaders("/", true))
	imm := out[0].GetImmediateResponse()
	if imm == nil || imm.GetStatus().GetCode() != 503 {
		t.Fatalf("expected 503 immediate response, got %v", out[0])
	}
}

func TestProcess_NoPolicyLoadedFailsClosed(t *testing.T) {
	s := newTestServer(nil, nil)
	out := run(t, s, reqHeaders("/", true))
	if out[0].GetImmediateResponse() == nil {
		t.Fatalf("expected immediate response before any policy is loaded")
	}
}

func TestProcess_NoRouteNoDefaultPassesThrough(t *testing.T) {
	s := newTestServer(&config.Config{}, map[string]engine.Chain{})
	out := run(t, s, reqHeaders("/", true))
	if out[0].GetRequestHeaders() == nil {
		t.Fatalf("expected pass-through headers response, got %v", out[0])
	}
}

// blockOnBody blocks during the request body phase only.
type blockOnBody struct{}

func (blockOnBody) Execute(ctx *engine.RequestContext) error {
	if ctx.Phase == engine.PhaseRequestHeaders {
		ctx.RequestBodyRequired = true
		return nil
	}
	if string(ctx.RequestBody) == "evil" {
		ctx.Block(403, "blocked")
	}
	return nil
}

func (blockOnBody) SupportedPhases() []engine.Phase {
	return []engine.Phase{engine.PhaseRequestHeaders, engine.PhaseRequestBody}
}

func TestProcess_BlockInBodyPhaseSendsImmediateResponse(t *testing.T) {
	cfg := &config.Config{Router: config.RouterConfig{DefaultChain: "c"}}
	s := newTestServer(cfg, map[string]engine.Chain{"c": {blockOnBody{}}})

	out := run(t, s, reqHeaders("/", false), reqBody("evil"))
	if len(out) != 2 {
		t.Fatalf("expected 2 responses, got %d", len(out))
	}
	if out[0].GetModeOverride().GetRequestBodyMode().String() != "BUFFERED" {
		t.Fatalf("expected BUFFERED mode override, got %v", out[0].GetModeOverride())
	}
	if imm := out[1].GetImmediateResponse(); imm == nil || imm.GetStatus().GetCode() != 403 {
		t.Fatalf("expected 403 immediate response in body phase, got %v", out[1])
	}
}

func TestProcess_BodyRequiredButNeverSentFailsClosed(t *testing.T) {
	cfg := &config.Config{Router: config.RouterConfig{DefaultChain: "c"}}
	s := newTestServer(cfg, map[string]engine.Chain{"c": {blockOnBody{}}})

	// Envoy ignored the mode override and went straight to the response.
	out := run(t, s, reqHeaders("/", false), respHeaders())
	if imm := out[1].GetImmediateResponse(); imm == nil || imm.GetStatus().GetCode() != 500 {
		t.Fatalf("expected 500 when body inspection was skipped, got %v", out[1])
	}
}

func TestProcess_DownstreamHeaderRemoval(t *testing.T) {
	cfg := &config.Config{Router: config.RouterConfig{DefaultChain: "c"}}
	hm := header_modifier.NewHeaderModifierFilter(header_modifier.HeaderModifierConfig{
		Downstream: header_modifier.HeaderOptions{Remove: []string{"Server"}},
	})
	s := newTestServer(cfg, map[string]engine.Chain{"c": {hm}})

	out := run(t, s, reqHeaders("/", true), respHeaders("server", "nginx"))
	mut := out[1].GetResponseHeaders().GetResponse().GetHeaderMutation()
	if mut == nil || len(mut.RemoveHeaders) != 1 || mut.RemoveHeaders[0] != "server" {
		t.Fatalf("expected server header removal, got %v", mut)
	}
}

func TestProcess_DenyOnUpstreamResponseHeader(t *testing.T) {
	cfg := &config.Config{Router: config.RouterConfig{DefaultChain: "c"}}
	d, err := deny.NewDenyFilter(deny.DenyFilterConfig{
		StatusCode: 502,
		Match:      deny.DenyMatchConfig{ResponseHeaders: map[string]string{"X-Debug": "*"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := newTestServer(cfg, map[string]engine.Chain{"c": {d}})

	out := run(t, s, reqHeaders("/", true), respHeaders("x-debug", "stacktrace"))
	if out[0].GetRequestHeaders() == nil {
		t.Fatalf("request phase must pass, got %v", out[0])
	}
	if imm := out[1].GetImmediateResponse(); imm == nil || imm.GetStatus().GetCode() != 502 {
		t.Fatalf("expected 502 on matching upstream header, got %v", out[1])
	}

	out = run(t, s, reqHeaders("/", true), respHeaders("content-type", "text/plain"))
	if out[1].GetResponseHeaders() == nil {
		t.Fatalf("non-matching response must pass, got %v", out[1])
	}
}

func TestProcess_StreamKeepsSnapshotAcrossReload(t *testing.T) {
	cfg := &config.Config{Router: config.RouterConfig{DefaultChain: "c"}}
	s := newTestServer(cfg, map[string]engine.Chain{"c": {}})

	snap := s.registry.Acquire()
	released := false
	s.registry.Swap(engine.NewSnapshot(cfg, map[string]engine.Chain{"c": {}}), func() { released = true })
	if released {
		t.Fatal("retired snapshot cleaned up while still referenced")
	}
	snap.Release()
	if !released {
		t.Fatal("retired snapshot not cleaned up after last release")
	}
}
