package grpc

import (
	"context"
	"testing"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/taha2samy/hypergate/internal/config"
	"github.com/taha2samy/hypergate/internal/engine"
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
		{"no metadata is inferred north-south", nil, selector.TrafficNorthSouth},
		{"invalid metadata is inferred", metadata.Pairs(selector.TrafficMetadataKey, "sideways"), selector.TrafficNorthSouth},
		{"any is not a request class", metadata.Pairs(selector.TrafficMetadataKey, "any"), selector.TrafficNorthSouth},
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
			if err := cfg.Router.Routes[i].Matches[j].Compile(); err != nil {
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
