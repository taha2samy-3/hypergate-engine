// Package jwt_auth provides a high-performance, zero-external-call JWT authentication filter
// for the Hypergate Engine. It supports:
//   - Local RS256/ES256/HS256 JWT validation with a pre-warmed JWKS key set
//   - Automatic background JWKS refresh (no request-path blocking)
//   - Optional REST fallback introspection endpoint for opaque tokens
//   - Configurable claim extraction → upstream header injection
//   - Per-request sub-millisecond verification via the lestrrat-go/jwx library
package jwt_auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jws"
	"github.com/lestrrat-go/jwx/v2/jwt"
	"go.uber.org/zap"

	"github.com/taha2samy/hypergate/internal/engine"
	mylogger "github.com/taha2samy/hypergate/internal/logger"
)

// ─────────────────────────────────────────────────────────────────────────────
// Configuration types
// ─────────────────────────────────────────────────────────────────────────────

// Config holds all settings for the JWT auth filter.
type Config struct {
	// Source is where to look for the token: "header" (default), "query", or "cookie".
	Source string `yaml:"source"`
	// HeaderName is the HTTP header to extract the token from. Default: "authorization".
	// The filter strips the "Bearer " prefix automatically.
	HeaderName string `yaml:"header_name"`
	// QueryParam is the query parameter name when Source is "query".
	QueryParam string `yaml:"query_param"`
	// CookieName is the cookie name when Source is "cookie".
	CookieName string `yaml:"cookie_name"`

	// JWKS remote endpoint for automatic key discovery.
	// When set, the filter performs local validation using the fetched keys.
	JWKSEndpoint string `yaml:"jwks_endpoint"`
	// JWKSRefreshInterval controls how often the JWKS keyset is refreshed in the background.
	// Default: 5m.
	JWKSRefreshInterval time.Duration `yaml:"jwks_refresh_interval"`

	// LocalSecret is the shared secret for HMAC (HS256/HS384/HS512) validation.
	// Use this when you don't have a JWKS endpoint.
	LocalSecret string `yaml:"local_secret"`
	// LocalSecretFile reads LocalSecret from a file (e.g. a mounted Kubernetes Secret)
	// so the secret never has to appear in the config itself.
	LocalSecretFile string `yaml:"local_secret_file"`
	// Algorithm specifies the expected signing algorithm for LocalSecret.
	// Default: "HS256" with a local secret, otherwise "RS256".
	Algorithm string `yaml:"algorithm"`

	// Issuer is the expected "iss" claim value. If empty, not validated.
	Issuer string `yaml:"issuer"`
	// Audience is the expected "aud" claim. If empty, not validated.
	Audience string `yaml:"audience"`

	// ClaimMappings maps JWT claim names → upstream header names to inject.
	// e.g. {"sub": "x-user-id", "email": "x-user-email"}
	ClaimMappings map[string]string `yaml:"claim_mappings"`

	// IntrospectionEndpoint is an optional REST endpoint for opaque token validation.
	// When the JWT parse fails, the filter calls this endpoint as a fallback.
	IntrospectionEndpoint string `yaml:"introspection_endpoint"`
	// IntrospectionAuthHeader is the Authorization header value sent to the introspection endpoint.
	IntrospectionAuthHeader string `yaml:"introspection_auth_header"`
	// IntrospectionAuthHeaderFile reads IntrospectionAuthHeader from a file.
	IntrospectionAuthHeaderFile string `yaml:"introspection_auth_header_file"`
	// IntrospectionTimeout is the timeout for the introspection HTTP call. Default: 2s.
	IntrospectionTimeout time.Duration `yaml:"introspection_timeout"`

	// FailOpen lets the request through (without claim headers) when the introspection
	// endpoint cannot be reached or answers 5xx. Invalid or inactive tokens are always
	// rejected. Default: false (fail closed).
	FailOpen bool `yaml:"fail_open"`

	// StripToken removes the Authorization header before forwarding to upstream.
	StripToken bool `yaml:"strip_token"`
}

// ApplyDefaults fills in sensible defaults for zero-value fields.
func (c *Config) ApplyDefaults() {
	if c.Source == "" {
		c.Source = "header"
	}
	if c.HeaderName == "" {
		c.HeaderName = "authorization"
	} else {
		c.HeaderName = strings.ToLower(c.HeaderName)
	}
	if c.JWKSRefreshInterval <= 0 {
		c.JWKSRefreshInterval = 5 * time.Minute
	}
	if c.IntrospectionTimeout <= 0 {
		c.IntrospectionTimeout = 2 * time.Second
	}
	// Claim names are case-sensitive (RFC 7519); only the header names are normalised.
	normalised := make(map[string]string, len(c.ClaimMappings))
	for claim, header := range c.ClaimMappings {
		normalised[claim] = strings.ToLower(header)
	}
	c.ClaimMappings = normalised
}

// loadSecretFiles resolves *_file options into their values.
func (c *Config) loadSecretFiles() error {
	read := func(path string) (string, error) {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("jwt_auth: read secret file %s: %w", path, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	if c.LocalSecretFile != "" {
		v, err := read(c.LocalSecretFile)
		if err != nil {
			return err
		}
		c.LocalSecret = v
	}
	if c.IntrospectionAuthHeaderFile != "" {
		v, err := read(c.IntrospectionAuthHeaderFile)
		if err != nil {
			return err
		}
		c.IntrospectionAuthHeader = v
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Filter implementation
// ─────────────────────────────────────────────────────────────────────────────

// Filter is the JWT authentication filter.
type Filter struct {
	cfg Config

	// keySet is an atomic pointer so the background refresher can swap it
	// without blocking request-path goroutines.
	keySet atomic.Pointer[jwk.Set]

	httpClient *http.Client // for introspection endpoint

	// refreshMu serialises on-demand JWKS refreshes; lastFetch (unix nanos) rate-limits them.
	refreshMu sync.Mutex
	lastFetch atomic.Int64

	stopRefresh chan struct{}
	closeOnce   sync.Once
}

// NewFilter creates and starts the JWT auth filter.
// It blocks until the initial JWKS fetch completes (or fails with an error),
// then kicks off a background refresh goroutine.
func NewFilter(cfg Config) (*Filter, error) {
	cfg.ApplyDefaults()
	if err := cfg.loadSecretFiles(); err != nil {
		return nil, err
	}
	if cfg.Algorithm == "" {
		// A shared secret implies HMAC; RS256 with an HMAC secret would reject every token.
		cfg.Algorithm = "RS256"
		if cfg.LocalSecret != "" {
			cfg.Algorithm = "HS256"
		}
	}
	if cfg.JWKSEndpoint == "" && cfg.LocalSecret == "" && cfg.IntrospectionEndpoint == "" {
		return nil, fmt.Errorf("jwt_auth: one of jwks_endpoint, local_secret(_file) or introspection_endpoint is required")
	}
	f := &Filter{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout: cfg.IntrospectionTimeout,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 100,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		stopRefresh: make(chan struct{}),
	}

	if cfg.JWKSEndpoint != "" {
		if err := f.fetchAndStoreJWKS(context.Background()); err != nil {
			return nil, fmt.Errorf("jwt_auth: initial JWKS fetch from %s failed: %w", cfg.JWKSEndpoint, err)
		}
		go f.backgroundRefresh()
	}

	return f, nil
}

// Close stops the background JWKS refresh goroutine and idle introspection connections.
func (f *Filter) Close() error {
	f.closeOnce.Do(func() {
		close(f.stopRefresh)
		f.httpClient.CloseIdleConnections()
	})
	return nil
}

// SupportedPhases declares that this filter only runs during request header processing.
func (f *Filter) SupportedPhases() []engine.Phase {
	return []engine.Phase{engine.PhaseRequestHeaders}
}

// Execute validates the JWT token and injects resolved claims as upstream headers.
func (f *Filter) Execute(ctx *engine.RequestContext) error {
	// Mapped headers are identity assertions for the upstream. Remove any value the
	// client sent up front, so a header can only ever come from a validated claim,
	// including when a claim is absent and on the fail_open path.
	for _, header := range f.cfg.ClaimMappings {
		ctx.RemoveHeaderUpstream(header)
	}

	rawToken := f.extractToken(ctx)
	if rawToken == "" {
		mylogger.Debug("jwt_auth: no token found in request")
		f.challenge(ctx, "missing authentication token", "")
		return nil
	}

	claims, err := f.validateJWT(rawToken)
	if err != nil {
		mylogger.Debug("jwt_auth: local JWT validation failed", zap.Error(err))

		if f.cfg.IntrospectionEndpoint == "" {
			// An invalid token is never let through, even with fail_open.
			f.challenge(ctx, "invalid token", "invalid_token")
			return nil
		}

		introClaims, introErr := f.introspect(ctx.Ctx, rawToken)
		if introErr != nil {
			var unavailable *introspectionUnavailableError
			if f.cfg.FailOpen && errors.As(introErr, &unavailable) {
				// fail_open only covers an unreachable/failing introspection endpoint.
				mylogger.Warn("jwt_auth: introspection unavailable, fail_open=true, allowing request", zap.Error(introErr))
				f.stripToken(ctx)
				return nil
			}
			mylogger.Warn("jwt_auth: token rejected by introspection", zap.Error(introErr))
			f.challenge(ctx, "token validation failed", "invalid_token")
			return nil
		}
		claims = introClaims
	}

	for claim, header := range f.cfg.ClaimMappings {
		if val, ok := claims[claim]; ok {
			if s := claimString(val); s != "" {
				ctx.SetHeaderUpstream(header, s)
			}
		}
	}

	f.stripToken(ctx)

	mylogger.Debug("jwt_auth: token validated successfully",
		zap.Int("claims_injected", len(f.cfg.ClaimMappings)),
	)
	return nil
}

// stripToken removes the token from wherever it was read before forwarding.
func (f *Filter) stripToken(ctx *engine.RequestContext) {
	if !f.cfg.StripToken {
		return
	}
	switch strings.ToLower(f.cfg.Source) {
	case "query":
		if stripped := removeQueryParam(ctx.Path, f.cfg.QueryParam); stripped != ctx.Path {
			ctx.SetPath(stripped)
		}
	case "cookie":
		if cookies, removed := removeCookie(ctx.GetHeader("cookie"), f.cfg.CookieName); removed {
			if cookies == "" {
				ctx.RemoveHeaderUpstream("cookie")
			} else {
				ctx.SetHeaderUpstream("cookie", cookies)
			}
		}
	default:
		ctx.RemoveHeaderUpstream(f.cfg.HeaderName)
	}
}

// claimString renders a claim as a header value: strings as-is, numbers without
// exponent notation, timestamps as Unix seconds, lists comma-separated, anything
// else as compact JSON. Control characters are dropped so a claim can never break
// out of its header.
func claimString(v interface{}) string {
	var out string
	switch t := v.(type) {
	case string:
		out = t
	case bool:
		out = strconv.FormatBool(t)
	case float64:
		out = strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		out = t.String()
	case int, int32, int64, uint, uint32, uint64:
		out = fmt.Sprint(t)
	case time.Time:
		out = strconv.FormatInt(t.Unix(), 10)
	case []string:
		out = strings.Join(t, ",")
	case []interface{}:
		parts := make([]string, 0, len(t))
		for _, item := range t {
			parts = append(parts, claimString(item))
		}
		out = strings.Join(parts, ",")
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		out = string(b)
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, out)
}

// challenge rejects with 401 and an RFC 6750 WWW-Authenticate header.
func (f *Filter) challenge(ctx *engine.RequestContext, message, bearerError string) {
	ctx.Block(401, `{"error":"unauthorized","message":"`+message+`"}`)
	ctx.SetHeaderDownstream("content-type", "application/json")
	challenge := "Bearer"
	if bearerError != "" {
		challenge += ` error="` + bearerError + `"`
	}
	ctx.SetHeaderDownstream("www-authenticate", challenge)
}

// introspectionUnavailableError marks failures to reach a verdict (network error,
// timeout, 5xx), as opposed to the endpoint saying the token is not valid.
type introspectionUnavailableError struct{ err error }

func (e *introspectionUnavailableError) Error() string { return e.err.Error() }
func (e *introspectionUnavailableError) Unwrap() error { return e.err }

// ─────────────────────────────────────────────────────────────────────────────
// Token extraction
// ─────────────────────────────────────────────────────────────────────────────

func (f *Filter) extractToken(ctx *engine.RequestContext) string {
	switch strings.ToLower(f.cfg.Source) {
	case "query":
		return extractQueryParam(ctx.Path, f.cfg.QueryParam)
	case "cookie":
		return extractCookie(ctx.GetHeader("cookie"), f.cfg.CookieName)
	default: // "header"
		raw := strings.TrimSpace(ctx.GetHeader(f.cfg.HeaderName))
		// The auth scheme is case-insensitive (RFC 7235).
		if len(raw) > 7 && strings.EqualFold(raw[:7], "bearer ") {
			return strings.TrimSpace(raw[7:])
		}
		return raw
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Local JWT validation
// ─────────────────────────────────────────────────────────────────────────────

func (f *Filter) validateJWT(rawToken string) (map[string]interface{}, error) {
	var parseOpts []jwt.ParseOption
	parseOpts = append(parseOpts, jwt.WithValidate(true))

	if f.cfg.LocalSecret != "" {
		alg := jwa.SignatureAlgorithm(f.cfg.Algorithm)
		parseOpts = append(parseOpts, jwt.WithKey(alg, []byte(f.cfg.LocalSecret)))
	} else if ks := f.keySet.Load(); ks != nil {
		// Use InferAlgorithmFromKey so the library matches by "kid" header and
		// infers the algorithm from the JWK key type — avoids hard-coding alg.
		parseOpts = append(parseOpts, jwt.WithKeySet(*ks, jws.WithInferAlgorithmFromKey(true)))
	} else {
		return nil, fmt.Errorf("no key material available for JWT validation")
	}

	if f.cfg.Issuer != "" {
		parseOpts = append(parseOpts, jwt.WithIssuer(f.cfg.Issuer))
	}
	if f.cfg.Audience != "" {
		parseOpts = append(parseOpts, jwt.WithAudience(f.cfg.Audience))
	}

	tok, err := jwt.ParseString(rawToken, parseOpts...)
	if err != nil && f.cfg.LocalSecret == "" && f.refreshAfterMiss() {
		// The identity provider may have rotated its keys since the last refresh: retry
		// once with a freshly fetched key set.
		if ks := f.keySet.Load(); ks != nil {
			parseOpts[1] = jwt.WithKeySet(*ks, jws.WithInferAlgorithmFromKey(true))
			tok, err = jwt.ParseString(rawToken, parseOpts...)
		}
	}
	if err != nil {
		return nil, err
	}

	raw, err := tok.AsMap(context.Background())
	if err != nil {
		return nil, fmt.Errorf("failed to convert token to map: %w", err)
	}
	return raw, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// REST introspection fallback
// ─────────────────────────────────────────────────────────────────────────────

// introspect calls the configured token introspection endpoint (RFC 7662).
// Returns the active claim map on success, error on failure or inactive token.
func (f *Filter) introspect(ctx context.Context, token string) (map[string]interface{}, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	reqCtx, cancel := context.WithTimeout(ctx, f.cfg.IntrospectionTimeout)
	defer cancel()

	form := url.Values{"token": {token}}
	body := strings.NewReader(form.Encode())
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, f.cfg.IntrospectionEndpoint, body)
	if err != nil {
		return nil, fmt.Errorf("introspection: failed to create request: %w", err)
	}
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	req.Header.Set("accept", "application/json")
	if f.cfg.IntrospectionAuthHeader != "" {
		req.Header.Set("authorization", f.cfg.IntrospectionAuthHeader)
	}

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return nil, &introspectionUnavailableError{fmt.Errorf("introspection: HTTP call failed: %w", err)}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 500 {
		return nil, &introspectionUnavailableError{fmt.Errorf("introspection: server returned HTTP %d", resp.StatusCode)}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("introspection: server returned HTTP %d", resp.StatusCode)
	}

	limited := io.LimitReader(resp.Body, 64*1024)
	var result map[string]interface{}
	if err := json.NewDecoder(limited).Decode(&result); err != nil {
		return nil, fmt.Errorf("introspection: failed to decode response: %w", err)
	}

	// RFC 7662 §2.2: the "active" boolean MUST be present and true.
	active, _ := result["active"].(bool)
	if !active {
		return nil, fmt.Errorf("introspection: token is not active")
	}

	// Apply the same issuer/audience policy as for local validation when the
	// introspection response carries those fields.
	if f.cfg.Issuer != "" {
		if iss, ok := result["iss"].(string); ok && iss != f.cfg.Issuer {
			return nil, fmt.Errorf("introspection: issuer %q not accepted", iss)
		}
	}
	if f.cfg.Audience != "" {
		if aud, present := result["aud"]; present && !audienceContains(aud, f.cfg.Audience) {
			return nil, fmt.Errorf("introspection: audience does not include %q", f.cfg.Audience)
		}
	}

	return result, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// JWKS management
// ─────────────────────────────────────────────────────────────────────────────

// jwksFetchTimeout bounds one JWKS download so a slow identity provider cannot hang
// filter creation (and with it a config reload).
const jwksFetchTimeout = 10 * time.Second

// jwksMinRefreshGap rate-limits refreshes triggered by unknown keys.
const jwksMinRefreshGap = 30 * time.Second

func (f *Filter) fetchAndStoreJWKS(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, jwksFetchTimeout)
	defer cancel()
	set, err := jwk.Fetch(ctx, f.cfg.JWKSEndpoint)
	f.lastFetch.Store(time.Now().UnixNano())
	if err != nil {
		return err
	}
	f.keySet.Store(&set)
	mylogger.Debug("jwt_auth: JWKS keyset refreshed", zap.String("endpoint", f.cfg.JWKSEndpoint))
	return nil
}

// refreshAfterMiss refetches the key set if the last fetch is older than
// jwksMinRefreshGap. It reports whether a new key set was loaded.
func (f *Filter) refreshAfterMiss() bool {
	if f.cfg.JWKSEndpoint == "" {
		return false
	}
	if time.Since(time.Unix(0, f.lastFetch.Load())) < jwksMinRefreshGap {
		return false
	}
	f.refreshMu.Lock()
	defer f.refreshMu.Unlock()
	// Another request may have refreshed while this one waited for the lock.
	if time.Since(time.Unix(0, f.lastFetch.Load())) < jwksMinRefreshGap {
		return true
	}
	if err := f.fetchAndStoreJWKS(context.Background()); err != nil {
		mylogger.Warn("jwt_auth: on-demand JWKS refresh failed", zap.Error(err))
		return false
	}
	return true
}

func (f *Filter) backgroundRefresh() {
	ticker := time.NewTicker(f.cfg.JWKSRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-f.stopRefresh:
			return
		case <-ticker.C:
			if err := f.fetchAndStoreJWKS(context.Background()); err != nil {
				mylogger.Error("jwt_auth: background JWKS refresh failed", zap.Error(err))
				// keep serving with the last known good key set
			}
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

func extractQueryParam(path, name string) string {
	idx := strings.IndexByte(path, '?')
	if idx == -1 {
		return ""
	}
	query := path[idx+1:]
	for query != "" {
		part := query
		if next := strings.IndexByte(query, '&'); next != -1 {
			part = query[:next]
			query = query[next+1:]
		} else {
			query = ""
		}
		if eqIdx := strings.IndexByte(part, '='); eqIdx != -1 {
			if part[:eqIdx] == name {
				if v, err := url.QueryUnescape(part[eqIdx+1:]); err == nil {
					return v
				}
				return part[eqIdx+1:]
			}
		}
	}
	return ""
}

// removeQueryParam drops every occurrence of name from the query string of path.
func removeQueryParam(path, name string) string {
	idx := strings.IndexByte(path, '?')
	if idx == -1 || name == "" {
		return path
	}
	var kept []string
	for _, part := range strings.Split(path[idx+1:], "&") {
		key := part
		if eq := strings.IndexByte(part, '='); eq != -1 {
			key = part[:eq]
		}
		if key != name && part != "" {
			kept = append(kept, part)
		}
	}
	if len(kept) == 0 {
		return path[:idx]
	}
	return path[:idx] + "?" + strings.Join(kept, "&")
}

// removeCookie drops the named cookie from a Cookie header value.
func removeCookie(cookieHeader, name string) (string, bool) {
	var kept []string
	removed := false
	for _, part := range strings.Split(cookieHeader, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if eq := strings.IndexByte(part, '='); eq != -1 && part[:eq] == name {
			removed = true
			continue
		}
		kept = append(kept, part)
	}
	return strings.Join(kept, "; "), removed
}

func audienceContains(aud interface{}, want string) bool {
	switch v := aud.(type) {
	case string:
		return v == want
	case []interface{}:
		for _, item := range v {
			if s, ok := item.(string); ok && s == want {
				return true
			}
		}
	}
	return false
}

func extractCookie(cookieHeader, name string) string {
	for _, part := range strings.Split(cookieHeader, ";") {
		part = strings.TrimSpace(part)
		if eqIdx := strings.IndexByte(part, '='); eqIdx != -1 {
			if part[:eqIdx] == name {
				return part[eqIdx+1:]
			}
		}
	}
	return ""
}
