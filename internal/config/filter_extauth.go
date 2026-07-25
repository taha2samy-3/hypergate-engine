package config

import (
	"fmt"
	"strings"
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

	// Normalize and validate protocol
	cfg.Protocol = strings.ToLower(cfg.Protocol)
	if cfg.Protocol == "" {
		cfg.Protocol = "http"
	} else if cfg.Protocol != "http" && cfg.Protocol != "grpc" {
		return nil, fmt.Errorf("external_auth: unsupported protocol %q (must be 'http' or 'grpc')", cfg.Protocol)
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

	// Pre-lowercase all header slices for zero-allocation hot-path lookup.
	for i := range cfg.ForwardHeaders {
		cfg.ForwardHeaders[i] = strings.ToLower(cfg.ForwardHeaders[i])
	}
	for i := range cfg.OnSuccess.UpstreamHeadersToAdd {
		cfg.OnSuccess.UpstreamHeadersToAdd[i] = strings.ToLower(cfg.OnSuccess.UpstreamHeadersToAdd[i])
	}
	for i := range cfg.OnSuccess.UpstreamHeadersToRemove {
		cfg.OnSuccess.UpstreamHeadersToRemove[i] = strings.ToLower(cfg.OnSuccess.UpstreamHeadersToRemove[i])
	}
	for i := range cfg.OnFailure.DownstreamPassThroughHeaders {
		cfg.OnFailure.DownstreamPassThroughHeaders[i] = strings.ToLower(cfg.OnFailure.DownstreamPassThroughHeaders[i])
	}

	return &cfg, nil
}