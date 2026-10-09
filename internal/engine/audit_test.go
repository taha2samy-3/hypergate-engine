package engine_test

import (
	"errors"
	"testing"

	"github.com/taha2samy/hypergate/internal/engine"
)

// headerFilter adds an upstream and a downstream header.
type headerFilter struct{ name string }

func (f headerFilter) Execute(ctx *engine.RequestContext) error {
	ctx.SetHeaderUpstream("x-"+f.name, "1")
	ctx.SetHeaderDownstream("x-"+f.name, "1")
	return nil
}

// denyingFilter sets headers, then denies, like the JWT filter does.
type denyingFilter struct{ status int32 }

func (f denyingFilter) Execute(ctx *engine.RequestContext) error {
	ctx.SetHeaderDownstream("content-type", "application/json")
	ctx.SetHeaderUpstream("x-claim", "leaked")
	ctx.Block(f.status, `{"error":"unauthorized"}`)
	return nil
}

type answeringFilter struct{}

func (answeringFilter) Execute(ctx *engine.RequestContext) error {
	ctx.Answer(204, "")
	return nil
}

type erroringFilter struct{}

func (erroringFilter) Execute(*engine.RequestContext) error { return errors.New("redis down") }

func TestExecutor_AuditedDenialIsRecordedNotEnforced(t *testing.T) {
	ctx := newCtx()
	chain := engine.Chain{headerFilter{"before"}, engine.Audit(denyingFilter{401}), headerFilter{"after"}}
	if err := engine.NewChainExecutor().Execute(ctx, chain, engine.PhaseRequestHeaders); err != nil {
		t.Fatal(err)
	}
	if ctx.Blocked || ctx.ResponseStatus != 0 {
		t.Fatalf("audited denial was enforced: blocked=%v status=%d", ctx.Blocked, ctx.ResponseStatus)
	}
	if len(ctx.AuditHits) != 1 || ctx.AuditHits[0] != (engine.AuditHit{Index: 1, Status: 401}) {
		t.Fatalf("audit hits = %+v", ctx.AuditHits)
	}
	// The denying filter's changes are rolled back; the others are kept.
	if ctx.GetHeader("x-claim") != "" || ctx.GetDownstreamHeader("content-type") != "" {
		t.Fatal("the audited denial left header changes behind")
	}
	if ctx.GetHeader("x-before") != "1" || ctx.GetHeader("x-after") != "1" || ctx.GetDownstreamHeader("x-after") != "1" {
		t.Fatal("changes by the other filters were lost")
	}
}

func TestExecutor_AuditedFailureIsRecordedNotEnforced(t *testing.T) {
	ctx := newCtx()
	chain := engine.Chain{engine.Audit(erroringFilter{}), headerFilter{"after"}}
	if err := engine.NewChainExecutor().Execute(ctx, chain, engine.PhaseRequestHeaders); err != nil {
		t.Fatalf("audited failure must not fail the chain: %v", err)
	}
	if ctx.Blocked || len(ctx.AuditHits) != 1 || !ctx.AuditHits[0].Failed || ctx.AuditHits[0].Status != 500 {
		t.Fatalf("blocked=%v hits=%+v", ctx.Blocked, ctx.AuditHits)
	}
	if ctx.GetHeader("x-after") != "1" {
		t.Fatal("the chain did not continue")
	}
}

func TestExecutor_AuditedAnswerIsStillSent(t *testing.T) {
	ctx := newCtx()
	chain := engine.Chain{engine.Audit(answeringFilter{}), headerFilter{"after"}}
	if err := engine.NewChainExecutor().Execute(ctx, chain, engine.PhaseRequestHeaders); err != nil {
		t.Fatal(err)
	}
	if !ctx.Blocked || !ctx.Answered || ctx.ResponseStatus != 204 || ctx.BlockedBy != 0 || len(ctx.AuditHits) != 0 {
		t.Fatalf("answer not sent: blocked=%v answered=%v status=%d by=%d hits=%v",
			ctx.Blocked, ctx.Answered, ctx.ResponseStatus, ctx.BlockedBy, ctx.AuditHits)
	}
	if ctx.GetHeader("x-after") != "" {
		t.Fatal("filters after an answer must not run")
	}
}

func TestExecutor_SharedInstanceEnforcedElsewhere(t *testing.T) {
	f := denyingFilter{403}
	ctx := newCtx()
	if err := engine.NewChainExecutor().Execute(ctx, engine.Chain{f}, engine.PhaseRequestHeaders); err != nil {
		t.Fatal(err)
	}
	if !ctx.Blocked || ctx.ResponseStatus != 403 || ctx.BlockedBy != 0 {
		t.Fatal("the same filter must still be enforced where it is not audited")
	}
}

func TestExecutor_AuditKeepsPhases(t *testing.T) {
	inner := &stubFilter{phases: []engine.Phase{engine.PhaseResponseHeaders}}
	chain := engine.Chain{engine.Audit(inner)}
	ex := engine.NewChainExecutor()
	_ = ex.Execute(newCtx(), chain, engine.PhaseRequestHeaders)
	_ = ex.Execute(newCtx(), chain, engine.PhaseResponseHeaders)
	if inner.callCount != 1 {
		t.Fatalf("audited filter ran %d times, want once (response headers only)", inner.callCount)
	}
}
