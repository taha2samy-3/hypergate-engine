package config

import (
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// Firewall Filter Configuration
// ---------------------------------------------------------------------------

// FirewallFilterConfig defines the configuration parameters for the firewall filter.
type FirewallFilterConfig struct {
	Protocol        string           `yaml:"protocol"`
	SocketPath      string           `yaml:"socket_path"`
	Timeout         string           `yaml:"timeout"`
	TimeoutDuration time.Duration    `yaml:"-"`
	InspectBody     bool             `yaml:"inspect_body"`
	MaxBodySizeKB   int32            `yaml:"max_body_size_kb"`
	ForwardHeaders  []string         `yaml:"forward_headers"`
	OnSuccess       AuthSuccessRules `yaml:"on_success"`
	OnFailure       AuthFailureRules `yaml:"on_failure"`
}

// ParseFirewallFilterConfig parses raw interface options into a typed FirewallFilterConfig.
func ParseFirewallFilterConfig(raw interface{}) (*FirewallFilterConfig, error) {
	optsBytes, err := yaml.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("firewall: failed to marshal raw options: %w", err)
	}

	var cfg FirewallFilterConfig
	if err := yaml.Unmarshal(optsBytes, &cfg); err != nil {
		return nil, fmt.Errorf("firewall: failed to unmarshal options: %w", err)
	}

	// Normalize and validate protocol
	cfg.Protocol = strings.ToLower(cfg.Protocol)
	if cfg.Protocol == "" {
		cfg.Protocol = "grpc"
	} else if cfg.Protocol != "http" && cfg.Protocol != "grpc" {
		return nil, fmt.Errorf("firewall: unsupported protocol %q (must be 'http' or 'grpc')", cfg.Protocol)
	}

	// Apply default values
	if cfg.Timeout == "" {
		cfg.Timeout = "2s"
	}

	d, err := time.ParseDuration(cfg.Timeout)
	if err != nil {
		cfg.TimeoutDuration = 2 * time.Second
	} else {
		cfg.TimeoutDuration = d
	}

	if cfg.MaxBodySizeKB <= 0 {
		cfg.MaxBodySizeKB = 1024
	}

	return &cfg, nil
}
