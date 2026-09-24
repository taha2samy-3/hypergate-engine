package router

import (
	"strings"

	"go.uber.org/zap"

	"github.com/taha2samy/hypergate/internal/config"
	"github.com/taha2samy/hypergate/internal/engine"
	mylogger "github.com/taha2samy/hypergate/internal/logger"
)

type EngineRouter struct{}

func NewEngineRouter() *EngineRouter {
	return &EngineRouter{}
}

func (r *EngineRouter) Route(ctx *engine.RequestContext) string {
	mylogger.Debug("Routing incoming request", zap.String("path", ctx.Path), zap.String("method", ctx.Method))

	activeCfg := config.GlobalConfig.Load()

	for i := 0; i < len(activeCfg.Router.Routes); i++ {
		route := &activeCfg.Router.Routes[i]

		for j := 0; j < len(route.Matches); j++ {
			match := &route.Matches[j]
			matchFailed := false

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
				continue
			}

			if len(match.Headers) > 0 {
				for headerKey, ruleHeader := range match.Headers {
					headerVal, exists := ctx.Headers[headerKey]

					if !exists {
						matchFailed = true
						break
					}

					if ruleHeader.Exact == "*" {
						continue
					}

					if ruleHeader.Exact != "" && ruleHeader.Exact != "*" {
						if headerVal != ruleHeader.Exact {
							matchFailed = true
							break
						}
					}

					if ruleHeader.CompiledRegex != nil {
						if !ruleHeader.CompiledRegex.MatchString(headerVal) {
							matchFailed = true
							break
						}
					}
				}
			}

			if matchFailed {
				continue
			}

			mylogger.Debug("Request matched route rule", zap.String("rule_name", route.Name), zap.String("target_chain", route.TargetChain))
			return route.TargetChain
		}
	}

	fallbackChain := activeCfg.Router.DefaultChain
	if fallbackChain == "" {
		fallbackChain = activeCfg.Router.Other
	}

	mylogger.Debug("No route rule matched, falling back to 'default_chain'", zap.String("default_chain", fallbackChain))
	return fallbackChain
}
