package header_modifier

import (
	"strings"

	"github.com/taha2samy/hypergate/internal/engine"
)

type HeaderOptions struct {
	Add      map[string]string `yaml:"add"`
	Override map[string]string `yaml:"override"`
	Remove   []string          `yaml:"remove"`
}

type HeaderModifierConfig struct {
	Upstream   HeaderOptions     `yaml:"upstream"`
	Downstream HeaderOptions     `yaml:"downstream"`
	Add        map[string]string `yaml:"add"`
	Override   map[string]string `yaml:"override"`
	Remove     []string          `yaml:"remove"`
}

type HeaderModifierFilter struct {
	config HeaderModifierConfig
}

func NewHeaderModifierFilter(config HeaderModifierConfig) *HeaderModifierFilter {
	lowercaseMap := func(m map[string]string) map[string]string {
		if m == nil {
			return nil
		}
		res := make(map[string]string, len(m))
		for k, v := range m {
			res[strings.ToLower(k)] = v
		}
		return res
	}
	lowercaseSlice := func(s []string) []string {
		if s == nil {
			return nil
		}
		res := make([]string, len(s))
		for i, v := range s {
			res[i] = strings.ToLower(v)
		}
		return res
	}

	config.Add = lowercaseMap(config.Add)
	config.Override = lowercaseMap(config.Override)
	config.Remove = lowercaseSlice(config.Remove)
	config.Upstream.Add = lowercaseMap(config.Upstream.Add)
	config.Upstream.Override = lowercaseMap(config.Upstream.Override)
	config.Upstream.Remove = lowercaseSlice(config.Upstream.Remove)
	config.Downstream.Add = lowercaseMap(config.Downstream.Add)
	config.Downstream.Override = lowercaseMap(config.Downstream.Override)
	config.Downstream.Remove = lowercaseSlice(config.Downstream.Remove)

	return &HeaderModifierFilter{config: config}
}

func (f *HeaderModifierFilter) Execute(ctx *engine.RequestContext) error {
	for k, v := range f.config.Add {
		ctx.SetHeaderUpstream(k, v)
	}
	for k, v := range f.config.Override {
		ctx.SetHeaderUpstream(k, v)
	}
	for _, k := range f.config.Remove {
		ctx.RemoveHeaderUpstream(k)
	}

	for k, v := range f.config.Upstream.Add {
		ctx.SetHeaderUpstream(k, v)
	}
	for k, v := range f.config.Upstream.Override {
		ctx.SetHeaderUpstream(k, v)
	}
	for _, k := range f.config.Upstream.Remove {
		ctx.RemoveHeaderUpstream(k)
	}

	for k, v := range f.config.Downstream.Add {
		ctx.SetHeaderDownstream(k, v)
	}
	for k, v := range f.config.Downstream.Override {
		ctx.SetHeaderDownstream(k, v)
	}
	for _, k := range f.config.Downstream.Remove {
		ctx.RemoveHeaderDownstream(k)
	}

	return nil
}
