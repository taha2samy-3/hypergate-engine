package router_test

import (
	"regexp"
	"sync/atomic"
	"testing"

	"github.com/taha2samy/hypergate/internal/config"
	"github.com/taha2samy/hypergate/internal/engine"
	"github.com/taha2samy/hypergate/internal/router"
)

func newCtx(path, method string, headers map[string]string) *engine.RequestContext {
	ctx := &engine.RequestContext{
		Headers:          make(map[string]string),
		UpstreamShadow:   make(map[string]string),
		DownstreamShadow: make(map[string]string),
		Path:             path,
		Method:           method,
	}
	for k, v := range headers {
		ctx.Headers[k] = v
	}
	return ctx
}

func loadConfig(cfg config.Config) {
	config.GlobalConfig.Store(&cfg)
}

func TestRouter_PathPrefix_Match(t *testing.T) {
	loadConfig(config.Config{
		Version: "v1",
		Router: config.RouterConfig{
			DefaultChain: "default",
			Routes: []config.RouteConfig{
				{
					Name:        "admin",
					TargetChain: "admin-chain",
					Matches: []config.MatchConfig{
						{PathPrefix: "/admin/"},
					},
				},
			},
		},
	})

	r := router.NewEngineRouter()
	ctx := newCtx("/admin/users", "GET", nil)
	chain := r.Route(ctx)
	if chain != "admin-chain" {
		t.Fatalf("expected 'admin-chain', got %q", chain)
	}
}

func TestRouter_PathPrefix_NoMatch_FallsToDefault(t *testing.T) {
	loadConfig(config.Config{
		Version: "v1",
		Router: config.RouterConfig{
			DefaultChain: "public-chain",
			Routes: []config.RouteConfig{
				{
					Name:        "admin",
					TargetChain: "admin-chain",
					Matches: []config.MatchConfig{
						{PathPrefix: "/admin/"},
					},
				},
			},
		},
	})

	r := router.NewEngineRouter()
	ctx := newCtx("/api/users", "GET", nil)
	chain := r.Route(ctx)
	if chain != "public-chain" {
		t.Fatalf("expected 'public-chain', got %q", chain)
	}
}

func TestRouter_PathRegex_Match(t *testing.T) {
	re := regexp.MustCompile(`^/api/v\d+/`)
	loadConfig(config.Config{
		Version: "v1",
		Router: config.RouterConfig{
			DefaultChain: "default",
			Routes: []config.RouteConfig{
				{
					Name:        "versioned-api",
					TargetChain: "versioned-chain",
					Matches: []config.MatchConfig{
						{CompiledPathRegex: re},
					},
				},
			},
		},
	})

	r := router.NewEngineRouter()
	for _, path := range []string{"/api/v1/orders", "/api/v2/products", "/api/v10/items"} {
		ctx := newCtx(path, "GET", nil)
		chain := r.Route(ctx)
		if chain != "versioned-chain" {
			t.Fatalf("path %q: expected 'versioned-chain', got %q", path, chain)
		}
	}
}

func TestRouter_HeaderExactMatch(t *testing.T) {
	loadConfig(config.Config{
		Version: "v1",
		Router: config.RouterConfig{
			DefaultChain: "default",
			Routes: []config.RouteConfig{
				{
					Name:        "internal",
					TargetChain: "internal-chain",
					Matches: []config.MatchConfig{
						{
							PathPrefix: "/api/",
							Headers: map[string]config.HeaderMatchConfig{
								"x-internal": {Exact: "true"},
							},
						},
					},
				},
			},
		},
	})

	r := router.NewEngineRouter()

	// Should match
	ctx := newCtx("/api/internal", "GET", map[string]string{"x-internal": "true"})
	if chain := r.Route(ctx); chain != "internal-chain" {
		t.Fatalf("expected 'internal-chain', got %q", chain)
	}

	// Header value wrong — should fall to default
	ctx2 := newCtx("/api/internal", "GET", map[string]string{"x-internal": "false"})
	if chain := r.Route(ctx2); chain != "default" {
		t.Fatalf("expected 'default', got %q", chain)
	}

	// Header missing — should fall to default
	ctx3 := newCtx("/api/internal", "GET", nil)
	if chain := r.Route(ctx3); chain != "default" {
		t.Fatalf("expected 'default' when header missing, got %q", chain)
	}
}

func TestRouter_HeaderWildcard(t *testing.T) {
	loadConfig(config.Config{
		Version: "v1",
		Router: config.RouterConfig{
			DefaultChain: "default",
			Routes: []config.RouteConfig{
				{
					Name:        "any-api-key",
					TargetChain: "keyed-chain",
					Matches: []config.MatchConfig{
						{
							Headers: map[string]config.HeaderMatchConfig{
								"x-api-key": {Exact: "*"},
							},
						},
					},
				},
			},
		},
	})

	r := router.NewEngineRouter()
	ctx := newCtx("/anything", "POST", map[string]string{"x-api-key": "some-random-key"})
	if chain := r.Route(ctx); chain != "keyed-chain" {
		t.Fatalf("wildcard header match: expected 'keyed-chain', got %q", chain)
	}
}

func TestRouter_OtherFallbackField(t *testing.T) {
	loadConfig(config.Config{
		Version: "v1",
		Router: config.RouterConfig{
			Other: "other-fallback",
		},
	})

	r := router.NewEngineRouter()
	ctx := newCtx("/unmatched", "GET", nil)
	chain := r.Route(ctx)
	if chain != "other-fallback" {
		t.Fatalf("expected 'other-fallback' from Other field, got %q", chain)
	}
}

func TestRouter_DefaultChainTakesPriorityOverOther(t *testing.T) {
	loadConfig(config.Config{
		Version: "v1",
		Router: config.RouterConfig{
			DefaultChain: "default-chain",
			Other:        "other-chain",
		},
	})

	r := router.NewEngineRouter()
	ctx := newCtx("/unmatched", "GET", nil)
	chain := r.Route(ctx)
	if chain != "default-chain" {
		t.Fatalf("DefaultChain should take priority over Other, got %q", chain)
	}
}

func TestRouter_NoRoutes_ReturnsDefault(t *testing.T) {
	loadConfig(config.Config{
		Version: "v1",
		Router: config.RouterConfig{
			DefaultChain: "solo-chain",
		},
	})

	r := router.NewEngineRouter()
	ctx := newCtx("/anything", "DELETE", nil)
	chain := r.Route(ctx)
	if chain != "solo-chain" {
		t.Fatalf("expected 'solo-chain', got %q", chain)
	}
}

func TestRouter_AtomicConfigReload(t *testing.T) {
	// Verify the router picks up a new config stored atomically without restarting.
	loadConfig(config.Config{
		Version: "v1",
		Router: config.RouterConfig{
			DefaultChain: "old-chain",
		},
	})

	r := router.NewEngineRouter()
	ctx := newCtx("/path", "GET", nil)
	if chain := r.Route(ctx); chain != "old-chain" {
		t.Fatalf("before reload: expected 'old-chain', got %q", chain)
	}

	// Simulate hot-reload: store a new config atomically.
	var p atomic.Pointer[config.Config]
	_ = p // just verify it compiles; config.GlobalConfig is the actual atomic.Pointer
	loadConfig(config.Config{
		Version: "v1",
		Router: config.RouterConfig{
			DefaultChain: "new-chain",
		},
	})

	ctx2 := newCtx("/path", "GET", nil)
	if chain := r.Route(ctx2); chain != "new-chain" {
		t.Fatalf("after reload: expected 'new-chain', got %q", chain)
	}
}
