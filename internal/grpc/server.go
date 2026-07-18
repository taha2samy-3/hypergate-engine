package grpc

import (
	"io"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"

	"github.com/taha/myprog/internal/config"
	"github.com/taha/myprog/internal/engine"
	mylogger "github.com/taha/myprog/internal/logger"
	"github.com/taha/myprog/internal/memory"
	"github.com/taha/myprog/internal/router"
)

type Server struct {
	pool     *memory.ContextPool
	router   *router.EngineRouter
	registry *engine.ChainRegistry
	executor *engine.ChainExecutor
	extprocv3.UnimplementedExternalProcessorServer
}

// NewGRPCServer initializes the gRPC server with high-performance keepalive settings.
func NewGRPCServer(
	pool *memory.ContextPool,
	routerInst *router.EngineRouter,
	registry *engine.ChainRegistry,
	executor *engine.ChainExecutor,
) *grpc.Server {
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

	grpcServer := grpc.NewServer(opts...)
	authServer := &Server{
		pool:     pool,
		router:   routerInst,
		registry: registry,
		executor: executor,
	}

	extprocv3.RegisterExternalProcessorServer(grpcServer, authServer)
	return grpcServer
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
		if err == io.EOF {
			return nil
		}
		if err != nil {
			if stream.Context().Err() != nil {
				return nil
			}
			mylogger.Error("ext_proc stream receive error", zap.Error(err))
			return err
		}

		// Dispatch to specific handlers based on the request type.
		// These handlers are implemented in handlers.go
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
			return err
		}
	}
}