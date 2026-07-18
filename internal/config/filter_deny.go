package config

// DenyMatchConfig holds criteria to match a request for blocking.
type DenyMatchConfig struct {
	PathPrefix         string            `yaml:"path_prefix"`
	PathRegex          string            `yaml:"path_regex"`
	Headers            map[string]string `yaml:"headers"`
	ResponseHeaders    map[string]string `yaml:"response_headers"`
	NotHeaders         map[string]string `yaml:"not_headers"`
	NotResponseHeaders map[string]string `yaml:"not_response_headers"`
}

// DenyFilterConfig defines the options for the "deny" filter.
type DenyFilterConfig struct {
	StatusCode int32           `yaml:"status_code"`
	Body       string          `yaml:"body"`
	Match      DenyMatchConfig `yaml:"match"`
}