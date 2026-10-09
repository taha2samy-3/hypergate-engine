package grpc

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"

	"github.com/taha2samy/hypergate/internal/config"
	"github.com/taha2samy/hypergate/internal/engine"
	"github.com/taha2samy/hypergate/internal/identity"
	mylogger "github.com/taha2samy/hypergate/internal/logger"
	"github.com/taha2samy/hypergate/internal/memory"
	"github.com/taha2samy/hypergate/internal/metrics"
	"github.com/taha2samy/hypergate/internal/router"
)

type Server struct {
	pool     *memory.ContextPool
	router   *router.EngineRouter
	registry *engine.ChainRegistry
	executor *engine.ChainExecutor
	// identity is the workload identity map, nil when identity is disabled.
	identity *identity.Index
	// activity records stream starts for graceful shutdown (may be nil).
	activity *Activity
	// metrics records request outcomes and timings (may be nil).
	metrics *metrics.Engine
	// limiter enforces per-chain max_concurrency.
	limiter chainLimiter
	extprocv3.UnimplementedExternalProcessorServer
}

// Option configures the ext_proc server.
type Option func(*Server)

// WithMetrics makes the server record Prometheus metrics in m.
func WithMetrics(m *metrics.Engine) Option {
	return func(s *Server) { s.metrics = m }
}

// WithIdentity makes the server resolve callers and destinations in the
// workload identity map.
func WithIdentity(idx *identity.Index) Option {
	return func(s *Server) { s.identity = idx }
}

// NewGRPCServer initializes the gRPC server with high-performance keepalive settings
// and optional TLS/mTLS based on server configuration.
func NewGRPCServer(
	pool *memory.ContextPool,
	routerInst *router.EngineRouter,
	registry *engine.ChainRegistry,
	executor *engine.ChainExecutor,
	options ...Option,
) (*grpc.Server, error) {
	activeCfg := config.GlobalConfig.Load()

	kaParams := keepalive.ServerParameters{
		MaxConnectionIdle:     15 * time.Minute,
		MaxConnectionAge:      30 * time.Minute,
		MaxConnectionAgeGrace: 5 * time.Minute,
		Time:                  5 * time.Minute,
		Timeout:               1 * time.Second,
	}

	kaEnforcement := keepalive.EnforcementPolicy{
		MinTime:             5 * time.Minute,
		PermitWithoutStream: true,
	}

	var opts []grpc.ServerOption
	opts = append(opts, grpc.KeepaliveParams(kaParams))
	opts = append(opts, grpc.KeepaliveEnforcementPolicy(kaEnforcement))

	if activeCfg != nil && activeCfg.Server.MaxConcurrentStreams > 0 {
		opts = append(opts, grpc.MaxConcurrentStreams(activeCfg.Server.MaxConcurrentStreams))
	} else {
		opts = append(opts, grpc.MaxConcurrentStreams(10000))
	}

	// Configure TLS or mTLS credentials
	if activeCfg != nil && activeCfg.Server.TLS.Enabled {
		creds, err := buildTLSCredentials(&activeCfg.Server.TLS)
		if err != nil {
			return nil, fmt.Errorf("failed to build TLS credentials: %w", err)
		}
		opts = append(opts, grpc.Creds(creds))
		if activeCfg.Server.TLS.MutualTLS {
			mylogger.Info("gRPC server: mTLS enabled (client certificate verification required)")
		} else {
			mylogger.Info("gRPC server: TLS enabled (server-side only)")
		}
	} else {
		// Insecure — Envoy handles the outer TLS when running inside the cluster mesh.
		opts = append(opts, grpc.Creds(insecure.NewCredentials()))
		mylogger.Info("gRPC server: running without TLS (insecure mode)")
	}

	grpcServer := grpc.NewServer(opts...)
	authServer := &Server{
		pool:     pool,
		router:   routerInst,
		registry: registry,
		executor: executor,
	}
	for _, o := range options {
		o(authServer)
	}

	extprocv3.RegisterExternalProcessorServer(grpcServer, authServer)
	return grpcServer, nil
}

// buildTLSCredentials constructs TLS server credentials from the provided config.
// When MutualTLS is true, the server requires clients to present a certificate
// signed by the configured CA (mutual authentication).
func buildTLSCredentials(cfg *config.TLSConfig) (credentials.TransportCredentials, error) {
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load server certificate/key pair (%s, %s): %w",
			cfg.CertFile, cfg.KeyFile, err)
	}

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	}

	if cfg.MutualTLS {
		if cfg.CAFile == "" {
			return nil, fmt.Errorf("mutual_tls is true but ca_file is empty")
		}
		caPEM, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read CA file %s: %w", cfg.CAFile, err)
		}
		caPool := x509.NewCertPool()
		if !caPool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("failed to parse CA certificate from %s", cfg.CAFile)
		}
		tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
		tlsCfg.ClientCAs = caPool
	}

	return credentials.NewTLS(tlsCfg), nil
}

// Process is the main bidirectional stream handler for Envoy ext_proc.
//
// The stream pins the policy snapshot that is current when it opens, so a hot
// reload never changes the chain applied to an in-flight request. Errors are
// returned to Envoy as gRPC statuses so its failure_mode_allow setting applies.
func (s *Server) Process(stream extprocv3.ExternalProcessor_ProcessServer) error {
	mylogger.Debug("ext_proc bidirectional stream opened")
	startTime := time.Now()

	if s.activity != nil {
		s.activity.streamStarted()
		defer s.activity.streamEnded()
	}

	st := &streamState{snap: s.registry.Acquire()}
	defer st.snap.Release()

	reqCtx := s.pool.Acquire()
	reqCtx.Ctx = stream.Context()
	reqCtx.Traffic = trafficFromContext(stream.Context())
	defer func() {
		s.recordOutcome(st, reqCtx)
		mylogger.Debug("ext_proc stream closing, releasing context", zap.Duration("duration", time.Since(startTime)))
		s.pool.Release(reqCtx)
	}()

	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			if stream.Context().Err() != nil || errors.Is(err, context.Canceled) || status.Code(err) == codes.Canceled {
				// Envoy closed the stream (request finished or was reset); nothing to report.
				return nil
			}
			mylogger.Error("ext_proc stream receive error", zap.Error(err))
			return err
		}

		msgStart := time.Now()
		var phase string
		switch msg := req.Request.(type) {
		case *extprocv3.ProcessingRequest_RequestHeaders:
			phase = "request_headers"
			err = s.handleRequestHeaders(stream, st, reqCtx, req, msg.RequestHeaders)
		case *extprocv3.ProcessingRequest_RequestBody:
			phase = "request_body"
			err = s.handleRequestBody(stream, st, reqCtx, msg.RequestBody)
		case *extprocv3.ProcessingRequest_RequestTrailers:
			phase = "request_trailers"
			err = s.handleRequestTrailers(stream, st, reqCtx, msg.RequestTrailers)
		case *extprocv3.ProcessingRequest_ResponseHeaders:
			phase = "response_headers"
			err = s.handleResponseHeaders(stream, st, reqCtx, msg.ResponseHeaders)
		case *extprocv3.ProcessingRequest_ResponseBody:
			phase = "response_body"
			err = s.handleResponseBody(stream, st, reqCtx, msg.ResponseBody)
		case *extprocv3.ProcessingRequest_ResponseTrailers:
			phase = "response_trailers"
			err = s.handleResponseTrailers(stream, st, reqCtx, msg.ResponseTrailers)
		default:
			err = status.Errorf(codes.InvalidArgument, "unsupported ext_proc message %T", req.Request)
		}
		if phase != "" {
			s.metrics.Observe(phase, st.name, time.Since(msgStart))
		}

		if err != nil {
			if stream.Context().Err() != nil {
				return nil
			}
			mylogger.Error("ext_proc stream send error", zap.Error(err))
			return err
		}
	}
}

// recordOutcome counts the finished request. Denials and failures also record
// which filter (or the engine itself) decided.
func (s *Server) recordOutcome(st *streamState, reqCtx *engine.RequestContext) {
	if s.metrics == nil || !st.resolved {
		return
	}
	for _, hit := range reqCtx.AuditHits {
		s.metrics.Audit(st.name, st.snap.FilterName(st.name, hit.Index), hit.Status)
	}
	outcome := metrics.OutcomeAllowed
	if reqCtx.Blocked {
		status := reqCtx.ResponseStatus
		if status == 0 {
			status = 403 // what immediateResponse sends for a block without a status
		}
		switch {
		case reqCtx.FilterFailed:
			outcome = metrics.OutcomeError
		case reqCtx.Answered || status < 400:
			outcome = metrics.OutcomeAnswered
		default:
			outcome = metrics.OutcomeDenied
		}
		if outcome != metrics.OutcomeAnswered {
			s.metrics.Deny(st.name, st.snap.FilterName(st.name, reqCtx.BlockedBy), status)
		}
	}
	s.metrics.Request(reqCtx.MatchedRoute, st.name, outcome)
}
