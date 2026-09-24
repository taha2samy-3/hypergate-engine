// Package redis provides topology-aware (SINGLE / CLUSTER / SENTINEL) Redis
// clients with grouped pipelining.
//
// Client lifecycles across config reloads are owned by internal/policy, which
// reuses clients whose configuration did not change and closes retired clients
// only after the last request using them has finished.
package redis

import (
	"context"

	"github.com/mediocregopher/radix/v4"
)

// ---------------------------------------------------------------------------
// Pipeline Types
// ---------------------------------------------------------------------------

// PipelineAction pairs a radix Action with its routing key so the manager can
// dispatch the action to the correct shard in a cluster topology.
type PipelineAction struct {
	// Action is the radix command to execute.
	Action radix.Action

	// Key is the Redis key targeted by Action. Required for cluster hash-slot
	// routing; may be empty for non-key commands in SINGLE mode.
	Key string
}

// Pipeline is an ordered slice of PipelineActions that are flushed atomically
// via a single round-trip to the Redis server (or shard, in CLUSTER mode).
type Pipeline []PipelineAction

// ---------------------------------------------------------------------------
// Client Interface
// ---------------------------------------------------------------------------

// Client abstracts the underlying radix connection pool so that callers are
// decoupled from the concrete topology (SINGLE / CLUSTER / SENTINEL) and unit
// tests can inject fakes without a running Redis instance.
//
// All methods are safe for concurrent use. The implementation returned by
// pool.go is backed by a radix pool whose internal synchronisation ensures
// that concurrent DoCmd / PipeDo calls do not race on the underlying TCP
// connection.
type Client interface {
	// DoCmd executes a single Redis command and scans the reply into rcv.
	// ctx controls cancellation and the deadline of the call.
	// cmd is the Redis command string (e.g. "GET", "SET").
	// key is the target key (used for cluster slot routing).
	// args contains any additional arguments after the key.
	//
	// Returns a wrapped error on network, protocol, or Redis-level failure.
	DoCmd(ctx context.Context, rcv interface{}, cmd, key string, args ...interface{}) error

	// PipeAppend appends a new action to pipeline and returns the updated
	// pipeline. It does not flush; call PipeDo to execute the batch.
	//
	// This method allocates no heap memory on the hot path when the caller
	// pre-allocates the Pipeline slice.
	PipeAppend(pipeline Pipeline, rcv interface{}, cmd, key string, args ...interface{}) Pipeline

	// PipeDo executes all actions accumulated in pipeline as a single atomic
	// batch. ctx controls the overall deadline/cancellation of the flush.
	//
	// If the context is cancelled before the flush completes, the in-flight
	// pipeline is abandoned and the connection is returned to the pool in a
	// clean state (radix handles this internally via conn.Encode+Decode).
	PipeDo(ctx context.Context, pipeline Pipeline) error

	// Close tears down the connection pool, releasing all file descriptors and
	// TCP sockets. After Close returns, any concurrent or subsequent call to
	// DoCmd / PipeDo will return an error.
	//
	// Close is idempotent; calling it more than once is safe.
	Close() error

	// NumActiveConns returns the current number of connections checked out of
	// the pool (i.e. in active use by callers). This is a point-in-time
	// snapshot suitable for metrics / health-check endpoints.
	NumActiveConns() int
}
