package grpc

import (
	"context"
	"net/netip"
	"testing"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/taha2samy/hypergate/internal/config"
	"github.com/taha2samy/hypergate/internal/engine"
	"github.com/taha2samy/hypergate/internal/identity"
	"github.com/taha2samy/hypergate/internal/selector"
)

// withAttributes adds Envoy request attributes the way the ext_proc filter sends them.
func withAttributes(req *extprocv3.ProcessingRequest, kv ...string) *extprocv3.ProcessingRequest {
	fields := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		fields[kv[i]] = kv[i+1]
	}
	s, _ := structpb.NewStruct(fields)
	req.Attributes = map[string]*structpb.Struct{"envoy.filters.http.ext_proc": s}
	return req
}

func runWithMetadata(t *testing.T, s *Server, md metadata.MD, msgs ...*extprocv3.ProcessingRequest) []*extprocv3.ProcessingResponse {
	t.Helper()
	stream := &fakeStream{in: msgs}
	if md != nil {
		stream.ctx = metadata.NewIncomingContext(context.Background(), md)
	}
	if err := s.Process(stream); err != nil {
		t.Fatalf("Process returned error: %v", err)
	}
	return stream.out
}

// statusFilter blocks every request with a fixed status, identifying the chain that ran.
type statusFilter int32

func (f statusFilter) Execute(ctx *engine.RequestContext) error {
	ctx.Block(int32(f), "chain")
	return nil
}

func (statusFilter) SupportedPhases() []engine.Phase {
	return []engine.Phase{engine.PhaseRequestHeaders}
}

func denyChain(status int32) engine.Chain { return engine.Chain{statusFilter(status)} }

func TestTrafficFromContext(t *testing.T) {
	tests := []struct {
		name string
		md   metadata.MD
		want selector.Traffic
	}{
		{"east-west metadata", metadata.Pairs(selector.TrafficMetadataKey, "east-west"), selector.TrafficEastWest},
		{"north-south metadata", metadata.Pairs(selector.TrafficMetadataKey, "north-south"), selector.TrafficNorthSouth},
		{"no metadata leaves the class to inference", nil, selector.TrafficAny},
		{"invalid metadata is ignored", metadata.Pairs(selector.TrafficMetadataKey, "sideways"), selector.TrafficAny},
		{"any is not a request class", metadata.Pairs(selector.TrafficMetadataKey, "any"), selector.TrafficAny},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			if tt.md != nil {
				ctx = metadata.NewIncomingContext(ctx, tt.md)
			}
			if got := trafficFromContext(ctx); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestProcess_RoutesByTrafficSourceAndDestination(t *testing.T) {
	cfg := &config.Config{Router: config.RouterConfig{
		Routes: []config.RouteConfig{{
			Name:        "ledger",
			TargetChain: "ledger",
			Matches: []config.MatchConfig{{
				Traffic:      "east_west",
				Sources:      []string{"cidr:10.244.0.0/16"},
				Destinations: []string{"ip:10.96.0.20"},
			}},
		}, {
			Name:        "partner",
			TargetChain: "partner",
			Matches: []config.MatchConfig{{
				Destinations: []string{"host:api.example.com"},
			}},
		}},
		DefaultChains: config.DefaultChainsConfig{NorthSouth: "public", EastWest: "internal"},
	}}
	for i := range cfg.Router.Routes {
		for j := range cfg.Router.Routes[i].Matches {
			if err := cfg.Router.Routes[i].Matches[j].Compile(false); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Each chain answers with its own status so the test can see which one ran.
	s := newTestServer(cfg, map[string]engine.Chain{
		"ledger":   denyChain(461),
		"partner":  denyChain(462),
		"public":   denyChain(463),
		"internal": denyChain(464),
	})
	ew := metadata.Pairs(selector.TrafficMetadataKey, "east-west")

	tests := []struct {
		name string
		md   metadata.MD
		req  *extprocv3.ProcessingRequest
		want int32
	}{
		{"east-west caller to ledger", ew,
			withAttributes(reqHeaders("/", true), "source.address", "10.244.1.5:51234", "destination.address", "10.96.0.20:8080"), 461},
		{"east-west caller to another service", ew,
			withAttributes(reqHeaders("/", true), "source.address", "10.244.1.5:51234", "destination.address", "10.96.0.30:8080"), 464},
		{"same addresses without metadata are north-south", nil,
			withAttributes(reqHeaders("/", true), "source.address", "10.244.1.5:51234", "destination.address", "10.96.0.20:8080"), 463},
		{"destination attribute missing", ew,
			withAttributes(reqHeaders("/", true), "source.address", "10.244.1.5:51234"), 464},
		{"host from :authority", nil,
			reqHeaders("/", true, ":authority", "API.example.com:443"), 462},
		{"host header fallback", nil,
			reqHeaders("/", true, "host", "api.example.com"), 462},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := runWithMetadata(t, s, tt.md, tt.req)
			imm := out[0].GetImmediateResponse()
			if imm == nil || int32(imm.GetStatus().GetCode()) != tt.want {
				t.Fatalf("want chain status %d, got %v", tt.want, out[0])
			}
		})
	}
}

func identityIndex() *identity.Index {
	idx := identity.NewIndex()
	pod := func(ip, ns, name, sa string, services ...string) *identity.Workload {
		return &identity.Workload{IP: netip.MustParseAddr(ip), Kind: identity.KindPod, Namespace: ns, Pod: name,
			ServiceAccount: sa, SPIFFEID: "spiffe://cluster.local/ns/" + ns + "/sa/" + sa, Services: services}
	}
	idx.Replace([]*identity.Workload{
		pod("10.244.1.5", "shop", "checkout-1", "checkout", "shop/checkout"),
		pod("10.244.2.9", "shop", "intruder", "default"),
		pod("10.244.3.3", "payments", "ledger-1", "ledger", "payments/ledger"), // a ledger backend
		{IP: netip.MustParseAddr("192.168.1.10"), Kind: identity.KindNode, Node: "n1"},
	}, []*identity.Service{
		{Namespace: "payments", Name: "ledger", ClusterIPs: []netip.Addr{netip.MustParseAddr("10.96.0.20")}},
	})
	return idx
}

func identityServer(t *testing.T, unknownSource string) *Server {
	t.Helper()
	cfg := &config.Config{
		Identity: config.IdentityConfig{Enabled: true},
		Router: config.RouterConfig{
			Routes: []config.RouteConfig{{
				Name:        "checkout-to-ledger",
				TargetChain: "ledger",
				Matches: []config.MatchConfig{{
					Traffic:      "east_west",
					Sources:      []string{"service:shop/checkout"},
					Destinations: []string{"service:payments/ledger"},
				}},
			}, {
				Name:        "pods-by-cidr",
				TargetChain: "cidr",
				Matches:     []config.MatchConfig{{Sources: []string{"cidr:10.244.0.0/16"}}},
			}, {
				Name:        "outsiders",
				TargetChain: "external",
				Matches:     []config.MatchConfig{{Sources: []string{"external"}}},
			}},
			DefaultChains: config.DefaultChainsConfig{NorthSouth: "public", EastWest: "internal"},
			UnknownSource: unknownSource,
		},
	}
	for i := range cfg.Router.Routes {
		for j := range cfg.Router.Routes[i].Matches {
			if err := cfg.Router.Routes[i].Matches[j].Compile(true); err != nil {
				t.Fatal(err)
			}
		}
	}
	s := newTestServer(cfg, map[string]engine.Chain{
		"ledger": denyChain(461), "cidr": denyChain(462), "external": denyChain(463),
		"public": denyChain(464), "internal": denyChain(465),
	})
	s.identity = identityIndex()
	return s
}

func TestProcess_IdentityRouting(t *testing.T) {
	s := identityServer(t, config.UnknownSourceDeny)
	from := func(src, dst string, kv ...string) *extprocv3.ProcessingRequest {
		attrs := []string{"source.address", src + ":40000"}
		if dst != "" {
			attrs = append(attrs, "destination.address", dst)
		}
		return withAttributes(reqHeaders("/", true, kv...), attrs...)
	}

	tests := []struct {
		name string
		md   metadata.MD
		req  *extprocv3.ProcessingRequest
		want int32
	}{
		{"checkout to ledger ClusterIP, class inferred east-west", nil, from("10.244.1.5", "10.96.0.20:80"), 461},
		{"checkout to a ledger backend pod (CNI already load-balanced)", nil, from("10.244.1.5", "10.244.3.3:8080"), 461},
		{"checkout to ledger by cluster-local authority", nil, from("10.244.1.5", "", ":authority", "ledger.payments.svc:80"), 461},
		{"another pod to ledger falls to the cidr route", nil, from("10.244.2.9", "10.96.0.20:80"), 462},
		{"unknown north-south caller is external", nil, from("203.0.113.7", ""), 463},
		{"node address is not a pod: north-south, and not external", nil, from("192.168.1.10", ""), 464},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := runWithMetadata(t, s, tt.md, tt.req)
			imm := out[0].GetImmediateResponse()
			if imm == nil || int32(imm.GetStatus().GetCode()) != tt.want {
				t.Fatalf("want chain status %d, got %v", tt.want, out[0])
			}
		})
	}
}

func TestProcess_UnknownSource(t *testing.T) {
	ew := metadata.Pairs(selector.TrafficMetadataKey, "east-west")
	req := withAttributes(reqHeaders("/", true), "source.address", "10.244.9.9:1234") // a pod not in the map yet

	deny := identityServer(t, config.UnknownSourceDeny)
	out := runWithMetadata(t, deny, ew, req)
	if imm := out[0].GetImmediateResponse(); imm == nil || imm.GetStatus().GetCode() != 503 {
		t.Fatalf("unknown east-west caller must get 503 with unknown_source deny, got %v", out[0])
	}

	// With "default", only network selectors can match and "external" does not.
	req = withAttributes(reqHeaders("/", true), "source.address", "10.244.9.9:1234")
	def := identityServer(t, config.UnknownSourceDefault)
	out = runWithMetadata(t, def, ew, req)
	if imm := out[0].GetImmediateResponse(); imm == nil || imm.GetStatus().GetCode() != 462 {
		t.Fatalf("expected the cidr route, got %v", out[0])
	}
	req = withAttributes(reqHeaders("/", true), "source.address", "198.51.100.1:1234")
	out = runWithMetadata(t, def, ew, req)
	if imm := out[0].GetImmediateResponse(); imm == nil || imm.GetStatus().GetCode() != 465 {
		t.Fatalf("unknown east-west caller must not match external, got %v", out[0])
	}
}

func TestProcess_UnknownSourceIgnoredWithoutIdentity(t *testing.T) {
	cfg := &config.Config{Router: config.RouterConfig{DefaultChain: "public", UnknownSource: config.UnknownSourceDeny}}
	s := newTestServer(cfg, map[string]engine.Chain{"public": denyChain(464)})
	ew := metadata.Pairs(selector.TrafficMetadataKey, "east-west")
	out := runWithMetadata(t, s, ew, withAttributes(reqHeaders("/", true), "source.address", "10.244.9.9:1234"))
	if imm := out[0].GetImmediateResponse(); imm == nil || imm.GetStatus().GetCode() != 464 {
		t.Fatalf("without identity, unknown_source must not apply, got %v", out[0])
	}
}
