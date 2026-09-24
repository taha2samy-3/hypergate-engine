package engine

import (
	"context"
	"strings"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
)

type Header struct {
	Key    string
	Value  string
	Append bool
}

type RequestContext struct {
	Ctx context.Context
	// Phase is the ext_proc phase currently being executed (set by ChainExecutor).
	Phase  Phase
	Path   string
	Method string
	// ClientIP is the resolved downstream client address (see internal/clientip).
	// It is derived from the Envoy peer address and trusted X-Forwarded-For hops,
	// never from raw client-supplied headers alone.
	ClientIP string
	// RequestEndOfStream is true when the request headers message carried
	// end_of_stream, i.e. the request has no body or trailers.
	RequestEndOfStream bool
	// RequestBodySeen is true once the RequestBody phase has been received.
	RequestBodySeen bool
	// Headers holds the original request headers (and request trailers).
	Headers map[string]string
	// ResponseHeaders holds the upstream response headers (and response trailers).
	// It is only populated once the ResponseHeaders phase has been reached.
	ResponseHeaders          map[string]string
	HeadersToAdd             []Header
	ResponseHeadersToAdd     []Header
	HeadersToRemove          []string
	ResponseHeadersToRemove  []string
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
	SetHeaderOptions         []*corev3.HeaderValueOption
	RawBodyBuffer            []byte
}

func (ctx *RequestContext) Reset() {
	ctx.Ctx = nil
	ctx.Phase = PhaseRequestHeaders
	ctx.RequestBodySeen = false
	ctx.Path = ""
	ctx.Method = ""
	ctx.ClientIP = ""
	ctx.RequestEndOfStream = false
	clear(ctx.Headers)
	clear(ctx.ResponseHeaders)
	ctx.HeadersToAdd = ctx.HeadersToAdd[:0]
	ctx.ResponseHeadersToAdd = ctx.ResponseHeadersToAdd[:0]
	ctx.HeadersToRemove = ctx.HeadersToRemove[:0]
	ctx.ResponseHeadersToRemove = ctx.ResponseHeadersToRemove[:0]
	clear(ctx.UpstreamShadow)
	clear(ctx.DownstreamShadow)
	ctx.Blocked = false
	ctx.ResponseStatus = 0
	ctx.ResponseBody = ""
	ctx.SetHeaderOptions = ctx.SetHeaderOptions[:0]

	if ctx.RawBodyBuffer != nil {
		ctx.RawBodyBuffer = ctx.RawBodyBuffer[:0]
	}

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

// Block marks the request as denied. The gRPC layer turns this into an
// ImmediateResponse in whichever phase the block happened.
func (ctx *RequestContext) Block(status int32, body string) {
	ctx.Blocked = true
	ctx.ResponseStatus = status
	ctx.ResponseBody = body
}

func (ctx *RequestContext) GetHeader(key string) string {
	if val, ok := ctx.UpstreamShadow[key]; ok {
		return val
	}
	if containsKey(ctx.HeadersToRemove, key) {
		return ""
	}
	return ctx.Headers[key]
}

// GetDownstreamHeader returns a value the gateway itself has set (or is going to set)
// on the downstream response. It does not look at upstream response headers; use
// GetResponseHeader for the effective response value.
func (ctx *RequestContext) GetDownstreamHeader(key string) string {
	key = strings.ToLower(key)
	return ctx.DownstreamShadow[key]
}

// GetResponseHeader returns the effective value of a response header: a value set by a
// filter wins, a header removed by a filter reads as empty, and otherwise the upstream
// response header is returned. Before the ResponseHeaders phase only filter-set values
// are visible.
func (ctx *RequestContext) GetResponseHeader(key string) string {
	key = strings.ToLower(key)
	if val, ok := ctx.DownstreamShadow[key]; ok {
		return val
	}
	if containsKey(ctx.ResponseHeadersToRemove, key) {
		return ""
	}
	return ctx.ResponseHeaders[key]
}

// SetPath rewrites the request :path sent upstream (e.g. to strip a credential from
// the query string). Changing only the query does not affect Envoy's route selection.
func (ctx *RequestContext) SetPath(path string) {
	ctx.Path = path
	ctx.SetHeaderUpstream(":path", path)
}

func (ctx *RequestContext) SetHeaderUpstream(key, value string) {
	key = strings.ToLower(key)
	ctx.UpstreamShadow[key] = value
	ctx.HeadersToRemove = removeKey(ctx.HeadersToRemove, key)
	ctx.HeadersToAdd = upsertHeader(ctx.HeadersToAdd, key, value)
}

func (ctx *RequestContext) RemoveHeaderUpstream(key string) {
	key = strings.ToLower(key)
	delete(ctx.UpstreamShadow, key)
	ctx.HeadersToAdd = removeHeader(ctx.HeadersToAdd, key)
	ctx.HeadersToRemove = appendKey(ctx.HeadersToRemove, key)
}

func (ctx *RequestContext) SetHeaderDownstream(key, value string) {
	key = strings.ToLower(key)
	ctx.DownstreamShadow[key] = value
	ctx.ResponseHeadersToRemove = removeKey(ctx.ResponseHeadersToRemove, key)
	ctx.ResponseHeadersToAdd = upsertHeader(ctx.ResponseHeadersToAdd, key, value)
}

// RemoveHeaderDownstream removes a header from the response sent to the client,
// including headers set by the upstream service.
func (ctx *RequestContext) RemoveHeaderDownstream(key string) {
	key = strings.ToLower(key)
	delete(ctx.DownstreamShadow, key)
	ctx.ResponseHeadersToAdd = removeHeader(ctx.ResponseHeadersToAdd, key)
	ctx.ResponseHeadersToRemove = appendKey(ctx.ResponseHeadersToRemove, key)
}

func (ctx *RequestContext) SetTrailerUpstream(key, value string) {
	key = strings.ToLower(key)
	ctx.RequestTrailersToRemove = removeKey(ctx.RequestTrailersToRemove, key)
	ctx.RequestTrailersToAdd = upsertHeader(ctx.RequestTrailersToAdd, key, value)
	ctx.RequestTrailersModified = true
}

func (ctx *RequestContext) RemoveHeaderUpstreamTrailer(key string) {
	key = strings.ToLower(key)
	ctx.RequestTrailersToAdd = removeHeader(ctx.RequestTrailersToAdd, key)
	ctx.RequestTrailersToRemove = appendKey(ctx.RequestTrailersToRemove, key)
	ctx.RequestTrailersModified = true
}

func (ctx *RequestContext) SetTrailerDownstream(key, value string) {
	key = strings.ToLower(key)
	ctx.ResponseTrailersToRemove = removeKey(ctx.ResponseTrailersToRemove, key)
	ctx.ResponseTrailersToAdd = upsertHeader(ctx.ResponseTrailersToAdd, key, value)
	ctx.ResponseTrailersModified = true
}

func (ctx *RequestContext) RemoveHeaderDownstreamTrailer(key string) {
	key = strings.ToLower(key)
	ctx.ResponseTrailersToAdd = removeHeader(ctx.ResponseTrailersToAdd, key)
	ctx.ResponseTrailersToRemove = appendKey(ctx.ResponseTrailersToRemove, key)
	ctx.ResponseTrailersModified = true
}

// upsertHeader overwrites the value for key if present, otherwise appends it.
func upsertHeader(headers []Header, key, value string) []Header {
	for i := range headers {
		if headers[i].Key == key {
			headers[i].Value = value
			return headers
		}
	}
	return append(headers, Header{Key: key, Value: value})
}

// removeHeader deletes key from headers (order is not preserved).
func removeHeader(headers []Header, key string) []Header {
	for i := range headers {
		if headers[i].Key == key {
			headers[i] = headers[len(headers)-1]
			return headers[:len(headers)-1]
		}
	}
	return headers
}

func containsKey(keys []string, key string) bool {
	for _, k := range keys {
		if k == key {
			return true
		}
	}
	return false
}

func appendKey(keys []string, key string) []string {
	if containsKey(keys, key) {
		return keys
	}
	return append(keys, key)
}

func removeKey(keys []string, key string) []string {
	for i := range keys {
		if keys[i] == key {
			keys[i] = keys[len(keys)-1]
			return keys[:len(keys)-1]
		}
	}
	return keys
}
