package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// External Auth Filter Configuration
// ---------------------------------------------------------------------------

// AuthSuccessRules defines what actions to take when external auth succeeds.
type AuthSuccessRules struct {
	UpstreamHeadersToAdd    []string `yaml:"upstream_headers_to_add"`
	UpstreamHeadersToRemove []string `yaml:"upstream_headers_to_remove"`
}

// AuthFailureRules defines what actions to take when external auth fails.
type AuthFailureRules struct {
	DownstreamPassThroughHeaders []string `yaml:"downstream_pass_through_headers"`
}

// ExternalAuthConfig defines the options for the external_auth filter.
type ExternalAuthConfig struct {
	Protocol        string           `yaml:"protocol"`
	SocketPath      string           `yaml:"socket_path"`
	Timeout         string           `yaml:"timeout"`
	TimeoutDuration time.Duration    `yaml:"-"`
	ForwardHeaders  []string         `yaml:"forward_headers"`
	OnSuccess       AuthSuccessRules `yaml:"on_success"`
	OnFailure       AuthFailureRules `yaml:"on_failure"`
}

// ParseExternalAuthConfig parses raw options into a typed ExternalAuthConfig.
func ParseExternalAuthConfig(raw interface{}) (*ExternalAuthConfig, error) {
	optsBytes, err := yaml.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("external_auth: failed to marshal raw options: %w", err)
	}

	var cfg ExternalAuthConfig
	if err := yaml.Unmarshal(optsBytes, &cfg); err != nil {
		return nil, fmt.Errorf("external_auth: failed to unmarshal options: %w", err)
	}

	// Apply default values
	if cfg.Protocol == "" {
		cfg.Protocol = "http"
	}

	if cfg.Timeout == "" {
		cfg.Timeout = "2s"
	}

	d, err := time.ParseDuration(cfg.Timeout)
	if err != nil {
		// Fallback to 2s if empty or invalid
		cfg.TimeoutDuration = 2 * time.Second
	} else {
		cfg.TimeoutDuration = d
	}

	return &cfg, nil
}