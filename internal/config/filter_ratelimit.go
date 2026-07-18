package config

import (
	"strings"
)

// DescriptorEntryDef defines a descriptor key/value criteria.
type DescriptorEntryDef struct {
	Key   string `yaml:"key"`
	Value string `yaml:"value"` // if empty we use it as other or default
}

// YamlDescriptor represents a single descriptor definition from the YAML config.
type YamlDescriptor struct {
	// Entries holds the slice of key-value criteria that must be matched (AND logic) to apply this limit.
	Entries                    []DescriptorEntryDef `yaml:"entries"`
	Limit                      uint32               `yaml:"limit"`
	Unit                       string               `yaml:"unit"` // e.g., second, minute, hour, day, week, month, year
	ShadowMode                 bool                 `yaml:"shadow_mode"`
	StopIncrementWhenOverlimit bool                 `yaml:"stop_increment_when_overlimit"`
	FailOpen                   bool                 `yaml:"fail_open"`
	ShareThresholdPattern      string               `yaml:"share_threshold_pattern"`
	MaxTokens                  float64              `yaml:"max_tokens"`      // For Token Bucket
	FillRate                   float64              `yaml:"fill_rate"`       // For Token Bucket (tokens per second)
	BucketCapacity             uint32               `yaml:"bucket_capacity"` // For Leaky Bucket
	LeakRate                   float64              `yaml:"leak_rate"`       // For Leaky Bucket (requests per second/unit)
}

// ResponseHeadersOpts defines configuration for injecting rate limit headers downstream.
type ResponseHeadersOpts struct {
	Enabled         bool   `yaml:"enabled"`
	LimitHeader     string `yaml:"limit_header"`     // Default: "RateLimit-Limit"
	RemainingHeader string `yaml:"remaining_header"` // Default: "RateLimit-Remaining"
	ResetHeader     string `yaml:"reset_header"`     // Default: "RateLimit-Reset"
}

// DynamicCostOpts defines configuration for extracting dynamic cost from headers.
type DynamicCostOpts struct {
	Enabled             bool   `yaml:"enabled"`
	SourceHeader        string `yaml:"source_header"`
	DefaultFallbackCost int64  `yaml:"default_fallback_cost"`
	MaxAllowedCost      int64  `yaml:"max_allowed_cost"`
}

// RateLimitConfig defines the configuration structure for the rate limiter filter.
type RateLimitConfig struct {
	Domain          string              `yaml:"domain"`
	Algorithm       string              `yaml:"algorithm"`
	RedisService    string              `yaml:"redis_service"`
	HeaderMappings  map[string]string   `yaml:"header_mappings"` // Maps descriptor key to request header
	ResponseHeaders ResponseHeadersOpts `yaml:"response_headers"`
	DynamicCost     DynamicCostOpts     `yaml:"dynamic_cost"`
	Descriptors     []YamlDescriptor    `yaml:"descriptors"`
}

// ApplyDefaults enforces strict struct-level defaults and optimizes maps for zero-allocation runtime lookup.
func (opts *RateLimitConfig) ApplyDefaults() {
	if opts.ResponseHeaders.LimitHeader == "" {
		opts.ResponseHeaders.LimitHeader = "RateLimit-Limit"
	}
	if opts.ResponseHeaders.RemainingHeader == "" {
		opts.ResponseHeaders.RemainingHeader = "RateLimit-Remaining"
	}
	if opts.ResponseHeaders.ResetHeader == "" {
		opts.ResponseHeaders.ResetHeader = "RateLimit-Reset"
	}

	// Pre-lowercase all keys in HeaderMappings to ensure zero dynamic heap allocations
	// during runtime lookup when calling context helpers or looking up maps.
	if opts.HeaderMappings != nil {
		lowerMappings := make(map[string]string, len(opts.HeaderMappings))
		for k, v := range opts.HeaderMappings {
			lowerMappings[strings.ToLower(k)] = strings.ToLower(v)
		}
		opts.HeaderMappings = lowerMappings
	}

	// Pre-lowercase all keys and values defined inside composite descriptors.
	// This ensures exact, case-insensitive matching during the hot execution path.
	for i := range opts.Descriptors {
		for j := range opts.Descriptors[i].Entries {
			opts.Descriptors[i].Entries[j].Key = strings.ToLower(opts.Descriptors[i].Entries[j].Key)
			if opts.Descriptors[i].Entries[j].Value != "" {
				opts.Descriptors[i].Entries[j].Value = strings.ToLower(opts.Descriptors[i].Entries[j].Value)
			}
		}
	}
}