package engine_test

import (
	"errors"
	"testing"

	"github.com/taha2samy/hypergate/internal/engine"
)

// ─────────────────────────────────────────────────────────────────────────────
// Test helpers / stubs
// ─────────────────────────────────────────────────────────────────────────────

type stubFilter struct {
	callCount int
	phases    []engine.Phase
	blockOn   engine.Phase
	returnErr error
}

func (f *stubFilter) Execute(ctx *engine.RequestContext) error {
	f.callCount++
	if f.returnErr != nil {
		return f.returnErr
	}
	return nil
}

func (f *stubFilter) SupportedPhases() []engine.Phase { return f.phases }

// blockingFilter blocks on first call.
type blockingFilter struct {
	callCount int
	status    int32
}

func (f *blockingFilter) Execute(ctx *engine.RequestContext) error {
	f.callCount++
	ctx.Blocked = true
	ctx.ResponseStatus = f.status
	return nil
}

func (f *blockingFilter) SupportedPhases() []engine.Phase {
	return []engine.Phase{engine.PhaseRequestHeaders}
}

// errorFilter returns an error.
type errorFilter struct {
	callCount int
}

func (f *errorFilter) Execute(ctx *engine.RequestContext) error {
	f.callCount++
	return errors.New("filter internal error")
}

func (f *errorFilter) SupportedPhases() []engine.Phase {
	return []engine.Phase{engine.PhaseRequestHeaders}
}

// legacyFilter does NOT implement PhaseAware — should only run on PhaseRequestHeaders.
type legacyFilter struct {
	callCount int
}

func (f *legacyFilter) Execute(ctx *engine.RequestContext) error {
	f.callCount++
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// ChainExecutor tests
// ─────────────────────────────────────────────────────────────────────────────

func newCtx() *engine.RequestContext {
	return &engine.RequestContext{
		Headers:          make(map[string]string),
		UpstreamShadow:   make(map[string]string),
		DownstreamShadow: make(map[string]string),
	}
}

func TestExecutor_RunsFilterOnCorrectPhase(t *testing.T) {
	exec := engine.NewChainExecutor()
	ctx := newCtx()

	f := &stubFilter{phases: []engine.Phase{engine.PhaseRequestHeaders}}
	chain := engine.Chain{f}

	_ = exec.Execute(ctx, chain, engine.PhaseRequestHeaders)
	if f.callCount != 1 {
		t.Fatalf("expected 1 call on PhaseRequestHeaders, got %d", f.callCount)
	}

	_ = exec.Execute(ctx, chain, engine.PhaseRequestBody)
	if f.callCount != 1 {
		t.Fatalf("filter should NOT run on PhaseRequestBody, still expected 1 call, got %d", f.callCount)
	}
}

func TestExecutor_MultiPhaseFilter(t *testing.T) {
	exec := engine.NewChainExecutor()
	ctx := newCtx()

	f := &stubFilter{phases: []engine.Phase{engine.PhaseRequestHeaders, engine.PhaseRequestBody}}
	chain := engine.Chain{f}

	_ = exec.Execute(ctx, chain, engine.PhaseRequestHeaders)
	_ = exec.Execute(ctx, chain, engine.PhaseRequestBody)
	_ = exec.Execute(ctx, chain, engine.PhaseResponseHeaders) // should NOT run

	if f.callCount != 2 {
		t.Fatalf("expected 2 calls (headers+body), got %d", f.callCount)
	}
}

func TestExecutor_LegacyFilterOnlyRunsOnRequestHeaders(t *testing.T) {
	exec := engine.NewChainExecutor()
	ctx := newCtx()

	f := &legacyFilter{}
	chain := engine.Chain{f}

	_ = exec.Execute(ctx, chain, engine.PhaseRequestHeaders)
	_ = exec.Execute(ctx, chain, engine.PhaseRequestBody)
	_ = exec.Execute(ctx, chain, engine.PhaseResponseHeaders)

	if f.callCount != 1 {
		t.Fatalf("legacy filter should only run on PhaseRequestHeaders, got %d calls", f.callCount)
	}
}

func TestExecutor_FastFailOnBlock(t *testing.T) {
	exec := engine.NewChainExecutor()
	ctx := newCtx()

	blocker := &blockingFilter{status: 429}
	after := &stubFilter{phases: []engine.Phase{engine.PhaseRequestHeaders}}
	chain := engine.Chain{blocker, after}

	_ = exec.Execute(ctx, chain, engine.PhaseRequestHeaders)

	if after.callCount != 0 {
		t.Fatal("filter after a blocking filter should NOT be called")
	}
	if ctx.ResponseStatus != 429 {
		t.Fatalf("expected status 429, got %d", ctx.ResponseStatus)
	}
}

func TestExecutor_ErrorFilter_BlocksAndReturnsError(t *testing.T) {
	exec := engine.NewChainExecutor()
	ctx := newCtx()

	ef := &errorFilter{}
	after := &stubFilter{phases: []engine.Phase{engine.PhaseRequestHeaders}}
	chain := engine.Chain{ef, after}

	err := exec.Execute(ctx, chain, engine.PhaseRequestHeaders)

	if err == nil {
		t.Fatal("expected non-nil error from executor")
	}
	if !ctx.Blocked {
		t.Fatal("ctx should be blocked after filter error")
	}
	if ctx.ResponseStatus != 500 {
		t.Fatalf("expected status 500 on internal error, got %d", ctx.ResponseStatus)
	}
	if after.callCount != 0 {
		t.Fatal("filter after error should NOT be called")
	}
}

func TestExecutor_EmptyChain(t *testing.T) {
	exec := engine.NewChainExecutor()
	ctx := newCtx()
	err := exec.Execute(ctx, engine.Chain{}, engine.PhaseRequestHeaders)
	if err != nil {
		t.Fatalf("empty chain should not error, got: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ChainRegistry tests
// ─────────────────────────────────────────────────────────────────────────────

func TestRegistry_RegisterAndGet(t *testing.T) {
	reg := engine.NewChainRegistry()
	f := &stubFilter{phases: []engine.Phase{engine.PhaseRequestHeaders}}
	chain := engine.Chain{f}

	reg.Register("mychain", chain)
	got, ok := reg.Get("mychain")
	if !ok {
		t.Fatal("expected to find registered chain")
	}
	if len(got) != 1 {
		t.Fatalf("expected chain length 1, got %d", len(got))
	}
}

func TestRegistry_GetMissing(t *testing.T) {
	reg := engine.NewChainRegistry()
	_, ok := reg.Get("nonexistent")
	if ok {
		t.Fatal("expected not to find unregistered chain")
	}
}

func TestRegistry_ReplaceAll(t *testing.T) {
	reg := engine.NewChainRegistry()
	reg.Register("old", engine.Chain{&stubFilter{}})

	newChains := map[string]engine.Chain{
		"new": {&stubFilter{}},
	}
	reg.ReplaceAll(newChains)

	if _, ok := reg.Get("old"); ok {
		t.Fatal("old chain should be gone after ReplaceAll")
	}
	if _, ok := reg.Get("new"); !ok {
		t.Fatal("new chain should be present after ReplaceAll")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// RequestContext tests
// ─────────────────────────────────────────────────────────────────────────────

func TestRequestContext_SetAndGetHeaderUpstream(t *testing.T) {
	ctx := newCtx()
	ctx.SetHeaderUpstream("X-User-ID", "42")

	if val := ctx.GetHeader("x-user-id"); val != "42" {
		t.Fatalf("expected '42', got %q", val)
	}
}

func TestRequestContext_RemoveHeaderUpstream(t *testing.T) {
	ctx := newCtx()
	ctx.Headers["authorization"] = "Bearer token"
	ctx.RemoveHeaderUpstream("Authorization")

	if val := ctx.GetHeader("authorization"); val != "" {
		t.Fatalf("expected empty after remove, got %q", val)
	}
}

func TestRequestContext_SetHeaderDownstream(t *testing.T) {
	ctx := newCtx()
	ctx.SetHeaderDownstream("Content-Type", "application/json")

	if val := ctx.GetDownstreamHeader("content-type"); val != "application/json" {
		t.Fatalf("expected 'application/json', got %q", val)
	}
}

func TestRequestContext_Reset(t *testing.T) {
	ctx := newCtx()
	ctx.Path = "/api/v1"
	ctx.Method = "POST"
	ctx.Blocked = true
	ctx.ResponseStatus = 429
	ctx.Headers["foo"] = "bar"
	ctx.HeadersToAdd = append(ctx.HeadersToAdd, engine.Header{Key: "x-test", Value: "1"})

	ctx.Reset()

	if ctx.Path != "" || ctx.Method != "" || ctx.Blocked || ctx.ResponseStatus != 0 {
		t.Fatal("Reset should clear path/method/blocked/status")
	}
	if len(ctx.Headers) != 0 {
		t.Fatal("Reset should clear headers map")
	}
	if len(ctx.HeadersToAdd) != 0 {
		t.Fatal("Reset should clear HeadersToAdd slice")
	}
}
