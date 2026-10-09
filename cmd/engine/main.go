package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/KimMachineGun/automemlimit/memlimit"
	"go.uber.org/zap"

	"github.com/taha2samy/hypergate/internal/config"
	"github.com/taha2samy/hypergate/internal/engine"
	mygrpc "github.com/taha2samy/hypergate/internal/grpc"
	"github.com/taha2samy/hypergate/internal/identity"
	mylogger "github.com/taha2samy/hypergate/internal/logger"
	"github.com/taha2samy/hypergate/internal/memory"
	"github.com/taha2samy/hypergate/internal/policy"
	"github.com/taha2samy/hypergate/internal/redis"
	"github.com/taha2samy/hypergate/internal/router"
)

// startHealthServer serves Kubernetes probes: /healthz reports process liveness and
// /readyz reports whether a policy is loaded and the gRPC listener is serving.
func startHealthServer(addr string, ready func() bool, extra map[string]http.Handler) *http.Server {
	mux := http.NewServeMux()
	for path, h := range extra {
		mux.Handle(path, h)
	}
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

	// Go does not derive its memory limit from the container's, so without this
	// the GC lets the heap grow until the kernel OOM-kills the engine. Uses 90 %
	// of the cgroup limit; GOMEMLIMIT or AUTOMEMLIMIT=off take precedence.
	if limit, err := memlimit.Set(); err != nil {
		mylogger.Warn("Could not set the Go memory limit from the container limit", zap.Error(err))
	} else if limit != math.MaxInt64 {
		mylogger.Info("Go memory limit set from the container limit", zap.Int64("bytes", limit))
	}

	registry := engine.NewChainRegistry()
	policyMgr := policy.NewManager(ctx, registry, redis.NewClientConn)

	// Serve probes before compiling the policy: compiling waits for every Redis
	// service (indefinitely by default at boot) and fetches JWKS. /healthz answers
	// during that wait so the liveness probe does not restart the pod, while
	// /readyz stays 503 until the policy is loaded and gRPC is serving.
	// Workload identity: the map is streamed from the operator. It is read at
	// start-up only; changing the identity block needs a restart.
	var identityIndex *identity.Index
	debugHandlers := map[string]http.Handler{}
	if idCfg := initialConfig.Identity; idCfg.Enabled {
		identityIndex = identity.NewIndex()
		client := identity.NewClient(identity.Options{
			Address:    idCfg.Address,
			CAFile:     idCfg.CAFile,
			ServerName: idCfg.ServerName,
			TokenFile:  idCfg.TokenFile,
			Insecure:   idCfg.Insecure,
			NodeID:     os.Getenv("NODE_NAME"),
		}, identityIndex)
		go client.Run(ctx)
		debugHandlers["/debug/identity"] = identity.DebugHandler(client)
		mylogger.Info("Workload identity enabled", zap.String("address", idCfg.Address))
	}

	var serving atomic.Bool
	healthSrv := startHealthServer(initialConfig.Server.HealthAddress, func() bool {
		// With identity enabled, requests are only accepted once the first full
		// identity map has arrived, so callers are never misclassified at start-up.
		return serving.Load() && policyMgr.Ready() && (identityIndex == nil || identityIndex.Synced())
	}, debugHandlers)
	var pprofSrv *http.Server
	if initialConfig.Server.PprofAddress != "" {
		pprofSrv = startPprofServer(initialConfig.Server.PprofAddress)
	}

	if err := policyMgr.Apply(initialConfig); err != nil {
		mylogger.Fatal("Failed to compile policy on boot", zap.Error(err))
	}

	// Hot reload: the new policy is only published once every chain compiled; on
	// failure the previous policy keeps serving.
	config.WatchConfig(configPath, func(newConfig *config.Config) error {
		mylogger.Info("Hot-reloading policy...", zap.String("version", newConfig.Version))
		// The identity client is started at boot only. Turning identity on or off
		// without a restart would leave workload selectors without a map.
		if newConfig.Identity.Enabled != (identityIndex != nil) {
			return fmt.Errorf("identity.enabled changed; restart the engine to apply it")
		}
		if newConfig.Identity != initialConfig.Identity {
			mylogger.Warn("Identity settings changed; they take effect after a restart")
		}
		if err := policyMgr.Apply(newConfig); err != nil {
			return err
		}
		mylogger.Info("Policy reloaded successfully")
		return nil
	}, func(err error) {
		mylogger.Error("Config reload rejected, keeping previous policy", zap.Error(err))
	})

	executor := engine.NewChainExecutor()
	if initialConfig.Server.PreallocBodyBufferBytes > 0 {
		mylogger.Warn("server.prealloc_body_buffer_bytes is deprecated and ignored")
	}
	pool := memory.NewContextPool(initialConfig.Server.InitialHeaderCapacity)
	pool.Prewarm(initialConfig.Server.PoolPrewarmSize)
	routerInst := router.NewEngineRouter()

	mylogger.Info("Core components successfully initialized",
		zap.Int("pool_prewarm_size", initialConfig.Server.PoolPrewarmSize),
		zap.Int("initial_header_capacity", initialConfig.Server.InitialHeaderCapacity),
	)

	activity := &mygrpc.Activity{}
	serverOpts := []mygrpc.Option{mygrpc.WithActivity(activity)}
	if identityIndex != nil {
		serverOpts = append(serverOpts, mygrpc.WithIdentity(identityIndex))
	}
	grpcServer, err := mygrpc.NewGRPCServer(pool, routerInst, registry, executor, serverOpts...)
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

	stop := make(chan os.Signal, 2)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	sig := <-stop

	// Stop sequence: report not ready, keep serving until Envoy has stopped
	// opening streams (it learns about the terminating endpoint asynchronously),
	// then drain open streams for a bounded time. A second signal skips the waits.
	sh := initialConfig.Server.Shutdown
	mylogger.Info("OS signal caught, shutting down gracefully",
		zap.String("signal", sig.String()),
		zap.Duration("quiet_period", sh.QuietPeriodDuration),
		zap.Duration("max_delay", sh.MaxDelayDuration),
		zap.Duration("drain_timeout", sh.DrainTimeoutDuration))
	serving.Store(false)

	hurry, hurryNow := context.WithCancel(context.Background())
	go func() {
		<-stop
		mylogger.Warn("Second signal, stopping without waiting")
		hurryNow()
	}()
	waited := mygrpc.WaitForQuiet(hurry, activity, sh.QuietPeriodDuration, sh.MinDelayDuration, sh.MaxDelayDuration)
	mylogger.Info("Stopped accepting new streams", zap.Duration("waited", waited), zap.Int64("open_streams", activity.Active()))

	drain := sh.DrainTimeoutDuration
	if hurry.Err() != nil {
		drain = 0
	}
	if forced := mygrpc.StopWithin(grpcServer, drain); forced {
		mylogger.Warn("Drain timeout reached, closed the remaining streams")
	}
	hurryNow()
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
