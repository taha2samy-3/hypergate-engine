package external_auth

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"

	"go.uber.org/zap"

	"github.com/taha/myprog/internal/config"
	"github.com/taha/myprog/internal/engine"
	mylogger "github.com/taha/myprog/internal/logger"
)

// ExternalAuthFilter implements the engine.Filter interface to delegate
// authorization to an external sidecar over a Unix Domain Socket (UDS).
type ExternalAuthFilter struct {
	config *config.ExternalAuthConfig
	client *http.Client
}

// NewExternalAuthFilter initializes a single reusable http.Client configured
// to communicate over a UDS. It returns an error if gRPC protocol is requested.
func NewExternalAuthFilter(cfg *config.ExternalAuthConfig) (*ExternalAuthFilter, error) {
	if cfg.Protocol == "grpc" {
		return nil, fmt.Errorf("gRPC protocol for external_auth is planned but not yet implemented")
	}

	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", cfg.SocketPath)
		},
		DisableKeepAlives:   false,
		MaxIdleConns:        1000,
		MaxIdleConnsPerHost: 1000,
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   cfg.TimeoutDuration,
	}

	return &ExternalAuthFilter{
		config: cfg,
		client: client,
	}, nil
}

// Execute intercepts the RequestContext and delegates auth to the configured sidecar.
func (f *ExternalAuthFilter) Execute(ctx *engine.RequestContext) error {
	// Defensive nil check: resolve a safe context that respects Envoy's stream
	// cancellation while guarding against an uninitialized stream context.
	reqCtx := ctx.Ctx
	if reqCtx == nil {
		reqCtx = context.Background()
	}

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, "http://localhost/", nil)
	if err != nil {
		mylogger.Error("external_auth: failed to create GET request", zap.Error(err))
		ctx.Blocked = true
		ctx.ResponseStatus = http.StatusInternalServerError
		ctx.ResponseBody = "Internal Server Error"
		return nil
	}

	// Forward specified headers
	for _, k := range f.config.ForwardHeaders {
		if val := ctx.GetHeader(k); val != "" {
			req.Header.Set(k, val)
		}
	}

	// Execute communication over Unix Domain Socket
	resp, err := f.client.Do(req)
	if err != nil {
		mylogger.Error("external_auth: UDS sidecar communication failed", zap.Error(err))
		ctx.Blocked = true
		ctx.ResponseStatus = http.StatusInternalServerError
		ctx.ResponseBody = "Internal Server Error"
		return nil
	}
	defer resp.Body.Close()

	// If Success (2xx)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		for _, k := range f.config.OnSuccess.UpstreamHeadersToAdd {
			if vals, ok := resp.Header[http.CanonicalHeaderKey(k)]; ok && len(vals) > 0 {
				ctx.SetHeaderUpstream(k, vals[0])
			}
		}
		for _, k := range f.config.OnSuccess.UpstreamHeadersToRemove {
			ctx.RemoveHeaderUpstream(k)
		}
		return nil
	}

	// If Failure (Non-2xx)
	ctx.Blocked = true
	ctx.ResponseStatus = int32(resp.StatusCode)

	limitReader := io.LimitReader(resp.Body, 16*1024) // Limit to 16KB
	bodyBytes, err := io.ReadAll(limitReader)
	if err == nil {
		ctx.ResponseBody = string(bodyBytes)
	} else {
		mylogger.Error("external_auth: failed to read response body", zap.Error(err))
	}

	for _, k := range f.config.OnFailure.DownstreamPassThroughHeaders {
		if vals, ok := resp.Header[http.CanonicalHeaderKey(k)]; ok && len(vals) > 0 {
			ctx.SetHeaderDownstream(k, vals[0])
		}
	}

	return nil
}
