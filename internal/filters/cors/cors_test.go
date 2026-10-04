package cors

import (
	"testing"

	"github.com/taha2samy/hypergate/internal/engine"
)

func newCtx(method string, headers map[string]string) *engine.RequestContext {
	ctx := &engine.RequestContext{
		Method:           method,
		Path:             "/api",
		Headers:          map[string]string{},
		ResponseHeaders:  map[string]string{},
		UpstreamShadow:   map[string]string{},
		DownstreamShadow: map[string]string{},
	}
	for k, v := range headers {
		ctx.Headers[k] = v
	}
	return ctx
}

func run(f *Filter, ctx *engine.RequestContext, phase engine.Phase) {
	_ = engine.NewChainExecutor().Execute(ctx, engine.Chain{f}, phase)
}

func mustFilter(t *testing.T, cfg Config) *Filter {
	t.Helper()
	f, err := NewFilter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestConfigValidation(t *testing.T) {
	cases := map[string]Config{
		"no origins":           {},
		"wildcard+credentials": {AllowOrigins: []string{"*"}, AllowCredentials: true},
		"bad regex":            {AllowOriginRegex: []string{"("}},
		"negative max age":     {AllowOrigins: []string{"https://a.com"}, MaxAge: -1},
	}
	for name, cfg := range cases {
		if _, err := NewFilter(cfg); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestPreflightAllowed(t *testing.T) {
	f := mustFilter(t, Config{
		AllowOrigins:     []string{"https://app.example.com"},
		AllowMethods:     []string{"get", "PUT"},
		AllowHeaders:     []string{"Authorization", "Content-Type"},
		AllowCredentials: true,
		MaxAge:           600,
	})
	ctx := newCtx("OPTIONS", map[string]string{
		"origin":                         "https://app.example.com",
		"access-control-request-method":  "PUT",
		"access-control-request-headers": "authorization",
	})
	run(f, ctx, engine.PhaseRequestHeaders)

	if !ctx.Blocked || ctx.ResponseStatus != 204 {
		t.Fatalf("preflight must be answered with 204, got blocked=%v status=%d", ctx.Blocked, ctx.ResponseStatus)
	}
	want := map[string]string{
		"access-control-allow-origin":      "https://app.example.com",
		"access-control-allow-methods":     "GET, PUT",
		"access-control-allow-headers":     "authorization, content-type",
		"access-control-allow-credentials": "true",
		"access-control-max-age":           "600",
		"vary":                             "Origin",
	}
	for k, v := range want {
		if got := ctx.GetDownstreamHeader(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

func TestPreflightDisallowedOrigin(t *testing.T) {
	f := mustFilter(t, Config{AllowOrigins: []string{"https://app.example.com"}})
	ctx := newCtx("OPTIONS", map[string]string{
		"origin":                        "https://evil.example",
		"access-control-request-method": "POST",
	})
	run(f, ctx, engine.PhaseRequestHeaders)
	if !ctx.Blocked || ctx.ResponseStatus != 403 {
		t.Fatalf("disallowed preflight must get 403, got %d", ctx.ResponseStatus)
	}
	if ctx.GetDownstreamHeader("access-control-allow-origin") != "" {
		t.Fatal("no CORS headers for a disallowed origin")
	}
}

func TestActualRequest(t *testing.T) {
	f := mustFilter(t, Config{
		AllowOriginRegex: []string{`https://[a-z0-9-]+\.example\.com`},
		ExposeHeaders:    []string{"X-Request-Id"},
	})

	ok := newCtx("GET", map[string]string{"origin": "https://shop.example.com"})
	run(f, ok, engine.PhaseRequestHeaders)
	if ok.Blocked {
		t.Fatal("actual request must continue upstream")
	}
	if ok.GetDownstreamHeader("access-control-allow-origin") != "https://shop.example.com" ||
		ok.GetDownstreamHeader("access-control-expose-headers") != "x-request-id" {
		t.Fatalf("missing CORS headers: %v", ok.DownstreamShadow)
	}

	// The regex is anchored: a suffix attack must not match.
	evil := newCtx("GET", map[string]string{"origin": "https://shop.example.com.evil.io"})
	run(f, evil, engine.PhaseRequestHeaders)
	if evil.Blocked || evil.GetDownstreamHeader("access-control-allow-origin") != "" {
		t.Fatal("unanchored origin match")
	}
}

func TestBlockDisallowedOrigins(t *testing.T) {
	f := mustFilter(t, Config{AllowOrigins: []string{"https://a.com"}, BlockDisallowedOrigins: true})
	ctx := newCtx("POST", map[string]string{"origin": "https://b.com"})
	run(f, ctx, engine.PhaseRequestHeaders)
	if !ctx.Blocked || ctx.ResponseStatus != 403 {
		t.Fatalf("expected 403, got %d", ctx.ResponseStatus)
	}
}

func TestWildcardOriginAndNoOrigin(t *testing.T) {
	f := mustFilter(t, Config{AllowOrigins: []string{"*"}})
	ctx := newCtx("GET", map[string]string{"origin": "https://anything.io"})
	run(f, ctx, engine.PhaseRequestHeaders)
	if ctx.GetDownstreamHeader("access-control-allow-origin") != "*" || ctx.GetDownstreamHeader("vary") != "" {
		t.Fatalf("wildcard must answer * without Vary, got %v", ctx.DownstreamShadow)
	}

	plain := newCtx("GET", nil)
	run(f, plain, engine.PhaseRequestHeaders)
	if len(plain.DownstreamShadow) != 0 {
		t.Fatal("requests without Origin must be untouched")
	}
}

func TestVaryMergedWithUpstream(t *testing.T) {
	f := mustFilter(t, Config{AllowOrigins: []string{"https://a.com"}})
	ctx := newCtx("GET", map[string]string{"origin": "https://a.com"})
	run(f, ctx, engine.PhaseRequestHeaders)
	ctx.ResponseHeaders["vary"] = "Accept-Encoding"
	run(f, ctx, engine.PhaseResponseHeaders)
	if got := ctx.GetDownstreamHeader("vary"); got != "Accept-Encoding, Origin" {
		t.Fatalf("vary = %q", got)
	}
}
