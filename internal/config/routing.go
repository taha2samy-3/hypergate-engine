package config

import (
	"regexp"

	"gopkg.in/yaml.v3"
)

// RouterConfig holds the routing rules and the fallback chain name.
type RouterConfig struct {
	Routes       []RouteConfig `yaml:"routes"`
	DefaultChain string        `yaml:"default_chain"`
	Other        string        `yaml:"other"`
}

// RouteConfig defines routing rules to a target chain.
type RouteConfig struct {
	Name        string        `yaml:"name"`
	Matches     []MatchConfig `yaml:"matches"`
	TargetChain string        `yaml:"target_chain"`
}

// MatchConfig holds conditions to evaluate an incoming request (AND logic).
type MatchConfig struct {
	PathPrefix        string                       `yaml:"path_prefix"`
	PathRegexPattern  string                       `yaml:"path_regex_pattern"`
	CompiledPathRegex *regexp.Regexp               `yaml:"-"` // Pre-compiled at startup
	Headers           map[string]HeaderMatchConfig `yaml:"headers"`
}

// HeaderMatchConfig holds exact or regex matching criteria for a header.
type HeaderMatchConfig struct {
	Exact         string         `yaml:"exact"`
	RegexPattern  string         `yaml:"regex_pattern"`
	CompiledRegex *regexp.Regexp `yaml:"-"` // Pre-compiled at startup
}

// UnmarshalYAML implements custom unmarshaling to support shorthand string syntax for headers.
func (h *HeaderMatchConfig) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		h.Exact = value.Value
		return nil
	}
	type alias HeaderMatchConfig
	var a alias
	if err := value.Decode(&a); err != nil {
		return err
	}
	*h = HeaderMatchConfig(a)
	return nil
}