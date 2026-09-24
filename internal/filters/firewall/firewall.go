package firewall

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	configv3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	_ "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/taha2samy/hypergate/internal/config"
	"github.com/taha2samy/hypergate/internal/engine"
	mylogger "github.com/taha2samy/hypergate/internal/logger"
)

type FirewallFilter struct {
	config     *config.FirewallFilterConfig
	httpClient *http.Client
	grpcClient extprocv3.ExternalProcessorClient
	grpcConn   *grpc.ClientConn
}

func NewFirewallFilter(cfg *config.FirewallFilterConfig) (*FirewallFilter, error) {
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
			return nil, fmt.Errorf("firewall: failed to dial gRPC UDS socket: %w", err)
		}

		return &FirewallFilter{
			config:     cfg,
			grpcConn:   conn,
			grpcClient: extprocv3.NewExternalProcessorClient(conn),
		}, nil
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

	httpClient := &http.Client{
		Transport: transport,
		Timeout:   cfg.TimeoutDuration,
	}

	return &FirewallFilter{
		config:     cfg,
		httpClient: httpClient,
	}, nil
}

func (f *FirewallFilter) Close() error {
	if f.grpcConn != nil {
		return f.grpcConn.Close()
	}
	return nil
}

// SupportedPhases runs the firewall on request headers and, when body inspection is
// enabled, on the buffered request body.
func (f *FirewallFilter) SupportedPhases() []engine.Phase {
	if f.config.InspectBody {
		return []engine.Phase{engine.PhaseRequestHeaders, engine.PhaseRequestBody}
	}
	return []engine.Phase{engine.PhaseRequestHeaders}
}

// Execute inspects the request exactly once. Without body inspection, or when the
// request has no body, the sidecar is called during the headers phase. Otherwise the
// headers phase only asks Envoy to buffer the body and the sidecar is called with
// headers and body together in the body phase.
func (f *FirewallFilter) Execute(ctx *engine.RequestContext) error {
	if f.config.InspectBody && !ctx.RequestEndOfStream && ctx.Phase == engine.PhaseRequestHeaders {
		// Requires Envoy's ext_proc `allow_mode_override: true` (or a static BUFFERED
		// request_body_mode). If the body never arrives the gRPC layer fails closed.
		ctx.RequestBodyRequired = true
		return nil
	}
	if ctx.Phase == engine.PhaseRequestBody && (!f.config.InspectBody || ctx.RequestEndOfStream) {
		return nil // already inspected during the headers phase
	}
	return f.inspect(ctx)
}

func (f *FirewallFilter) inspect(ctx *engine.RequestContext) error {
	if strings.EqualFold(f.config.Protocol, "grpc") {
		return f.executeGRPC(ctx)
	}
	return f.executeHTTP(ctx)
}

func (f *FirewallFilter) executeGRPC(ctx *engine.RequestContext) error {
	reqCtx := ctx.Ctx
	if reqCtx == nil {
		reqCtx = context.Background()
	}

	stream, err := f.grpcClient.Process(reqCtx)
	if err != nil {
		mylogger.Error("firewall: failed to open gRPC ext_proc stream to sidecar", zap.Error(err))
		ctx.Blocked = true
		ctx.ResponseStatus = http.StatusInternalServerError
		ctx.ResponseBody = "Internal Server Error"
		return nil
	}

	var headerValues []*configv3.HeaderValue
	headerValues = append(headerValues, &configv3.HeaderValue{
		Key:   ":path",
		Value: ctx.Path,
	})
	headerValues = append(headerValues, &configv3.HeaderValue{
		Key:   ":method",
		Value: ctx.Method,
	})

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
				headerValues = append(headerValues, &configv3.HeaderValue{Key: k, Value: val})
			}
		}
	} else {
		for _, k := range f.config.ForwardHeaders {
			if val := ctx.GetHeader(k); val != "" {
				headerValues = append(headerValues, &configv3.HeaderValue{Key: k, Value: val})
			}
		}
	}

	headerMsg := &extprocv3.ProcessingRequest_RequestHeaders{
		RequestHeaders: &extprocv3.HttpHeaders{
			Headers: &configv3.HeaderMap{
				Headers: headerValues,
			},
		},
	}

	if err := stream.Send(&extprocv3.ProcessingRequest{Request: headerMsg}); err != nil {
		mylogger.Error("firewall: failed to send request headers over gRPC ext_proc", zap.Error(err))
		ctx.Blocked = true
		ctx.ResponseStatus = http.StatusInternalServerError
		ctx.ResponseBody = "Internal Server Error"
		return nil
	}

	resp, err := stream.Recv()
	if err != nil {
		mylogger.Error("firewall: failed to receive response for request headers over gRPC ext_proc", zap.Error(err))
		ctx.Blocked = true
		ctx.ResponseStatus = http.StatusInternalServerError
		ctx.ResponseBody = "Internal Server Error"
		return nil
	}

	if imm := resp.GetImmediateResponse(); imm != nil {
		ctx.Blocked = true
		statusCode := int32(http.StatusForbidden)
		if st := imm.GetStatus(); st != nil && st.GetCode() != 0 {
			statusCode = int32(st.GetCode())
		}
		ctx.ResponseStatus = statusCode
		ctx.ResponseBody = string(imm.GetBody())

		if mutations := imm.GetHeaders(); mutations != nil {
			for _, hOption := range mutations.GetSetHeaders() {
				if hOption != nil && hOption.GetHeader() != nil {
					ctx.SetHeaderDownstream(hOption.GetHeader().GetKey(), hOption.GetHeader().GetValue())
				}
			}
		}
		return nil
	}

	if f.config.InspectBody && len(ctx.RequestBody) > 0 {
		maxBytes := int(f.config.MaxBodySizeKB) * 1024
		bodyBytes := ctx.RequestBody
		if len(bodyBytes) > maxBytes {
			bodyBytes = bodyBytes[:maxBytes]
		}

		bodyMsg := &extprocv3.ProcessingRequest_RequestBody{
			RequestBody: &extprocv3.HttpBody{
				Body: bodyBytes,
			},
		}

		if err := stream.Send(&extprocv3.ProcessingRequest{Request: bodyMsg}); err != nil {
			mylogger.Error("firewall: failed to send request body over gRPC ext_proc", zap.Error(err))
			ctx.Blocked = true
			ctx.ResponseStatus = http.StatusInternalServerError
			ctx.ResponseBody = "Internal Server Error"
			return nil
		}

		resp, err = stream.Recv()
		if err != nil {
			mylogger.Error("firewall: failed to receive response for request body over gRPC ext_proc", zap.Error(err))
			ctx.Blocked = true
			ctx.ResponseStatus = http.StatusInternalServerError
			ctx.ResponseBody = "Internal Server Error"
			return nil
		}

		if imm := resp.GetImmediateResponse(); imm != nil {
			ctx.Blocked = true
			statusCode := int32(http.StatusForbidden)
			if st := imm.GetStatus(); st != nil && st.GetCode() != 0 {
				statusCode = int32(st.GetCode())
			}
			ctx.ResponseStatus = statusCode
			ctx.ResponseBody = string(imm.GetBody())

			if mutations := imm.GetHeaders(); mutations != nil {
				for _, hOption := range mutations.GetSetHeaders() {
					if hOption != nil && hOption.GetHeader() != nil {
						ctx.SetHeaderDownstream(hOption.GetHeader().GetKey(), hOption.GetHeader().GetValue())
					}
				}
			}
			return nil
		}
	}

	if hResp := resp.GetRequestHeaders(); hResp != nil && hResp.GetResponse() != nil {
		if mutations := hResp.GetResponse().GetHeaderMutation(); mutations != nil {
			for _, hOption := range mutations.GetSetHeaders() {
				if hOption != nil && hOption.GetHeader() != nil {
					ctx.SetHeaderUpstream(hOption.GetHeader().GetKey(), hOption.GetHeader().GetValue())
				}
			}
			for _, k := range mutations.GetRemoveHeaders() {
				ctx.RemoveHeaderUpstream(k)
			}
		}
	} else if bResp := resp.GetRequestBody(); bResp != nil && bResp.GetResponse() != nil {
		if mutations := bResp.GetResponse().GetHeaderMutation(); mutations != nil {
			for _, hOption := range mutations.GetSetHeaders() {
				if hOption != nil && hOption.GetHeader() != nil {
					ctx.SetHeaderUpstream(hOption.GetHeader().GetKey(), hOption.GetHeader().GetValue())
				}
			}
			for _, k := range mutations.GetRemoveHeaders() {
				ctx.RemoveHeaderUpstream(k)
			}
		}
	}

	for _, k := range f.config.OnSuccess.UpstreamHeadersToRemove {
		ctx.RemoveHeaderUpstream(k)
	}

	return nil
}

func (f *FirewallFilter) executeHTTP(ctx *engine.RequestContext) error {
	reqCtx := ctx.Ctx
	if reqCtx == nil {
		reqCtx = context.Background()
	}

	method := http.MethodGet
	if len(ctx.RequestBody) > 0 {
		method = http.MethodPost
	}

	var bodyReader io.Reader
	if f.config.InspectBody && len(ctx.RequestBody) > 0 {
		maxBytes := int(f.config.MaxBodySizeKB) * 1024
		bodySlice := ctx.RequestBody
		if len(bodySlice) > maxBytes {
			bodySlice = bodySlice[:maxBytes]
		}
		bodyReader = bytes.NewReader(bodySlice)
	}

	req, err := http.NewRequestWithContext(reqCtx, method, "http://localhost/", bodyReader)
	if err != nil {
		mylogger.Error("firewall: failed to create request for sidecar", zap.Error(err))
		ctx.Blocked = true
		ctx.ResponseStatus = http.StatusInternalServerError
		ctx.ResponseBody = "Internal Server Error"
		return nil
	}

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
				req.Header.Set(k, val)
			}
		}
	} else {
		for _, k := range f.config.ForwardHeaders {
			if val := ctx.GetHeader(k); val != "" {
				req.Header.Set(k, val)
			}
		}
	}

	resp, err := f.httpClient.Do(req)
	if err != nil {
		mylogger.Error("firewall: UDS sidecar communication failed", zap.Error(err))
		ctx.Blocked = true
		ctx.ResponseStatus = http.StatusInternalServerError
		ctx.ResponseBody = "Internal Server Error"
		return nil
	}
	defer func() {
		_ = resp.Body.Close()
	}()

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

	ctx.Blocked = true
	ctx.ResponseStatus = int32(resp.StatusCode)

	limitReader := io.LimitReader(resp.Body, 16*1024)
	bodyBytes, err := io.ReadAll(limitReader)
	if err == nil {
		ctx.ResponseBody = string(bodyBytes)
	} else {
		mylogger.Error("firewall: failed to read response body from sidecar", zap.Error(err))
	}

	for _, k := range f.config.OnFailure.DownstreamPassThroughHeaders {
		if vals, ok := resp.Header[http.CanonicalHeaderKey(k)]; ok && len(vals) > 0 {
			ctx.SetHeaderDownstream(k, vals[0])
		}
	}

	return nil
}
