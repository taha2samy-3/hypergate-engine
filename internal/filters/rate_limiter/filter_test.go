package rate_limiter

import (
	"regexp"
	"strings"
	"testing"

	"github.com/taha2samy/hypergate/internal/engine"
	"github.com/taha2samy/hypergate/internal/redis/redistest"
)

func newCtx(headers map[string]string) *engine.RequestContext {
	ctx := &engine.RequestContext{
		Headers:          map[string]string{},
		UpstreamShadow:   map[string]string{},
		DownstreamShadow: map[string]string{},
	}
	for k, v := range headers {
		ctx.Headers[k] = v
	}
	return ctx
}

func newFixedWindow(t *testing.T, client *redistest.Client, opts FilterOptions) *RateLimiterFilter {
	t.Helper()
	opts.ApplyDefaults()
	exec, err := ResolveExecutor("fixed_window", client, opts)
	if err != nil {
		t.Fatal(err)
	}
	return NewRateLimiterFilter("test", opts, exec)
}

func TestRequestCost(t *testing.T) {
	opts := FilterOptions{DynamicCost: DynamicCostOpts{Enabled: true, SourceHeader: "X-Cost", MaxAllowedCost: 10}}
	opts.ApplyDefaults()
	f := &RateLimiterFilter{options: opts}

	cases := map[string]int64{
		"":        1, // missing -> default (forced to >= 1)
		"abc":     1,
		"-1000":   1, // negative must never refund quota
		"0":       1, // zero must never be free
		"4":       4,
		"1000000": 10, // capped
	}
	for header, want := range cases {
		ctx := newCtx(map[string]string{"x-cost": header})
		if got := f.requestCost(ctx); got != want {
			t.Errorf("cost header %q: got %d, want %d", header, got, want)
		}
	}
}

func TestRequestCost_NoCapWhenMaxUnset(t *testing.T) {
	opts := FilterOptions{DynamicCost: DynamicCostOpts{Enabled: true, SourceHeader: "x-cost"}}
	opts.ApplyDefaults()
	f := &RateLimiterFilter{options: opts}
	if got := f.requestCost(newCtx(map[string]string{"x-cost": "7"})); got != 7 {
		t.Fatalf("max_allowed_cost unset must not cap to 0, got %d", got)
	}
}

func TestNegativeCostCannotBypassLimit(t *testing.T) {
	client := redistest.New()
	f := newFixedWindow(t, client, FilterOptions{
		Domain:      "d",
		DynamicCost: DynamicCostOpts{Enabled: true, SourceHeader: "x-cost"},
		Descriptors: []YamlDescriptor{{Entries: []DescriptorEntryDef{{Key: "user"}}, Limit: 2, Unit: "minute"}},
	})

	blocked := false
	for i := 0; i < 5; i++ {
		ctx := newCtx(map[string]string{"user": "alice", "x-cost": "-50"})
		if err := f.Execute(ctx); err != nil {
			t.Fatal(err)
		}
		blocked = blocked || ctx.Blocked
	}
	if !blocked {
		t.Fatal("negative cost header let requests past the limit")
	}
}

func TestClientIPKeyIgnoresForwardedFor(t *testing.T) {
	client := redistest.New()
	f := newFixedWindow(t, client, FilterOptions{
		Domain:      "d",
		Descriptors: []YamlDescriptor{{Entries: []DescriptorEntryDef{{Key: "client_ip"}}, Limit: 1, Unit: "minute"}},
	})

	// Rotating X-Forwarded-For must not create fresh buckets.
	for i, xff := range []string{"1.1.1.1", "2.2.2.2"} {
		ctx := newCtx(map[string]string{"x-forwarded-for": xff})
		ctx.ClientIP = "10.0.0.7"
		if err := f.Execute(ctx); err != nil {
			t.Fatal(err)
		}
		if i == 1 && !ctx.Blocked {
			t.Fatal("spoofed X-Forwarded-For bypassed the client_ip limit")
		}
	}
	for _, k := range client.Keys {
		if strings.Contains(k, "1.1.1.1") || strings.Contains(k, "2.2.2.2") {
			t.Fatalf("rate limit key built from client-supplied header: %s", k)
		}
	}
}

func TestFixedWindowLongKeyKeepsWindowSuffix(t *testing.T) {
	client := redistest.New()
	f := newFixedWindow(t, client, FilterOptions{
		Domain:      "d",
		Descriptors: []YamlDescriptor{{Entries: []DescriptorEntryDef{{Key: "tenant"}}, Limit: 100, Unit: "minute"}},
	})

	ctx := newCtx(map[string]string{"tenant": strings.Repeat("t", 600)}) // forces the heap-buffer path
	if err := f.Execute(ctx); err != nil {
		t.Fatal(err)
	}
	if len(client.Keys) == 0 {
		t.Fatal("no redis key written")
	}
	if !regexp.MustCompile(`_\d+$`).MatchString(client.Keys[0]) {
		t.Fatalf("long key is missing its window timestamp: ...%s", client.Keys[0][len(client.Keys[0])-20:])
	}
}
