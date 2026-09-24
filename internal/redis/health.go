// Package redis — health.go
//
// health.go implements an active connection health monitor that periodically
// sends PING commands to the underlying Redis pool and reflects the result in a
// HealthChecker whose status is exposed to Kubernetes liveness / readiness probes.
//
// # Architecture
//
// HealthChecker is a lightweight, self-contained component. It stores an
// atomic health state so that HTTP probe handlers can read it with zero lock
// contention. The state transitions follow a simple two-state machine:
//
//	healthy  ──(PING fails)──► unhealthy
//	unhealthy ──(PING ok)────► healthy
//
// StartHealthCheck launches a single background goroutine tied to the
// application context. When the context is cancelled (e.g. SIGTERM) the
// goroutine exits cleanly — no leaked timers, no leaked goroutines.
//
// # Integration with Kubernetes
//
// Expose HealthChecker.IsHealthy via an HTTP handler on the liveness or
// readiness probe endpoint. Example:
//
//	http.HandleFunc("/healthz/redis", func(w http.ResponseWriter, r *http.Request) {
//	    if !hc.IsHealthy() {
//	        http.Error(w, "redis unhealthy", http.StatusServiceUnavailable)
//	        return
//	    }
//	    w.WriteHeader(http.StatusOK)
//	})
//
// # Concurrency
//
//   - HealthChecker.state is an int32 accessed via sync/atomic — all reads and
//     writes are race-free without any mutex.
//   - StartHealthCheck owns the single writer goroutine. Multiple concurrent
//     readers (HTTP probe handlers) are always safe.
package redis

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/mediocregopher/radix/v4"
	mylogger "github.com/taha2samy/hypergate/internal/logger"
	"go.uber.org/zap"
)

// ---------------------------------------------------------------------------
// Health state constants
// ---------------------------------------------------------------------------

const (
	// healthStateHealthy is the int32 value stored in HealthChecker.state when
	// the most recent PING succeeded.
	healthStateHealthy int32 = 0

	// healthStateUnhealthy is the int32 value stored when PING failed or timed
	// out.
	healthStateUnhealthy int32 = 1

	// RedisHealthComponentName is the canonical string identifier used in log
	// messages and health-status reporting for the Redis subsystem.
	RedisHealthComponentName = "redis"

	// defaultPingTimeout is the maximum duration a PING is allowed to take
	// before it is considered a failure. Kept tight (200 ms) so that a slow
	// Redis node is detected quickly without blocking the probe handler.
	defaultPingTimeout = 200 * time.Millisecond
)

// ---------------------------------------------------------------------------
// HealthChecker
// ---------------------------------------------------------------------------

// HealthChecker tracks the liveness of a single Redis service. It is safe for
// concurrent use: IsHealthy can be called from any number of goroutines while
// the background monitor goroutine writes state via atomic operations.
//
// Zero value is healthy — create with NewHealthChecker.
type HealthChecker struct {
	// state is 0 (healthy) or 1 (unhealthy). Accessed exclusively via
	// sync/atomic to guarantee visibility across goroutines without a mutex.
	state int32

	// serviceName is used in log messages only; it is immutable after
	// construction.
	serviceName string
}

// NewHealthChecker returns a HealthChecker that starts in the healthy state.
// serviceName is used only for observability (log messages).
func NewHealthChecker(serviceName string) *HealthChecker {
	return &HealthChecker{
		state:       healthStateHealthy,
		serviceName: serviceName,
	}
}

// IsHealthy returns true if the most recent PING succeeded.
// This method is safe for concurrent use and imposes no lock.
func (h *HealthChecker) IsHealthy() bool {
	return atomic.LoadInt32(&h.state) == healthStateHealthy
}

// fail transitions the checker to the unhealthy state. Calling fail when
// already unhealthy is a no-op (the atomic swap returns the previous value so
// we can detect duplicate transitions and suppress redundant log lines).
func (h *HealthChecker) fail() {
	prev := atomic.SwapInt32(&h.state, healthStateUnhealthy)
	if prev != healthStateUnhealthy {
		mylogger.Error("Redis health check: service marked UNHEALTHY",
			zap.String("service", h.serviceName),
		)
	}
}

// ok transitions the checker back to the healthy state. Calling ok when
// already healthy is a no-op.
func (h *HealthChecker) ok() {
	prev := atomic.SwapInt32(&h.state, healthStateHealthy)
	if prev != healthStateHealthy {
		mylogger.Info("Redis health check: service restored to HEALTHY",
			zap.String("service", h.serviceName),
		)
	}
}

// ---------------------------------------------------------------------------
// StartHealthCheck — background monitor goroutine
// ---------------------------------------------------------------------------

// StartHealthCheck launches a non-blocking background goroutine that probes
// the given Client by executing a PING at the specified interval.
//
// Parameters:
//
//   - ctx        — application lifetime context; the goroutine exits when ctx
//     is cancelled, making cleanup automatic on SIGTERM.
//   - client     — the Redis Client to probe. The probe runs outside any
//     user-request path so it does not affect request latency.
//   - checker    — the HealthChecker whose state is updated based on probe
//     outcomes.
//   - interval   — how often to send PING. Recommended: 5 * time.Second.
//
// The goroutine is launched immediately and begins probing after the first
// interval tick (not at time zero) so that the service has time to finish
// initialising before the first probe runs.
//
// StartHealthCheck does not block; it returns as soon as the goroutine is
// scheduled.
func StartHealthCheck(
	ctx context.Context,
	client Client,
	checker *HealthChecker,
	interval time.Duration,
) {
	if interval <= 0 {
		interval = 5 * time.Second
	}

	go runHealthCheckLoop(ctx, client, checker, interval)
}

// runHealthCheckLoop is the body of the background goroutine. It uses a
// time.Ticker (not time.After) to avoid creating a new timer on every
// iteration, which would be wasteful in a hot loop.
func runHealthCheckLoop(
	ctx context.Context,
	client Client,
	checker *HealthChecker,
	interval time.Duration,
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	mylogger.Info("Redis health monitor started",
		zap.String("service", checker.serviceName),
		zap.Duration("interval", interval),
	)

	for {
		select {
		case <-ctx.Done():
			mylogger.Info("Redis health monitor stopped",
				zap.String("service", checker.serviceName),
				zap.String("reason", "context cancelled"),
			)
			return

		case <-ticker.C:
			probeOnce(ctx, client, checker)
		}
	}
}

// probeOnce executes a single PING under a tight timeout context and updates
// checker state accordingly.
//
// A new child context is created for each probe so that a slow Redis response
// does not block the next tick; the tight deadline (defaultPingTimeout = 200ms)
// ensures rapid failure detection.
//
// DoCmd is part of the Client interface so no type assertion is required.
// The probeCtx deadline is enforced by the underlying radix pool's dial
// timeout; we also pass it explicitly so that any context-aware middleware in
// the call chain respects it.
func probeOnce(ctx context.Context, client Client, checker *HealthChecker) {
	probeCtx, cancel := context.WithTimeout(ctx, defaultPingTimeout)
	defer cancel()

	// DoCmd is defined on the Client interface — call it directly.
	// key is "" because PING takes no key argument.
	var reply string
	err := client.DoCmd(probeCtx, &reply, "PING", "")

	// Ensure the probe context cancellation is observed even if DoCmd
	// returned before the deadline fired (e.g. the pool is fast but the
	// context is already done from a prior shutdown signal).
	if probeCtx.Err() != nil && err == nil {
		err = probeCtx.Err()
	}

	if err != nil {
		checker.fail()
		mylogger.Warn("Redis health check PING failed",
			zap.String("service", checker.serviceName),
			zap.Error(err),
		)
		return
	}

	if reply != "PONG" {
		checker.fail()
		mylogger.Warn("Redis health check PING returned unexpected reply",
			zap.String("service", checker.serviceName),
			zap.String("reply", reply),
		)
		return
	}

	checker.ok()
}

// ---------------------------------------------------------------------------
// Direct-use helper: StartHealthCheckForClient
// ---------------------------------------------------------------------------

// StartHealthCheckForClient is the simplest entry point: given a Client and a
// service name, it creates a HealthChecker, starts the background monitor, and
// returns the checker for the caller to wire into a health endpoint.
//
// Use this when you hold a Client directly (e.g. in a filter that acquired it
// from Manager.GetClient).
//
//	hc := redis.StartHealthCheckForClient(ctx, client, "rate-limiter", 5*time.Second)
//	// later:
//	if !hc.IsHealthy() { ... }
func StartHealthCheckForClient(
	ctx context.Context,
	client Client,
	serviceName string,
	interval time.Duration,
) *HealthChecker {
	hc := NewHealthChecker(serviceName)
	StartHealthCheck(ctx, client, hc, interval)
	return hc
}

// ---------------------------------------------------------------------------
// Radix PING helper — used internally by probeOnce and pool.go startup
// ---------------------------------------------------------------------------

// PingClient sends a PING to the given radix.Client under the provided context
// and returns an error if the reply is not "PONG". This is the same check
// performed by the pool startup loop and is exposed here for reuse.
func PingClient(ctx context.Context, c radix.Client) error {
	var reply string
	if err := c.Do(ctx, radix.Cmd(&reply, "PING")); err != nil {
		return fmt.Errorf("PING: %w", err)
	}
	if reply != "PONG" {
		return fmt.Errorf("PING: unexpected reply %q (expected \"PONG\")", reply)
	}
	return nil
}
