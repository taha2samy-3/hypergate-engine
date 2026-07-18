package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// Redis Configuration
// ---------------------------------------------------------------------------

// RedisServiceConfig defines the full configuration surface for a single Redis
// service, covering every field supported by the Envoy Rate Limit service.
//
// Field defaults are applied via UnmarshalYAML so that any omitted YAML key
// resolves to its canonical default rather than the Go zero-value.
type RedisServiceConfig struct {
	// Type selects the Redis topology. Valid values: "SINGLE", "CLUSTER", "SENTINEL".
	Type string `yaml:"type"`

	// SocketType selects the network transport. Valid values: "tcp", "unix".
	SocketType string `yaml:"socket_type"`

	// URL is the connection endpoint.
	//   SINGLE:   "host:port"
	//   CLUSTER:  comma-separated "host1:port1,host2:port2,..."
	//   SENTINEL: "<master_name>,<sentinel_host1>:26379,..."
	URL string `yaml:"url"`

	// PoolSize is the maximum number of connections in the pool.
	PoolSize int `yaml:"pool_size"`

	// Auth is the Redis authentication credential.
	// Accepts either "password" or "username:password" formats.
	Auth string `yaml:"auth"`

	// TLS enables TLS for the Redis connection.
	TLS bool `yaml:"tls"`

	// TLSClientCert is the path to the PEM-encoded client certificate file.
	TLSClientCert string `yaml:"tls_client_cert"`

	// TLSClientKey is the path to the PEM-encoded client private key file.
	TLSClientKey string `yaml:"tls_client_key"`

	// TLSCACert is the path to the PEM-encoded CA certificate file used to
	// verify the server's certificate chain.
	TLSCACert string `yaml:"tls_cacert"`

	// TLSSkipHostnameVerification disables TLS server-name verification when true.
	// WARNING: enabling this in production degrades transport security.
	TLSSkipHostnameVerification bool `yaml:"tls_skip_hostname_verification"`

	// PipelineWindow controls the maximum time a pipeline waits for more
	// commands before flushing. Stored as a raw string; parsed to
	// time.Duration by ApplyDefaults. "0s" disables implicit pipelining.
	PipelineWindow string `yaml:"pipeline_window"`

	// PipelineWindowDuration is the parsed form of PipelineWindow.
	PipelineWindowDuration time.Duration `yaml:"-"`

	// ClusterPipelineParallelism sets the number of concurrent goroutines used
	// to flush a pipeline across cluster shards.
	ClusterPipelineParallelism int `yaml:"cluster_pipeline_parallelism"`

	// SentinelAuth is the password for authenticating to Sentinel nodes
	// (separate from the Redis data-node password).
	SentinelAuth string `yaml:"sentinel_auth"`

	// ActiveConnHealthCheck enables periodic health checks on idle connections.
	ActiveConnHealthCheck bool `yaml:"active_conn_health_check"`

	// Timeout is the dial / read / write deadline for Redis operations.
	// Stored as a raw string; parsed to time.Duration by ApplyDefaults.
	Timeout string `yaml:"timeout"`

	// TimeoutDuration is the parsed form of Timeout.
	TimeoutDuration time.Duration `yaml:"-"`

	// OnEmptyBehavior controls pool-exhaustion behavior.
	// Only "WAIT" is supported (radix/v4 always blocks).
	OnEmptyBehavior string `yaml:"on_empty_behavior"`

	// WaitTimeout is the maximum duration to block waiting for a connection
	// when the pool is exhausted. Stored as a raw string; parsed to
	// time.Duration by ApplyDefaults.
	WaitTimeout string `yaml:"wait_timeout"`

	// WaitTimeoutDuration is the parsed form of WaitTimeout.
	WaitTimeoutDuration time.Duration `yaml:"-"`

	// StartupInitialInterval is the first back-off interval used during
	// connection retries at startup.
	StartupInitialInterval string `yaml:"startup_initial_interval"`

	// StartupInitialIntervalDuration is the parsed form of StartupInitialInterval.
	StartupInitialIntervalDuration time.Duration `yaml:"-"`

	// StartupMaxInterval is the ceiling back-off interval during startup retries.
	StartupMaxInterval string `yaml:"startup_max_interval"`

	// StartupMaxIntervalDuration is the parsed form of StartupMaxInterval.
	StartupMaxIntervalDuration time.Duration `yaml:"-"`

	// StartupMaxElapsedTime is the total budget for startup retries.
	// "0s" means retry indefinitely.
	StartupMaxElapsedTime string `yaml:"startup_max_elapsed_time"`

	// StartupMaxElapsedTimeDuration is the parsed form of StartupMaxElapsedTime.
	StartupMaxElapsedTimeDuration time.Duration `yaml:"-"`
}

// applyRedisDefaults fills in every zero-value field of cfg with its
// canonical default. It is called unconditionally during UnmarshalYAML so
// that the caller can rely on a fully-initialized struct regardless of how
// sparse the YAML block is.
func applyRedisDefaults(cfg *RedisServiceConfig) error {
	if cfg.Type == "" {
		cfg.Type = "SINGLE"
	}
	if cfg.SocketType == "" {
		cfg.SocketType = "tcp"
	}
	if cfg.URL == "" {
		cfg.URL = "localhost:6379"
	}
	if cfg.PoolSize == 0 {
		cfg.PoolSize = 10
	}
	if cfg.PipelineWindow == "" {
		cfg.PipelineWindow = "0s"
	}
	if cfg.ClusterPipelineParallelism == 0 {
		cfg.ClusterPipelineParallelism = 1
	}
	if cfg.Timeout == "" {
		cfg.Timeout = "10s"
	}
	if cfg.OnEmptyBehavior == "" {
		cfg.OnEmptyBehavior = "WAIT"
	}
	if cfg.WaitTimeout == "" {
		cfg.WaitTimeout = "1s"
	}
	if cfg.StartupInitialInterval == "" {
		cfg.StartupInitialInterval = "1s"
	}
	if cfg.StartupMaxInterval == "" {
		cfg.StartupMaxInterval = "30s"
	}
	if cfg.StartupMaxElapsedTime == "" {
		cfg.StartupMaxElapsedTime = "0s"
	}

	// --- Semantic Validation ---
	switch cfg.Type {
	case "SINGLE", "CLUSTER", "SENTINEL":
		// valid
	default:
		return fmt.Errorf("redis: invalid type %q: must be SINGLE, CLUSTER, or SENTINEL", cfg.Type)
	}
	switch cfg.SocketType {
	case "tcp", "unix":
		// valid
	default:
		return fmt.Errorf("redis: invalid socket_type %q: must be tcp or unix", cfg.SocketType)
	}
	if cfg.OnEmptyBehavior != "WAIT" {
		return fmt.Errorf("redis: invalid on_empty_behavior %q: only \"WAIT\" is supported (radix/v4 always blocks)", cfg.OnEmptyBehavior)
	}

	// --- Duration Parsing ---
	var err error
	if cfg.PipelineWindowDuration, err = time.ParseDuration(cfg.PipelineWindow); err != nil {
		return fmt.Errorf("redis: invalid pipeline_window %q: %w", cfg.PipelineWindow, err)
	}
	if cfg.TimeoutDuration, err = time.ParseDuration(cfg.Timeout); err != nil {
		return fmt.Errorf("redis: invalid timeout %q: %w", cfg.Timeout, err)
	}
	if cfg.WaitTimeoutDuration, err = time.ParseDuration(cfg.WaitTimeout); err != nil {
		return fmt.Errorf("redis: invalid wait_timeout %q: %w", cfg.WaitTimeout, err)
	}
	if cfg.StartupInitialIntervalDuration, err = time.ParseDuration(cfg.StartupInitialInterval); err != nil {
		return fmt.Errorf("redis: invalid startup_initial_interval %q: %w", cfg.StartupInitialInterval, err)
	}
	if cfg.StartupMaxIntervalDuration, err = time.ParseDuration(cfg.StartupMaxInterval); err != nil {
		return fmt.Errorf("redis: invalid startup_max_interval %q: %w", cfg.StartupMaxInterval, err)
	}
	if cfg.StartupMaxElapsedTimeDuration, err = time.ParseDuration(cfg.StartupMaxElapsedTime); err != nil {
		return fmt.Errorf("redis: invalid startup_max_elapsed_time %q: %w", cfg.StartupMaxElapsedTime, err)
	}

	return nil
}

// UnmarshalYAML implements yaml.Unmarshaler for RedisServiceConfig.
//
// It first decodes the YAML node into the struct using a type alias (to avoid
// infinite recursion), then calls applyRedisDefaults to fill any omitted
// fields and parse all duration strings. This guarantees that callers always
// receive a fully-initialized, validated struct.
func (r *RedisServiceConfig) UnmarshalYAML(value *yaml.Node) error {
	type alias RedisServiceConfig
	var a alias
	if err := value.Decode(&a); err != nil {
		return fmt.Errorf("redis: failed to decode service config: %w", err)
	}
	*r = RedisServiceConfig(a)
	return applyRedisDefaults(r)
}