package config

// HeaderOptions defines custom header modifications.
type HeaderOptions struct {
	Add      map[string]string `yaml:"add"`
	Override map[string]string `yaml:"override"`
	Remove   []string          `yaml:"remove"`
}

// HeaderModifierConfig holds the configuration for the header_modifier filter.
type HeaderModifierConfig struct {
	// Upstream holds modifications applied to the upstream (backend-bound) request.
	Upstream HeaderOptions `yaml:"upstream"`

	// Downstream holds modifications applied to the downstream (client-bound) response.
	Downstream HeaderOptions `yaml:"downstream"`

	// Top-level shorthands for upstream modifications (backward compatibility)
	Add      map[string]string `yaml:"add"`
	Override map[string]string `yaml:"override"`
	Remove   []string          `yaml:"remove"`
}