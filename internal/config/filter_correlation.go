package config

// CorrelationConfig defines the settings for request tracing and ID generation.
type CorrelationConfig struct {
	HeaderName            string `yaml:"header_name"`
	Algorithm             string `yaml:"algorithm"` // uuidv4, uuidv7, xid, ulid
	Mode                  string `yaml:"mode"`      // overwrite, if_missing
	Prefix                string `yaml:"prefix"`
	PropagateToUpstream   bool   `yaml:"propagate_to_upstream"`
	PropagateToDownstream bool   `yaml:"propagate_to_downstream"`
	InputHeaderName       string `yaml:"input_header_name"`
	ResponseHeaderName    string `yaml:"response_header_name"`
	ValidationRegex       string `yaml:"validation_regex"`
}