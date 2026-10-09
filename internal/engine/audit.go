package engine

import "maps"

// auditedFilter runs a filter in audit mode: its denials and failures are
// recorded but not enforced. The executor recognises it with isAudited.
type auditedFilter struct {
	Filter
}

// Audit wraps a filter so that the executor records its denials instead of
// enforcing them. The same filter instance can be enforced in another chain.
func Audit(f Filter) Filter { return auditedFilter{f} }

// SupportedPhases keeps the wrapped filter's phase behaviour.
func (a auditedFilter) SupportedPhases() []Phase {
	if pa, ok := a.Filter.(PhaseAware); ok {
		return pa.SupportedPhases()
	}
	return []Phase{PhaseRequestHeaders}
}

func isAudited(f Filter) (Filter, bool) {
	if a, ok := f.(auditedFilter); ok {
		return a.Filter, true
	}
	return f, false
}

// mutations is the header and path state an audited filter may change. It is
// saved before the filter runs and restored if the filter denies, so a denial
// that is not enforced leaves no trace on the request or the response.
type mutations struct {
	path                    string
	headersToAdd            []Header
	headersToRemove         []string
	upstreamShadow          map[string]string
	responseHeadersToAdd    []Header
	responseHeadersToRemove []string
	downstreamShadow        map[string]string
	setHeaderOptions        int
}

func (ctx *RequestContext) saveMutations() mutations {
	return mutations{
		path:                    ctx.Path,
		headersToAdd:            append([]Header(nil), ctx.HeadersToAdd...),
		headersToRemove:         append([]string(nil), ctx.HeadersToRemove...),
		upstreamShadow:          maps.Clone(ctx.UpstreamShadow),
		responseHeadersToAdd:    append([]Header(nil), ctx.ResponseHeadersToAdd...),
		responseHeadersToRemove: append([]string(nil), ctx.ResponseHeadersToRemove...),
		downstreamShadow:        maps.Clone(ctx.DownstreamShadow),
		setHeaderOptions:        len(ctx.SetHeaderOptions),
	}
}

func (ctx *RequestContext) restoreMutations(m mutations) {
	ctx.Path = m.path
	ctx.HeadersToAdd = append(ctx.HeadersToAdd[:0], m.headersToAdd...)
	ctx.HeadersToRemove = append(ctx.HeadersToRemove[:0], m.headersToRemove...)
	clear(ctx.UpstreamShadow)
	maps.Copy(ctx.UpstreamShadow, m.upstreamShadow)
	ctx.ResponseHeadersToAdd = append(ctx.ResponseHeadersToAdd[:0], m.responseHeadersToAdd...)
	ctx.ResponseHeadersToRemove = append(ctx.ResponseHeadersToRemove[:0], m.responseHeadersToRemove...)
	clear(ctx.DownstreamShadow)
	maps.Copy(ctx.DownstreamShadow, m.downstreamShadow)
	ctx.SetHeaderOptions = ctx.SetHeaderOptions[:m.setHeaderOptions]
}

// unblock clears a block that audit mode does not enforce.
func (ctx *RequestContext) unblock() {
	ctx.Blocked = false
	ctx.Answered = false
	ctx.ResponseStatus = 0
	ctx.ResponseBody = ""
}
