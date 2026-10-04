// Package leader configures Kubernetes Lease-based leader election for the
// operator and provides runnable helpers that make the active/standby split
// explicit:
//
//   - LeaderOnly runnables start only on the replica holding the Lease, and are
//     stopped when it is lost (reconcilers, the identity stream server, the
//     routing EndpointSlice publisher).
//   - AllReplicas runnables start on every replica regardless of leadership
//     (webhooks, the warm-standby identity cache).
//
// When the manager loses the Lease it stops and mgr.Start returns an error; the
// process then exits and the container restarts as a standby. Combined with
// ReleaseOnCancel this keeps leadership exclusive and makes voluntary handovers
// (rolling upgrades) fast.
package leader

import (
	"flag"
	"fmt"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
)

// Defaults match controller-runtime's own defaults.
const (
	DefaultLeaseDuration = 15 * time.Second
	DefaultRenewDeadline = 10 * time.Second
	DefaultRetryPeriod   = 2 * time.Second
	DefaultLeaseName     = "hyper-operator.hyper.io"
)

// ElectionConfig holds the leader election settings of the operator.
type ElectionConfig struct {
	// Enabled turns leader election on. Required with more than one replica.
	Enabled bool
	// LeaseName is the name of the coordination.k8s.io/v1 Lease.
	LeaseName string
	// Namespace of the Lease. Empty means the operator's own namespace (in-cluster).
	Namespace string
	// LeaseDuration is how long standbys wait after the last renewal before taking over.
	LeaseDuration time.Duration
	// RenewDeadline is how long the leader keeps retrying a renewal before stepping down.
	RenewDeadline time.Duration
	// RetryPeriod is the interval between acquire/renew attempts.
	RetryPeriod time.Duration
	// ReleaseOnCancel releases the Lease when the manager stops, so a standby takes over
	// after one RetryPeriod instead of waiting for LeaseDuration. Safe because the
	// process exits as soon as the manager returns.
	ReleaseOnCancel bool
}

// DefaultElectionConfig returns the defaults used when no flags are given.
func DefaultElectionConfig() ElectionConfig {
	return ElectionConfig{
		LeaseName:       DefaultLeaseName,
		LeaseDuration:   DefaultLeaseDuration,
		RenewDeadline:   DefaultRenewDeadline,
		RetryPeriod:     DefaultRetryPeriod,
		ReleaseOnCancel: true,
	}
}

// BindFlags registers the leader election flags, keeping the existing
// --leader-elect flag name.
func (c *ElectionConfig) BindFlags(fs *flag.FlagSet) {
	d := DefaultElectionConfig()
	fs.BoolVar(&c.Enabled, "leader-elect", false,
		"Enable leader election. Only the replica holding the Lease reconciles and serves the identity stream; "+
			"other replicas stay as warm standbys.")
	fs.StringVar(&c.LeaseName, "leader-elect-lease-name", d.LeaseName, "Name of the coordination.k8s.io Lease used for leader election.")
	fs.StringVar(&c.Namespace, "leader-elect-namespace", "", "Namespace of the Lease (default: the operator's namespace).")
	fs.DurationVar(&c.LeaseDuration, "leader-elect-lease-duration", d.LeaseDuration,
		"How long standbys wait after the leader's last renewal before taking over.")
	fs.DurationVar(&c.RenewDeadline, "leader-elect-renew-deadline", d.RenewDeadline,
		"How long the leader retries renewing the Lease before giving it up. Must be less than the lease duration.")
	fs.DurationVar(&c.RetryPeriod, "leader-elect-retry-period", d.RetryPeriod,
		"Interval between attempts to acquire or renew the Lease. Must be less than the renew deadline.")
	fs.BoolVar(&c.ReleaseOnCancel, "leader-elect-release-on-cancel", d.ReleaseOnCancel,
		"Release the Lease on shutdown so a standby takes over immediately (faster rolling upgrades).")
}

// Validate checks the timing invariant required by client-go's leader election:
// 0 < RetryPeriod < RenewDeadline < LeaseDuration.
func (c ElectionConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.LeaseName == "" {
		return fmt.Errorf("leader election: lease name must not be empty")
	}
	if c.RetryPeriod <= 0 || c.RenewDeadline <= 0 || c.LeaseDuration <= 0 {
		return fmt.Errorf("leader election: durations must be positive (lease %s, renew %s, retry %s)",
			c.LeaseDuration, c.RenewDeadline, c.RetryPeriod)
	}
	if c.RetryPeriod >= c.RenewDeadline {
		return fmt.Errorf("leader election: retry period (%s) must be less than renew deadline (%s)", c.RetryPeriod, c.RenewDeadline)
	}
	if c.RenewDeadline >= c.LeaseDuration {
		return fmt.Errorf("leader election: renew deadline (%s) must be less than lease duration (%s)", c.RenewDeadline, c.LeaseDuration)
	}
	return nil
}

// Apply copies the settings into controller-runtime manager options.
func (c ElectionConfig) Apply(opts *ctrl.Options) {
	opts.LeaderElection = c.Enabled
	if !c.Enabled {
		return
	}
	lease, renew, retry := c.LeaseDuration, c.RenewDeadline, c.RetryPeriod
	opts.LeaderElectionID = c.LeaseName
	opts.LeaderElectionNamespace = c.Namespace
	opts.LeaderElectionResourceLock = "leases"
	opts.LeaseDuration = &lease
	opts.RenewDeadline = &renew
	opts.RetryPeriod = &retry
	opts.LeaderElectionReleaseOnCancel = c.ReleaseOnCancel
}
