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

type clientImpl struct {
	client                     radix.Client
	serviceName                string
	topology                   string
	configuredPoolSize         int
	isCluster                  bool
	clusterPipelineParallelism int
	activeConns                int64
}

func (c *clientImpl) DoCmd(rcv interface{}, cmd, key string, args ...interface{}) error {
	atomic.AddInt64(&c.activeConns, 1)
	defer atomic.AddInt64(&c.activeConns, -1)

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

func (c *clientImpl) Close() error {
	if err := c.client.Close(); err != nil {
		return fmt.Errorf("redis[%s].Close: %w", c.serviceName, err)
	}
	return nil
}

func (c *clientImpl) NumActiveConns() int {
	return int(atomic.LoadInt64(&c.activeConns))
}

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

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("redis.NewClientConn[%s]: context cancelled during startup: %w",
				serviceName, ctx.Err())
		default:
		}

		conn, dialErr := dialTopology(ctx, serviceName, cfg, dialer)
		if dialErr == nil {
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

		if hasDeadline && time.Now().After(deadline) {
			return nil, fmt.Errorf(
				"redis.NewClientConn[%s]: startup_max_elapsed_time (%s) exceeded after %d attempt(s): %w",
				serviceName, cfg.StartupMaxElapsedTime, attempt, dialErr,
			)
		}

		jitterFactor := 0.5 + rand.Float64()
		sleep := time.Duration(float64(backoff) * jitterFactor)

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

		backoff = time.Duration(math.Min(
			float64(backoff*2),
			float64(maxBackoff),
		))
	}
}

func NewPoolClient(serviceName string, cfg config.RedisServiceConfig) (Client, error) {
	return NewClientConn(context.Background(), serviceName, cfg)
}

type multiClientAdapter struct {
	mc radix.MultiClient
}

func (a *multiClientAdapter) Do(ctx context.Context, action radix.Action) error {
	return a.mc.Do(ctx, action)
}

func (a *multiClientAdapter) Addr() net.Addr {
	return multiClientAddr{}
}

func (a *multiClientAdapter) Close() error {
	return a.mc.Close()
}

type multiClientAddr struct{}

func (multiClientAddr) Network() string { return "tcp" }
func (multiClientAddr) String() string  { return "<multi>" }

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
		return nil, fmt.Errorf("unknown topology type %q", cfg.Type)
	}
}

func dialSingle(ctx context.Context, cfg config.RedisServiceConfig, dialer radix.Dialer) (radix.Client, error) {
	if cfg.PipelineWindowDuration > 0 {
		dialer.WriteFlushInterval = cfg.PipelineWindowDuration
	}

	poolCfg := radix.PoolConfig{
		Size:                 cfg.PoolSize,
		Dialer:               dialer,
		MinReconnectInterval: cfg.StartupInitialIntervalDuration,
		MaxReconnectInterval: cfg.StartupMaxIntervalDuration,
	}

	conn, err := poolCfg.New(ctx, cfg.SocketType, cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("SINGLE pool %s://%s: %w", cfg.SocketType, cfg.URL, err)
	}
	return conn, nil
}

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

	sentinelDialer := dataDialer
	sentinelDialer.AuthUser = ""
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

func buildDialer(cfg config.RedisServiceConfig) (radix.Dialer, error) {
	dialer := radix.Dialer{
		NetDialer: &net.Dialer{
			Timeout:   cfg.TimeoutDuration,
			KeepAlive: 10 * time.Second,
		},
	}

	if cfg.Auth != "" {
		if idx := strings.IndexByte(cfg.Auth, ':'); idx >= 0 {
			dialer.AuthUser = cfg.Auth[:idx]
			dialer.AuthPass = cfg.Auth[idx+1:]
		} else {
			dialer.AuthPass = cfg.Auth
		}
	}

	if cfg.TLS {
		tlsCfg, err := buildTLSConfig(cfg)
		if err != nil {
			return radix.Dialer{}, fmt.Errorf("buildDialer: tls config: %w", err)
		}

		netDialer := dialer.NetDialer.(*net.Dialer)
		dialer.NetDialer = &tlsNetDialer{
			inner:  netDialer,
			tlsCfg: tlsCfg,
		}
	}

	return dialer, nil
}

func buildTLSConfig(cfg config.RedisServiceConfig) (*tls.Config, error) {
	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}

	if cfg.TLSClientCert != "" && cfg.TLSClientKey != "" {
		cert, err := tls.LoadX509KeyPair(cfg.TLSClientCert, cfg.TLSClientKey)
		if err != nil {
			return nil, fmt.Errorf("load client cert/key (%s / %s): %w",
				cfg.TLSClientCert, cfg.TLSClientKey, err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}

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

	if cfg.TLSSkipHostnameVerification {
		tlsCfg.InsecureSkipVerify = true

		pool := caPool
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
				Roots:         pool,
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

type tlsNetDialer struct {
	inner  *net.Dialer
	tlsCfg *tls.Config
}

func (d *tlsNetDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	rawConn, err := d.inner.DialContext(ctx, network, addr)
	if err != nil {
		return nil, fmt.Errorf("tls dial %s://%s: %w", network, addr, err)
	}

	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		_ = rawConn.Close()
		return nil, fmt.Errorf("tls: parse host from addr %q: %w", addr, err)
	}

	cfg := d.tlsCfg.Clone()
	if !cfg.InsecureSkipVerify {
		cfg.ServerName = host
	}

	tlsConn := tls.Client(rawConn, cfg)

	handshakeCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	handshakeDone := make(chan error, 1)
	go func() {
		handshakeDone <- tlsConn.Handshake()
	}()

	select {
	case <-handshakeCtx.Done():
		_ = rawConn.Close()
		return nil, fmt.Errorf("tls handshake with %s: %w", addr, handshakeCtx.Err())
	case err := <-handshakeDone:
		if err != nil {
			_ = rawConn.Close()
			return nil, fmt.Errorf("tls handshake with %s: %w", addr, err)
		}
	}

	return tlsConn, nil
}

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