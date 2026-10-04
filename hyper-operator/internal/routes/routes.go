// Package routes validates HyperRoutes and compiles them into engine route
// configuration. The admission webhook and the compiler share it, so a route the
// webhook accepts is a route the compiler (and the engine) accepts.
//
// The operator only reads routing intent from HyperRoutes. It never creates or
// modifies Gateway API HTTPRoutes: those belong to the application developer.
package routes

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	hyperv1alpha1 "github.com/taha2samy/hypergate/hyper-operator/api/v1alpha1"
	"github.com/taha2samy/hypergate/internal/config"
	"github.com/taha2samy/hypergate/internal/selector"
)

// Validate reports every problem of a HyperRoute spec, joined into one error.
func Validate(spec *hyperv1alpha1.HyperRouteSpec) error {
	_, err := Compile(spec)
	return err
}

// Compile converts the matches of a HyperRoute into engine match entries. It
// returns an error, listing every problem, when any match is invalid.
func Compile(spec *hyperv1alpha1.HyperRouteSpec) ([]config.MatchConfig, error) {
	var errs []error
	if spec.TargetPolicy == "" {
		errs = append(errs, errors.New("targetPolicy must name a HyperChain"))
	}
	if len(spec.Matches) == 0 {
		errs = append(errs, errors.New("matches must contain at least one entry"))
	}

	out := make([]config.MatchConfig, 0, len(spec.Matches))
	for i, m := range spec.Matches {
		mc, err := compileMatch(&m)
		if err != nil {
			errs = append(errs, fmt.Errorf("matches[%d]: %w", i, err))
			continue
		}
		out = append(out, mc)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return out, nil
}

func compileMatch(m *hyperv1alpha1.MatchRule) (config.MatchConfig, error) {
	var errs []error

	traffic, err := engineTraffic(m.Traffic)
	if err != nil {
		errs = append(errs, err)
	}
	if _, err := selector.Compile(m.Sources, selector.Source); err != nil {
		errs = append(errs, fmt.Errorf("sources: %w", err))
	}
	if _, err := selector.Compile(m.Destinations, selector.Destination); err != nil {
		errs = append(errs, fmt.Errorf("destinations: %w", err))
	}
	if m.PathRegexPattern != "" {
		if _, err := regexp.Compile(m.PathRegexPattern); err != nil {
			errs = append(errs, fmt.Errorf("pathRegexPattern: %w", err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return config.MatchConfig{}, err
	}

	var headers map[string]config.HeaderMatchConfig
	if m.Headers != nil {
		headers = make(map[string]config.HeaderMatchConfig, len(m.Headers))
		for k, v := range m.Headers {
			headers[strings.ToLower(k)] = config.HeaderMatchConfig{Exact: v}
		}
	}
	return config.MatchConfig{
		Traffic:          traffic,
		Sources:          m.Sources,
		Destinations:     m.Destinations,
		PathPrefix:       m.PathPrefix,
		PathRegexPattern: m.PathRegexPattern,
		Headers:          headers,
	}, nil
}

// engineTraffic maps the CRD enum to the engine value; Any and empty are omitted.
func engineTraffic(t hyperv1alpha1.TrafficClass) (string, error) {
	switch t {
	case "", hyperv1alpha1.TrafficAny:
		return "", nil
	case hyperv1alpha1.TrafficNorthSouth:
		return selector.TrafficNorthSouth.String(), nil
	case hyperv1alpha1.TrafficEastWest:
		return selector.TrafficEastWest.String(), nil
	}
	return "", fmt.Errorf("traffic: invalid value %q (expected NorthSouth, EastWest or Any)", t)
}

// DefaultChains returns the engine default chains of a HyperConfig.
func DefaultChains(spec *hyperv1alpha1.HyperConfigSpec) config.DefaultChainsConfig {
	if spec.DefaultChains == nil {
		return config.DefaultChainsConfig{}
	}
	return config.DefaultChainsConfig{
		NorthSouth: spec.DefaultChains.NorthSouth,
		EastWest:   spec.DefaultChains.EastWest,
	}
}

// ReferencedChains lists the HyperChains a HyperConfig uses as fallbacks, with
// the field that references each one.
func ReferencedChains(spec *hyperv1alpha1.HyperConfigSpec) map[string]string {
	refs := map[string]string{}
	add := func(field, name string) {
		if name != "" {
			if _, seen := refs[name]; !seen {
				refs[name] = field
			}
		}
	}
	add("defaultChain", spec.DefaultChain)
	if dc := spec.DefaultChains; dc != nil {
		add("defaultChains.northSouth", dc.NorthSouth)
		add("defaultChains.eastWest", dc.EastWest)
	}
	return refs
}
