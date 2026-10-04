package router_test

import (
	"testing"

	"github.com/taha2samy/hypergate/internal/config"
	"github.com/taha2samy/hypergate/internal/engine"
	"github.com/taha2samy/hypergate/internal/router"
	"github.com/taha2samy/hypergate/internal/selector"
)

func compiledRouter(t *testing.T, rc config.RouterConfig) *config.RouterConfig {
	t.Helper()
	for i := range rc.Routes {
		for j := range rc.Routes[i].Matches {
			if err := rc.Routes[i].Matches[j].Compile(false); err != nil {
				t.Fatal(err)
			}
		}
	}
	return &rc
}

type req struct {
	traffic selector.Traffic
	client  string
	dest    string
	host    string
	path    string
}

func (r req) ctx() *engine.RequestContext {
	path := r.path
	if path == "" {
		path = "/"
	}
	ctx := newCtx(path, "GET", nil)
	ctx.Traffic = r.traffic
	ctx.ClientIP = r.client
	ctx.ClientAddr = selector.ParseAddr(r.client)
	ctx.DestinationAddress = r.dest
	ctx.DestinationAddr = selector.ParseAddr(r.dest)
	ctx.Host = selector.NormalizeHost(r.host)
	return ctx
}

func TestRouter_TrafficSourcesDestinations(t *testing.T) {
	rc := compiledRouter(t, config.RouterConfig{
		Routes: []config.RouteConfig{
			{
				Name:        "partner-api",
				TargetChain: "partner",
				Matches: []config.MatchConfig{{
					Traffic:      "north_south",
					Sources:      []string{"cidr:203.0.113.0/24", "ip:192.0.2.7"},
					Destinations: []string{"host:api.example.com"},
				}},
			},
			{
				Name:        "internal-ledger",
				TargetChain: "internal-strict",
				Matches: []config.MatchConfig{{
					Traffic:      "east_west",
					Sources:      []string{"cidr:10.244.0.0/16"},
					Destinations: []string{"ip:10.96.0.20"},
					PathPrefix:   "/v1/charges",
				}},
			},
			{
				// Two entries: either one is enough (OR across matches).
				Name:        "admin-hosts",
				TargetChain: "admin",
				Matches: []config.MatchConfig{
					{Destinations: []string{"host:*.admin.example.com"}},
					{Destinations: []string{"cidr:10.100.0.0/16"}},
				},
			},
		},
		DefaultChains: config.DefaultChainsConfig{NorthSouth: "public", EastWest: "deny-unlisted"},
		DefaultChain:  "legacy",
	})

	tests := []struct {
		name string
		req  req
		want string
	}{
		{"partner from cidr", req{traffic: selector.TrafficNorthSouth, client: "203.0.113.9", host: "api.example.com:443"}, "partner"},
		{"partner from ip, host case-insensitive", req{traffic: selector.TrafficNorthSouth, client: "192.0.2.7", host: "API.Example.com"}, "partner"},
		{"partner source but other host", req{traffic: selector.TrafficNorthSouth, client: "203.0.113.9", host: "www.example.com"}, "public"},
		{"partner host but other source", req{traffic: selector.TrafficNorthSouth, client: "198.51.100.1", host: "api.example.com"}, "public"},
		{"partner source and host but east-west", req{traffic: selector.TrafficEastWest, client: "203.0.113.9", host: "api.example.com"}, "deny-unlisted"},
		{"partner unknown client", req{traffic: selector.TrafficNorthSouth, host: "api.example.com"}, "public"},

		{"ledger all conditions", req{traffic: selector.TrafficEastWest, client: "10.244.3.4", dest: "10.96.0.20:8080", path: "/v1/charges/1"}, "internal-strict"},
		{"ledger wrong path", req{traffic: selector.TrafficEastWest, client: "10.244.3.4", dest: "10.96.0.20:8080", path: "/v1/refunds"}, "deny-unlisted"},
		{"ledger destination unknown", req{traffic: selector.TrafficEastWest, client: "10.244.3.4", path: "/v1/charges"}, "deny-unlisted"},
		{"ledger ipv6-mapped destination", req{traffic: selector.TrafficEastWest, client: "10.244.3.4", dest: "[::ffff:10.96.0.20]:80", path: "/v1/charges"}, "internal-strict"},

		{"admin by wildcard host", req{traffic: selector.TrafficNorthSouth, host: "ops.admin.example.com"}, "admin"},
		{"admin by destination cidr", req{traffic: selector.TrafficEastWest, dest: "10.100.4.4:80"}, "admin"},
		{"admin apex does not match wildcard", req{traffic: selector.TrafficNorthSouth, host: "admin.example.com"}, "public"},

		{"unknown class falls to default_chain", req{traffic: selector.TrafficAny}, "legacy"},
	}
	r := router.NewEngineRouter()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := r.RouteWith(rc, tt.req.ctx()); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRouter_FirstMatchingRouteWins(t *testing.T) {
	rc := compiledRouter(t, config.RouterConfig{
		Routes: []config.RouteConfig{
			{Name: "specific", TargetChain: "first", Matches: []config.MatchConfig{{Sources: []string{"ip:10.0.0.1"}}}},
			{Name: "broad", TargetChain: "second", Matches: []config.MatchConfig{{Sources: []string{"any"}}}},
		},
	})
	r := router.NewEngineRouter()
	if got := r.RouteWith(rc, req{client: "10.0.0.1"}.ctx()); got != "first" {
		t.Fatalf("got %q", got)
	}
	if got := r.RouteWith(rc, req{client: "10.0.0.2"}.ctx()); got != "second" {
		t.Fatalf("got %q", got)
	}
	if got := r.RouteWith(rc, req{}.ctx()); got != "second" {
		t.Fatalf("any must match without a client address, got %q", got)
	}
}

func TestRouter_DefaultChainsFallback(t *testing.T) {
	r := router.NewEngineRouter()
	tests := []struct {
		name    string
		rc      config.RouterConfig
		traffic selector.Traffic
		want    string
	}{
		{"north-south class chain", config.RouterConfig{DefaultChains: config.DefaultChainsConfig{NorthSouth: "ns", EastWest: "ew"}, DefaultChain: "d"}, selector.TrafficNorthSouth, "ns"},
		{"east-west class chain", config.RouterConfig{DefaultChains: config.DefaultChainsConfig{NorthSouth: "ns", EastWest: "ew"}, DefaultChain: "d"}, selector.TrafficEastWest, "ew"},
		{"class unset uses default_chain", config.RouterConfig{DefaultChains: config.DefaultChainsConfig{NorthSouth: "ns"}, DefaultChain: "d"}, selector.TrafficEastWest, "d"},
		{"then other", config.RouterConfig{Other: "o"}, selector.TrafficNorthSouth, "o"},
		{"nothing configured", config.RouterConfig{}, selector.TrafficNorthSouth, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := r.RouteWith(&tt.rc, req{traffic: tt.traffic}.ctx()); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// Matches built in code without Compile must never match more than intended.
func TestRouter_UncompiledMatchFailsClosed(t *testing.T) {
	rc := &config.RouterConfig{
		Routes: []config.RouteConfig{
			{Name: "src", TargetChain: "src", Matches: []config.MatchConfig{{Sources: []string{"ip:10.0.0.1"}}}},
			{Name: "dst", TargetChain: "dst", Matches: []config.MatchConfig{{Destinations: []string{"host:a.example.com"}}}},
			{Name: "traffic", TargetChain: "traffic", Matches: []config.MatchConfig{{Traffic: "east_west"}}},
			{Name: "any", TargetChain: "any", Matches: []config.MatchConfig{{Traffic: "any", PathPrefix: "/any"}}},
		},
		DefaultChain: "default",
	}
	r := router.NewEngineRouter()
	got := r.RouteWith(rc, req{traffic: selector.TrafficNorthSouth, client: "10.0.0.1", host: "a.example.com"}.ctx())
	if got != "default" {
		t.Fatalf("uncompiled selectors matched: %q", got)
	}
	if got := r.RouteWith(rc, req{traffic: selector.TrafficNorthSouth, path: "/any"}.ctx()); got != "any" {
		t.Fatalf("traffic: any must match without Compile, got %q", got)
	}
}
