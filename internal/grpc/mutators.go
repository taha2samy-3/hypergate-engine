package grpc

import (
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocfilterv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_proc/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"

	"github.com/taha/myprog/internal/engine"
)

// buildHeaderMutation transforms internal Header additions and removals into Envoy's Protobuf format.
func (s *Server) buildHeaderMutation(headers []engine.Header, removes []string) *extprocv3.HeaderMutation {
	var setHeaders []*corev3.HeaderValueOption
	for _, h := range headers {
		setHeaders = append(setHeaders, &corev3.HeaderValueOption{
			Header: &corev3.HeaderValue{
				Key:      h.Key,
				RawValue: []byte(h.Value),
			},
			// Default action is to overwrite if the header already exists, or add it if not.
			AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
		})
	}
	return &extprocv3.HeaderMutation{
		SetHeaders:    setHeaders,
		RemoveHeaders: removes,
	}
}

// buildBodyMutation prepares the body modification instruction for Envoy.
func (s *Server) buildBodyMutation(body []byte, modified bool) *extprocv3.BodyMutation {
	if !modified {
		return nil
	}
	// If the body is modified but empty, we send a ClearBody instruction.
	if len(body) == 0 {
		return &extprocv3.BodyMutation{
			Mutation: &extprocv3.BodyMutation_ClearBody{
				ClearBody: true,
			},
		}
	}
	// Otherwise, we send the new body content.
	return &extprocv3.BodyMutation{
		Mutation: &extprocv3.BodyMutation_Body{
			Body: body,
		},
	}
}

// buildModeOverride determines if we need to change how Envoy processes subsequent phases
// of the current request (e.g., asking Envoy to send the body if a filter needs it).
func (s *Server) buildModeOverride(reqCtx *engine.RequestContext) *extprocfilterv3.ProcessingMode {
	// If no filters requested body or trailer access, we don't need an override.
	if !reqCtx.RequestBodyRequired && !reqCtx.ResponseBodyRequired && 
	   !reqCtx.RequestTrailersRequired && !reqCtx.ResponseTrailersRequired {
		return nil
	}

	mode := &extprocfilterv3.ProcessingMode{
		RequestHeaderMode:  extprocfilterv3.ProcessingMode_SEND,
		ResponseHeaderMode: extprocfilterv3.ProcessingMode_SEND,
	}

	// Handle Request Body Mode
	if reqCtx.RequestBodyRequired {
		mode.RequestBodyMode = extprocfilterv3.ProcessingMode_BUFFERED
	} else {
		mode.RequestBodyMode = extprocfilterv3.ProcessingMode_NONE
	}

	// Handle Response Body Mode
	if reqCtx.ResponseBodyRequired {
		mode.ResponseBodyMode = extprocfilterv3.ProcessingMode_BUFFERED
	} else {
		mode.ResponseBodyMode = extprocfilterv3.ProcessingMode_NONE
	}

	// Handle Request Trailers Mode
	if reqCtx.RequestTrailersRequired {
		mode.RequestTrailerMode = extprocfilterv3.ProcessingMode_SEND
	} else {
		mode.RequestTrailerMode = extprocfilterv3.ProcessingMode_SKIP
	}

	// Handle Response Trailers Mode
	if reqCtx.ResponseTrailersRequired {
		mode.ResponseTrailerMode = extprocfilterv3.ProcessingMode_SEND
	} else {
		mode.ResponseTrailerMode = extprocfilterv3.ProcessingMode_SKIP
	}

	return mode
}