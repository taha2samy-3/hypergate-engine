package router

import (
	"strings"

	"go.uber.org/zap"

	"github.com/taha2samy/hypergate/internal/config"
	"github.com/taha2samy/hypergate/internal/engine"
	mylogger "github.com/taha2samy/hypergate/internal/logger"
	"github.com/taha2samy/hypergate/internal/selector"
)

type EngineRouter struct{}

func NewEngineRouter() *EngineRouter {
	return &EngineRouter{}
}

// Route matches the request against the globally active config.
func (r *EngineRouter) Route(ctx *engine.RequestContext) string {
	activeCfg := config.GlobalConfig.Load()
	if activeCfg == nil {
		return ""
	}
	return r.RouteWith(&activeCfg.Router, ctx)
}

// RouteWith matches the request against rc and returns the target chain name.
// An empty result means no route and no default chain are configured.
func (r *EngineRouter) RouteWith(rc *config.RouterConfig, ctx *engine.RequestContext) string {
	mylogger.Debug("Routing incoming request", zap.String("path", ctx.Path), zap.String("method", ctx.Method))

	for i := 0; i < len(rc.Routes); i++ {
		route := &rc.Routes[i]
		for j := 0; j < len(route.Matches); j++ {
			if matches(&route.Matches[j], ctx) {
				mylogger.Debug("Request matched route rule",
					zap.String("rule_name", route.Name),
					zap.String("target_chain", route.TargetChain),
					zap.Stringer("traffic", ctx.Traffic),
					zap.String("client_ip", ctx.ClientIP),
					zap.String("destination", ctx.DestinationAddress),
					zap.String("host", ctx.Host),
				)
				return route.TargetChain
			}
		}
	}

	fallbackChain := defaultChain(rc, ctx.Traffic)
	mylogger.Debug("No route rule matched, using the default chain",
		zap.Stringer("traffic", ctx.Traffic), zap.String("default_chain", fallbackChain))
	return fallbackChain
}

// defaultChain returns the fallback for the traffic class, then default_chain, then other.
func defaultChain(rc *config.RouterConfig, traffic selector.Traffic) string {
	switch traffic {
	case selector.TrafficNorthSouth:
		if rc.DefaultChains.NorthSouth != "" {
			return rc.DefaultChains.NorthSouth
		}
	case selector.TrafficEastWest:
		if rc.DefaultChains.EastWest != "" {
			return rc.DefaultChains.EastWest
		}
	}
	if rc.DefaultChain != "" {
		return rc.DefaultChain
	}
	return rc.Other
}

// matches reports whether every condition of one match entry holds.
func matches(match *config.MatchConfig, ctx *engine.RequestContext) bool {
	if !trafficMatches(match, ctx.Traffic) {
		return false
	}
	if match.PathPrefix != "" && !strings.HasPrefix(ctx.Path, match.PathPrefix) {
		return false
	}
	if match.CompiledPathRegex != nil && !match.CompiledPathRegex.MatchString(ctx.Path) {
		return false
	}
	// Selectors that were never compiled must not widen the match.
	if len(match.Sources) > 0 && match.CompiledSources == nil ||
		len(match.Destinations) > 0 && match.CompiledDestinations == nil {
		return false
	}
	// "external" means not a workload; an east-west caller that is merely
	// missing from the identity map must not count as external.
	externalOK := ctx.SourceWorkload == nil && ctx.Traffic != selector.TrafficEastWest
	if !match.CompiledSources.MatchSource(ctx.ClientAddr, ctx.SourceWorkload, externalOK) {
		return false
	}
	if !match.CompiledDestinations.MatchDestination(ctx.DestinationAddr, ctx.Host, ctx.DestinationServices) {
		return false
	}

	for headerKey, ruleHeader := range match.Headers {
		headerVal, exists := ctx.Headers[headerKey]
		if !exists {
			return false
		}
		if ruleHeader.Exact == "*" {
			continue
		}
		if ruleHeader.Exact != "" && headerVal != ruleHeader.Exact {
			return false
		}
		if ruleHeader.CompiledRegex != nil && !ruleHeader.CompiledRegex.MatchString(headerVal) {
			return false
		}
	}
	return true
}

func trafficMatches(match *config.MatchConfig, traffic selector.Traffic) bool {
	if match.CompiledTraffic != selector.TrafficAny {
		return match.CompiledTraffic == traffic
	}
	// A traffic value that was never compiled must not widen the match.
	return match.Traffic == "" || strings.EqualFold(match.Traffic, "any")
}
