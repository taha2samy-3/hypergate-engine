package metrics

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func scrape(t *testing.T, m *Engine) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body, _ := io.ReadAll(rec.Body)
	return string(body)
}

func TestEngineMetrics(t *testing.T) {
	m := New()
	m.Request("api", "web-api", OutcomeAllowed)
	m.Request("api", "web-api", OutcomeAllowed)
	m.Request("", "", OutcomeDenied)
	m.Deny("web-api", "JwtAuthFilter/users", 401)
	m.Observe("request_headers", "web-api", 120*time.Microsecond)
	m.UnknownSource()
	m.Reload(true)
	m.Reload(false)
	m.GaugeFunc("hypergate_test_gauge", "test", func() float64 { return 7 })

	out := scrape(t, m)
	for _, want := range []string{
		`hypergate_requests_total{chain="web-api",outcome="allowed",route="api"} 2`,
		`hypergate_requests_total{chain="none",outcome="denied",route="default"} 1`,
		`hypergate_denies_total{chain="web-api",filter="JwtAuthFilter/users",status="401"} 1`,
		`hypergate_message_duration_seconds_count{chain="web-api",phase="request_headers"} 1`,
		`hypergate_unknown_source_total 1`,
		`hypergate_policy_reloads_total{result="success"} 1`,
		`hypergate_policy_reloads_total{result="rejected"} 1`,
		`hypergate_test_gauge 7`,
		`go_goroutines`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestNilEngineIsSafe(t *testing.T) {
	var m *Engine
	m.Request("a", "b", OutcomeAllowed)
	m.Deny("b", "f", 403)
	m.Observe("request_headers", "b", time.Millisecond)
	m.UnknownSource()
	m.Reload(true)
}

func BenchmarkRequestAndObserve(b *testing.B) {
	m := New()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			m.Observe("request_headers", "web-api", 100*time.Microsecond)
			m.Request("api", "web-api", OutcomeAllowed)
		}
	})
}
