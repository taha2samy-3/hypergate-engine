package config

// FilterConfig defines a single filter configuration.
type FilterConfig struct {
	// Name labels the filter in metrics and logs (the operator sets "<Kind>/<name>").
	// Optional; it does not affect which filter instances are shared.
	Name    string                 `yaml:"name,omitempty"`
	Type    string                 `yaml:"type"`
	Options map[string]interface{} `yaml:"options"`
}

// Chain defines a filter chain configuration as a sequence of filters.
type Chain []FilterConfig
