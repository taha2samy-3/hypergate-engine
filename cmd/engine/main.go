package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/taha2samy/hypergate/internal/config"
	"github.com/taha2samy/hypergate/internal/engine"
	mygrpc "github.com/taha2samy/hypergate/internal/grpc"
	mylogger "github.com/taha2samy/hypergate/internal/logger"
	"github.com/taha2samy/hypergate/internal/memory"
	"github.com/taha2samy/hypergate/internal/policy"
	"github.com/taha2samy/hypergate/internal/redis"
	"github.com/taha2samy/hypergate/internal/router"
)

// startHealthServer serves Kubernetes probes: /healthz reports process liveness and
// /readyz reports whether a policy is loaded and the gRPC listener is serving.
func startHealthServer(addr string, ready func() bool) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ready"))
	})
	return serveHTTP("health", addr, mux)
}

// startPprofServer exposes profiling only when explicitly configured.
func startPprofServer(addr string) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return serveHTTP("pprof", addr, mux)
}

func serveHTTP(name, addr string, handler http.Handler) *http.Server {
	srv := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			mylogger.Error("HTTP server failed", zap.String("server", name), zap.String("address", addr), zap.Error(err))
		}
	}()
	mylogger.Info("HTTP server listening", zap.String("server", name), zap.String("address", addr))
	return srv
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	initialConfig, configPath, err := config.LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Fatal error loading config: %v\n", err)
		os.Exit(1)
	}

	if err := mylogger.InitLogger(&initialConfig.Telemetry.Logging); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize logger: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		_ = mylogger.Sync()
	}()

	mylogger.Info("Configuration loaded successfully", zap.String("version", initialConfig.Version))

	registry := engine.NewChainRegistry()
	policyMgr := policy.NewManager(ctx, registry, redis.NewPoolClient)
	if err := policyMgr.Apply(initialConfig); err != nil {
		mylogger.Fatal("Failed to compile policy on boot", zap.Error(err))
	}

	var serving atomic.Bool
	healthSrv := startHealthServer(initialConfig.Server.HealthAddress, func() bool {
		return serving.Load() && policyMgr.Ready()
	})
	var pprofSrv *http.Server
	if initialConfig.Server.PprofAddress != "" {
		pprofSrv = startPprofServer(initialConfig.Server.PprofAddress)
	}

	// Hot reload: the new policy is only published once every chain compiled; on
	// failure the previous policy keeps serving.
	config.WatchConfig(configPath, func(newConfig *config.Config) error {
		mylogger.Info("Hot-reloading policy...", zap.String("version", newConfig.Version))
		if err := policyMgr.Apply(newConfig); err != nil {
			return err
		}
		mylogger.Info("Policy reloaded successfully")
		return nil
	}, func(err error) {
		mylogger.Error("Config reload rejected, keeping previous policy", zap.Error(err))
	})

	executor := engine.NewChainExecutor()
	pool := memory.NewContextPool(initialConfig.Server.InitialHeaderCapacity, initialConfig.Server.PreallocBodyBufferBytes)
	pool.Prewarm(initialConfig.Server.PoolPrewarmSize)
	routerInst := router.NewEngineRouter()

	mylogger.Info("Core components successfully initialized",
		zap.Int("pool_prewarm_size", initialConfig.Server.PoolPrewarmSize),
		zap.Int("initial_header_capacity", initialConfig.Server.InitialHeaderCapacity),
	)

	grpcServer, err := mygrpc.NewGRPCServer(pool, routerInst, registry, executor)
	if err != nil {
		mylogger.Fatal("Failed to initialize gRPC server", zap.Error(err))
	}

	address := initialConfig.Server.Address
	if address == "" {
		address = ":9001"
	}

	listener, err := net.Listen("tcp", address)
	if err != nil {
		mylogger.Fatal("Failed to bind network listener", zap.String("address", address), zap.Error(err))
	}

	mylogger.Info("Starting ext_proc gRPC Server", zap.String("address", address))

	go func() {
		if err := grpcServer.Serve(listener); err != nil {
			mylogger.Fatal("gRPC server encountered a fatal error", zap.Error(err))
		}
	}()
	serving.Store(true)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	sig := <-stop

	mylogger.Info("OS signal caught, initiating graceful shutdown...", zap.String("signal", sig.String()))
	serving.Store(false)

	grpcServer.GracefulStop()
	cancel()
	policyMgr.Close()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = healthSrv.Shutdown(shutdownCtx)
	if pprofSrv != nil {
		_ = pprofSrv.Shutdown(shutdownCtx)
	}

	mylogger.Info("gRPC server stopped gracefully")
}
