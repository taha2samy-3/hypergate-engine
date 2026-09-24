package engine

// Phase represents a specific processing stage in the Envoy ext_proc lifecycle.
// Each phase corresponds to one of the six possible message types Envoy sends.
type Phase uint8

const (
	// PhaseRequestHeaders is called once per request with request headers and path/method.
	PhaseRequestHeaders Phase = iota
	// PhaseRequestBody is called when Envoy sends the buffered request body.
	PhaseRequestBody
	// PhaseRequestTrailers is called when Envoy sends request trailers.
	PhaseRequestTrailers
	// PhaseResponseHeaders is called when the upstream response headers arrive.
	PhaseResponseHeaders
	// PhaseResponseBody is called when Envoy sends the buffered response body.
	PhaseResponseBody
	// PhaseResponseTrailers is called when Envoy sends response trailers.
	PhaseResponseTrailers
)

// PhaseAware is an optional interface that filters can implement to declare
// which phases they wish to handle. Filters that do NOT implement PhaseAware
// are treated as PhaseRequestHeaders-only (legacy behaviour preserved).
type PhaseAware interface {
	// SupportedPhases returns the set of phases this filter should execute on.
	SupportedPhases() []Phase
}
