// Package metrics exposes the engine's Prometheus metrics on the health port
// (/metrics). Labels only take values from the configuration (route, chain and
// filter names) or small fixed sets, never paths, addresses or users.
//
// The hot path avoids hashing label values on every request: resolved series are
// cached per label combination.
package metrics

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Request outcomes.
const (
	OutcomeAllowed  = "allowed"  // the request continued to the upstream
	OutcomeDenied   = "denied"   // a filter or the engine refused it (status >= 400)
	OutcomeAnswered = "answered" // answered without the upstream, not a denial (e.g. a CORS preflight)
	OutcomeError    = "error"    // a filter failed internally
)

// Engine holds the engine's metrics.
type Engine struct {
	registry *prometheus.Registry

	requests      *prometheus.CounterVec
	denies        *prometheus.CounterVec
	audits        *prometheus.CounterVec
	duration      *prometheus.HistogramVec
	unknownSource prometheus.Counter
	reloads       *prometheus.CounterVec
	lastReload    prometheus.Gauge

	requestSeries  sync.Map // requestKey -> prometheus.Counter
	durationSeries sync.Map // durationKey -> prometheus.Observer
}

type requestKey struct{ route, chain, outcome string }

type durationKey struct {
	phase string
	chain string
}

// New creates the engine metrics in their own registry, together with the Go
// runtime and process collectors.
func New() *Engine {
	reg := prometheus.NewRegistry()
	m := &Engine{
		registry: reg,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "hypergate_requests_total",
			Help: "Requests processed, by matched route, chain and outcome (allowed, denied, answered, error).",
		}, []string{"route", "chain", "outcome"}),
		denies: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "hypergate_denies_total",
			Help: "Requests denied or failed, by chain, deciding filter (\"engine\" for engine decisions) and status.",
		}, []string{"chain", "filter", "status"}),
		audits: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "hypergate_audit_denies_total",
			Help: "Denials and failures of audited filters that were recorded but not enforced, by chain, filter and status.",
		}, []string{"chain", "filter", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "hypergate_message_duration_seconds",
			Help:    "Time the engine spent on one ext_proc message (what Envoy waits for), by phase and chain.",
			Buckets: prometheus.ExponentialBuckets(25e-6, 2.5, 12), // 25µs … ~1.5s
		}, []string{"phase", "chain"}),
		unknownSource: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "hypergate_unknown_source_total",
			Help: "East-west requests whose caller was not in the identity map.",
		}),
		reloads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "hypergate_policy_reloads_total",
			Help: "Policy reloads, by result (success, rejected).",
		}, []string{"result"}),
		lastReload: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "hypergate_policy_last_reload_success_timestamp_seconds",
			Help: "Unix time of the last policy that was applied successfully (including the one loaded at start-up).",
		}),
	}
	reg.MustRegister(m.requests, m.denies, m.audits, m.duration, m.unknownSource, m.reloads, m.lastReload,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return m
}

// Handler serves the metrics in the Prometheus text format.
func (m *Engine) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{Registry: m.registry})
}

// Registerer lets other components add their own metrics.
func (m *Engine) Registerer() prometheus.Registerer { return m.registry }

// Request counts one finished request.
func (m *Engine) Request(route, chain, outcome string) {
	if m == nil {
		return
	}
	key := requestKey{route, chain, outcome}
	c, ok := m.requestSeries.Load(key)
	if !ok {
		c, _ = m.requestSeries.LoadOrStore(key, m.requests.WithLabelValues(labelOr(route, "default"), labelOr(chain, "none"), outcome))
	}
	c.(prometheus.Counter).Inc()
}

// Deny counts a denial or failure and the filter that made it. It is not on the
// path of allowed requests, so the label lookup is not cached.
func (m *Engine) Deny(chain, filter string, status int32) {
	if m == nil {
		return
	}
	m.denies.WithLabelValues(labelOr(chain, "none"), filter, strconv.Itoa(int(status))).Inc()
}

// Audit counts a denial or failure of an audited filter that was not enforced.
func (m *Engine) Audit(chain, filter string, status int32) {
	if m == nil {
		return
	}
	m.audits.WithLabelValues(labelOr(chain, "none"), filter, strconv.Itoa(int(status))).Inc()
}

// Observe records the time spent on one ext_proc message.
func (m *Engine) Observe(phase, chain string, d time.Duration) {
	if m == nil {
		return
	}
	key := durationKey{phase, chain}
	o, ok := m.durationSeries.Load(key)
	if !ok {
		o, _ = m.durationSeries.LoadOrStore(key, m.duration.WithLabelValues(phase, labelOr(chain, "none")))
	}
	o.(prometheus.Observer).Observe(d.Seconds())
}

// UnknownSource counts an east-west request from a caller missing from the identity map.
func (m *Engine) UnknownSource() {
	if m != nil {
		m.unknownSource.Inc()
	}
}

// Reload records the result of applying a policy.
func (m *Engine) Reload(ok bool) {
	if m == nil {
		return
	}
	if ok {
		m.reloads.WithLabelValues("success").Inc()
		m.lastReload.SetToCurrentTime()
		return
	}
	m.reloads.WithLabelValues("rejected").Inc()
}

// GaugeFunc registers a gauge whose value is read at scrape time.
func (m *Engine) GaugeFunc(name, help string, f func() float64) {
	m.registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: name, Help: help}, f))
}

// CounterFunc registers a counter whose value is read at scrape time.
func (m *Engine) CounterFunc(name, help string, f func() float64) {
	m.registry.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{Name: name, Help: help}, f))
}

func labelOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
