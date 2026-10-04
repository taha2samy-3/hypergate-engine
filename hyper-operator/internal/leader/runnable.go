package leader

import (
	"context"
	"os"

	"github.com/prometheus/client_golang/prometheus"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// runnable adapts a function to manager.Runnable and states explicitly whether it
// needs leadership (controller-runtime treats runnables that do not say so as
// leader-only, which is easy to get wrong for code that must run everywhere).
type runnable struct {
	name       string
	needLeader bool
	fn         func(ctx context.Context) error
}

func (r *runnable) Start(ctx context.Context) error { return r.fn(ctx) }

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (r *runnable) NeedLeaderElection() bool { return r.needLeader }

// LeaderOnly returns a runnable that the manager starts only on the replica that
// holds the Lease and stops when the Lease is lost. fn must return when ctx is done.
func LeaderOnly(name string, fn func(ctx context.Context) error) manager.Runnable {
	return &runnable{name: name, needLeader: true, fn: fn}
}

// AllReplicas returns a runnable that the manager starts on every replica,
// independent of leadership (warm standby work). fn must return when ctx is done.
func AllReplicas(name string, fn func(ctx context.Context) error) manager.Runnable {
	return &runnable{name: name, needLeader: false, fn: fn}
}

// isLeader reports 1 on the replica currently holding the Lease and 0 elsewhere.
var isLeader = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "hypergate_operator_is_leader",
	Help: "1 if this operator replica holds the leader election Lease, 0 otherwise.",
})

func init() {
	metrics.Registry.MustRegister(isLeader)
}

// NewLeadershipReporter returns a leader-only runnable that sets the
// hypergate_operator_is_leader metric and logs leadership transitions. Without
// leader election the single replica acts as leader and reports 1.
func NewLeadershipReporter() manager.Runnable {
	return LeaderOnly("leadership-reporter", func(ctx context.Context) error {
		log := ctrl.Log.WithName("leader")
		identity, _ := os.Hostname()
		isLeader.Set(1)
		log.Info("This replica is now the leader", "identity", identity)
		<-ctx.Done()
		isLeader.Set(0)
		log.Info("This replica stopped leading", "identity", identity)
		return nil
	})
}
