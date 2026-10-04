package leader

import (
	"context"
	"flag"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func TestValidate(t *testing.T) {
	ok := DefaultElectionConfig()
	ok.Enabled = true
	if err := ok.Validate(); err != nil {
		t.Fatalf("defaults must be valid: %v", err)
	}

	cases := map[string]func(c *ElectionConfig){
		"retry >= renew":   func(c *ElectionConfig) { c.RetryPeriod = c.RenewDeadline },
		"renew >= lease":   func(c *ElectionConfig) { c.RenewDeadline = c.LeaseDuration },
		"zero retry":       func(c *ElectionConfig) { c.RetryPeriod = 0 },
		"negative lease":   func(c *ElectionConfig) { c.LeaseDuration = -time.Second },
		"empty lease name": func(c *ElectionConfig) { c.LeaseName = "" },
	}
	for name, mutate := range cases {
		c := DefaultElectionConfig()
		c.Enabled = true
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}

	disabled := DefaultElectionConfig()
	disabled.RetryPeriod = time.Hour // nonsense, but irrelevant when disabled
	if err := disabled.Validate(); err != nil {
		t.Fatalf("disabled config must not be validated: %v", err)
	}
}

func TestBindFlagsAndApply(t *testing.T) {
	c := DefaultElectionConfig()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	c.BindFlags(fs)
	err := fs.Parse([]string{
		"--leader-elect",
		"--leader-elect-lease-duration=30s",
		"--leader-elect-renew-deadline=20s",
		"--leader-elect-retry-period=4s",
		"--leader-elect-namespace=hyper-operator-system",
		"--leader-elect-release-on-cancel=false",
	})
	if err != nil {
		t.Fatal(err)
	}

	var opts ctrl.Options
	c.Apply(&opts)
	if !opts.LeaderElection || opts.LeaderElectionID != DefaultLeaseName || opts.LeaderElectionNamespace != "hyper-operator-system" {
		t.Fatalf("election identity not applied: %+v", opts)
	}
	if opts.LeaderElectionResourceLock != "leases" {
		t.Fatalf("expected Lease lock, got %q", opts.LeaderElectionResourceLock)
	}
	if *opts.LeaseDuration != 30*time.Second || *opts.RenewDeadline != 20*time.Second || *opts.RetryPeriod != 4*time.Second {
		t.Fatalf("timings not applied: %v %v %v", *opts.LeaseDuration, *opts.RenewDeadline, *opts.RetryPeriod)
	}
	if opts.LeaderElectionReleaseOnCancel {
		t.Fatal("release-on-cancel flag not applied")
	}
}

func TestDefaultsReleaseOnCancel(t *testing.T) {
	if !DefaultElectionConfig().ReleaseOnCancel {
		t.Fatal("release on cancel must be on by default for fast handovers")
	}
}

func TestRunnableLeadershipFlags(t *testing.T) {
	lo, ok := LeaderOnly("x", func(context.Context) error { return nil }).(manager.LeaderElectionRunnable)
	if !ok || !lo.NeedLeaderElection() {
		t.Fatal("LeaderOnly must need leader election")
	}
	ar, ok := AllReplicas("y", func(context.Context) error { return nil }).(manager.LeaderElectionRunnable)
	if !ok || ar.NeedLeaderElection() {
		t.Fatal("AllReplicas must not need leader election")
	}
}

// replica is one operator instance competing for the shared Lease.
type replica struct {
	name          string
	cancel        context.CancelFunc
	done          chan struct{}
	leaderRunning atomic.Bool
	warmRunning   atomic.Bool
}

// startReplica starts a real controller-runtime manager whose Lease lives in a
// fake clientset shared by all replicas, so election behaves as in a cluster
// without needing an API server.
func startReplica(t *testing.T, name string, lockClient *fake.Clientset) *replica {
	t.Helper()
	lock, err := resourcelock.New(resourcelock.LeasesResourceLock, "hyper-system", DefaultLeaseName,
		lockClient.CoreV1(), lockClient.CoordinationV1(), resourcelock.ResourceLockConfig{Identity: name})
	if err != nil {
		t.Fatal(err)
	}

	cfg := ElectionConfig{
		Enabled: true, LeaseName: DefaultLeaseName, Namespace: "hyper-system",
		LeaseDuration: 2 * time.Second, RenewDeadline: 1500 * time.Millisecond, RetryPeriod: 200 * time.Millisecond,
		ReleaseOnCancel: true,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	opts := ctrl.Options{
		Metrics:                             metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress:              "0",
		LeaderElectionResourceLockInterface: lock,
	}
	cfg.Apply(&opts)

	mgr, err := ctrl.NewManager(&rest.Config{Host: "https://127.0.0.1:1"}, opts)
	if err != nil {
		t.Fatal(err)
	}

	r := &replica{name: name, done: make(chan struct{})}
	_ = mgr.Add(LeaderOnly("identity-server", func(ctx context.Context) error {
		r.leaderRunning.Store(true)
		<-ctx.Done()
		r.leaderRunning.Store(false)
		return nil
	}))
	_ = mgr.Add(AllReplicas("identity-cache", func(ctx context.Context) error {
		r.warmRunning.Store(true)
		<-ctx.Done()
		r.warmRunning.Store(false)
		return nil
	}))

	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go func() {
		defer close(r.done)
		_ = mgr.Start(ctx)
	}()
	return r
}

func (r *replica) stop() {
	r.cancel()
	<-r.done
}

func eventually(t *testing.T, timeout time.Duration, msg string, cond func() bool) time.Duration {
	t.Helper()
	start := time.Now()
	for time.Since(start) < timeout {
		if cond() {
			return time.Since(start)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %s: %s", timeout, msg)
	return 0
}

func TestActiveStandbyAndFastHandover(t *testing.T) {
	lockClient := fake.NewSimpleClientset()

	a := startReplica(t, "replica-a", lockClient)
	eventually(t, 5*time.Second, "replica-a should become leader", a.leaderRunning.Load)

	b := startReplica(t, "replica-b", lockClient)
	eventually(t, 3*time.Second, "replica-b warm-standby work should run without leadership", b.warmRunning.Load)

	// While a holds the Lease, b must never run leader-only work.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if b.leaderRunning.Load() {
			t.Fatal("standby started leader-only work while another replica holds the Lease")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !a.warmRunning.Load() {
		t.Fatal("warm-standby work must also run on the leader")
	}

	// Graceful stop of the leader (rolling upgrade): with ReleaseOnCancel the standby
	// takes over after about one retry period instead of the full lease duration.
	a.stop()
	took := eventually(t, 5*time.Second, "replica-b should take over", b.leaderRunning.Load)
	if took >= 2*time.Second {
		t.Fatalf("handover took %s; with ReleaseOnCancel it should be well under the 2s lease duration", took)
	}

	lease, err := lockClient.CoordinationV1().Leases("hyper-system").Get(context.Background(), DefaultLeaseName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != "replica-b" {
		t.Fatalf("lease holder = %v, want replica-b", lease.Spec.HolderIdentity)
	}

	b.stop()
	if b.leaderRunning.Load() || b.warmRunning.Load() {
		t.Fatal("runnables must stop when the manager stops")
	}
}

func TestLeadershipReporterMetric(t *testing.T) {
	r := NewLeadershipReporter()
	if lr, ok := r.(manager.LeaderElectionRunnable); !ok || !lr.NeedLeaderElection() {
		t.Fatal("the reporter must only run on the leader")
	}

	isLeader.Set(0)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = r.Start(ctx)
	}()
	eventually(t, 2*time.Second, "metric should report leadership", func() bool { return testutil.ToFloat64(isLeader) == 1 })
	cancel()
	wg.Wait()
	if got := testutil.ToFloat64(isLeader); got != 0 {
		t.Fatalf("metric should drop to 0 after leadership ends, got %v", got)
	}
}
