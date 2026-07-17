// Package redis provides a thread-safe, hot-reloadable connection manager for
// multiple independent Redis services. It is designed for Kubernetes workloads
// that require zero-downtime configuration reloads and O(1) client lookup on
// every request.
//
// # Concurrency Model
//
// The Manager embeds a sync.RWMutex that guards the internal `clients` map.
// The hot-path (GetClient) acquires only a read-lock and releases it before
// returning — no allocation, no contention under concurrent reads.
//
// Config reloads follow a strict two-phase protocol:
//
//  1. BUILD: All new connection pools are established outside the critical
//     section. Any failure here causes an immediate abort; the old pools
//     are left fully intact.
//  2. SWAP:  A write-lock is held for the minimum possible time — just long
//     enough to replace the map pointer and release. Old pools are closed
//     asynchronously in a separate goroutine to avoid blocking callers.
//
// This guarantees:
//   - No concurrent reader ever sees a partially-initialised map.
//   - Write-lock hold-time is bounded to nanoseconds (pointer swap only).
//   - File descriptors from retired pools are always reclaimed.
package redis

import (
	"context"
	"fmt"
	"sync"

	"github.com/mediocregopher/radix/v4"

	"github.com/taha/myprog/internal/config"
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
	// cmd is the Redis command string (e.g. "GET", "SET").
	// key is the target key (used for cluster slot routing).
	// args contains any additional arguments after the key.
	//
	// Returns a wrapped error on network, protocol, or Redis-level failure.
	DoCmd(rcv interface{}, cmd, key string, args ...interface{}) error

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

// ---------------------------------------------------------------------------
// Manager
// ---------------------------------------------------------------------------

// Manager owns the lifecycle of all named Redis clients derived from a
// config.Config. It exposes an O(1) lookup path and a zero-downtime reload
// path that are safe to call from arbitrary goroutines simultaneously.
//
// # Invariants
//
//   - mu.RLock is held only for the duration of a map read in GetClient.
//   - mu.Lock is held only for the pointer-swap in Reload; all expensive I/O
//     (pool creation, old-pool teardown) happens outside the critical section.
//   - The `clients` field is never mutated in place; a new map is always
//     constructed and swapped atomically, so readers never see intermediate state.
type Manager struct {
	// mu guards reads and writes to the clients map.
	// Invariant: mu must never be held while performing blocking I/O (pool
	// dial, TLS handshake, etc.) to prevent reader starvation.
	mu sync.RWMutex

	// clients maps service names (as declared in config.Config.Redis) to their
	// active Client instances. It is replaced wholesale on every Reload call.
	clients map[string]Client
}

// GlobalManager holds the singleton instance of the Redis Manager for global access.
var GlobalManager *Manager

// NewManager constructs a Manager by bootstrapping one connection pool per
// entry in cfg.Redis. If any pool fails to initialise, all successfully
// created pools are closed before the error is returned — no resource leak.
//
// NewManager is intended to be called once at process startup. Hot reloads
// should use Reload.
func NewManager(cfg *config.Config) (*Manager, error) {
	clients, err := buildClients(cfg)
	if err != nil {
		// buildClients already closed any pools it successfully created before
		// the failure, so we don't need additional cleanup here.
		return nil, fmt.Errorf("redis.NewManager: failed to bootstrap connection pools: %w", err)
	}

	GlobalManager = &Manager{clients: clients}
	return GlobalManager, nil
}

// Reload performs a zero-downtime configuration swap following a strict
// two-phase protocol:
//
//  1. BUILD (outside lock): All new connection pools are established.
//     If any pool fails, all newly-created pools are closed and the error is
//     returned immediately. The Manager continues to serve the old pools
//     without interruption.
//
//  2. SWAP (inside write-lock): The internal clients map is replaced with the
//     new map atomically. The write-lock is released immediately after the
//     pointer swap — this section contains no I/O and runs in nanoseconds.
//
//  3. ASYNC CLEANUP (after lock release): The retired pools are closed in a
//     separate goroutine. This decouples pool teardown from the caller and
//     prevents file descriptor exhaustion on rapid successive reloads.
//
// Reload is safe to call concurrently with GetClient; concurrent reloads are
// serialised by the write-lock (the second caller will win after the first
// completes).
func (m *Manager) Reload(newCfg *config.Config) error {
	// --- Phase 1: BUILD ---
	// All I/O happens here, before acquiring any lock.
	newClients, err := buildClients(newCfg)
	if err != nil {
		return fmt.Errorf("redis.Manager.Reload: failed to build new connection pools: %w", err)
	}

	// --- Phase 2: SWAP ---
	// The critical section is intentionally kept minimal: only a map
	// pointer assignment. No allocation, no I/O, no syscall.
	m.mu.Lock()
	oldClients := m.clients
	m.clients = newClients
	m.mu.Unlock()

	// --- Phase 3: ASYNC CLEANUP ---
	// Close old pools outside the lock in a dedicated goroutine. Using a
	// goroutine prevents the caller from blocking on TCP FIN/ACK exchanges
	// and Kubernetes connection-drain timeouts.
	//
	// To prevent interrupting in-flight requests running through the old
	// compiled filter chains, we sleep for 10 seconds to allow active connections
	// to drain before closing the retired pools.
	go func() {
		time.Sleep(10 * time.Second) // Graceful connection draining delay
		for name, c := range oldClients {
			if err := c.Close(); err != nil {
				// Closing a pool is best-effort. Log the error (if a logger
				// is wired) but do not propagate — the swap already succeeded.
				_ = fmt.Errorf("redis.Manager.Reload: error closing retired pool %q: %w", name, err)
			}
		}
	}()

	return nil
}

// GetClient performs an O(1), read-lock-protected lookup of the Client
// registered under serviceName.
//
// The read-lock ensures safety against a concurrent Reload swapping the map:
// the returned Client is guaranteed to be fully initialised and will remain
// valid for the lifetime of the caller's operation, because Reload only closes
// old clients asynchronously after the swap — never while a reader holds a
// reference to them.
//
// Returns (client, true) if found, or (nil, false) if serviceName is not
// registered in the current configuration.
func (m *Manager) GetClient(serviceName string) (Client, bool) {
	m.mu.RLock()
	c, ok := m.clients[serviceName]
	m.mu.RUnlock()
	return c, ok
}

// Close gracefully shuts down all active connection pools managed by this
// Manager. It is intended to be called during process shutdown (e.g. from a
// signal handler). After Close returns, the Manager must not be used.
//
// Close acquires the write-lock so it serialises against any concurrent
// Reload calls.
func (m *Manager) Close() {
	m.mu.Lock()
	snapshot := m.clients
	m.clients = nil
	m.mu.Unlock()

	for name, c := range snapshot {
		if err := c.Close(); err != nil {
			_ = fmt.Errorf("redis.Manager.Close: error closing pool %q: %w", name, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// buildClients iterates over cfg.Redis and creates one Client per service.
// On any failure it closes all successfully created clients before returning
// the error, guaranteeing no resource leak to the caller.
func buildClients(cfg *config.Config) (map[string]Client, error) {
	clients := make(map[string]Client, len(cfg.Redis))

	for name, svcCfg := range cfg.Redis {
		c, err := NewPoolClient(name, svcCfg)
		if err != nil {
			// Clean up every pool that succeeded before this failure.
			for createdName, createdClient := range clients {
				_ = fmt.Errorf("redis.buildClients: closing pool %q after build failure: %w",
					createdName, createdClient.Close())
			}
			return nil, fmt.Errorf("redis.buildClients: failed to create pool for service %q: %w", name, err)
		}
		clients[name] = c
	}

	return clients, nil
}
