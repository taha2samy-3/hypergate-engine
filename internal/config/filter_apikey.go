package config

import (
	"fmt"
)

// APIKeyStatusCheck defines the status check config for the API Key validator.
type APIKeyStatusCheck struct {
	Enabled       bool   `yaml:"enabled"`
	FieldName     string `yaml:"field_name"`
	ExpectedValue string `yaml:"expected_value"`
}

// APIKeyOutputMapping defines the target header and field mapping.
type APIKeyOutputMapping struct {
	TargetHeader string `yaml:"target_header"`
	RedisField   string `yaml:"redis_field,omitempty"`
	JSONPath     string `yaml:"json_path,omitempty"`
}

// APIKeyFilterConfig holds the configuration for the "api_key" filter.
type APIKeyFilterConfig struct {
	KeyNames        []string              `yaml:"key_names"`
	KeyInHeader     bool                  `yaml:"key_in_header"`
	KeyInQuery      bool                  `yaml:"key_in_query"`
	HideCredentials bool                  `yaml:"hide_credentials"`
	RedisService    string                `yaml:"redis_service"`
	RedisKeyPrefix  string                `yaml:"redis_key_prefix"`
	HashAlgorithm   string                `yaml:"hash_algorithm"`
	ValueFormat     string                `yaml:"value_format"` // "plain", "hash", or "json"
	Delimiter       string                `yaml:"delimiter,omitempty"`
	StatusCheck     APIKeyStatusCheck     `yaml:"status_check,omitempty"`
	OutputMappings  []APIKeyOutputMapping `yaml:"output_mappings,omitempty"`
}

// ApplyDefaults validates and defaults the config fields.
func (cfg *APIKeyFilterConfig) ApplyDefaults() error {
	if cfg.HashAlgorithm == "" {
		cfg.HashAlgorithm = "sha256"
	}
	if cfg.RedisKeyPrefix == "" {
		cfg.RedisKeyPrefix = "apikey:"
	}
	if cfg.ValueFormat == "" {
		cfg.ValueFormat = "hash"
	}
	if cfg.Delimiter == "" && cfg.ValueFormat == "plain" {
		cfg.Delimiter = "|"
	}
	if len(cfg.KeyNames) == 0 {
		cfg.KeyNames = []string{"x-api-key"}
	}
	if !cfg.KeyInHeader && !cfg.KeyInQuery {
		cfg.KeyInHeader = true
	}

	// Validate ValueFormat
	switch cfg.ValueFormat {
	case "plain", "hash", "json":
	default:
		return fmt.Errorf("invalid value_format: %s", cfg.ValueFormat)
	}

	// Validate HashAlgorithm
	switch cfg.HashAlgorithm {
	case "sha256", "md5", "none":
	default:
		return fmt.Errorf("invalid hash_algorithm: %s", cfg.HashAlgorithm)
	}

	return nil
}