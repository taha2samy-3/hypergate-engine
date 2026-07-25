package firewall

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"strings"

	"go.uber.org/zap"

	"github.com/taha/myprog/internal/config"
	"github.com/taha/myprog/internal/engine"
	mylogger "github.com/taha/myprog/internal/logger"
)

// FirewallFilter implements the engine.Filter interface to perform request inspection
// (Headers, URI, Request Body) over a local Unix Domain Socket (UDS) against a firewall sidecar.
type FirewallFilter struct {
	config *config.FirewallFilterConfig
	client *http.Client
}

// NewFirewallFilter initializes a reusable http.Client configured to communicate with the firewall sidecar over UDS.
func NewFirewallFilter(cfg *config.FirewallFilterConfig) (*FirewallFilter, error) {
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

	return &FirewallFilter{
		config: cfg,
		client: client,
	}, nil
}

// Execute inspects the request context and delegates WAF evaluation to the firewall sidecar over UDS.
func (f *FirewallFilter) Execute(ctx *engine.RequestContext) error {
	// Signal to engine if full request body inspection is required
	if f.config.InspectBody {
		ctx.RequestBodyRequired = true
	}

	// Defensive nil check for stream context
	reqCtx := ctx.Ctx
	if reqCtx == nil {
		reqCtx = context.Background()
	}

	// Select method based on payload presence
	method := http.MethodGet
	if len(ctx.RequestBody) > 0 {
		method = http.MethodPost
	}

	// Attach request body payload if body inspection is enabled and payload exists
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

	// Determine header forwarding strategy (Allow-list or Forward All)
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

	// Execute communication over Unix Domain Socket
	resp, err := f.client.Do(req)
	if err != nil {
		mylogger.Error("firewall: UDS sidecar communication failed", zap.Error(err))
		ctx.Blocked = true
		ctx.ResponseStatus = http.StatusInternalServerError
		ctx.ResponseBody = "Internal Server Error"
		return nil
	}
	defer resp.Body.Close()

	// 2xx WAF Pass
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

	// Non-2xx WAF Threat Blocked
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
