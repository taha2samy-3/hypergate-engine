package config

import (
	"fmt"
	"time"

	"github.com/taha2samy/hypergate/internal/logger"
)

// TelemetryConfig defines observability settings like logging.
type TelemetryConfig struct {
	Logging logger.LoggingConfig `yaml:"logging"`
}

// TLSConfig defines TLS/mTLS options for the gRPC listener.
type TLSConfig struct {
	// Enabled toggles TLS on the gRPC listener. Default: false.
	Enabled bool `yaml:"enabled"`
	// CertFile is the path to the server's PEM-encoded certificate.
	CertFile string `yaml:"cert_file"`
	// KeyFile is the path to the server's PEM-encoded private key.
	KeyFile string `yaml:"key_file"`
	// CAFile is the path to the PEM-encoded CA certificate used to verify
	// client certificates. Required for mutual TLS.
	CAFile string `yaml:"ca_file"`
	// MutualTLS enables client certificate verification (mTLS). Requires CAFile.
	MutualTLS bool `yaml:"mutual_tls"`
}

// ClientIPConfig controls how the downstream client address is resolved.
type ClientIPConfig struct {
	// TrustedProxyHops is the number of proxies/load balancers in front of Envoy
	// whose X-Forwarded-For entries are trusted. 0 (default) means the client is
	// Envoy's direct peer. See internal/clientip.
	TrustedProxyHops int `yaml:"trusted_proxy_hops"`
}

// ServerConfig defines the gRPC server settings.
type ServerConfig struct {
	Address                 string    `yaml:"address"`
	MaxConcurrentStreams    uint32    `yaml:"max_concurrent_streams"`
	PoolPrewarmSize         int       `yaml:"pool_prewarm_size"`
	InitialHeaderCapacity   int       `yaml:"initial_header_capacity"`
	PreallocBodyBufferBytes int       `yaml:"prealloc_body_buffer_bytes"`
	TLS                     TLSConfig `yaml:"tls"`
	// HealthAddress serves /healthz and /readyz for Kubernetes probes. Default ":9003".
	HealthAddress string `yaml:"health_address"`
	// PprofAddress enables the Go pprof endpoints on the given address when set,
	// e.g. "127.0.0.1:6060". Disabled by default; never expose it publicly.
	PprofAddress string         `yaml:"pprof_address"`
	ClientIP     ClientIPConfig `yaml:"client_ip"`
	// Shutdown controls how the engine stops on SIGTERM. Read at start-up only.
	Shutdown ShutdownConfig `yaml:"shutdown"`
}

// ShutdownConfig controls the stop sequence: after SIGTERM the engine reports not
// ready but keeps accepting ext_proc streams until none has arrived for
// QuietPeriod (waiting at least MinDelay and at most MaxDelay), so Envoy has
// time to stop sending; then it drains open streams for up to DrainTimeout and
// closes the rest.
type ShutdownConfig struct {
	QuietPeriod  string `yaml:"quiet_period"`
	MinDelay     string `yaml:"min_delay"`
	MaxDelay     string `yaml:"max_delay"`
	DrainTimeout string `yaml:"drain_timeout"`

	QuietPeriodDuration  time.Duration `yaml:"-"`
	MinDelayDuration     time.Duration `yaml:"-"`
	MaxDelayDuration     time.Duration `yaml:"-"`
	DrainTimeoutDuration time.Duration `yaml:"-"`
}

// Default shutdown timings. The operator sets terminationGracePeriodSeconds to
// cover MaxDelay + DrainTimeout.
const (
	DefaultShutdownQuietPeriod  = 2 * time.Second
	DefaultShutdownMinDelay     = 3 * time.Second
	DefaultShutdownMaxDelay     = 15 * time.Second
	DefaultShutdownDrainTimeout = 20 * time.Second
)

// applyDefaults parses the durations, using the defaults for empty values.
func (c *ShutdownConfig) applyDefaults() error {
	for _, f := range []struct {
		name string
		raw  string
		def  time.Duration
		out  *time.Duration
	}{
		{"quiet_period", c.QuietPeriod, DefaultShutdownQuietPeriod, &c.QuietPeriodDuration},
		{"min_delay", c.MinDelay, DefaultShutdownMinDelay, &c.MinDelayDuration},
		{"max_delay", c.MaxDelay, DefaultShutdownMaxDelay, &c.MaxDelayDuration},
		{"drain_timeout", c.DrainTimeout, DefaultShutdownDrainTimeout, &c.DrainTimeoutDuration},
	} {
		if f.raw == "" {
			*f.out = f.def
			continue
		}
		d, err := time.ParseDuration(f.raw)
		if err != nil || d < 0 {
			return fmt.Errorf("server.shutdown.%s: invalid duration %q", f.name, f.raw)
		}
		*f.out = d
	}
	if c.MinDelayDuration > c.MaxDelayDuration {
		return fmt.Errorf("server.shutdown.min_delay (%s) must not exceed max_delay (%s)", c.MinDelayDuration, c.MaxDelayDuration)
	}
	return nil
}
