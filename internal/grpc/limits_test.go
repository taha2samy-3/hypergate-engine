package grpc

import (
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"strings"
	"testing"
	"time"

	"github.com/taha2samy/hypergate/internal/config"
	"github.com/taha2samy/hypergate/internal/engine"
	"github.com/taha2samy/hypergate/internal/metrics"
)

// slowFilter waits for its context like a Redis or sidecar call would.
type slowFilter struct{}

func (slowFilter) Execute(ctx *engine.RequestContext) error {
	select {
	case <-ctx.Ctx.Done():
		return ctx.Ctx.Err()
	case <-time.After(5 * time.Second):
		return nil
	}
}

// gateFilter blocks until released, to hold the chain's concurrency slot.
type gateFilter struct {
	entered chan struct{}
	release chan struct{}
}

func (g gateFilter) Execute(*engine.RequestContext) error {
	g.entered <- struct{}{}
	<-g.release
	return nil
}

// limitsServer parses chain_settings like a real configuration and serves the
// default chain "c" with the given filters.
func limitsServer(t *testing.T, settingsYAML string, chain engine.Chain) (*Server, *metrics.Engine) {
	t.Helper()
	cfg, err := config.ParseBytes([]byte("version: v1\nchains:\n  c: [{type: test}]\nchain_settings:\n  c: " + settingsYAML + "\nrouter:\n  default_chain: c\n"))
	if err != nil {
		t.Fatal(err)
	}
	s := newTestServer(cfg, map[string]engine.Chain{"c": chain})
	m := metrics.New()
	s.metrics = m
	return s, m
}

func respStatus(t *testing.T, out []*extprocv3.ProcessingResponse) int32 {
	t.Helper()
	if imm := out[0].GetImmediateResponse(); imm != nil {
		return int32(imm.GetStatus().GetCode())
	}
	return 200
}

func TestChainTimeout(t *testing.T) {
	s, m := limitsServer(t, `{timeout: 50ms}`, engine.Chain{slowFilter{}})
	start := time.Now()
	out := run(t, s, reqHeaders("/", true))
	if got := respStatus(t, out); got != 503 {
		t.Fatalf("status = %d, want 503", got)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("the deadline did not stop the filter")
	}
	if !strings.Contains(scrapeMetrics(t, m), `hypergate_chain_rejections_total{chain="c",reason="timeout"} 1`) {
		t.Fatal("timeout not counted")
	}

	s, _ = limitsServer(t, `{timeout: 50ms, on_timeout: allow}`, engine.Chain{slowFilter{}})
	if got := respStatus(t, run(t, s, reqHeaders("/", true))); got != 200 {
		t.Fatalf("on_timeout allow: status = %d, want the request to continue", got)
	}
}

func TestChainMaxConcurrency(t *testing.T) {
	for _, tc := range []struct {
		settings string
		want     int32
	}{{`{max_concurrency: 1}`, 503}, {`{max_concurrency: 1, on_overload: allow}`, 200}} {
		t.Run(tc.settings, func(t *testing.T) {
			g := gateFilter{entered: make(chan struct{}, 1), release: make(chan struct{})}
			s, m := limitsServer(t, tc.settings, engine.Chain{g})

			done := make(chan struct{})
			go func() {
				defer close(done)
				run(t, s, reqHeaders("/", true))
			}()
			<-g.entered // the first request holds the only slot

			// The second request finds the chain full. With "allow" it skips the
			// chain, so the gate is never entered.
			out := run(t, s, reqHeaders("/", true))
			close(g.release)
			<-done

			if got := respStatus(t, out); got != tc.want {
				t.Fatalf("status = %d, want %d", got, tc.want)
			}
			if tc.want == 503 {
				hdrs := out[0].GetImmediateResponse().GetHeaders().GetSetHeaders()
				found := false
				for _, h := range hdrs {
					if h.GetHeader().GetKey() == "retry-after" {
						found = true
					}
				}
				if !found {
					t.Fatal("Retry-After missing on overload")
				}
			}
			if !strings.Contains(scrapeMetrics(t, m), `hypergate_chain_rejections_total{chain="c",reason="overload"} 1`) {
				t.Fatal("overload not counted")
			}
		})
	}
}

func TestAuditMetrics(t *testing.T) {
	cfg := &config.Config{
		Chains: map[string]config.Chain{"c": {{Name: "JwtAuthFilter/users", Type: "jwt_auth", Audit: true}}},
		Router: config.RouterConfig{DefaultChain: "c"},
	}
	s := newTestServer(cfg, map[string]engine.Chain{"c": {engine.Audit(statusFilter(401))}})
	m := metrics.New()
	s.metrics = m
	out := run(t, s, reqHeaders("/", true))
	if out[0].GetImmediateResponse() != nil {
		t.Fatal("audited denial was enforced")
	}
	got := scrapeMetrics(t, m)
	if !strings.Contains(got, `hypergate_audit_denies_total{chain="c",filter="JwtAuthFilter/users",status="401"} 1`) ||
		!strings.Contains(got, `hypergate_requests_total{chain="c",outcome="allowed",route="default"} 1`) {
		t.Fatalf("audit not recorded:\n%s", got)
	}
}
