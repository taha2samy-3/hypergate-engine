package grpc

import (
	"net/http"
	"strings"

	"go.uber.org/zap"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"

	"github.com/taha2samy/hypergate/internal/clientip"
	"github.com/taha2samy/hypergate/internal/engine"
	mylogger "github.com/taha2samy/hypergate/internal/logger"
)

// streamState is the per-stream routing decision. It is made once, against the
// snapshot the stream acquired when it opened, and reused for every phase.
type streamState struct {
	snap     *engine.Snapshot
	resolved bool
	chain    engine.Chain
	name     string
}

// resolve picks the filter chain for the request. A route that points at a chain
// the snapshot does not contain fails closed; only "no route and no default chain"
// lets a request through without policy.
func (s *Server) resolve(st *streamState, reqCtx *engine.RequestContext) {
	if st.resolved {
		return
	}
	st.resolved = true

	if st.snap.Config == nil {
		mylogger.Error("No policy loaded, rejecting request", zap.String("path", reqCtx.Path))
		reqCtx.Block(http.StatusServiceUnavailable, "Service Unavailable")
		return
	}

	st.name = s.router.RouteWith(&st.snap.Config.Router, reqCtx)
	if st.name == "" {
		return
	}
	chain, ok := st.snap.Chains[st.name]
	if !ok {
		mylogger.Error("Route targets a chain that is not loaded, rejecting request",
			zap.String("chain", st.name), zap.String("path", reqCtx.Path))
		reqCtx.Block(http.StatusServiceUnavailable, "Service Unavailable")
		return
	}
	st.chain = chain
}

func (s *Server) run(st *streamState, reqCtx *engine.RequestContext, phase engine.Phase) {
	if reqCtx.Blocked || len(st.chain) == 0 {
		return
	}
	if err := s.executor.Execute(reqCtx, st.chain, phase); err != nil {
		mylogger.Error("Error executing chain", zap.String("chain", st.name), zap.Uint8("phase", uint8(phase)), zap.Error(err))
	}
}

// immediateResponse ends the request with the status and body set by the blocking filter.
func (s *Server) immediateResponse(stream extprocv3.ExternalProcessor_ProcessServer, reqCtx *engine.RequestContext) error {
	status := reqCtx.ResponseStatus
	if status == 0 {
		status = http.StatusForbidden
	}
	mylogger.Info("Request blocked by filter chain, sending ImmediateResponse",
		zap.String("path", reqCtx.Path),
		zap.Int32("status_code", status),
		zap.Uint8("phase", uint8(reqCtx.Phase)),
	)
	return stream.Send(&extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_ImmediateResponse{
			ImmediateResponse: &extprocv3.ImmediateResponse{
				Status:  &typev3.HttpStatus{Code: typev3.StatusCode(status)},
				Headers: s.buildHeaderMutation(reqCtx, reqCtx.ResponseHeadersToAdd, nil),
				Body:    []byte(reqCtx.ResponseBody),
			},
		},
	})
}

// handleRequestHeaders processes the initial metadata and headers of an incoming request.
func (s *Server) handleRequestHeaders(
	stream extprocv3.ExternalProcessor_ProcessServer,
	st *streamState,
	reqCtx *engine.RequestContext,
	req *extprocv3.ProcessingRequest,
	msg *extprocv3.HttpHeaders,
) error {
	mylogger.Debug("Received RequestHeaders phase")
	copyHeaders(reqCtx.Headers, msg.GetHeaders())

	reqCtx.Path = reqCtx.Headers[":path"]
	reqCtx.Method = reqCtx.Headers[":method"]
	reqCtx.RequestEndOfStream = msg.GetEndOfStream()

	hops := 0
	if st.snap.Config != nil {
		hops = st.snap.Config.Server.ClientIP.TrustedProxyHops
	}
	reqCtx.ClientIP = clientip.Resolve(reqCtx.Headers["x-forwarded-for"], clientip.PeerFromAttributes(req.GetAttributes()), hops)

	mylogger.Debug("Parsed RequestHeaders attributes",
		zap.String("path", reqCtx.Path),
		zap.String("method", reqCtx.Method),
		zap.String("client_ip", reqCtx.ClientIP),
	)

	s.resolve(st, reqCtx)
	s.run(st, reqCtx, engine.PhaseRequestHeaders)

	if reqCtx.Blocked {
		return s.immediateResponse(stream, reqCtx)
	}

	return stream.Send(&extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_RequestHeaders{
			RequestHeaders: &extprocv3.HeadersResponse{
				Response: &extprocv3.CommonResponse{
					HeaderMutation: s.buildHeaderMutation(reqCtx, reqCtx.HeadersToAdd, reqCtx.HeadersToRemove),
				},
			},
		},
		ModeOverride: s.buildModeOverride(reqCtx),
	})
}

// handleRequestBody processes the payload of the request if buffering is enabled.
func (s *Server) handleRequestBody(
	stream extprocv3.ExternalProcessor_ProcessServer,
	st *streamState,
	reqCtx *engine.RequestContext,
	msg *extprocv3.HttpBody,
) error {
	mylogger.Debug("Received RequestBody phase")
	reqCtx.RequestBodySeen = true
	bodyLen := len(msg.Body)
	if bodyLen <= cap(reqCtx.RawBodyBuffer) {
		reqCtx.RawBodyBuffer = append(reqCtx.RawBodyBuffer[:0], msg.Body...)
		reqCtx.RequestBody = reqCtx.RawBodyBuffer
	} else {
		// Fallback for bodies larger than the pooled buffer
		reqCtx.RequestBody = msg.Body
	}

	s.resolve(st, reqCtx)
	s.run(st, reqCtx, engine.PhaseRequestBody)

	if reqCtx.Blocked {
		return s.immediateResponse(stream, reqCtx)
	}

	return stream.Send(&extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_RequestBody{
			RequestBody: &extprocv3.BodyResponse{
				Response: &extprocv3.CommonResponse{
					// In buffered mode the request headers are still held by Envoy, so header
					// changes made while inspecting the body (e.g. by a firewall) are applied too.
					// Mutations already sent with the headers are idempotent and harmless to repeat.
					HeaderMutation: s.buildHeaderMutation(reqCtx, reqCtx.HeadersToAdd, reqCtx.HeadersToRemove),
					BodyMutation:   s.buildBodyMutation(reqCtx.RequestBody, reqCtx.RequestBodyModified),
				},
			},
		},
	})
}

// handleRequestTrailers processes any gRPC or HTTP trailers sent with the request.
func (s *Server) handleRequestTrailers(
	stream extprocv3.ExternalProcessor_ProcessServer,
	st *streamState,
	reqCtx *engine.RequestContext,
	msg *extprocv3.HttpTrailers,
) error {
	mylogger.Debug("Received RequestTrailers phase")
	copyHeaders(reqCtx.Headers, msg.GetTrailers())

	s.resolve(st, reqCtx)
	s.run(st, reqCtx, engine.PhaseRequestTrailers)

	if reqCtx.Blocked {
		return s.immediateResponse(stream, reqCtx)
	}

	return stream.Send(&extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_RequestTrailers{
			RequestTrailers: &extprocv3.TrailersResponse{
				HeaderMutation: s.buildHeaderMutation(reqCtx, reqCtx.RequestTrailersToAdd, reqCtx.RequestTrailersToRemove),
			},
		},
	})
}

// handleResponseHeaders processes the headers returned by the upstream service.
func (s *Server) handleResponseHeaders(
	stream extprocv3.ExternalProcessor_ProcessServer,
	st *streamState,
	reqCtx *engine.RequestContext,
	msg *extprocv3.HttpHeaders,
) error {
	mylogger.Debug("Received ResponseHeaders phase")
	copyHeaders(reqCtx.ResponseHeaders, msg.GetHeaders())

	s.resolve(st, reqCtx)

	// A filter asked to inspect the request body but Envoy forwarded the request
	// without sending it (mode override not allowed by the Envoy ext_proc config).
	// The inspection never happened, so do not let the response through.
	if !reqCtx.Blocked && reqCtx.RequestBodyRequired && !reqCtx.RequestBodySeen && !reqCtx.RequestEndOfStream {
		mylogger.Error("Request body inspection was required but Envoy never sent the body; " +
			"set allow_mode_override: true (or request_body_mode: BUFFERED) on the ext_proc filter")
		reqCtx.Block(http.StatusInternalServerError, "Internal Server Error")
	}

	s.run(st, reqCtx, engine.PhaseResponseHeaders)

	if reqCtx.Blocked {
		return s.immediateResponse(stream, reqCtx)
	}

	return stream.Send(&extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_ResponseHeaders{
			ResponseHeaders: &extprocv3.HeadersResponse{
				Response: &extprocv3.CommonResponse{
					HeaderMutation: s.buildHeaderMutation(reqCtx, reqCtx.ResponseHeadersToAdd, reqCtx.ResponseHeadersToRemove),
				},
			},
		},
		ModeOverride: s.buildModeOverride(reqCtx),
	})
}

// handleResponseBody processes the upstream response body.
func (s *Server) handleResponseBody(
	stream extprocv3.ExternalProcessor_ProcessServer,
	st *streamState,
	reqCtx *engine.RequestContext,
	msg *extprocv3.HttpBody,
) error {
	mylogger.Debug("Received ResponseBody phase")
	bodyLen := len(msg.Body)
	if bodyLen <= cap(reqCtx.RawBodyBuffer) {
		reqCtx.RawBodyBuffer = append(reqCtx.RawBodyBuffer[:0], msg.Body...)
		reqCtx.ResponseBodyBytes = reqCtx.RawBodyBuffer
	} else {
		// Fallback for response bodies larger than the pooled buffer
		reqCtx.ResponseBodyBytes = msg.Body
	}

	s.resolve(st, reqCtx)
	s.run(st, reqCtx, engine.PhaseResponseBody)

	if reqCtx.Blocked {
		return s.immediateResponse(stream, reqCtx)
	}

	return stream.Send(&extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_ResponseBody{
			ResponseBody: &extprocv3.BodyResponse{
				Response: &extprocv3.CommonResponse{
					BodyMutation: s.buildBodyMutation(reqCtx.ResponseBodyBytes, reqCtx.ResponseBodyModified),
				},
			},
		},
	})
}

// handleResponseTrailers processes the upstream response trailers.
func (s *Server) handleResponseTrailers(
	stream extprocv3.ExternalProcessor_ProcessServer,
	st *streamState,
	reqCtx *engine.RequestContext,
	msg *extprocv3.HttpTrailers,
) error {
	mylogger.Debug("Received ResponseTrailers phase")
	copyHeaders(reqCtx.ResponseHeaders, msg.GetTrailers())

	s.resolve(st, reqCtx)
	s.run(st, reqCtx, engine.PhaseResponseTrailers)

	// Response headers have already been sent downstream, so a block can no longer
	// replace the response; trailers are forwarded with the requested mutations.
	return stream.Send(&extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_ResponseTrailers{
			ResponseTrailers: &extprocv3.TrailersResponse{
				HeaderMutation: s.buildHeaderMutation(reqCtx, reqCtx.ResponseTrailersToAdd, reqCtx.ResponseTrailersToRemove),
			},
		},
	})
}

// copyHeaders lower-cases keys and folds repeated headers into one value.
func copyHeaders(dst map[string]string, headers *corev3.HeaderMap) {
	if headers == nil {
		return
	}
	for _, h := range headers.Headers {
		key := strings.ToLower(h.Key)
		val := h.Value
		if len(h.RawValue) > 0 {
			val = string(h.RawValue)
		}
		if prev, ok := dst[key]; ok && prev != "" {
			sep := ", "
			if key == "cookie" {
				sep = "; "
			}
			val = prev + sep + val
		}
		dst[key] = val
	}
}
