package grpc

import (
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc/metadata"

	"github.com/taha2samy/hypergate/internal/config"
	"github.com/taha2samy/hypergate/internal/engine"
	"github.com/taha2samy/hypergate/internal/metrics"
	"github.com/taha2samy/hypergate/internal/selector"
)

type passFilter struct{}

func (passFilter) Execute(*engine.RequestContext) error { return nil }

type failFilter struct{}

func (failFilter) Execute(*engine.RequestContext) error { return errors.New("redis down") }

func scrapeMetrics(t *testing.T, m *metrics.Engine) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	b, _ := io.ReadAll(rec.Body)
	return string(b)
}

func TestProcess_RecordsMetrics(t *testing.T) {
	cfg := &config.Config{
		Chains: map[string]config.Chain{
			"web-api": {{Name: "CorsFilter/web", Type: "cors"}, {Name: "JwtAuthFilter/users", Type: "jwt_auth"}},
			"unnamed": {{Type: "deny"}},
			"answer":  {{Type: "cors"}},
			"broken":  {{Type: "rate_limit"}},
		},
		Router: config.RouterConfig{
			Routes: []config.RouteConfig{
				{Name: "api", TargetChain: "web-api", Matches: []config.MatchConfig{{PathPrefix: "/api"}}},
				{Name: "plain", TargetChain: "unnamed", Matches: []config.MatchConfig{{PathPrefix: "/plain"}}},
				{Name: "preflight", TargetChain: "answer", Matches: []config.MatchConfig{{PathPrefix: "/preflight"}}},
				{Name: "fail", TargetChain: "broken", Matches: []config.MatchConfig{{PathPrefix: "/fail"}}},
				{Name: "gone", TargetChain: "missing", Matches: []config.MatchConfig{{PathPrefix: "/gone"}}},
			},
		},
	}
	s := newTestServer(cfg, map[string]engine.Chain{
		"web-api": {passFilter{}, statusFilter(401)},
		"unnamed": {statusFilter(403)},
		"answer":  {statusFilter(204)},
		"broken":  {failFilter{}},
	})
	m := metrics.New()
	s.metrics = m

	for _, path := range []string{"/api", "/plain", "/preflight", "/fail", "/gone", "/other"} {
		run(t, s, reqHeaders(path, true))
	}
	out := scrapeMetrics(t, m)
	for _, want := range []string{
		`hypergate_requests_total{chain="web-api",outcome="denied",route="api"} 1`,
		`hypergate_denies_total{chain="web-api",filter="JwtAuthFilter/users",status="401"} 1`,
		`hypergate_denies_total{chain="unnamed",filter="0:deny",status="403"} 1`,
		`hypergate_requests_total{chain="answer",outcome="answered",route="preflight"} 1`,
		`hypergate_requests_total{chain="broken",outcome="error",route="fail"} 1`,
		`hypergate_denies_total{chain="broken",filter="0:rate_limit",status="500"} 1`,
		`hypergate_denies_total{chain="missing",filter="engine",status="503"} 1`,
		`hypergate_requests_total{chain="none",outcome="allowed",route="default"} 1`,
		`hypergate_message_duration_seconds_count{chain="web-api",phase="request_headers"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(out, `filter="CorsFilter/web"`) {
		t.Error("a filter that let the request pass must not be counted as the decider")
	}
}

func TestProcess_UnknownSourceMetric(t *testing.T) {
	s := identityServer(t, config.UnknownSourceDeny)
	m := metrics.New()
	s.metrics = m
	ew := metadata.Pairs(selector.TrafficMetadataKey, "east-west")
	runWithMetadata(t, s, ew, withAttributes(reqHeaders("/", true), "source.address", "10.244.9.9:1234"))
	out := scrapeMetrics(t, m)
	if !strings.Contains(out, "hypergate_unknown_source_total 1") ||
		!strings.Contains(out, `hypergate_denies_total{chain="none",filter="engine",status="503"} 1`) {
		t.Fatalf("unknown source not recorded:\n%s", out)
	}
}
