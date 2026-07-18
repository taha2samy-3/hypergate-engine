package config

// FilterConfig defines a single filter configuration.
type FilterConfig struct {
	Type    string                 `yaml:"type"`
	Options map[string]interface{} `yaml:"options"`
}

// Chain defines a filter chain configuration as a sequence of filters.
type Chain []FilterConfig