package external_auth

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	_ "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/taha/myprog/internal/config"
	"github.com/taha/myprog/internal/engine"
	mylogger "github.com/taha/myprog/internal/logger"
)

// ExternalAuthFilter implements the engine.Filter interface to delegate
// authorization to an external sidecar over HTTP or gRPC using Unix Domain Sockets (UDS).
type ExternalAuthFilter struct {
	config     *config.ExternalAuthConfig
	client     *http.Client
	grpcClient authv3.AuthorizationClient
	grpcConn   *grpc.ClientConn
}

// NewExternalAuthFilter initializes a single reusable http.Client or gRPC client
// configured to communicate over a UDS socket.
func NewExternalAuthFilter(cfg *config.ExternalAuthConfig) (*ExternalAuthFilter, error) {
	if strings.EqualFold(cfg.Protocol, "grpc") {
		target := "unix://" + cfg.SocketPath
		conn, err := grpc.NewClient(
			target,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", cfg.SocketPath)
			}),
		)
		if err != nil {
			return nil, fmt.Errorf("external_auth: failed to dial gRPC UDS socket: %w", err)
		}

		return &ExternalAuthFilter{
			config:     cfg,
			grpcConn:   conn,
			grpcClient: authv3.NewAuthorizationClient(conn),
		}, nil
	}

	// Default to HTTP protocol
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

// Close closes the underlying gRPC client connection if active.
func (f *ExternalAuthFilter) Close() error {
	if f.grpcConn != nil {
		return f.grpcConn.Close()
	}
	return nil
}

// Execute intercepts the RequestContext and delegates auth to the configured sidecar via HTTP or gRPC.
func (f *ExternalAuthFilter) Execute(ctx *engine.RequestContext) error {
	if strings.EqualFold(f.config.Protocol, "grpc") {
		return f.executeGRPC(ctx)
	}
	return f.executeHTTP(ctx)
}

// executeGRPC handles authorization check over Envoy gRPC ext_authz protocol.
func (f *ExternalAuthFilter) executeGRPC(ctx *engine.RequestContext) error {
	reqCtx := ctx.Ctx
	if reqCtx == nil {
		reqCtx = context.Background()
	}

	headers := make(map[string]string, len(ctx.Headers))
	forwardAll := len(f.config.ForwardHeaders) == 0
	if !forwardAll {
		for _, h := range f.config.ForwardHeaders {
			lower := strings.ToLower(h)
			if lower == "*" || lower == "all" {
				forwardAll = true
				break
			}
		}
	}

	if forwardAll {
		for k := range ctx.Headers {
			if val := ctx.GetHeader(k); val != "" {
				headers[k] = val
			}
		}
	} else {
		for _, k := range f.config.ForwardHeaders {
			if val := ctx.GetHeader(k); val != "" {
				headers[k] = val
			}
		}
	}

	checkReq := &authv3.CheckRequest{
		Attributes: &authv3.AttributeContext{
			Request: &authv3.AttributeContext_Request{
				Http: &authv3.AttributeContext_HttpRequest{
					Path:    ctx.Path,
					Method:  ctx.Method,
					Headers: headers,
				},
			},
		},
	}

	resp, err := f.grpcClient.Check(reqCtx, checkReq)
	if err != nil {
		mylogger.Error("external_auth: gRPC UDS sidecar communication failed", zap.Error(err))
		ctx.Blocked = true
		ctx.ResponseStatus = http.StatusInternalServerError
		ctx.ResponseBody = "Internal Server Error"
		return nil
	}

	statusCode := resp.GetStatus().GetCode()

	// Success: 0 / OK
	if statusCode == int32(codes.OK) {
		okHeaders := make(map[string]string)
		if okResp := resp.GetOkResponse(); okResp != nil {
			for _, hOption := range okResp.GetHeaders() {
				if hOption != nil && hOption.GetHeader() != nil {
					okHeaders[strings.ToLower(hOption.GetHeader().GetKey())] = hOption.GetHeader().GetValue()
				}
			}
		}

		for _, k := range f.config.OnSuccess.UpstreamHeadersToAdd {
			if val, ok := okHeaders[strings.ToLower(k)]; ok {
				ctx.SetHeaderUpstream(k, val)
			}
		}
		for _, k := range f.config.OnSuccess.UpstreamHeadersToRemove {
			ctx.RemoveHeaderUpstream(k)
		}
		return nil
	}

	// Auth Blocked / Denied (Non-zero status)
	ctx.Blocked = true
	deniedStatus := int32(http.StatusForbidden)
	deniedResp := resp.GetDeniedResponse()
	if deniedResp != nil {
		if st := deniedResp.GetStatus(); st != nil && st.GetCode() != 0 {
			deniedStatus = int32(st.GetCode())
		}
		ctx.ResponseBody = deniedResp.GetBody()
	}
	ctx.ResponseStatus = deniedStatus

	deniedHeaders := make(map[string]string)
	if deniedResp != nil {
		for _, hOption := range deniedResp.GetHeaders() {
			if hOption != nil && hOption.GetHeader() != nil {
				deniedHeaders[strings.ToLower(hOption.GetHeader().GetKey())] = hOption.GetHeader().GetValue()
			}
		}
	}

	for _, k := range f.config.OnFailure.DownstreamPassThroughHeaders {
		if val, ok := deniedHeaders[strings.ToLower(k)]; ok {
			ctx.SetHeaderDownstream(k, val)
		}
	}

	return nil
}

// executeHTTP handles authorization check over HTTP protocol.
func (f *ExternalAuthFilter) executeHTTP(ctx *engine.RequestContext) error {
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
