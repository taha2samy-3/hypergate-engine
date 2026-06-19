package router

import (
	"strings"

	"go.uber.org/zap"

	"github.com/taha/myprog/internal/config"
	"github.com/taha/myprog/internal/engine"
	mylogger "github.com/taha/myprog/internal/logger"
)

// EngineRouter performs high-performance, zero-allocation request routing.
type EngineRouter struct{}

// NewEngineRouter creates a new EngineRouter instance.
func NewEngineRouter() *EngineRouter {
	return &EngineRouter{}
}

// Route evaluates the RequestContext against the pre-compiled routing rules
// and returns the target chain name. It executes with 0 allocs/op.
func (r *EngineRouter) Route(ctx *engine.RequestContext) string {
	mylogger.Debug("Routing incoming request", zap.String("path", ctx.Path), zap.String("method", ctx.Method))

	activeCfg := config.GlobalConfig.Load()

	// 1. Loop through the Routes
	for i := 0; i < len(activeCfg.Router.Routes); i++ {
		route := &activeCfg.Router.Routes[i]

		// 2. Loop through the Matches (OR Logic)
		for j := 0; j < len(route.Matches); j++ {
			match := &route.Matches[j]
			matchFailed := false

			// 3. Evaluate a Single Match Block (AND Logic)

			// A. Path Matching
			if match.PathPrefix != "" {
				if !strings.HasPrefix(ctx.Path, match.PathPrefix) {
					matchFailed = true
				}
			}
			if !matchFailed && match.CompiledPathRegex != nil {
				if !match.CompiledPathRegex.MatchString(ctx.Path) {
					matchFailed = true
				}
			}

			if matchFailed {
				continue // Skip to next match block
			}

			// B. Header Matching
			if len(match.Headers) > 0 {
				for headerKey, ruleHeader := range match.Headers {
					headerVal, exists := ctx.Headers[headerKey]

					// If header does not exist, match fails
					if !exists {
						matchFailed = true
						break
					}

					// Wildcard * Match (Existence Check)
					if ruleHeader.Exact == "*" {
						continue
					}

					// Exact Match
					if ruleHeader.Exact != "" && ruleHeader.Exact != "*" {
						if headerVal != ruleHeader.Exact {
							matchFailed = true
							break
						}
					}

					// Regex Match
					if ruleHeader.CompiledRegex != nil {
						if !ruleHeader.CompiledRegex.MatchString(headerVal) {
							matchFailed = true
							break
						}
					}
				}
			}

			if matchFailed {
				continue // Skip to next match block
			}

			// 4. Target Return (If any match block succeeds)
			mylogger.Debug("Request matched route rule", zap.String("rule_name", route.Name), zap.String("target_chain", route.TargetChain))
			return route.TargetChain
		}
	}

	// 5. The Other Fallback
	mylogger.Debug("No route rule matched, falling back to 'other'", zap.String("fallback_chain", activeCfg.Router.Other))
	return activeCfg.Router.Other
}
