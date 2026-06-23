package grpc

import (
	"context"
	"time"

	"go.uber.org/zap"
	rpcstatus "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"

	"github.com/taha/myprog/internal/config"
	"github.com/taha/myprog/internal/engine"
	mylogger "github.com/taha/myprog/internal/logger"
	"github.com/taha/myprog/internal/memory"
	"github.com/taha/myprog/internal/router"
	authv3 "github.com/taha/myprog/pkg/api/envoy/service/auth/v3"
)

type Server struct {
	pool     *memory.ContextPool
	router   *router.EngineRouter
	registry *engine.ChainRegistry
	executor *engine.ChainExecutor
	authv3.UnimplementedAuthorizationServer
}

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

	authv3.RegisterAuthorizationServer(grpcServer, authServer)

	return grpcServer
}

func (s *Server) Check(ctx context.Context, req *authv3.CheckRequest) (*authv3.CheckResponse, error) {
	startTime := time.Now()

	reqCtx := s.pool.Acquire()
	reqCtx.Ctx = ctx // Bind the dynamic gRPC request context
	defer s.pool.Release(reqCtx)

	if req.Attributes != nil && req.Attributes.Request != nil && req.Attributes.Request.Http != nil {
		httpReq := req.Attributes.Request.Http
		reqCtx.Path = httpReq.Path
		reqCtx.Method = httpReq.Method

		mylogger.Debug("gRPC check request received",
			zap.String("path", reqCtx.Path),
			zap.String("method", reqCtx.Method),
		)

		for k, v := range httpReq.Headers {
			reqCtx.Headers[k] = v
		}
	}

	targetChainName := s.router.Route(reqCtx)

	if targetChainName != "" {
		chain, exists := s.registry.Get(targetChainName)
		if !exists {
			mylogger.Warn("Target chain not found in registry, failing open", zap.String("chain", targetChainName))
		} else {
			err := s.executor.Execute(reqCtx, chain)
			if err != nil {
				mylogger.Error("Error executing chain", zap.String("chain", targetChainName), zap.Error(err))
			}
		}
	}

	latency := time.Since(startTime)

	if reqCtx.Blocked {
		mylogger.Info("gRPC check request denied",
			zap.String("path", reqCtx.Path),
			zap.String("method", reqCtx.Method),
			zap.Duration("latency", latency),
			zap.Int32("status_code", int32(reqCtx.ResponseStatus)),
			zap.String("decision", "DENY"),
		)

		deniedResponse := &authv3.DeniedHttpResponse{
			Status: &typev3.HttpStatus{
				Code: typev3.StatusCode(reqCtx.ResponseStatus),
			},
			Body: reqCtx.ResponseBody,
		}

		for _, h := range reqCtx.ResponseHeadersToAdd {
			opt := &corev3.HeaderValueOption{
				Header: &corev3.HeaderValue{
					Key:   h.Key,
					Value: h.Value,
				},
				AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
			}
			deniedResponse.Headers = append(deniedResponse.Headers, opt)
		}

		return &authv3.CheckResponse{
			Status: &rpcstatus.Status{
				Code: int32(codes.PermissionDenied),
			},
			HttpResponse: &authv3.CheckResponse_DeniedResponse{
				DeniedResponse: deniedResponse,
			},
		}, nil
	}

	mylogger.Debug("gRPC check request allowed",
		zap.String("path", reqCtx.Path),
		zap.String("method", reqCtx.Method),
		zap.Duration("latency", latency),
		zap.String("decision", "ALLOW"),
	)

	okResponse := &authv3.OkHttpResponse{}

	for _, h := range reqCtx.HeadersToAdd {
		opt := &corev3.HeaderValueOption{
			Header: &corev3.HeaderValue{
				Key:   h.Key,
				Value: h.Value,
			},
			AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
		}
		okResponse.Headers = append(okResponse.Headers, opt)
	}

	for _, h := range reqCtx.ResponseHeadersToAdd {
		opt := &corev3.HeaderValueOption{
			Header: &corev3.HeaderValue{
				Key:   h.Key,
				Value: h.Value,
			},
			AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
		}
		okResponse.ResponseHeadersToAdd = append(okResponse.ResponseHeadersToAdd, opt)
	}

	okResponse.HeadersToRemove = append(okResponse.HeadersToRemove, reqCtx.HeadersToRemove...)

	return &authv3.CheckResponse{
		Status: &rpcstatus.Status{
			Code: int32(codes.OK),
		},
		HttpResponse: &authv3.CheckResponse_OkResponse{
			OkResponse: okResponse,
		},
	}, nil
}
