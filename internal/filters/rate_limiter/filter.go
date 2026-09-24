package rate_limiter

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/taha2samy/hypergate/internal/engine"
	mylogger "github.com/taha2samy/hypergate/internal/logger"
	"go.uber.org/zap"
)

type contextKey string

const requestIDKey contextKey = "request-id"

type DescriptorEntryDef struct {
	Key   string `yaml:"key"`
	Value string `yaml:"value"` // if empy we use it as other or default

}

// YamlDescriptor represents a single descriptor definition from the YAML config.
type YamlDescriptor struct {
	// Entries holds the slice of key-value criteria that must be matched (AND logic) to apply this limit.
	// This replaces the legacy single 'Key' field to support multi-key rate-limiting dimensions.
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

// FilterOptions defines the configuration structure for the rate limiter filter.
type FilterOptions struct {
	Domain          string              `yaml:"domain"`
	Algorithm       string              `yaml:"algorithm"`
	RedisService    string              `yaml:"redis_service"`
	HeaderMappings  map[string]string   `yaml:"header_mappings"` // Maps descriptor key to request header
	ResponseHeaders ResponseHeadersOpts `yaml:"response_headers"`
	DynamicCost     DynamicCostOpts     `yaml:"dynamic_cost"`
	Descriptors     []YamlDescriptor    `yaml:"descriptors"`
}

// ApplyDefaults enforces strict struct-level defaults and optimizes maps for zero-allocation runtime lookup.
func (opts *FilterOptions) ApplyDefaults() {
	if opts.DynamicCost.SourceHeader != "" {
		opts.DynamicCost.SourceHeader = strings.ToLower(opts.DynamicCost.SourceHeader)
	}
	// Every request must consume at least one unit; max_allowed_cost <= 0 means "no cap".
	if opts.DynamicCost.DefaultFallbackCost < 1 {
		opts.DynamicCost.DefaultFallbackCost = 1
	}
	if opts.ResponseHeaders.LimitHeader == "" {
		opts.ResponseHeaders.LimitHeader = "ratelimit-limit"
	} else {
		opts.ResponseHeaders.LimitHeader = strings.ToLower(opts.ResponseHeaders.LimitHeader)
	}
	if opts.ResponseHeaders.RemainingHeader == "" {
		opts.ResponseHeaders.RemainingHeader = "ratelimit-remaining"
	} else {
		opts.ResponseHeaders.RemainingHeader = strings.ToLower(opts.ResponseHeaders.RemainingHeader)
	}
	if opts.ResponseHeaders.ResetHeader == "" {
		opts.ResponseHeaders.ResetHeader = "ratelimit-reset"
	} else {
		opts.ResponseHeaders.ResetHeader = strings.ToLower(opts.ResponseHeaders.ResetHeader)
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

// requestCost returns the quota units consumed by this request. A cost below 1 would
// let a caller refund or skip quota (INCRBY with a negative value), so missing,
// malformed, zero and negative values all fall back to the configured default.
func (f *RateLimiterFilter) requestCost(ctx *engine.RequestContext) int64 {
	dc := f.options.DynamicCost
	if !dc.Enabled {
		return 1
	}
	cost := dc.DefaultFallbackCost
	if parsed, err := strconv.ParseInt(ctx.GetHeader(dc.SourceHeader), 10, 64); err == nil && parsed >= 1 {
		cost = parsed
	}
	if dc.MaxAllowedCost > 0 && cost > dc.MaxAllowedCost {
		cost = dc.MaxAllowedCost
	}
	return cost
}

// RateLimiterFilter is the entry-point filter of the embedded rate-limiting system.
// It implements the engine.Filter interface.
type RateLimiterFilter struct {
	name     string // Holds the configured filter name from the chain
	options  FilterOptions
	executor RateLimitExecutor // Strategy Interface to be resolved at boot
}

// NewRateLimiterFilter creates a new initialized rate limiter filter.
func NewRateLimiterFilter(name string, opts FilterOptions, executor RateLimitExecutor) *RateLimiterFilter {
	opts.ApplyDefaults()
	return &RateLimiterFilter{
		name:     name,
		options:  opts,
		executor: executor,
	}
}

// Execute performs runtime header extraction, invokes the limit check, and applies headers/blocks.
func (f *RateLimiterFilter) Execute(ctx *engine.RequestContext) error {
	// Pre-allocate slice capacity based on configured descriptors to minimize runtime slice grow allocations.
	descriptors := make([]DescriptorEntry, 0, len(f.options.Descriptors)*2)

	// Track already extracted keys using a stack-allocated map to prevent duplicate header lookups
	// in the same request lifecycle when keys are shared across multiple composite descriptors.
	extractedKeys := make(map[string]bool, len(f.options.Descriptors)*2)

	// Dynamically extract the client's runtime values for all active keys defined in the policy
	for _, desc := range f.options.Descriptors {
		for _, entry := range desc.Entries {
			// Skip if this key has already been extracted from the request headers
			if extractedKeys[entry.Key] {
				continue
			}
			extractedKeys[entry.Key] = true

			var val string

			// Rule A (Explicit Mapping)
			if headerName, ok := f.options.HeaderMappings[entry.Key]; ok {
				val = ctx.GetHeader(headerName)
			} else {
				// Rule B (Implicit/Default Mapping)
				val = ctx.GetHeader(entry.Key)
			}

			// Rule C (Client IP): resolved from Envoy's peer address and trusted proxy
			// hops only. Raw X-Forwarded-For is client-controlled and must never be
			// used as a rate-limit key, so an explicit header mapping is ignored here.
			if entry.Key == "ip" || entry.Key == "client_ip" || entry.Key == "remote_ip" {
				val = ctx.ClientIP
			}

			// Rule D (Absolute Fallback)
			if val == "" {
				val = "default"
			}

			descriptors = append(descriptors, DescriptorEntry{Key: entry.Key, Value: val})
		}
	}

	cost := f.requestCost(ctx)

	// Execution Dispatch
	// Use propagated context with a defensive check to default to context.Background() if nil
	evalCtx := ctx.Ctx
	if evalCtx == nil {
		evalCtx = context.Background()
	}
	if reqID := ctx.GetHeader("x-request-id"); reqID != "" {
		evalCtx = context.WithValue(evalCtx, requestIDKey, reqID)
	}
	result, err := f.executor.Evaluate(evalCtx, descriptors, cost)
	if err != nil {
		mylogger.Error("Rate limit evaluation failed",
			zap.String("filter", f.name),
			zap.Error(err),
		)
		// Return the error to abort the filter chain safely
		return err
	}

	// Downstream Response Headers Injection
	if f.options.ResponseHeaders.Enabled {
		ctx.SetHeaderDownstream(f.options.ResponseHeaders.LimitHeader, fmt.Sprintf("%d", result.Limit))
		ctx.SetHeaderDownstream(f.options.ResponseHeaders.RemainingHeader, fmt.Sprintf("%d", result.LimitRemaining))

		// Reset header usually represents epoch timestamp or seconds remaining.
		// Using seconds remaining as a straightforward string representation.
		resetSeconds := int64(math.Ceil(result.ResetDuration.Seconds()))
		ctx.SetHeaderDownstream(f.options.ResponseHeaders.ResetHeader, fmt.Sprintf("%d", resetSeconds))
	}

	// Decision Enforcement
	if result.Blocked {
		ctx.Blocked = true
		ctx.ResponseStatus = 429
		ctx.ResponseBody = "Too Many Requests"

		mylogger.Debug("Rate limit exceeded, blocking request",
			zap.String("filter", f.name),
		)
	}

	return nil
}
