package config

import (
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

// ServerConfig defines the gRPC server settings.
type ServerConfig struct {
	Address                 string    `yaml:"address"`
	MaxConcurrentStreams    uint32    `yaml:"max_concurrent_streams"`
	PoolPrewarmSize         int       `yaml:"pool_prewarm_size"`
	InitialHeaderCapacity   int       `yaml:"initial_header_capacity"`
	PreallocBodyBufferBytes int       `yaml:"prealloc_body_buffer_bytes"`
	TLS                     TLSConfig `yaml:"tls"`
}