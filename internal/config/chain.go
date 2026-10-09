package config

import (
	"fmt"
	"time"
)

// FilterConfig defines a single filter configuration.
type FilterConfig struct {
	// Name labels the filter in metrics and logs (the operator sets "<Kind>/<name>").
	// Optional; it does not affect which filter instances are shared.
	Name string `yaml:"name,omitempty"`
	// Audit records the filter's denials and failures (metrics, logs) without
	// enforcing them, so a new policy can be tried on live traffic.
	Audit   bool                   `yaml:"audit,omitempty"`
	Type    string                 `yaml:"type"`
	Options map[string]interface{} `yaml:"options"`
}

// Chain defines a filter chain configuration as a sequence of filters.
type Chain []FilterConfig

// Values of ChainSettings.OnTimeout and OnOverload.
const (
	LimitActionDeny  = "deny"
	LimitActionAllow = "allow"
)

// ChainSettings bound the work one chain may cause on an engine, which all
// workloads of a node share.
type ChainSettings struct {
	// Timeout is a deadline on the chain's work for each ext_proc message.
	// Filters that call Redis or sidecars stop at the deadline.
	Timeout string `yaml:"timeout,omitempty"`
	// MaxConcurrency limits how many requests may run this chain's filters at the
	// same time on one engine (0 = no limit).
	MaxConcurrency int `yaml:"max_concurrency,omitempty"`
	// OnTimeout is "deny" (default, 503) or "allow" (continue without the
	// remaining filters).
	OnTimeout string `yaml:"on_timeout,omitempty"`
	// OnOverload is "deny" (default, 503 with Retry-After) or "allow" (skip the
	// chain for this request).
	OnOverload string `yaml:"on_overload,omitempty"`

	TimeoutDuration time.Duration `yaml:"-"`
}

func (c *ChainSettings) validate(name string) error {
	if c.Timeout != "" {
		d, err := time.ParseDuration(c.Timeout)
		if err != nil || d <= 0 {
			return fmt.Errorf("chain_settings.%s.timeout: invalid duration %q", name, c.Timeout)
		}
		c.TimeoutDuration = d
	}
	if c.MaxConcurrency < 0 {
		return fmt.Errorf("chain_settings.%s.max_concurrency must be >= 0", name)
	}
	for field, v := range map[string]*string{"on_timeout": &c.OnTimeout, "on_overload": &c.OnOverload} {
		switch *v {
		case "":
			*v = LimitActionDeny
		case LimitActionDeny, LimitActionAllow:
		default:
			return fmt.Errorf("chain_settings.%s.%s must be %q or %q", name, field, LimitActionDeny, LimitActionAllow)
		}
	}
	return nil
}
