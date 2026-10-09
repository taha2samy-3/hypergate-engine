package config

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
