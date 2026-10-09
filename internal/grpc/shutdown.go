package grpc

import (
	"context"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
)

// Activity records ext_proc stream activity, so shutdown can wait until Envoy
// has stopped opening new streams.
type Activity struct {
	lastStart atomic.Int64 // unix nanoseconds of the most recent stream start
	active    atomic.Int64
}

func (a *Activity) streamStarted() {
	a.lastStart.Store(time.Now().UnixNano())
	a.active.Add(1)
}

func (a *Activity) streamEnded() { a.active.Add(-1) }

// Active returns the number of open streams.
func (a *Activity) Active() int64 { return a.active.Load() }

// LastStart returns when the most recent stream started (zero if none).
func (a *Activity) LastStart() time.Time {
	if n := a.lastStart.Load(); n != 0 {
		return time.Unix(0, n)
	}
	return time.Time{}
}

// WithActivity makes the server record stream activity in a.
func WithActivity(a *Activity) Option {
	return func(s *Server) { s.activity = a }
}

// WaitForQuiet blocks until no stream has started for quiet, but at least
// minDelay and at most maxDelay after it is called, or until ctx is done. It
// returns how long it waited. Waiting for quiet instead of a fixed time adapts to
// however long Envoy takes to learn that this engine is going away.
func WaitForQuiet(ctx context.Context, a *Activity, quiet, minDelay, maxDelay time.Duration) time.Duration {
	start := time.Now()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		elapsed := time.Since(start)
		if elapsed >= maxDelay {
			return elapsed
		}
		if elapsed >= minDelay && time.Since(a.LastStart()) >= quiet {
			return elapsed
		}
		select {
		case <-ctx.Done():
			return time.Since(start)
		case <-tick.C:
		}
	}
}

// StopWithin stops srv gracefully, waiting for open streams for at most timeout,
// then closes the remaining ones. It reports whether streams had to be closed.
func StopWithin(srv *grpc.Server, timeout time.Duration) (forced bool) {
	done := make(chan struct{})
	go func() {
		srv.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
		return false
	case <-time.After(timeout):
		srv.Stop()
		<-done
		return true
	}
}
