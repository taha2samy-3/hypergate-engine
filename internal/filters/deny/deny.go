package deny

import (
	"fmt"
	"regexp"
	"strings"

	"go.uber.org/zap"

	"github.com/taha/myprog/internal/engine"
	mylogger "github.com/taha/myprog/internal/logger"
)

type DenyMatchConfig struct {
	PathPrefix         string            `yaml:"path_prefix"`
	PathRegex          string            `yaml:"path_regex"`
	CompiledPathRegex  *regexp.Regexp    `yaml:"-"`
	Headers            map[string]string `yaml:"headers"`
	ResponseHeaders    map[string]string `yaml:"response_headers"`
	NotHeaders         map[string]string `yaml:"not_headers"`
	NotResponseHeaders map[string]string `yaml:"not_response_headers"`
}

type DenyFilterConfig struct {
	StatusCode int32           `yaml:"status_code"`
	Body       string          `yaml:"body"`
	Match      DenyMatchConfig `yaml:"match"`
}

type DenyFilter struct {
	config        DenyFilterConfig
	hasConditions bool
}

func NewDenyFilter(cfg DenyFilterConfig) (*DenyFilter, error) {
	if cfg.Match.PathRegex != "" {
		compiled, err := regexp.Compile(cfg.Match.PathRegex)
		if err != nil {
			return nil, fmt.Errorf("deny filter: invalid path_regex %q: %w", cfg.Match.PathRegex, err)
		}
		cfg.Match.CompiledPathRegex = compiled
	}

	if cfg.StatusCode == 0 {
		cfg.StatusCode = 403
	}
	if cfg.Body == "" {
		cfg.Body = "Forbidden"
	}

	hasConditions := cfg.Match.PathPrefix != "" ||
		cfg.Match.PathRegex != "" ||
		len(cfg.Match.Headers) > 0 ||
		len(cfg.Match.ResponseHeaders) > 0 ||
		len(cfg.Match.NotHeaders) > 0 ||
		len(cfg.Match.NotResponseHeaders) > 0

	return &DenyFilter{
		config:        cfg,
		hasConditions: hasConditions,
	}, nil
}

func (f *DenyFilter) Execute(ctx *engine.RequestContext) error {
	if !f.hasConditions {
		f.applyBlock(ctx)
		mylogger.Warn("Request blocked unconditionally by deny filter",
			zap.String("path", ctx.Path),
			zap.Int32("status_code", f.config.StatusCode),
		)
		return nil
	}

	if f.config.Match.PathPrefix != "" && !strings.HasPrefix(ctx.Path, f.config.Match.PathPrefix) {
		return nil
	}

	if f.config.Match.CompiledPathRegex != nil && !f.config.Match.CompiledPathRegex.MatchString(ctx.Path) {
		return nil
	}

	for key, expected := range f.config.Match.Headers {
		actual := ctx.GetHeader(key)
		if expected == "*" {
			if actual == "" {
				return nil
			}
		} else if actual != expected {
			return nil
		}
	}

	for key, expected := range f.config.Match.ResponseHeaders {
		actual := ctx.GetDownstreamHeader(key)
		if expected == "*" {
			if actual == "" {
				return nil
			}
		} else if actual != expected {
			return nil
		}
	}

	for key, expected := range f.config.Match.NotHeaders {
		actual := ctx.GetHeader(key)
		if expected == "*" {
			if actual != "" {
				return nil
			}
		} else if actual == expected {
			return nil
		}
	}

	for key, expected := range f.config.Match.NotResponseHeaders {
		actual := ctx.GetDownstreamHeader(key)
		if expected == "*" {
			if actual != "" {
				return nil
			}
		} else if actual == expected {
			return nil
		}
	}

	f.applyBlock(ctx)
	mylogger.Warn("Request blocked by conditional deny filter",
		zap.String("path", ctx.Path),
		zap.Int32("status_code", f.config.StatusCode),
	)
	return nil
}

func (f *DenyFilter) applyBlock(ctx *engine.RequestContext) {
	ctx.Blocked = true
	ctx.ResponseStatus = f.config.StatusCode
	ctx.ResponseBody = f.config.Body
}
