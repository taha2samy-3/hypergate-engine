package config

import (
	"sync/atomic"
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

	// ChainSettings bound each chain's work (timeout, concurrency), by chain name.
	ChainSettings map[string]ChainSettings `yaml:"chain_settings,omitempty"`

	// Identity connects the engine to the operator's workload identity stream.
	Identity IdentityConfig `yaml:"identity,omitempty"`
}

// IdentityConfig configures the workload identity client. It is read at start-up;
// changing it requires a restart.
type IdentityConfig struct {
	Enabled bool `yaml:"enabled"`
	// Address of the operator's identity Service, host:port.
	Address string `yaml:"address,omitempty"`
	// CAFile verifies the operator's certificate.
	CAFile string `yaml:"ca_file,omitempty"`
	// ServerName overrides the certificate name checked (default: host of Address).
	ServerName string `yaml:"server_name,omitempty"`
	// TokenFile is the projected ServiceAccount token (audience hypergate-identity).
	TokenFile string `yaml:"token_file,omitempty"`
	// Insecure dials without TLS (development only).
	Insecure bool `yaml:"insecure,omitempty"`
}
