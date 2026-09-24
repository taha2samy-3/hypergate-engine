// Package sidecar holds helpers shared by filters that call a sidecar over a Unix
// domain socket (external_auth, firewall).
package sidecar

import (
	"io"
	"net/http"
	"strings"

	"github.com/taha2samy/hypergate/internal/engine"
)

// hopByHop headers describe a single connection and must not be forwarded.
var hopByHop = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-connection":    true,
	"transfer-encoding":   true,
	"te":                  true,
	"trailer":             true,
	"upgrade":             true,
	"content-length":      true,
	"proxy-authorization": true,
}

// ForwardAll reports whether a forward_headers list means "all headers":
// empty, or containing "*" / "all".
func ForwardAll(list []string) bool {
	if len(list) == 0 {
		return true
	}
	for _, h := range list {
		if h == "*" || strings.EqualFold(h, "all") {
			return true
		}
	}
	return false
}

// SelectHeaders returns the request headers to send to a sidecar, reflecting
// changes made by earlier filters in the chain. Pseudo-headers (":path", ...) are
// never returned; use SetForwardedRequest to convey them over HTTP.
func SelectHeaders(ctx *engine.RequestContext, list []string) map[string]string {
	out := make(map[string]string, len(ctx.Headers))
	add := func(k string) {
		k = strings.ToLower(k)
		if strings.HasPrefix(k, ":") {
			return
		}
		if v := ctx.GetHeader(k); v != "" {
			out[k] = v
		}
	}
	if ForwardAll(list) {
		for k := range ctx.Headers {
			add(k)
		}
		for k := range ctx.UpstreamShadow {
			add(k)
		}
		return out
	}
	for _, k := range list {
		add(k)
	}
	return out
}

// SetHTTPHeaders copies selected headers onto an outgoing HTTP request, skipping
// hop-by-hop headers.
func SetHTTPHeaders(req *http.Request, headers map[string]string) {
	for k, v := range headers {
		if hopByHop[k] || k == "host" {
			continue
		}
		req.Header.Set(k, v)
	}
}

// SetForwardedRequest describes the original request to an HTTP sidecar using the
// conventions of Traefik ForwardAuth (X-Forwarded-*) and nginx auth_request
// (X-Original-*), so common auth services work unmodified.
func SetForwardedRequest(req *http.Request, ctx *engine.RequestContext) {
	host := ctx.Headers[":authority"]
	if host == "" {
		host = ctx.Headers["host"]
	}
	scheme := ctx.Headers[":scheme"]
	if scheme == "" {
		scheme = "http"
	}
	req.Header.Set("X-Forwarded-Method", ctx.Method)
	req.Header.Set("X-Forwarded-Uri", ctx.Path)
	req.Header.Set("X-Forwarded-Proto", scheme)
	req.Header.Set("X-Original-Method", ctx.Method)
	req.Header.Set("X-Original-Uri", ctx.Path)
	if host != "" {
		req.Header.Set("X-Forwarded-Host", host)
		req.Header.Set("X-Original-Url", scheme+"://"+host+ctx.Path)
	}
	if ctx.ClientIP != "" {
		req.Header.Set("X-Forwarded-For", ctx.ClientIP)
		req.Header.Set("X-Real-Ip", ctx.ClientIP)
	}
}

// DrainAndClose reads a bounded remainder of body so the keep-alive connection
// can be reused, then closes it.
func DrainAndClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 64<<10))
	_ = body.Close()
}
