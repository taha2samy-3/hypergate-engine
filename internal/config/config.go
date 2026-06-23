package config

import (
	"fmt"
	"regexp"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/taha/myprog/internal/logger"
)

// GlobalConfig stores the currently active configuration in a thread-safe atomic pointer.
var GlobalConfig atomic.Pointer[Config]

// Config represents the top-level configuration loaded from YAML.
type Config struct {
	Version   string           `yaml:"version"`
	Server    ServerConfig     `yaml:"server"`
	Telemetry TelemetryConfig  `yaml:"telemetry"`
	Chains    map[string]Chain `yaml:"chains"`
	Router    RouterConfig     `yaml:"router"`
	// Redis holds a named map of independent Redis service configurations.
	// Each key becomes the service name used for O(1) lookup in the redis.Manager.
	Redis map[string]RedisServiceConfig `yaml:"redis"`
}

// RouterConfig holds the routing rules and the fallback chain name.
type RouterConfig struct {
	Routes       []RouteConfig `yaml:"routes"`
	DefaultChain string        `yaml:"default_chain"`
}

// TelemetryConfig defines observability settings like logging.
type TelemetryConfig struct {
	Logging logger.LoggingConfig `yaml:"logging"`
}

// ServerConfig defines the gRPC server settings.
type ServerConfig struct {
	Address              string `yaml:"address"`
	MaxConcurrentStreams uint32 `yaml:"max_concurrent_streams"`
}

// FilterConfig defines a single filter configuration.
type FilterConfig struct {
	Type    string                 `yaml:"type"`
	Options map[string]interface{} `yaml:"options"`
}

// Chain defines a filter chain configuration as a sequence of filters.
type Chain []FilterConfig

// RouteConfig defines routing rules to a target chain.
type RouteConfig struct {
	Name        string        `yaml:"name"`
	Matches     []MatchConfig `yaml:"matches"`
	TargetChain string        `yaml:"target_chain"`
}

// MatchConfig holds conditions to evaluate an incoming request (AND logic).
type MatchConfig struct {
	PathPrefix        string                       `yaml:"path_prefix"`
	PathRegexPattern  string                       `yaml:"path_regex_pattern"`
	CompiledPathRegex *regexp.Regexp               `yaml:"-"` // Pre-compiled at startup
	Headers           map[string]HeaderMatchConfig `yaml:"headers"`
}

// HeaderMatchConfig holds exact or regex matching criteria for a header.
type HeaderMatchConfig struct {
	Exact         string         `yaml:"exact"`
	RegexPattern  string         `yaml:"regex_pattern"`
	CompiledRegex *regexp.Regexp `yaml:"-"` // Pre-compiled at startup
}

// UnmarshalYAML implements custom unmarshaling to support shorthand string syntax for headers.
func (h *HeaderMatchConfig) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		h.Exact = value.Value
		return nil
	}
	type alias HeaderMatchConfig
	var a alias
	if err := value.Decode(&a); err != nil {
		return err
	}
	*h = HeaderMatchConfig(a)
	return nil
}

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

// ---------------------------------------------------------------------------
// OpenID Connect Filter Configuration
// ---------------------------------------------------------------------------

// ClaimToHeaderMapping instructs the OIDC filter to extract a JWT/UserInfo
// claim and inject it as an upstream request header.
type ClaimToHeaderMapping struct {
	HeaderName string `yaml:"header_name"`
	ClaimName  string `yaml:"claim_name"`
}

// OIDCFilterConfig is the fully-typed options struct for the "openid-connect"
// filter type. Fields that represent durations are stored as raw strings and
// must be parsed by the filter constructor via time.ParseDuration.
type OIDCFilterConfig struct {
	// ProviderName is a human-readable label used in log messages.
	ProviderName string `yaml:"provider_name"`

	// IssuerURL is the OIDC issuer base URL (e.g. https://accounts.example.com).
	IssuerURL string `yaml:"issuer_url"`

	// ClientID is the OAuth2 client identifier expected in the JWT aud claim.
	ClientID string `yaml:"client_id"`

	// SkipDiscovery disables automatic OIDC discovery; when true, JwksURL
	// must be supplied explicitly.
	SkipDiscovery bool `yaml:"skip_discovery"`

	// JwksURL is the explicit JWKS endpoint. Required when SkipDiscovery is
	// true; otherwise constructed automatically from IssuerURL.
	JwksURL string `yaml:"jwks_url"`

	// JwksCacheDuration is the maximum age of a cached JWKS key set.
	// Parsed as a Go duration string (e.g. "300s", "5m"). Default: "300s".
	JwksCacheDuration string `yaml:"jwks_cache_duration"`

	// JwksTimeout is the HTTP client deadline for fetching the JWKS endpoint.
	// Default: "2s".
	JwksTimeout string `yaml:"jwks_timeout"`

	// ClockSkew is the maximum tolerated clock difference when validating JWT
	// time claims (nbf, exp). Default: "60s".
	ClockSkew string `yaml:"clock_skew"`

	// UserInfoURL is the OIDC UserInfo endpoint used to validate opaque
	// (non-JWT) access tokens.
	UserInfoURL string `yaml:"userinfo_url"`

	// UserInfoTimeout is the HTTP client deadline for calling UserInfoURL.
	// Default: "2s".
	UserInfoTimeout string `yaml:"userinfo_timeout"`

	// UserIDClaim is the JWT claim used as the principal identifier.
	// Default: "sub".
	UserIDClaim string `yaml:"user_id_claim"`

	// EmailClaim is the JWT claim containing the user's email address.
	// Default: "email".
	EmailClaim string `yaml:"email_claim"`

	// GroupsClaim is the JWT claim containing the user's group memberships.
	// Default: "groups".
	GroupsClaim string `yaml:"groups_claim"`

	// AllowUnverifiedEmail permits requests from users whose email_verified
	// claim is explicitly false. Default: false (strict).
	AllowUnverifiedEmail bool `yaml:"allow_unverified_email"`

	// ClaimToHeaders defines zero or more claim-to-upstream-header mappings.
	ClaimToHeaders []ClaimToHeaderMapping `yaml:"claim_to_headers"`
}

// applyOIDCDefaults fills every zero-value string field in cfg with its
// canonical default. It is called by ParseOIDCFilterConfig after decoding.
func applyOIDCDefaults(cfg *OIDCFilterConfig) {
	if cfg.UserIDClaim == "" {
		cfg.UserIDClaim = "sub"
	}
	if cfg.EmailClaim == "" {
		cfg.EmailClaim = "email"
	}
	if cfg.GroupsClaim == "" {
		cfg.GroupsClaim = "groups"
	}
	if cfg.JwksCacheDuration == "" {
		cfg.JwksCacheDuration = "300s"
	}
	if cfg.JwksTimeout == "" {
		cfg.JwksTimeout = "2s"
	}
	if cfg.UserInfoTimeout == "" {
		cfg.UserInfoTimeout = "2s"
	}
	if cfg.ClockSkew == "" {
		cfg.ClockSkew = "60s"
	}
}

// ParseOIDCFilterConfig decodes the generic map[string]interface{} carried by
// FilterConfig.Options into a typed OIDCFilterConfig.
//
// It follows the same round-trip pattern used throughout this codebase:
// marshal rawOpts back to YAML bytes, then unmarshal into the concrete struct.
// This lets callers use the existing yaml.v3 tag rules without any manual
// reflection or type assertions.
func ParseOIDCFilterConfig(rawOpts interface{}) (*OIDCFilterConfig, error) {
	optsBytes, err := yaml.Marshal(rawOpts)
	if err != nil {
		return nil, fmt.Errorf("openid-connect: failed to marshal raw options: %w", err)
	}

	var cfg OIDCFilterConfig
	if err := yaml.Unmarshal(optsBytes, &cfg); err != nil {
		return nil, fmt.Errorf("openid-connect: failed to unmarshal options: %w", err)
	}

	applyOIDCDefaults(&cfg)
	return &cfg, nil
}

// RedisMetadataEnricherConfig holds the configuration for the Redis Metadata Enricher filter.
type RedisMetadataEnricherConfig struct {
	RedisService   string              `yaml:"redis_service"`
	CacheSizeMB    int                 `yaml:"cache_size_mb,omitempty"`
	CacheTimeout   string              `yaml:"cache_timeout,omitempty"`
	Variables      map[string]Variable `yaml:"variables"`
	KeyPattern     string              `yaml:"key_pattern"`
	OutputMappings []OutputMappingSpec `yaml:"output_mappings"`
}

// Variable defines how to extract and fall back a single input parameter.
type Variable struct {
	Source       string `yaml:"source"`
	Default      string `yaml:"default,omitempty"`
	RegexPattern string `yaml:"regex_pattern,omitempty"`
	JSONPath     string `yaml:"json_path,omitempty"`
}

// OutputMappingSpec defines where to extract from the returned JSON and which header to inject it into.
type OutputMappingSpec struct {
	JSONPath     string `yaml:"json_path,omitempty"`
	TargetHeader string `yaml:"target_header"`
}
