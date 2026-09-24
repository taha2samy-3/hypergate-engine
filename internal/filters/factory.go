package filters

import (
	"fmt"

	"github.com/taha2samy/hypergate/internal/config"
	"github.com/taha2samy/hypergate/internal/engine"
	"github.com/taha2samy/hypergate/internal/filters/api_key"
	"github.com/taha2samy/hypergate/internal/filters/correlation_id"
	"github.com/taha2samy/hypergate/internal/filters/deny"
	"github.com/taha2samy/hypergate/internal/filters/external_auth"
	"github.com/taha2samy/hypergate/internal/filters/firewall"
	"github.com/taha2samy/hypergate/internal/filters/header_modifier"
	"github.com/taha2samy/hypergate/internal/filters/jwt_auth"
	"github.com/taha2samy/hypergate/internal/filters/rate_limiter"
	"github.com/taha2samy/hypergate/internal/filters/redis_metadata_enricher"
	"github.com/taha2samy/hypergate/internal/redis"
	"gopkg.in/yaml.v3"
)

// RedisLookup resolves a configured Redis service name to its client.
type RedisLookup func(service string) (redis.Client, bool)

func resolveRedis(lookup RedisLookup, filterType, service string) (redis.Client, error) {
	if lookup == nil {
		return nil, fmt.Errorf("%s: no redis services configured", filterType)
	}
	client, ok := lookup(service)
	if !ok {
		return nil, fmt.Errorf("%s: configured redis service %q not found", filterType, service)
	}
	return client, nil
}

// CreateFilter instantiates the correct polymorphic filter instance based on the configuration type.
// Redis-backed filters resolve their client through lookup.
func CreateFilter(filterType string, rawOptions interface{}, lookup RedisLookup) (engine.Filter, error) {
	optsBytes, err := yaml.Marshal(rawOptions)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal raw options for filter type %q: %w", filterType, err)
	}

	switch filterType {
	case "api_key":
		var cfg config.APIKeyFilterConfig
		if err := yaml.Unmarshal(optsBytes, &cfg); err != nil {
			return nil, fmt.Errorf("failed to unmarshal config for api_key: %w", err)
		}

		if err := cfg.ApplyDefaults(); err != nil {
			return nil, fmt.Errorf("failed to apply defaults/validate config for api_key: %w", err)
		}

		client, err := resolveRedis(lookup, filterType, cfg.RedisService)
		if err != nil {
			return nil, err
		}
		return api_key.NewAPIKeyFilter("api_key", cfg, client), nil

	case "redis_metadata_enricher":
		var cfg config.RedisMetadataEnricherConfig
		if err := yaml.Unmarshal(optsBytes, &cfg); err != nil {
			return nil, fmt.Errorf("failed to unmarshal config for redis_metadata_enricher: %w", err)
		}

		client, err := resolveRedis(lookup, filterType, cfg.RedisService)
		if err != nil {
			return nil, err
		}

		return redis_metadata_enricher.NewRedisMetadataEnricherFilter("redis_metadata_enricher", cfg, client), nil

	case "embedded_rate_limiter":
		var cfg rate_limiter.FilterOptions
		if err := yaml.Unmarshal(optsBytes, &cfg); err != nil {
			return nil, fmt.Errorf("failed to unmarshal config for embedded_rate_limiter: %w", err)
		}

		// Enforce default values for response headers
		cfg.ApplyDefaults()

		client, err := resolveRedis(lookup, filterType, cfg.RedisService)
		if err != nil {
			return nil, err
		}

		// Compile and instantiate the selected polymorphic strategy
		executor, err := rate_limiter.ResolveExecutor(cfg.Algorithm, client, cfg)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve rate limiter executor: %w", err)
		}

		// Construct and return the RateLimiterFilter instance
		return rate_limiter.NewRateLimiterFilter("embedded_rate_limiter", cfg, executor), nil

	case "header_modifier":
		var cfg header_modifier.HeaderModifierConfig
		if err := yaml.Unmarshal(optsBytes, &cfg); err != nil {
			return nil, fmt.Errorf("failed to unmarshal config for header_modifier: %w", err)
		}
		return header_modifier.NewHeaderModifierFilter(cfg), nil

	case "deny":
		var cfg deny.DenyFilterConfig
		if err := yaml.Unmarshal(optsBytes, &cfg); err != nil {
			return nil, fmt.Errorf("failed to unmarshal config for deny: %w", err)
		}
		f, err := deny.NewDenyFilter(cfg)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize deny filter: %w", err)
		}
		return f, nil

	case "correlation_id":
		cfg := correlation_id.CorrelationConfig{
			PropagateToUpstream:   true,
			PropagateToDownstream: true,
		}
		if err := yaml.Unmarshal(optsBytes, &cfg); err != nil {
			return nil, fmt.Errorf("failed to unmarshal config for correlation_id: %w", err)
		}
		f, err := correlation_id.NewCorrelationFilter(cfg)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize correlation_id filter: %w", err)
		}
		return f, nil

	case "external_auth":
		cfg, err := config.ParseExternalAuthConfig(rawOptions)
		if err != nil {
			return nil, fmt.Errorf("failed to parse config for external_auth: %w", err)
		}
		return external_auth.NewExternalAuthFilter(cfg)

	case "firewall":
		cfg, err := config.ParseFirewallFilterConfig(rawOptions)
		if err != nil {
			return nil, fmt.Errorf("failed to parse config for firewall: %w", err)
		}
		return firewall.NewFirewallFilter(cfg)

	case "jwt_auth":
		var cfg jwt_auth.Config
		if err := yaml.Unmarshal(optsBytes, &cfg); err != nil {
			return nil, fmt.Errorf("failed to unmarshal config for jwt_auth: %w", err)
		}
		return jwt_auth.NewFilter(cfg)

	default:
		return nil, fmt.Errorf("unknown filter type: %s", filterType)
	}
}
