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
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"

	"github.com/taha2samy/hypergate/internal/config"
	"github.com/taha2samy/hypergate/internal/engine"
	mylogger "github.com/taha2samy/hypergate/internal/logger"
	"github.com/taha2samy/hypergate/internal/memory"
	"github.com/taha2samy/hypergate/internal/router"
)

type Server struct {
	pool     *memory.ContextPool
	router   *router.EngineRouter
	registry *engine.ChainRegistry
	executor *engine.ChainExecutor
	extprocv3.UnimplementedExternalProcessorServer
}

// NewGRPCServer initializes the gRPC server with high-performance keepalive settings
// and optional TLS/mTLS based on server configuration.
func NewGRPCServer(
	pool *memory.ContextPool,
	routerInst *router.EngineRouter,
	registry *engine.ChainRegistry,
	executor *engine.ChainExecutor,
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
func (s *Server) Process(stream extprocv3.ExternalProcessor_ProcessServer) error {
	mylogger.Debug("ext_proc bidirectional stream opened")
	startTime := time.Now()

	reqCtx := s.pool.Acquire()
	reqCtx.Ctx = stream.Context()
	defer func() {
		mylogger.Debug("ext_proc stream closing, releasing context", zap.Duration("duration", time.Since(startTime)))
		s.pool.Release(reqCtx)
	}()

	var targetChainName string

	for {
		req, err := stream.Recv()
		// Gracefully handle stream closure and context cancellation to prevent transport resets
		if err == io.EOF || errors.Is(err, context.Canceled) {
			return nil
		}
		if err != nil {
			if stream.Context().Err() != nil {
				return nil
			}
			mylogger.Error("ext_proc stream receive error", zap.Error(err))
			return nil
		}

		// Dispatch to specific handlers based on the request type.
		switch msg := req.Request.(type) {
		case *extprocv3.ProcessingRequest_RequestHeaders:
			targetChainName, err = s.handleRequestHeaders(stream, reqCtx, msg.RequestHeaders)
		case *extprocv3.ProcessingRequest_RequestBody:
			err = s.handleRequestBody(stream, reqCtx, msg.RequestBody, targetChainName)
		case *extprocv3.ProcessingRequest_RequestTrailers:
			err = s.handleRequestTrailers(stream, reqCtx, msg.RequestTrailers, targetChainName)
		case *extprocv3.ProcessingRequest_ResponseHeaders:
			err = s.handleResponseHeaders(stream, reqCtx, msg.ResponseHeaders)
		case *extprocv3.ProcessingRequest_ResponseBody:
			err = s.handleResponseBody(stream, reqCtx, msg.ResponseBody, targetChainName)
		case *extprocv3.ProcessingRequest_ResponseTrailers:
			err = s.handleResponseTrailers(stream, reqCtx, msg.ResponseTrailers, targetChainName)
		}

		if err != nil {
			return nil
		}
	}
}