package grpc

import (
	"go.uber.org/zap"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"

	"github.com/taha/myprog/internal/engine"
	mylogger "github.com/taha/myprog/internal/logger"
)

// handleRequestHeaders processes the initial metadata and headers of an incoming request.
func (s *Server) handleRequestHeaders(
	stream extprocv3.ExternalProcessor_ProcessServer,
	reqCtx *engine.RequestContext,
	msg *extprocv3.HttpHeaders,
) (string, error) {
	mylogger.Debug("Received RequestHeaders phase")
	headers := msg.Headers
	if headers != nil {
		for _, h := range headers.Headers {
			key := h.Key
			var val string
			if len(h.RawValue) > 0 {
				val = string(h.RawValue)
			} else {
				val = h.Value
			}
			reqCtx.Headers[key] = val
		}
	}

	reqCtx.Path = reqCtx.Headers[":path"]
	reqCtx.Method = reqCtx.Headers[":method"]

	mylogger.Debug("Parsed RequestHeaders attributes",
		zap.String("path", reqCtx.Path),
		zap.String("method", reqCtx.Method),
	)

	// Routing logic to find the appropriate filter chain.
	targetChainName := s.router.Route(reqCtx)
	if targetChainName != "" {
		chain, exists := s.registry.Get(targetChainName)
		if !exists {
			mylogger.Warn("Target chain not found in registry", zap.String("chain", targetChainName))
		} else {
			if err := s.executor.Execute(reqCtx, chain); err != nil {
				mylogger.Error("Error executing chain", zap.String("chain", targetChainName), zap.Error(err))
			}
		}
	}

	// If a filter blocked the request, send an ImmediateResponse (e.g., 401, 403, 429).
	if reqCtx.Blocked {
		mylogger.Info("Request blocked by filter chain, sending ImmediateResponse",
			zap.String("path", reqCtx.Path),
			zap.Int32("status_code", reqCtx.ResponseStatus),
		)

		resp := &extprocv3.ProcessingResponse{
			Response: &extprocv3.ProcessingResponse_ImmediateResponse{
				ImmediateResponse: &extprocv3.ImmediateResponse{
					Status: &typev3.HttpStatus{
						Code: typev3.StatusCode(reqCtx.ResponseStatus),
					},
					Headers: s.buildHeaderMutation(reqCtx.ResponseHeadersToAdd, nil),
					Body:    []byte(reqCtx.ResponseBody),
				},
			},
		}
		return targetChainName, stream.Send(resp)
	}

	// Normal path: send mutations and potential mode overrides.
	resp := &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_RequestHeaders{
			RequestHeaders: &extprocv3.HeadersResponse{
				Response: &extprocv3.CommonResponse{
					HeaderMutation: s.buildHeaderMutation(reqCtx.HeadersToAdd, reqCtx.HeadersToRemove),
				},
			},
		},
		ModeOverride: s.buildModeOverride(reqCtx),
	}
	return targetChainName, stream.Send(resp)
}

// handleRequestBody processes the payload of the request if buffering is enabled.
func (s *Server) handleRequestBody(
	stream extprocv3.ExternalProcessor_ProcessServer,
	reqCtx *engine.RequestContext,
	msg *extprocv3.HttpBody,
	targetChainName string,
) error {
	mylogger.Debug("Received RequestBody phase")
	reqCtx.RequestBody = msg.Body

	if targetChainName != "" {
		chain, exists := s.registry.Get(targetChainName)
		if exists {
			if err := s.executor.Execute(reqCtx, chain); err != nil {
				mylogger.Error("Error executing chain on body", zap.Error(err))
			}
		}
	}

	resp := &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_RequestBody{
			RequestBody: &extprocv3.BodyResponse{
				Response: &extprocv3.CommonResponse{
					BodyMutation: s.buildBodyMutation(reqCtx.RequestBody, reqCtx.RequestBodyModified),
				},
			},
		},
	}
	return stream.Send(resp)
}

// handleRequestTrailers processes any gRPC or HTTP trailers sent with the request.
func (s *Server) handleRequestTrailers(
	stream extprocv3.ExternalProcessor_ProcessServer,
	reqCtx *engine.RequestContext,
	msg *extprocv3.HttpTrailers,
	targetChainName string,
) error {
	mylogger.Debug("Received RequestTrailers phase")
	trailers := msg.Trailers
	if trailers != nil {
		for _, h := range trailers.Headers {
			key := h.Key
			var val string
			if len(h.RawValue) > 0 {
				val = string(h.RawValue)
			} else {
				val = h.Value
			}
			reqCtx.Headers[key] = val
		}
	}

	if targetChainName != "" {
		chain, exists := s.registry.Get(targetChainName)
		if exists {
			if err := s.executor.Execute(reqCtx, chain); err != nil {
				mylogger.Error("Error executing chain on request trailers", zap.Error(err))
			}
		}
	}

	resp := &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_RequestTrailers{
			RequestTrailers: &extprocv3.TrailersResponse{
				HeaderMutation: s.buildHeaderMutation(reqCtx.RequestTrailersToAdd, reqCtx.RequestTrailersToRemove),
			},
		},
	}
	return stream.Send(resp)
}

// handleResponseHeaders processes the headers returned by the upstream service.
func (s *Server) handleResponseHeaders(
	stream extprocv3.ExternalProcessor_ProcessServer,
	reqCtx *engine.RequestContext,
	msg *extprocv3.HttpHeaders,
) error {
	mylogger.Debug("Received ResponseHeaders phase")
	headers := msg.Headers
	if headers != nil {
		for _, h := range headers.Headers {
			key := h.Key
			var val string
			if len(h.RawValue) > 0 {
				val = string(h.RawValue)
			} else {
				val = h.Value
			}
			reqCtx.Headers[key] = val
		}
	}

	resp := &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_ResponseHeaders{
			ResponseHeaders: &extprocv3.HeadersResponse{
				Response: &extprocv3.CommonResponse{
					HeaderMutation: s.buildHeaderMutation(reqCtx.ResponseHeadersToAdd, nil),
				},
			},
		},
		ModeOverride: s.buildModeOverride(reqCtx),
	}
	return stream.Send(resp)
}

// handleResponseBody processes the upstream response body.
func (s *Server) handleResponseBody(
	stream extprocv3.ExternalProcessor_ProcessServer,
	reqCtx *engine.RequestContext,
	msg *extprocv3.HttpBody,
	targetChainName string,
) error {
	mylogger.Debug("Received ResponseBody phase")
	reqCtx.ResponseBodyBytes = msg.Body

	if targetChainName != "" {
		chain, exists := s.registry.Get(targetChainName)
		if exists {
			if err := s.executor.Execute(reqCtx, chain); err != nil {
				mylogger.Error("Error executing chain on response body", zap.Error(err))
			}
		}
	}

	resp := &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_ResponseBody{
			ResponseBody: &extprocv3.BodyResponse{
				Response: &extprocv3.CommonResponse{
					BodyMutation: s.buildBodyMutation(reqCtx.ResponseBodyBytes, reqCtx.ResponseBodyModified),
				},
			},
		},
	}
	return stream.Send(resp)
}

// handleResponseTrailers processes the upstream response trailers.
func (s *Server) handleResponseTrailers(
	stream extprocv3.ExternalProcessor_ProcessServer,
	reqCtx *engine.RequestContext,
	msg *extprocv3.HttpTrailers,
	targetChainName string,
) error {
	mylogger.Debug("Received ResponseTrailers phase")
	trailers := msg.Trailers
	if trailers != nil {
		for _, h := range trailers.Headers {
			key := h.Key
			var val string
			if len(h.RawValue) > 0 {
				val = string(h.RawValue)
			} else {
				val = h.Value
			}
			reqCtx.Headers[key] = val
		}
	}

	if targetChainName != "" {
		chain, exists := s.registry.Get(targetChainName)
		if exists {
			if err := s.executor.Execute(reqCtx, chain); err != nil {
				mylogger.Error("Error executing chain on response trailers", zap.Error(err))
			}
		}
	}

	resp := &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_ResponseTrailers{
			ResponseTrailers: &extprocv3.TrailersResponse{
				HeaderMutation: s.buildHeaderMutation(reqCtx.ResponseTrailersToAdd, reqCtx.ResponseTrailersToRemove),
			},
		},
	}
	return stream.Send(resp)
}