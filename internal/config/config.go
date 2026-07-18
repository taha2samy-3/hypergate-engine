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
}