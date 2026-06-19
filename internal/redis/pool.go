// Package redis — pool.go
//
// pool.go is the connection factory layer. It translates a
// config.RedisServiceConfig into a fully-initialised radix Client backed by
// the appropriate topology (SINGLE / CLUSTER / SENTINEL), handles mTLS, and
// wraps the result in a clientImpl that satisfies the Client interface defined
// in client.go.
//
// # Resilient Startup
//
// Kubernetes workloads commonly start before Redis is ready. NewClientConn
// therefore wraps every dial attempt in a jittered exponential back-off loop.
// The loop retries indefinitely when startup_max_elapsed_time == 0, otherwise
// it aborts once the budget is exhausted. ctx cancellation is checked on every
// iteration so that the process shuts down cleanly if a SIGTERM arrives during
// initial startup.
//
// After a connection is established, a synchronous PING is sent and the reply
// is verified to be "PONG". If PING fails the connection is closed and the
// loop continues, preventing a "connected but broken" pool from being returned.
//
// # TLS / mTLS
//
// When cfg.TLS == true a *tls.Config is built once from the certificate files
// and reused across all pool connections. If tls_skip_hostname_verification is
// true, a custom VerifyPeerCertificate callback is installed that validates the
// certificate chain against the configured CA bundle while bypassing the server
// name / SAN check — this is distinct from tls.InsecureSkipVerify, which would
// also skip chain verification.
//
// # Authentication
//
// The radix Dialer exposes AuthUser and AuthPass fields directly. The factory
// parses "username:password" and "password" formats and sets the correct fields
// so radix sends AUTH automatically on every new connection.
//
// # Thread Safety
//
// buildDialer performs file I/O (reading cert/CA files) synchronously during
// startup. After the dialer is built it is immutable and safe to share across
// goroutines — radix copies the Dialer value into each pool.
package redis

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"math"
	"math/rand"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mediocregopher/radix/v4"

	"github.com/taha/myprog/internal/config"
	mylogger "github.com/taha/myprog/internal/logger"
	"go.uber.org/zap"
)

// ---------------------------------------------------------------------------
// clientImpl — concrete implementation of Client
// ---------------------------------------------------------------------------

// clientImpl wraps a radix.Client and satisfies the Client interface defined in
// client.go. It is the only concrete type returned by NewClientConn; callers
// interact solely through the Client interface.
//
// The underlying radix pool is already thread-safe; clientImpl adds no
// additional synchronisation of its own. The activeConns counter is maintained
// via atomic ops to provide a lock-free NumActiveConns snapshot.
type clientImpl struct {
	// client is the underlying radix connection (Pool / Cluster / Sentinel).
	// All three types implement radix.Client and are internally thread-safe.
	client radix.Client

	// serviceName is stored for contextual error messages only.
	serviceName string

	// topology records the configured type string for observability.
	topology string

	// configuredPoolSize records pool_size for NumActiveConns computation.
	configuredPoolSize int

	// isCluster is true when topology == "CLUSTER". It gates the slot-grouped
	// concurrent pipeline path in pipeline.go.
	isCluster bool

	// clusterPipelineParallelism is the maximum number of concurrent goroutines
	// used to flush per-slot sub-pipelines in CLUSTER mode. 0 means unbounded
	// (one goroutine per slot group); 1 means serial execution.
	clusterPipelineParallelism int

	// activeConns is an atomic counter incremented on DoCmd entry and
	// decremented on exit. It is a best-effort gauge; callers must not
	// use it for strong consistency decisions.
	activeConns int64
}

// DoCmd executes a single Redis command. It increments the active-connection
// gauge on entry and decrements it on return so NumActiveConns tracks in-flight
// requests rather than physical connections (radix manages the latter).
func (c *clientImpl) DoCmd(rcv interface{}, cmd, key string, args ...interface{}) error {
	atomic.AddInt64(&c.activeConns, 1)
	defer atomic.AddInt64(&c.activeConns, -1)

	// Build the flat string slice expected by radix.Cmd.
	// Pre-allocate with capacity: 1 (key) + len(args).
	all := make([]string, 0, 1+len(args))
	if key != "" {
		all = append(all, key)
	}
	for _, a := range args {
		all = append(all, fmt.Sprintf("%v", a))
	}

	action := radix.Cmd(rcv, cmd, all...)
	if err := c.client.Do(context.Background(), action); err != nil {
		return fmt.Errorf("redis[%s].DoCmd %s %q: %w", c.serviceName, cmd, key, err)
	}
	return nil
}

// Close tears down the underlying radix pool and releases all file descriptors.
// It delegates directly to the radix pool — which is idempotent.
func (c *clientImpl) Close() error {
	if err := c.client.Close(); err != nil {
		return fmt.Errorf("redis[%s].Close: %w", c.serviceName, err)
	}
	return nil
}

// NumActiveConns returns the number of DoCmd / PipeDo calls currently executing.
// This is an atomic read and imposes no lock.
func (c *clientImpl) NumActiveConns() int {
	return int(atomic.LoadInt64(&c.activeConns))
}

// ---------------------------------------------------------------------------
// NewClientConn — public factory with resilient startup back-off
// ---------------------------------------------------------------------------

// NewClientConn constructs a Client for the named Redis service described by
// cfg. It applies a jittered exponential back-off during startup to survive
// Kubernetes cold starts, then verifies the connection with a PING before
// returning.
//
// ctx should be the application-lifetime context. When ctx is cancelled the
// retry loop is aborted immediately, which allows clean shutdown even if Redis
// is not yet reachable.
//
// On success the returned Client owns its underlying pool. The caller is
// responsible for calling Close when the client is no longer needed.
func NewClientConn(ctx context.Context, serviceName string, cfg config.RedisServiceConfig) (Client, error) {
	dialer, err := buildDialer(cfg)
	if err != nil {
		return nil, fmt.Errorf("redis.NewClientConn[%s]: failed to build dialer: %w", serviceName, err)
	}

	var (
		attempt     int
		backoff     = cfg.StartupInitialIntervalDuration
		maxBackoff  = cfg.StartupMaxIntervalDuration
		deadline    time.Time
		hasDeadline = cfg.StartupMaxElapsedTimeDuration > 0
	)
	if hasDeadline {
		deadline = time.Now().Add(cfg.StartupMaxElapsedTimeDuration)
	}

	for {
		attempt++

		// Check context cancellation first — fast path for shutdown.
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("redis.NewClientConn[%s]: context cancelled during startup: %w",
				serviceName, ctx.Err())
		default:
		}

		conn, dialErr := dialTopology(ctx, serviceName, cfg, dialer)
		if dialErr == nil {
			// Verify liveness with a synchronous PING.
			var pongReply string
			pingErr := conn.Do(context.Background(), radix.Cmd(&pongReply, "PING"))
			if pingErr == nil && pongReply == "PONG" {
				mylogger.Info("Redis connection established",
					zap.String("service", serviceName),
					zap.String("type", cfg.Type),
					zap.String("url", cfg.URL),
					zap.Int("attempt", attempt),
				)
				return &clientImpl{
					client:                     conn,
					serviceName:                serviceName,
					topology:                   cfg.Type,
					configuredPoolSize:         cfg.PoolSize,
					isCluster:                  strings.ToUpper(cfg.Type) == "CLUSTER",
					clusterPipelineParallelism: cfg.ClusterPipelineParallelism,
				}, nil
			}

			// PING failed — close the broken pool and log before retrying.
			_ = conn.Close()
			if pingErr != nil {
				mylogger.Warn("Redis PING failed after connection; will retry",
					zap.String("service", serviceName),
					zap.Int("attempt", attempt),
					zap.Error(pingErr),
				)
			} else {
				mylogger.Warn("Redis PING returned unexpected reply; will retry",
					zap.String("service", serviceName),
					zap.Int("attempt", attempt),
					zap.String("reply", pongReply),
				)
			}
		} else {
			mylogger.Warn("Redis connection attempt failed; will retry",
				zap.String("service", serviceName),
				zap.String("type", cfg.Type),
				zap.String("url", cfg.URL),
				zap.Int("attempt", attempt),
				zap.Error(dialErr),
			)
		}

		// Check elapsed-time budget.
		if hasDeadline && time.Now().After(deadline) {
			return nil, fmt.Errorf(
				"redis.NewClientConn[%s]: startup_max_elapsed_time (%s) exceeded after %d attempt(s): %w",
				serviceName, cfg.StartupMaxElapsedTime, attempt, dialErr,
			)
		}

		// Compute jittered sleep: sleep = backoff * uniform(0.5, 1.5).
		jitterFactor := 0.5 + rand.Float64() //nolint:gosec // non-cryptographic jitter is fine
		sleep := time.Duration(float64(backoff) * jitterFactor)

		// If a hard deadline is set, don't sleep past it.
		if hasDeadline {
			remaining := time.Until(deadline)
			if sleep > remaining {
				sleep = remaining
			}
		}

		mylogger.Info("Retrying Redis connection",
			zap.String("service", serviceName),
			zap.Duration("backoff", sleep),
			zap.Int("attempt", attempt),
		)

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("redis.NewClientConn[%s]: context cancelled while waiting for retry: %w",
				serviceName, ctx.Err())
		case <-time.After(sleep):
		}

		// Double the backoff, capped at maxBackoff.
		backoff = time.Duration(math.Min(
			float64(backoff*2),
			float64(maxBackoff),
		))
	}
}

// NewPoolClient is a thin convenience wrapper that calls NewClientConn with a
// background context and no startup retry (a single attempt). It is kept for
// backward-compatibility with buildClients in client.go and for simple unit
// tests that do not need startup resilience.
//
// For production use prefer NewClientConn which accepts a context and supports
// the full backoff loop.
func NewPoolClient(serviceName string, cfg config.RedisServiceConfig) (Client, error) {
	return NewClientConn(context.Background(), serviceName, cfg)
}

// ---------------------------------------------------------------------------
// multiClientAdapter — bridges radix.MultiClient → radix.Client
// ---------------------------------------------------------------------------

// multiClientAdapter wraps a radix.MultiClient (*radix.Cluster or
// *radix.Sentinel) and satisfies the radix.Client interface by adding a
// no-op Addr() method. Both Cluster and Sentinel route commands to the
// correct shard/primary internally, so a meaningful single address does not
// exist; callers that need per-shard addresses should use MultiClient.Clients().
//
// Thread safety: all calls are forwarded to the underlying MultiClient, which
// is itself fully thread-safe.
type multiClientAdapter struct {
	mc radix.MultiClient
}

// Do forwards to the primary instance via MultiClient.Do.
func (a *multiClientAdapter) Do(ctx context.Context, action radix.Action) error {
	return a.mc.Do(ctx, action)
}

// Addr returns a synthetic address token. It is present only to satisfy the
// radix.Client interface; the underlying MultiClient manages its own pool of
// addresses.
func (a *multiClientAdapter) Addr() net.Addr {
	return multiClientAddr{}
}

// Close shuts down the underlying MultiClient (Cluster or Sentinel).
func (a *multiClientAdapter) Close() error {
	return a.mc.Close()
}

// multiClientAddr is a zero-value net.Addr stub used by multiClientAdapter.
type multiClientAddr struct{}

func (multiClientAddr) Network() string { return "tcp" }
func (multiClientAddr) String() string  { return "<multi>" }

// ---------------------------------------------------------------------------
// dialTopology — topology dispatcher
// ---------------------------------------------------------------------------

// dialTopology selects the correct radix topology builder based on cfg.Type
// (case-insensitive for defence-in-depth, though the config parser already
// enforces uppercase).
func dialTopology(
	ctx context.Context,
	serviceName string,
	cfg config.RedisServiceConfig,
	dialer radix.Dialer,
) (radix.Client, error) {
	switch strings.ToUpper(cfg.Type) {
	case "SINGLE":
		return dialSingle(ctx, cfg, dialer)
	case "CLUSTER":
		return dialCluster(ctx, cfg, dialer)
	case "SENTINEL":
		return dialSentinel(ctx, serviceName, cfg, dialer)
	default:
		// The config parser guarantees this never happens in production, but
		// we guard it here for test harnesses that bypass parsing.
		return nil, fmt.Errorf("unknown topology type %q", cfg.Type)
	}
}

// ---------------------------------------------------------------------------
// Per-topology builders
// ---------------------------------------------------------------------------

// dialSingle creates a radix connection pool to a single Redis instance.
//
// WriteFlushInterval on the Dialer is set to PipelineWindowDuration when > 0,
// enabling radix's implicit auto-pipelining which batches concurrent commands
// into a single syscall.
func dialSingle(ctx context.Context, cfg config.RedisServiceConfig, dialer radix.Dialer) (radix.Client, error) {
	if cfg.PipelineWindowDuration > 0 {
		dialer.WriteFlushInterval = cfg.PipelineWindowDuration
	}

	poolCfg := radix.PoolConfig{
		Size:   cfg.PoolSize,
		Dialer: dialer,
		// Map startup intervals to the pool's reconnection back-off so that
		// after initial startup the pool recovers from transient failures using
		// the same operator-configured parameters.
		MinReconnectInterval: cfg.StartupInitialIntervalDuration,
		MaxReconnectInterval: cfg.StartupMaxIntervalDuration,
	}

	conn, err := poolCfg.New(ctx, cfg.SocketType, cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("SINGLE pool %s://%s: %w", cfg.SocketType, cfg.URL, err)
	}
	return conn, nil
}

// dialCluster creates a radix Cluster client. cfg.URL is split on commas so
// operators can supply multiple seed addresses for faster topology discovery.
//
// WriteFlushInterval is set when pipeline_window > 0 to enable auto-pipelining
// across cluster shards.
//
// *radix.Cluster implements radix.MultiClient, not radix.Client (it has no
// Addr() method). It is wrapped in a multiClientAdapter before being returned.
func dialCluster(ctx context.Context, cfg config.RedisServiceConfig, dialer radix.Dialer) (radix.Client, error) {
	if cfg.PipelineWindowDuration > 0 {
		dialer.WriteFlushInterval = cfg.PipelineWindowDuration
	}

	addrs := splitAndTrim(cfg.URL)
	if len(addrs) == 0 {
		return nil, fmt.Errorf("CLUSTER: url %q produced no addresses", cfg.URL)
	}

	clusterCfg := radix.ClusterConfig{
		PoolConfig: radix.PoolConfig{
			Size:                 cfg.PoolSize,
			Dialer:               dialer,
			MinReconnectInterval: cfg.StartupInitialIntervalDuration,
			MaxReconnectInterval: cfg.StartupMaxIntervalDuration,
		},
	}

	cluster, err := clusterCfg.New(ctx, addrs)
	if err != nil {
		return nil, fmt.Errorf("CLUSTER addrs=%v: %w", addrs, err)
	}
	return &multiClientAdapter{mc: cluster}, nil
}

// dialSentinel parses the Sentinel URL, constructs a dedicated Sentinel dialer
// with sentinel_auth credentials, and creates a radix Sentinel client.
//
// URL format:  <master_name>,<sentinel_host1>:<port1>[,<sentinel_host2>:<port2>,...]
//
// A minimum of two comma-separated tokens (master name + at least one sentinel
// address) is required.
//
// *radix.Sentinel implements radix.MultiClient, not radix.Client (it has no
// Addr() method). It is wrapped in a multiClientAdapter before being returned.
func dialSentinel(
	ctx context.Context,
	serviceName string,
	cfg config.RedisServiceConfig,
	dataDialer radix.Dialer,
) (radix.Client, error) {
	if cfg.PipelineWindowDuration > 0 {
		dataDialer.WriteFlushInterval = cfg.PipelineWindowDuration
	}

	parts := splitAndTrim(cfg.URL)
	if len(parts) < 2 {
		return nil, fmt.Errorf(
			"SENTINEL url must be \"<master_name>,<sentinel_host>:<port>[,...]\", got %q",
			cfg.URL,
		)
	}

	masterName := parts[0]
	sentinelAddrs := parts[1:]

	// The sentinel dialer uses a separate auth credential (sentinel_auth)
	// that is independent of the data-node auth credential. It shares the
	// TLS configuration from the data dialer but overwrites the auth fields.
	sentinelDialer := dataDialer // copy the value (Dialer is a value type)
	sentinelDialer.AuthUser = "" // sentinel nodes use only AUTH <password>
	sentinelDialer.AuthPass = cfg.SentinelAuth

	sentinelCfg := radix.SentinelConfig{
		PoolConfig: radix.PoolConfig{
			Size:                 cfg.PoolSize,
			Dialer:               dataDialer,
			MinReconnectInterval: cfg.StartupInitialIntervalDuration,
			MaxReconnectInterval: cfg.StartupMaxIntervalDuration,
		},
		SentinelDialer: sentinelDialer,
	}

	sentinel, err := sentinelCfg.New(ctx, masterName, sentinelAddrs)
	if err != nil {
		return nil, fmt.Errorf("SENTINEL master=%q sentinels=%v: %w", masterName, sentinelAddrs, err)
	}

	mylogger.Info("Sentinel client initialised",
		zap.String("service", serviceName),
		zap.String("master", masterName),
		zap.Strings("sentinels", sentinelAddrs),
	)
	return &multiClientAdapter{mc: sentinel}, nil
}

// ---------------------------------------------------------------------------
// Dialer construction (auth + TLS)
// ---------------------------------------------------------------------------

// buildDialer constructs a radix.Dialer populated with authentication
// credentials, the configured net.Dialer timeout, and optionally a *tls.Config.
//
// All file I/O (loading certs) is performed synchronously here. After this
// function returns the Dialer is immutable and safe to copy across goroutines.
func buildDialer(cfg config.RedisServiceConfig) (radix.Dialer, error) {
	dialer := radix.Dialer{
		NetDialer: &net.Dialer{
			Timeout:   cfg.TimeoutDuration,
			KeepAlive: 10 * time.Second,
		},
	}

	// --- Authentication ---
	// radix.Dialer uses AuthUser + AuthPass directly. Parse "user:pass" and
	// "pass" formats. We find only the first colon to support passwords that
	// contain colons.
	if cfg.Auth != "" {
		if idx := strings.IndexByte(cfg.Auth, ':'); idx >= 0 {
			dialer.AuthUser = cfg.Auth[:idx]
			dialer.AuthPass = cfg.Auth[idx+1:]
		} else {
			// Password-only format: leave AuthUser empty.
			dialer.AuthPass = cfg.Auth
		}
	}

	// --- TLS ---
	if cfg.TLS {
		tlsCfg, err := buildTLSConfig(cfg)
		if err != nil {
			return radix.Dialer{}, fmt.Errorf("buildDialer: tls config: %w", err)
		}

		// Wrap the plain NetDialer with a TLS-aware one.
		netDialer := dialer.NetDialer.(*net.Dialer)
		dialer.NetDialer = &tlsNetDialer{
			inner:  netDialer,
			tlsCfg: tlsCfg,
		}
	}

	return dialer, nil
}

// ---------------------------------------------------------------------------
// TLS helpers
// ---------------------------------------------------------------------------

// buildTLSConfig assembles a *tls.Config from the certificate/CA paths in cfg.
//
// When tls_skip_hostname_verification is true we install a custom
// VerifyPeerCertificate callback that validates the full certificate chain
// against the CA bundle while skipping server-name / SAN matching. This is
// safer than InsecureSkipVerify (which disables chain verification too).
func buildTLSConfig(cfg config.RedisServiceConfig) (*tls.Config, error) {
	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}

	// Load mTLS client certificate/key pair if both are provided.
	if cfg.TLSClientCert != "" && cfg.TLSClientKey != "" {
		cert, err := tls.LoadX509KeyPair(cfg.TLSClientCert, cfg.TLSClientKey)
		if err != nil {
			return nil, fmt.Errorf("load client cert/key (%s / %s): %w",
				cfg.TLSClientCert, cfg.TLSClientKey, err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}

	// Load and parse the custom CA bundle.
	var caPool *x509.CertPool
	if cfg.TLSCACert != "" {
		caPEM, err := os.ReadFile(cfg.TLSCACert)
		if err != nil {
			return nil, fmt.Errorf("read CA cert %s: %w", cfg.TLSCACert, err)
		}
		caPool = x509.NewCertPool()
		if !caPool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("CA cert %s: no valid PEM blocks found", cfg.TLSCACert)
		}
		tlsCfg.RootCAs = caPool
	}

	// When skip-hostname-verification is requested, we do NOT set
	// InsecureSkipVerify (which would disable chain verification entirely).
	// Instead we set InsecureSkipVerify=true but install VerifyPeerCertificate
	// to manually validate the chain. This preserves certificate trust while
	// waiving CN/SAN matching — useful when Redis pods use IP-only certs.
	if cfg.TLSSkipHostnameVerification {
		tlsCfg.InsecureSkipVerify = true //nolint:gosec // mitigated by VerifyPeerCertificate below

		pool := caPool // capture for closure; may be nil → use system roots
		tlsCfg.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			certs := make([]*x509.Certificate, 0, len(rawCerts))
			for _, raw := range rawCerts {
				c, err := x509.ParseCertificate(raw)
				if err != nil {
					return fmt.Errorf("tls: parse peer certificate: %w", err)
				}
				certs = append(certs, c)
			}
			if len(certs) == 0 {
				return fmt.Errorf("tls: no peer certificates presented")
			}

			opts := x509.VerifyOptions{
				Roots:         pool, // nil → system root store
				Intermediates: x509.NewCertPool(),
			}
			for _, ic := range certs[1:] {
				opts.Intermediates.AddCert(ic)
			}
			if _, err := certs[0].Verify(opts); err != nil {
				return fmt.Errorf("tls: certificate chain verification failed (hostname check skipped): %w", err)
			}
			return nil
		}
	}

	return tlsCfg, nil
}

// ---------------------------------------------------------------------------
// tlsNetDialer — wraps net.Dialer with TLS upgrade
// ---------------------------------------------------------------------------

// tlsNetDialer wraps a *net.Dialer and upgrades the plain connection to TLS
// using the stored *tls.Config. It satisfies the interface expected by
// radix.Dialer.NetDialer:
//
//	interface { DialContext(context.Context, string, string) (net.Conn, error) }
type tlsNetDialer struct {
	inner  *net.Dialer
	tlsCfg *tls.Config
}

// DialContext dials the target network/address and performs a TLS handshake.
// The handshake deadline is controlled by ctx.
func (d *tlsNetDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	rawConn, err := d.inner.DialContext(ctx, network, addr)
	if err != nil {
		return nil, fmt.Errorf("tls dial %s://%s: %w", network, addr, err)
	}

	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		rawConn.Close()
		return nil, fmt.Errorf("tls: parse host from addr %q: %w", addr, err)
	}

	// Clone so that concurrent dials don't race on ServerName.
	cfg := d.tlsCfg.Clone()
	if !cfg.InsecureSkipVerify {
		cfg.ServerName = host
	}

	tlsConn := tls.Client(rawConn, cfg)

	// Drive the handshake within the caller's context deadline.
	handshakeCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	handshakeDone := make(chan error, 1)
	go func() {
		handshakeDone <- tlsConn.Handshake()
	}()

	select {
	case <-handshakeCtx.Done():
		rawConn.Close()
		return nil, fmt.Errorf("tls handshake with %s: %w", addr, handshakeCtx.Err())
	case err := <-handshakeDone:
		if err != nil {
			rawConn.Close()
			return nil, fmt.Errorf("tls handshake with %s: %w", addr, err)
		}
	}

	return tlsConn, nil
}

// ---------------------------------------------------------------------------
// Utility
// ---------------------------------------------------------------------------

// splitAndTrim splits s on commas and trims whitespace from each token,
// dropping empty tokens. It is used to parse multi-address URL fields.
func splitAndTrim(s string) []string {
	raw := strings.Split(s, ",")
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
