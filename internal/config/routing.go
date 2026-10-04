package config

import (
	"regexp"

	"gopkg.in/yaml.v3"

	"github.com/taha2samy/hypergate/internal/selector"
)

// RouterConfig holds the routing rules and the fallback chain names.
type RouterConfig struct {
	Routes []RouteConfig `yaml:"routes"`
	// DefaultChains picks the fallback chain by traffic class; DefaultChain is
	// used when the class has none.
	DefaultChains DefaultChainsConfig `yaml:"default_chains,omitempty"`
	DefaultChain  string              `yaml:"default_chain"`
	Other         string              `yaml:"other"`
	// UnknownSource decides what happens to east-west requests whose caller is
	// not in the identity map: "deny" (default, 503) or "default" (continue; only
	// network selectors can match). It applies only when identity is enabled.
	UnknownSource string `yaml:"unknown_source,omitempty"`
}

// Values of RouterConfig.UnknownSource.
const (
	UnknownSourceDeny    = "deny"
	UnknownSourceDefault = "default"
)

// DefaultChainsConfig holds the fallback chain per traffic class.
type DefaultChainsConfig struct {
	NorthSouth string `yaml:"north_south,omitempty"`
	EastWest   string `yaml:"east_west,omitempty"`
}

// RouteConfig defines routing rules to a target chain.
type RouteConfig struct {
	Name        string        `yaml:"name"`
	Matches     []MatchConfig `yaml:"matches"`
	TargetChain string        `yaml:"target_chain"`
}

// MatchConfig holds conditions to evaluate an incoming request (AND logic).
type MatchConfig struct {
	// Traffic restricts the match to north_south or east_west requests (empty or any: both).
	Traffic string `yaml:"traffic,omitempty"`
	// Sources and Destinations are lists of `prefix:value` selectors (OR within a list).
	Sources      []string `yaml:"sources,omitempty"`
	Destinations []string `yaml:"destinations,omitempty"`

	CompiledTraffic      selector.Traffic `yaml:"-"`
	CompiledSources      *selector.Set    `yaml:"-"` // Compiled at startup
	CompiledDestinations *selector.Set    `yaml:"-"` // Compiled at startup

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

// Compile parses the traffic class and selectors of the match. It must be called
// before the match is used for routing; ParseBytes does it for loaded configs.
// Workload selectors are accepted only when allowWorkloads is true (identity enabled).
func (m *MatchConfig) Compile(allowWorkloads bool) error {
	t, err := selector.ParseTraffic(m.Traffic)
	if err != nil {
		return err
	}
	src, err := selector.Compile(m.Sources, selector.Source, allowWorkloads)
	if err != nil {
		return err
	}
	dst, err := selector.Compile(m.Destinations, selector.Destination, allowWorkloads)
	if err != nil {
		return err
	}
	m.CompiledTraffic, m.CompiledSources, m.CompiledDestinations = t, src, dst
	return nil
}
