package config

import (
	"github.com/taha/myprog/internal/logger"
)

// TelemetryConfig defines observability settings like logging.
type TelemetryConfig struct {
	Logging logger.LoggingConfig `yaml:"logging"`
}

// ServerConfig defines the gRPC server settings.
type ServerConfig struct {
	Address              string `yaml:"address"`
	MaxConcurrentStreams uint32 `yaml:"max_concurrent_streams"`
}