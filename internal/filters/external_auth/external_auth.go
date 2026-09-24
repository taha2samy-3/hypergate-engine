package external_auth

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/taha2samy/hypergate/internal/config"
	"github.com/taha2samy/hypergate/internal/engine"
	"github.com/taha2samy/hypergate/internal/filters/sidecar"
	mylogger "github.com/taha2samy/hypergate/internal/logger"
)

// ExternalAuthFilter delegates the allow/deny decision to a sidecar reachable over a
// Unix domain socket, speaking either plain HTTP (forward-auth style: any 2xx allows)
// or Envoy's ext_authz gRPC API (envoy.service.auth.v3.Authorization/Check).
// Every failure to get a verdict (dial error, timeout, protocol error) denies with 500.
type ExternalAuthFilter struct {
	config     *config.ExternalAuthConfig
	client     *http.Client
	transport  *http.Transport
	grpcClient authv3.AuthorizationClient
	grpcConn   *grpc.ClientConn
}

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

	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", cfg.SocketPath)
		},
		MaxIdleConns:        1000,
		MaxIdleConnsPerHost: 1000,
	}

	return &ExternalAuthFilter{
		config:    cfg,
		transport: transport,
		client: &http.Client{
			Transport: transport,
			Timeout:   cfg.TimeoutDuration,
			// The sidecar's redirect (e.g. to a login page) is the answer to relay to
			// the client, not something to follow.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// Close releases the sidecar connections. Called when a reload retires the filter.
func (f *ExternalAuthFilter) Close() error {
	if f.transport != nil {
		f.transport.CloseIdleConnections()
	}
	if f.grpcConn != nil {
		return f.grpcConn.Close()
	}
	return nil
}

func (f *ExternalAuthFilter) Execute(ctx *engine.RequestContext) error {
	if strings.EqualFold(f.config.Protocol, "grpc") {
		return f.executeGRPC(ctx)
	}
	return f.executeHTTP(ctx)
}

func sidecarFailure(ctx *engine.RequestContext, msg string, err error) {
	mylogger.Error("external_auth: "+msg, zap.Error(err))
	ctx.Block(http.StatusInternalServerError, "Internal Server Error")
}

func (f *ExternalAuthFilter) executeGRPC(ctx *engine.RequestContext) error {
	reqCtx := ctx.Ctx
	if reqCtx == nil {
		reqCtx = context.Background()
	}
	// The timeout applies to the whole sidecar exchange, as it does for HTTP.
	reqCtx, cancel := context.WithTimeout(reqCtx, f.config.TimeoutDuration)
	defer cancel()

	host := ctx.Headers[":authority"]
	if host == "" {
		host = ctx.Headers["host"]
	}
	attrs := &authv3.AttributeContext{
		Request: &authv3.AttributeContext_Request{
			Http: &authv3.AttributeContext_HttpRequest{
				Id:       ctx.GetHeader("x-request-id"),
				Method:   ctx.Method,
				Path:     ctx.Path,
				Host:     host,
				Scheme:   ctx.Headers[":scheme"],
				Protocol: "HTTP/2",
				Headers:  sidecar.SelectHeaders(ctx, f.config.ForwardHeaders),
			},
		},
	}
	if ctx.ClientIP != "" {
		attrs.Source = &authv3.AttributeContext_Peer{
			Address: &corev3.Address{Address: &corev3.Address_SocketAddress{
				SocketAddress: &corev3.SocketAddress{Address: ctx.ClientIP},
			}},
		}
	}

	resp, err := f.grpcClient.Check(reqCtx, &authv3.CheckRequest{Attributes: attrs})
	if err != nil {
		sidecarFailure(ctx, "gRPC UDS sidecar communication failed", err)
		return nil
	}

	if resp.GetStatus().GetCode() == int32(codes.OK) {
		okHeaders := make(map[string]string)
		if okResp := resp.GetOkResponse(); okResp != nil {
			for _, hOption := range okResp.GetHeaders() {
				if h := hOption.GetHeader(); h != nil {
					okHeaders[strings.ToLower(h.GetKey())] = headerValue(h)
				}
			}
		}
		f.applySuccess(ctx, func(k string) (string, bool) { v, ok := okHeaders[k]; return v, ok })
		return nil
	}

	deniedStatus := int32(http.StatusForbidden)
	body := ""
	deniedHeaders := make(map[string]string)
	if deniedResp := resp.GetDeniedResponse(); deniedResp != nil {
		if code := deniedResp.GetStatus().GetCode(); code != 0 {
			deniedStatus = int32(code)
		}
		body = deniedResp.GetBody()
		for _, hOption := range deniedResp.GetHeaders() {
			if h := hOption.GetHeader(); h != nil {
				deniedHeaders[strings.ToLower(h.GetKey())] = headerValue(h)
			}
		}
	}
	ctx.Block(deniedStatus, body)
	f.applyFailure(ctx, func(k string) (string, bool) { v, ok := deniedHeaders[k]; return v, ok })
	return nil
}

func (f *ExternalAuthFilter) executeHTTP(ctx *engine.RequestContext) error {
	reqCtx := ctx.Ctx
	if reqCtx == nil {
		reqCtx = context.Background()
	}

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, "http://localhost"+f.config.Path, nil)
	if err != nil {
		sidecarFailure(ctx, "failed to create request", err)
		return nil
	}
	sidecar.SetHTTPHeaders(req, sidecar.SelectHeaders(ctx, f.config.ForwardHeaders))
	sidecar.SetForwardedRequest(req, ctx)

	resp, err := f.client.Do(req)
	if err != nil {
		sidecarFailure(ctx, "UDS sidecar communication failed", err)
		return nil
	}
	defer sidecar.DrainAndClose(resp.Body)

	lookup := func(k string) (string, bool) {
		if vals, ok := resp.Header[http.CanonicalHeaderKey(k)]; ok && len(vals) > 0 {
			return vals[0], true
		}
		return "", false
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		f.applySuccess(ctx, lookup)
		return nil
	}

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
	if err != nil {
		mylogger.Error("external_auth: failed to read response body", zap.Error(err))
	}
	ctx.Block(int32(resp.StatusCode), string(bodyBytes))
	f.applyFailure(ctx, lookup)
	return nil
}

// applySuccess copies the configured sidecar response headers upstream and removes
// the configured request headers (e.g. the session cookie).
func (f *ExternalAuthFilter) applySuccess(ctx *engine.RequestContext, lookup func(string) (string, bool)) {
	for _, k := range f.config.OnSuccess.UpstreamHeadersToAdd {
		if v, ok := lookup(k); ok {
			ctx.SetHeaderUpstream(k, v)
		}
	}
	for _, k := range f.config.OnSuccess.UpstreamHeadersToRemove {
		ctx.RemoveHeaderUpstream(k)
	}
}

// applyFailure relays the configured sidecar headers (e.g. Location, WWW-Authenticate,
// Set-Cookie) to the client together with the sidecar's status and body.
func (f *ExternalAuthFilter) applyFailure(ctx *engine.RequestContext, lookup func(string) (string, bool)) {
	for _, k := range f.config.OnFailure.DownstreamPassThroughHeaders {
		if v, ok := lookup(k); ok {
			ctx.SetHeaderDownstream(k, v)
		}
	}
}

func headerValue(h *corev3.HeaderValue) string {
	if len(h.GetRawValue()) > 0 {
		return string(h.GetRawValue())
	}
	return h.GetValue()
}
