// Package cors implements Cross-Origin Resource Sharing for browser clients.
//
// Preflight requests (OPTIONS with Origin and Access-Control-Request-Method) are
// answered by the engine itself: 204 with the CORS headers for an allowed origin,
// 403 otherwise. They never reach later filters (such as authentication, which a
// preflight cannot satisfy because browsers send no credentials) or the upstream.
//
// For actual requests from an allowed origin the Access-Control-* response headers
// are queued during the request phase, so they are also present on responses
// produced by later filters (401, 429, ...), and Vary is merged with the upstream's
// own Vary value when the response arrives.
package cors

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/taha2samy/hypergate/internal/engine"
)

// Config is the engine configuration of the "cors" filter.
type Config struct {
	// AllowOrigins lists exact origins such as "https://app.example.com", or "*".
	AllowOrigins []string `yaml:"allow_origins"`
	// AllowOriginRegex lists RE2 patterns matched against the whole origin.
	AllowOriginRegex []string `yaml:"allow_origin_regex"`
	// AllowMethods are returned in Access-Control-Allow-Methods. "*" echoes the
	// requested method. Default: GET, HEAD, POST.
	AllowMethods []string `yaml:"allow_methods"`
	// AllowHeaders are returned in Access-Control-Allow-Headers. "*" echoes the
	// requested headers.
	AllowHeaders []string `yaml:"allow_headers"`
	// ExposeHeaders are returned in Access-Control-Expose-Headers on actual requests.
	ExposeHeaders []string `yaml:"expose_headers"`
	// AllowCredentials sets Access-Control-Allow-Credentials: true. Not allowed with "*".
	AllowCredentials bool `yaml:"allow_credentials"`
	// MaxAge is how long (seconds) browsers may cache a preflight result. 0 omits it.
	MaxAge int `yaml:"max_age"`
	// BlockDisallowedOrigins rejects actual (non-preflight) requests from origins that
	// are not allowed with 403. By default they are forwarded without CORS headers and
	// the browser withholds the response from the calling page.
	BlockDisallowedOrigins bool `yaml:"block_disallowed_origins"`
}

// Filter is the CORS filter.
type Filter struct {
	cfg           Config
	anyOrigin     bool
	origins       map[string]struct{}
	originRegexps []*regexp.Regexp
	methods       string
	anyMethod     bool
	headers       string
	anyHeader     bool
	expose        string
	maxAge        string
}

// NewFilter validates the configuration and builds the filter.
func NewFilter(cfg Config) (*Filter, error) {
	f := &Filter{cfg: cfg, origins: make(map[string]struct{})}

	for _, o := range cfg.AllowOrigins {
		o = strings.TrimSpace(o)
		switch {
		case o == "*":
			f.anyOrigin = true
		case o != "":
			f.origins[strings.ToLower(strings.TrimSuffix(o, "/"))] = struct{}{}
		}
	}
	for _, pattern := range cfg.AllowOriginRegex {
		re, err := regexp.Compile("^(?:" + pattern + ")$")
		if err != nil {
			return nil, fmt.Errorf("cors: invalid allow_origin_regex %q: %w", pattern, err)
		}
		f.originRegexps = append(f.originRegexps, re)
	}
	if !f.anyOrigin && len(f.origins) == 0 && len(f.originRegexps) == 0 {
		return nil, fmt.Errorf("cors: at least one of allow_origins or allow_origin_regex is required")
	}
	if f.anyOrigin && cfg.AllowCredentials {
		// The CORS spec forbids "*" with credentials, and reflecting every origin with
		// credentials would let any website make authenticated calls on a user's behalf.
		return nil, fmt.Errorf("cors: allow_credentials cannot be combined with allow_origins \"*\"; list the origins explicitly")
	}
	if cfg.MaxAge < 0 {
		return nil, fmt.Errorf("cors: max_age must be >= 0")
	}

	methods := cfg.AllowMethods
	if len(methods) == 0 {
		methods = []string{"GET", "HEAD", "POST"}
	}
	f.methods, f.anyMethod = joinList(methods, strings.ToUpper)
	f.headers, f.anyHeader = joinList(cfg.AllowHeaders, strings.ToLower)
	f.expose, _ = joinList(cfg.ExposeHeaders, strings.ToLower)
	if cfg.MaxAge > 0 {
		f.maxAge = strconv.Itoa(cfg.MaxAge)
	}
	return f, nil
}

// joinList normalises and joins a header list; it reports whether "*" was present.
func joinList(items []string, norm func(string) string) (string, bool) {
	out := make([]string, 0, len(items))
	for _, it := range items {
		it = strings.TrimSpace(it)
		if it == "*" {
			return "", true
		}
		if it != "" {
			out = append(out, norm(it))
		}
	}
	return strings.Join(out, ", "), false
}

// SupportedPhases: request headers for the CORS decision, response headers to merge Vary.
func (f *Filter) SupportedPhases() []engine.Phase {
	return []engine.Phase{engine.PhaseRequestHeaders, engine.PhaseResponseHeaders}
}

func (f *Filter) Execute(ctx *engine.RequestContext) error {
	origin := ctx.GetHeader("origin")
	if origin == "" {
		return nil // not a cross-origin browser request
	}
	allowed := f.originAllowed(origin)

	if ctx.Phase == engine.PhaseResponseHeaders {
		if allowed && f.reflectsOrigin() {
			ctx.SetHeaderDownstream("vary", mergeVary(ctx.ResponseHeaders["vary"], "Origin"))
		}
		return nil
	}

	preflight := ctx.Method == "OPTIONS" && ctx.GetHeader("access-control-request-method") != ""
	if !allowed {
		if preflight || f.cfg.BlockDisallowedOrigins {
			ctx.Block(403, "CORS origin not allowed")
		}
		return nil
	}

	f.setAllowOrigin(ctx, origin)

	if preflight {
		methods := f.methods
		if f.anyMethod {
			methods = strings.ToUpper(ctx.GetHeader("access-control-request-method"))
		}
		ctx.SetHeaderDownstream("access-control-allow-methods", methods)

		headers := f.headers
		if f.anyHeader {
			headers = ctx.GetHeader("access-control-request-headers")
		}
		if headers != "" {
			ctx.SetHeaderDownstream("access-control-allow-headers", headers)
		}
		if f.maxAge != "" {
			ctx.SetHeaderDownstream("access-control-max-age", f.maxAge)
		}
		ctx.Block(204, "")
		return nil
	}

	if f.expose != "" {
		ctx.SetHeaderDownstream("access-control-expose-headers", f.expose)
	}
	return nil
}

func (f *Filter) originAllowed(origin string) bool {
	if f.anyOrigin {
		return true
	}
	o := strings.ToLower(origin)
	if _, ok := f.origins[o]; ok {
		return true
	}
	for _, re := range f.originRegexps {
		if re.MatchString(origin) {
			return true
		}
	}
	return false
}

// reflectsOrigin is true when the allowed origin is echoed back (rather than "*"),
// which makes the response depend on the Origin header.
func (f *Filter) reflectsOrigin() bool {
	return !f.anyOrigin // "*" with credentials is rejected in NewFilter
}

func (f *Filter) setAllowOrigin(ctx *engine.RequestContext, origin string) {
	if f.reflectsOrigin() {
		ctx.SetHeaderDownstream("access-control-allow-origin", origin)
		ctx.SetHeaderDownstream("vary", "Origin")
	} else {
		ctx.SetHeaderDownstream("access-control-allow-origin", "*")
	}
	if f.cfg.AllowCredentials {
		ctx.SetHeaderDownstream("access-control-allow-credentials", "true")
	}
}

// mergeVary adds token to an existing Vary value unless already present.
func mergeVary(existing, token string) string {
	if existing == "" {
		return token
	}
	for _, part := range strings.Split(existing, ",") {
		p := strings.TrimSpace(part)
		if p == "*" || strings.EqualFold(p, token) {
			return existing
		}
	}
	return existing + ", " + token
}
