package policy_test

import (
	"context"
	"errors"
	"testing"

	"github.com/taha2samy/hypergate/internal/config"
	"github.com/taha2samy/hypergate/internal/engine"
	"github.com/taha2samy/hypergate/internal/filters"
	"github.com/taha2samy/hypergate/internal/policy"
	"github.com/taha2samy/hypergate/internal/redis"
	"github.com/taha2samy/hypergate/internal/redis/redistest"
)

type fakeFilter struct {
	typ    string
	closed bool
}

func (f *fakeFilter) Execute(*engine.RequestContext) error { return nil }
func (f *fakeFilter) Close() error                         { f.closed = true; return nil }

type harness struct {
	registry *engine.ChainRegistry
	mgr      *policy.Manager
	built    []*fakeFilter
	clients  []*redistest.Client
}

func newHarness() *harness {
	h := &harness{registry: engine.NewChainRegistry()}
	h.mgr = policy.NewManager(context.Background(), h.registry, func(string, config.RedisServiceConfig) (redis.Client, error) {
		c := redistest.New()
		h.clients = append(h.clients, c)
		return c, nil
	}).WithFilterFactory(func(typ string, _ interface{}, lookup filters.RedisLookup) (engine.Filter, error) {
		if typ == "broken" {
			return nil, errors.New("does not compile")
		}
		f := &fakeFilter{typ: typ}
		h.built = append(h.built, f)
		return f, nil
	})
	return h
}

func cfgWith(chains map[string]config.Chain, defaultChain string) *config.Config {
	return &config.Config{
		Version: "v1",
		Chains:  chains,
		Router:  config.RouterConfig{DefaultChain: defaultChain},
		Redis:   map[string]config.RedisServiceConfig{"main": {URL: "redis:6379", Type: "SINGLE"}},
	}
}

func filter(typ string, opts map[string]interface{}) config.FilterConfig {
	return config.FilterConfig{Type: typ, Options: opts}
}

func TestApply_FailureKeepsPreviousPolicy(t *testing.T) {
	h := newHarness()
	good := cfgWith(map[string]config.Chain{"auth": {filter("a", nil)}}, "auth")
	if err := h.mgr.Apply(good); err != nil {
		t.Fatal(err)
	}
	before := h.registry.Current()

	bad := cfgWith(map[string]config.Chain{"auth": {filter("a", map[string]interface{}{"v": 2}), filter("broken", nil)}}, "auth")
	if err := h.mgr.Apply(bad); err == nil {
		t.Fatal("expected compile error")
	}
	if h.registry.Current() != before {
		t.Fatal("failed reload replaced the active snapshot")
	}
	if config.GlobalConfig.Load() != good {
		t.Fatal("failed reload published its config")
	}
	// The filter built before the failure must be released.
	if last := h.built[len(h.built)-1]; !last.closed {
		t.Fatal("filter created by the failed reload leaked")
	}
	if h.built[0].closed {
		t.Fatal("active filter closed by a failed reload")
	}
}

func TestApply_ReusesUnchangedFiltersAndClients(t *testing.T) {
	h := newHarness()
	cfg := cfgWith(map[string]config.Chain{
		"a": {filter("x", map[string]interface{}{"k": 1})},
		"b": {filter("x", map[string]interface{}{"k": 1})}, // identical definition shares one instance
	}, "a")
	if err := h.mgr.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	if len(h.built) != 1 {
		t.Fatalf("identical filters should share one instance, built %d", len(h.built))
	}

	// Unrelated change: add a chain. Existing filter and redis client are reused.
	cfg2 := cfgWith(map[string]config.Chain{
		"a": {filter("x", map[string]interface{}{"k": 1})},
		"c": {filter("y", nil)},
	}, "a")
	if err := h.mgr.Apply(cfg2); err != nil {
		t.Fatal(err)
	}
	if len(h.built) != 2 || h.built[0].closed {
		t.Fatalf("unchanged filter was rebuilt or closed (built=%d closed=%v)", len(h.built), h.built[0].closed)
	}
	if len(h.clients) != 1 || h.clients[0].Closed {
		t.Fatal("unchanged redis client was rebuilt or closed")
	}
}

func TestApply_RetiredResourcesCloseAfterDrain(t *testing.T) {
	h := newHarness()
	if err := h.mgr.Apply(cfgWith(map[string]config.Chain{"a": {filter("x", nil)}}, "a")); err != nil {
		t.Fatal(err)
	}
	inflight := h.registry.Acquire() // a stream still using the old policy

	next := cfgWith(map[string]config.Chain{"a": {filter("z", nil)}}, "a")
	next.Redis = map[string]config.RedisServiceConfig{"main": {URL: "other:6379", Type: "SINGLE"}}
	if err := h.mgr.Apply(next); err != nil {
		t.Fatal(err)
	}
	if h.built[0].closed || h.clients[0].Closed {
		t.Fatal("resources closed while an in-flight stream still uses them")
	}

	inflight.Release()
	if !h.built[0].closed {
		t.Fatal("retired filter not closed after drain")
	}
	if !h.clients[0].Closed {
		t.Fatal("retired redis client not closed after drain")
	}
	if h.built[1].closed || h.clients[1].Closed {
		t.Fatal("active resources closed")
	}
}

func TestApply_RoutesAndChainsSwapTogether(t *testing.T) {
	h := newHarness()
	cfg := cfgWith(map[string]config.Chain{"new": {filter("x", nil)}}, "new")
	if err := h.mgr.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	snap := h.registry.Current()
	if snap.Config != cfg {
		t.Fatal("snapshot config not updated")
	}
	if _, ok := snap.Chains[snap.Config.Router.DefaultChain]; !ok {
		t.Fatal("snapshot routes to a chain it does not contain")
	}
	if !h.mgr.Ready() {
		t.Fatal("manager not ready after successful apply")
	}
}
