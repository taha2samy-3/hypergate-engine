package engine

import (
	"context"
	"strings"
)

type Header struct {
	Key    string
	Value  string
	Append bool
}

type RequestContext struct {
	Ctx                      context.Context
	Path                     string
	Method                   string
	Headers                  map[string]string
	HeadersToAdd             []Header
	ResponseHeadersToAdd     []Header
	HeadersToRemove          []string
	Blocked                  bool
	ResponseStatus           int32
	ResponseBody             string
	UpstreamShadow           map[string]string
	DownstreamShadow         map[string]string
	RequestBody              []byte
	ResponseBodyBytes        []byte
	RequestTrailersToAdd     []Header
	ResponseTrailersToAdd    []Header
	RequestTrailersToRemove  []string
	ResponseTrailersToRemove []string
	RequestBodyRequired      bool
	ResponseBodyRequired     bool
	RequestTrailersRequired  bool
	ResponseTrailersRequired bool
	RequestBodyModified      bool
	ResponseBodyModified     bool
	RequestTrailersModified  bool
	ResponseTrailersModified bool
}

func (ctx *RequestContext) Reset() {
	ctx.Ctx = nil
	ctx.Path = ""
	ctx.Method = ""
	clear(ctx.Headers)
	ctx.HeadersToAdd = ctx.HeadersToAdd[:0]
	ctx.ResponseHeadersToAdd = ctx.ResponseHeadersToAdd[:0]
	ctx.HeadersToRemove = ctx.HeadersToRemove[:0]
	clear(ctx.UpstreamShadow)
	clear(ctx.DownstreamShadow)
	ctx.Blocked = false
	ctx.ResponseStatus = 0
	ctx.ResponseBody = ""

	if ctx.RequestBody != nil {
		ctx.RequestBody = ctx.RequestBody[:0]
	}
	if ctx.ResponseBodyBytes != nil {
		ctx.ResponseBodyBytes = ctx.ResponseBodyBytes[:0]
	}

	ctx.RequestTrailersToAdd = ctx.RequestTrailersToAdd[:0]
	ctx.ResponseTrailersToAdd = ctx.ResponseTrailersToAdd[:0]
	ctx.RequestTrailersToRemove = ctx.RequestTrailersToRemove[:0]
	ctx.ResponseTrailersToRemove = ctx.ResponseTrailersToRemove[:0]

	ctx.RequestBodyRequired = false
	ctx.ResponseBodyRequired = false
	ctx.RequestTrailersRequired = false
	ctx.ResponseTrailersRequired = false

	ctx.RequestBodyModified = false
	ctx.ResponseBodyModified = false
	ctx.RequestTrailersModified = false
	ctx.ResponseTrailersModified = false
}

func (ctx *RequestContext) GetHeader(key string) string {
	key = strings.ToLower(key)
	if val, ok := ctx.UpstreamShadow[key]; ok {
		return val
	}
	for i := 0; i < len(ctx.HeadersToRemove); i++ {
		if ctx.HeadersToRemove[i] == key {
			return ""
		}
	}
	return ctx.Headers[key]
}

func (ctx *RequestContext) GetDownstreamHeader(key string) string {
	key = strings.ToLower(key)
	return ctx.DownstreamShadow[key]
}

func (ctx *RequestContext) SetHeaderUpstream(key, value string) {
	key = strings.ToLower(key)
	ctx.UpstreamShadow[key] = value

	for i := 0; i < len(ctx.HeadersToRemove); i++ {
		if ctx.HeadersToRemove[i] == key {
			ctx.HeadersToRemove[i] = ctx.HeadersToRemove[len(ctx.HeadersToRemove)-1]
			ctx.HeadersToRemove = ctx.HeadersToRemove[:len(ctx.HeadersToRemove)-1]
			break
		}
	}

	for i := 0; i < len(ctx.HeadersToAdd); i++ {
		if ctx.HeadersToAdd[i].Key == key {
			ctx.HeadersToAdd[i].Value = value
			return
		}
	}
	ctx.HeadersToAdd = append(ctx.HeadersToAdd, Header{Key: key, Value: value, Append: false})
}

func (ctx *RequestContext) RemoveHeaderUpstream(key string) {
	key = strings.ToLower(key)
	delete(ctx.UpstreamShadow, key)

	for i := 0; i < len(ctx.HeadersToAdd); i++ {
		if ctx.HeadersToAdd[i].Key == key {
			ctx.HeadersToAdd[i] = ctx.HeadersToAdd[len(ctx.HeadersToAdd)-1]
			ctx.HeadersToAdd = ctx.HeadersToAdd[:len(ctx.HeadersToAdd)-1]
			break
		}
	}

	for i := 0; i < len(ctx.HeadersToRemove); i++ {
		if ctx.HeadersToRemove[i] == key {
			return
		}
	}
	ctx.HeadersToRemove = append(ctx.HeadersToRemove, key)
}

func (ctx *RequestContext) SetHeaderDownstream(key, value string) {
	key = strings.ToLower(key)
	ctx.DownstreamShadow[key] = value

	for i := 0; i < len(ctx.ResponseHeadersToAdd); i++ {
		if ctx.ResponseHeadersToAdd[i].Key == key {
			ctx.ResponseHeadersToAdd[i].Value = value
			return
		}
	}
	ctx.ResponseHeadersToAdd = append(ctx.ResponseHeadersToAdd, Header{Key: key, Value: value, Append: false})
}

func (ctx *RequestContext) RemoveHeaderDownstream(key string) {
	key = strings.ToLower(key)
	delete(ctx.DownstreamShadow, key)

	for i := 0; i < len(ctx.ResponseHeadersToAdd); i++ {
		if ctx.ResponseHeadersToAdd[i].Key == key {
			ctx.ResponseHeadersToAdd[i] = ctx.ResponseHeadersToAdd[len(ctx.ResponseHeadersToAdd)-1]
			ctx.ResponseHeadersToAdd = ctx.ResponseHeadersToAdd[:len(ctx.ResponseHeadersToAdd)-1]
			break
		}
	}
}

func (ctx *RequestContext) SetTrailerUpstream(key, value string) {
	key = strings.ToLower(key)
	for i := 0; i < len(ctx.RequestTrailersToRemove); i++ {
		if ctx.RequestTrailersToRemove[i] == key {
			ctx.RequestTrailersToRemove[i] = ctx.RequestTrailersToRemove[len(ctx.RequestTrailersToRemove)-1]
			ctx.RequestTrailersToRemove = ctx.RequestTrailersToRemove[:len(ctx.RequestTrailersToRemove)-1]
			break
		}
	}
	for i := 0; i < len(ctx.RequestTrailersToAdd); i++ {
		if ctx.RequestTrailersToAdd[i].Key == key {
			ctx.RequestTrailersToAdd[i].Value = value
			ctx.RequestTrailersModified = true
			return
		}
	}
	// BUG FIX: was incorrectly appending to HeadersToAdd (request headers);
	// trailers must be appended to RequestTrailersToAdd.
	ctx.RequestTrailersToAdd = append(ctx.RequestTrailersToAdd, Header{Key: key, Value: value, Append: false})
	ctx.RequestTrailersModified = true
}

func (ctx *RequestContext) requestHeaderActionChecked(key string) {
	for i := 0; i < len(ctx.HeadersToAdd); i++ {
		if ctx.HeadersToAdd[i].Key == key {
			return
		}
	}
}

func (ctx *RequestContext) RemoveHeaderUpstreamTrailer(key string) {
	key = strings.ToLower(key)
	for i := 0; i < len(ctx.HeadersToAdd); i++ {
		if ctx.HeadersToAdd[i].Key == key {
			ctx.HeadersToAdd[i] = ctx.HeadersToAdd[len(ctx.HeadersToAdd)-1]
			ctx.HeadersToAdd = ctx.HeadersToAdd[:len(ctx.HeadersToAdd)-1]
			break
		}
	}
	for i := 0; i < len(ctx.RequestTrailersToRemove); i++ {
		if ctx.RequestTrailersToRemove[i] == key {
			return
		}
	}
	ctx.RequestTrailersToRemove = append(ctx.RequestTrailersToRemove, key)
	ctx.RequestTrailersModified = true
}

func (ctx *RequestContext) SetTrailerDownstream(key, value string) {
	key = strings.ToLower(key)
	for i := 0; i < len(ctx.ResponseTrailersToRemove); i++ {
		if ctx.ResponseTrailersToRemove[i] == key {
			ctx.ResponseTrailersToRemove[i] = ctx.ResponseTrailersToRemove[len(ctx.ResponseTrailersToRemove)-1]
			ctx.ResponseTrailersToRemove = ctx.ResponseTrailersToRemove[:len(ctx.ResponseTrailersToRemove)-1]
			break
		}
	}
	// BUG FIX: was incorrectly scanning/modifying ResponseHeadersToAdd (response headers);
	// trailers must target ResponseTrailersToAdd.
	for i := 0; i < len(ctx.ResponseTrailersToAdd); i++ {
		if ctx.ResponseTrailersToAdd[i].Key == key {
			ctx.ResponseTrailersToAdd[i].Value = value
			ctx.ResponseTrailersModified = true
			return
		}
	}
	ctx.ResponseTrailersToAdd = append(ctx.ResponseTrailersToAdd, Header{Key: key, Value: value, Append: false})
	ctx.ResponseTrailersModified = true
}

func (ctx *RequestContext) RemoveHeaderDownstreamTrailer(key string) {
	key = strings.ToLower(key)
	for i := 0; i < len(ctx.ResponseHeadersToAdd); i++ {
		if ctx.ResponseHeadersToAdd[i].Key == key {
			ctx.ResponseHeadersToAdd[i] = ctx.ResponseHeadersToAdd[len(ctx.ResponseHeadersToAdd)-1]
			ctx.ResponseHeadersToAdd = ctx.ResponseHeadersToAdd[:len(ctx.ResponseHeadersToAdd)-1]
			break
		}
	}
}

func (ctx *RequestContext) RequestHeadersToAddChecked() {
	ctx.RequestTrailersModified = true
}
