package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/taha/myprog/internal/config"
	"github.com/taha/myprog/internal/engine"
	"github.com/taha/myprog/internal/filters"
	mygrpc "github.com/taha/myprog/internal/grpc"
	mylogger "github.com/taha/myprog/internal/logger"
	"github.com/taha/myprog/internal/memory"
	"github.com/taha/myprog/internal/redis"
	"github.com/taha/myprog/internal/router"
)

type activeChecker struct {
	checker *redis.HealthChecker
	cancel  context.CancelFunc
}

var activeHealthCheckers = make(map[string]activeChecker)

// activeCheckersMu guards all read and write access to activeHealthCheckers.
// Maps are not safe for concurrent use; the hot-reload goroutine and the boot
// path both mutate this map, so a mutex is mandatory to prevent data races.
var activeCheckersMu sync.RWMutex

func compileAndRegister(cfg *config.Config, registry *engine.ChainRegistry) error {
	mylogger.Debug("Starting compileAndRegister execution")
	newChains := make(map[string]engine.Chain)
	for name, chainConfig := range cfg.Chains {
		var compiledChain engine.Chain
		mylogger.Debug("Compiling chain", zap.String("chain_name", name))
		for _, filterCfg := range chainConfig {
			mylogger.Debug("Creating filter", zap.String("type", filterCfg.Type))
			filter, err := filters.CreateFilter(filterCfg.Type, filterCfg.Options)
			if err != nil {
				mylogger.Error("Failed to compile filter", zap.String("chain", name), zap.Error(err))
				return err
			}
			compiledChain = append(compiledChain, filter)
		}
		newChains[name] = compiledChain
		mylogger.Info("Compiled filter chain", zap.String("chain_name", name), zap.Int("filters_count", len(compiledChain)))
	}
	registry.ReplaceAll(newChains)
	mylogger.Debug("Chains replaced in registry successfully")
	return nil
}

func startHealthCheckForService(parentCtx context.Context, name string, client redis.Client) {
	mylogger.Debug("startHealthCheckForService triggered", zap.String("service", name))

	activeCheckersMu.Lock()
	if existing, ok := activeHealthCheckers[name]; ok {
		mylogger.Debug("Canceling existing health checker", zap.String("service", name))
		existing.cancel()
	}
	activeCheckersMu.Unlock()

	childCtx, cancel := context.WithCancel(parentCtx)
	hc := redis.StartHealthCheckForClient(childCtx, client, name, 5*time.Second)

	activeCheckersMu.Lock()
	activeHealthCheckers[name] = activeChecker{
		checker: hc,
		cancel:  cancel,
	}
	activeCheckersMu.Unlock()
	mylogger.Debug("New health checker registered successfully", zap.String("service", name))
}

func startRedisServices(ctx context.Context, cfg *config.Config) error {
	mylogger.Debug("startRedisServices triggered")
	if len(cfg.Redis) == 0 {
		mylogger.Debug("No Redis services defined in configuration")
		return nil
	}

	mylogger.Info("Initializing dynamic Redis manager for K8s services...")
	_, err := redis.NewManager(cfg)
	if err != nil {
		return fmt.Errorf("failed to initialize Redis manager: %w", err)
	}

	for name, svcCfg := range cfg.Redis {
		if svcCfg.ActiveConnHealthCheck {
			client, ok := redis.GlobalManager.GetClient(name)
			if ok {
				mylogger.Info("Starting background active connection health check...", zap.String("service", name))
				startHealthCheckForService(ctx, name, client)
			}
		}
	}

	return nil
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	initialConfig, configPath, err := config.LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Fatal error loading config: %v\n", err)
		os.Exit(1)
	}

	config.GlobalConfig.Store(initialConfig)

	if err := mylogger.InitLogger(&initialConfig.Telemetry.Logging); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize logger: %v\n", err)
		os.Exit(1)
	}
	defer mylogger.Sync()

	mylogger.Info("Configuration loaded successfully", zap.String("version", initialConfig.Version))
	mylogger.Debug("Application configuration fully stored in atomic pointer")

	if err := startRedisServices(ctx, initialConfig); err != nil {
		mylogger.Fatal("Failed to bootstrap Redis services on boot", zap.Error(err))
	}

	mylogger.Debug("Initializing core components")
	registry := engine.NewChainRegistry()
	executor := engine.NewChainExecutor()
	pool := memory.NewContextPool()
	routerInst := router.NewEngineRouter()

	mylogger.Debug("Core components successfully initialized")

	if err := compileAndRegister(initialConfig, registry); err != nil {
		mylogger.Fatal("Failed to compile chains on boot", zap.Error(err))
	}

	mylogger.Debug("Starting config hot-reloader goroutine")
	go config.WatchConfig(configPath, func(newConfig *config.Config) {
		mylogger.Info("Hot-reloading system connections and filter chains...")

		if len(newConfig.Redis) > 0 {
			if redis.GlobalManager == nil {
				mylogger.Debug("Redis manager is nil, performing cold start initialization")
				if err := startRedisServices(ctx, newConfig); err != nil {
					mylogger.Error("Failed to dynamically bootstrap Redis manager on hot-reload", zap.Error(err))
				}
			} else {
				mylogger.Debug("Redis manager exists, invoking graceful reload")
				if err := redis.GlobalManager.Reload(newConfig); err != nil {
					mylogger.Error("Failed to gracefully reload Redis connection pools", zap.Error(err))
				} else {
					mylogger.Debug("Redis connection pools successfully swapped")
					for name, svcCfg := range newConfig.Redis {
						client, ok := redis.GlobalManager.GetClient(name)
						if ok && svcCfg.ActiveConnHealthCheck {
							startHealthCheckForService(ctx, name, client)
						} else {
							activeCheckersMu.Lock()
							if existing, ok := activeHealthCheckers[name]; ok {
								existing.cancel()
								delete(activeHealthCheckers, name)
							}
							activeCheckersMu.Unlock()
						}
					}
					activeCheckersMu.Lock()
					for name, existing := range activeHealthCheckers {
						if _, ok := newConfig.Redis[name]; !ok {
							existing.cancel()
							delete(activeHealthCheckers, name)
						}
					}
					activeCheckersMu.Unlock()
				}
			}
		}

		mylogger.Debug("Re-compiling filter chains dynamically")
		_ = compileAndRegister(newConfig, registry)
	})

	mylogger.Debug("Initializing gRPC Server wrapper")
	grpcServer := mygrpc.NewGRPCServer(pool, routerInst, registry, executor)

	address := initialConfig.Server.Address
	if address == "" {
		address = ":9001"
	}

	mylogger.Debug("Binding network TCP listener", zap.String("address", address))
	listener, err := net.Listen("tcp", address)
	if err != nil {
		mylogger.Fatal("Failed to bind network listener", zap.String("address", address), zap.Error(err))
	}

	mylogger.Info("Starting high-performance ext_proc gRPC Server", zap.String("address", address))

	go func() {
		mylogger.Debug("gRPC Server Serve goroutine spawned")
		if err := grpcServer.Serve(listener); err != nil {
			mylogger.Fatal("gRPC server encountered a fatal error", zap.Error(err))
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	mylogger.Debug("Server is fully active, waiting for shutdown signals")
	sig := <-stop

	mylogger.Info("OS signal caught, initiating graceful shutdown...", zap.String("signal", sig.String()))

	mylogger.Debug("Canceling global background context")
	cancel()

	mylogger.Debug("Stopping gRPC server gracefully")
	grpcServer.GracefulStop()

	if redis.GlobalManager != nil {
		mylogger.Debug("Closing Redis connection pools")
		redis.GlobalManager.Close()
	}

	mylogger.Info("gRPC server stopped gracefully")
}
