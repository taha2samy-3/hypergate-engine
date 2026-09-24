// Package policy turns a parsed config into a live engine.Snapshot and owns the
// lifecycle of everything a snapshot depends on: Redis clients, compiled filter
// instances and Redis health checkers.
//
// Apply is transactional. The complete new policy (Redis clients and every filter
// of every chain) is built before anything is published; if any step fails, the
// resources created so far are released and the previously active policy keeps
// serving untouched. Only after a successful build are the routes and chains
// swapped in together, so a request can never be routed to a missing chain.
//
// Resources are reused across reloads when their configuration is unchanged,
// which keeps L1 caches, JWKS key sets and sidecar connections warm. Resources a
// reload no longer needs are closed only once the last in-flight stream holding
// the previous snapshot has finished.
package policy

import (
	"context"
	"fmt"
	"io"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"gopkg.in/yaml.v3"

	"github.com/taha2samy/hypergate/internal/config"
	"github.com/taha2samy/hypergate/internal/engine"
	"github.com/taha2samy/hypergate/internal/filters"
	mylogger "github.com/taha2samy/hypergate/internal/logger"
	"github.com/taha2samy/hypergate/internal/redis"
)

// RedisFactory creates a Redis client for a named service. ctx bounds connection retries.
type RedisFactory func(ctx context.Context, name string, cfg config.RedisServiceConfig) (redis.Client, error)

// FilterFactory compiles one filter. It defaults to filters.CreateFilter.
type FilterFactory func(filterType string, options interface{}, lookup filters.RedisLookup) (engine.Filter, error)

type redisEntry struct {
	cfg        config.RedisServiceConfig
	client     redis.Client
	stopHealth context.CancelFunc
}

// Manager applies configs to an engine.ChainRegistry.
type Manager struct {
	ctx       context.Context
	registry  *engine.ChainRegistry
	newRedis  RedisFactory
	newFilter FilterFactory

	// HealthCheckInterval is the PING interval for services with active_conn_health_check.
	HealthCheckInterval time.Duration
	// ReloadDialTimeout bounds how long a reload waits for a new Redis service whose
	// startup_max_elapsed_time is unset. The first Apply (boot) may wait indefinitely,
	// but a reload must not block later reloads forever; on timeout the previous
	// policy stays active.
	ReloadDialTimeout time.Duration

	mu      sync.Mutex
	redis   map[string]*redisEntry
	filters map[string]engine.Filter
	ready   atomic.Bool
}

// NewManager creates a Manager. ctx bounds background work such as health checks.
func NewManager(ctx context.Context, registry *engine.ChainRegistry, newRedis RedisFactory) *Manager {
	return &Manager{
		ctx:                 ctx,
		registry:            registry,
		newRedis:            newRedis,
		newFilter:           filters.CreateFilter,
		HealthCheckInterval: 5 * time.Second,
		ReloadDialTimeout:   30 * time.Second,
		redis:               make(map[string]*redisEntry),
		filters:             make(map[string]engine.Filter),
	}
}

// WithFilterFactory overrides how filters are compiled (used by tests).
func (m *Manager) WithFilterFactory(f FilterFactory) *Manager {
	m.newFilter = f
	return m
}

// Ready reports whether at least one config has been applied successfully.
func (m *Manager) Ready() bool {
	return m.ready.Load()
}

// Apply compiles cfg and, only if every part of it compiles, publishes it.
func (m *Manager) Apply(cfg *config.Config) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var createdClients []redis.Client
	var createdFilters []engine.Filter
	abort := func(err error) error {
		closeFilters(createdFilters)
		for _, c := range createdClients {
			_ = c.Close()
		}
		return err
	}

	// 1. Redis clients: reuse unchanged services, dial new or changed ones.
	nextRedis := make(map[string]*redisEntry, len(cfg.Redis))
	for name, svcCfg := range cfg.Redis {
		if old, ok := m.redis[name]; ok && reflect.DeepEqual(old.cfg, svcCfg) {
			nextRedis[name] = old
			continue
		}
		dialCtx, cancel := m.ctx, context.CancelFunc(func() {})
		if m.ready.Load() && svcCfg.StartupMaxElapsedTimeDuration <= 0 && m.ReloadDialTimeout > 0 {
			dialCtx, cancel = context.WithTimeout(m.ctx, m.ReloadDialTimeout)
		}
		client, err := m.newRedis(dialCtx, name, svcCfg)
		cancel()
		if err != nil {
			return abort(fmt.Errorf("redis service %q: %w", name, err))
		}
		createdClients = append(createdClients, client)
		nextRedis[name] = &redisEntry{cfg: svcCfg, client: client}
	}
	lookup := func(service string) (redis.Client, bool) {
		e, ok := nextRedis[service]
		if !ok {
			return nil, false
		}
		return e.client, true
	}

	// 2. Filters: identical definitions (same type, options and Redis client) share
	// one instance, within this config and with the previous one.
	nextFilters := make(map[string]engine.Filter)
	chains := make(map[string]engine.Chain, len(cfg.Chains))
	for chainName, filterCfgs := range cfg.Chains {
		chain := make(engine.Chain, 0, len(filterCfgs))
		for i, fc := range filterCfgs {
			key, err := filterKey(fc, nextRedis)
			if err != nil {
				return abort(fmt.Errorf("chain %q filter #%d (%s): %w", chainName, i, fc.Type, err))
			}
			f, ok := nextFilters[key]
			if !ok {
				f, ok = m.filters[key]
			}
			if !ok {
				f, err = m.newFilter(fc.Type, fc.Options, lookup)
				if err != nil {
					return abort(fmt.Errorf("chain %q filter #%d (%s): %w", chainName, i, fc.Type, err))
				}
				createdFilters = append(createdFilters, f)
			}
			nextFilters[key] = f
			chain = append(chain, f)
		}
		chains[chainName] = chain
		mylogger.Info("Compiled filter chain", zap.String("chain_name", chainName), zap.Int("filters_count", len(chain)))
	}

	// 3. Work out what the new policy no longer uses.
	var retiredFilters []engine.Filter
	for key, f := range m.filters {
		if _, kept := nextFilters[key]; !kept {
			retiredFilters = append(retiredFilters, f)
		}
	}
	var retiredRedis []*redisEntry
	for name, e := range m.redis {
		if nextRedis[name] != e {
			retiredRedis = append(retiredRedis, e)
		}
	}

	// 4. Publish routes and chains atomically. Retired resources are released when
	// the last stream holding the previous snapshot finishes.
	m.registry.Swap(engine.NewSnapshot(cfg, chains), func() {
		closeFilters(retiredFilters)
		for _, e := range retiredRedis {
			if err := e.client.Close(); err != nil {
				mylogger.Warn("Failed to close retired redis client", zap.Error(err))
			}
		}
	})
	config.GlobalConfig.Store(cfg)

	// 5. Health checks follow client lifetimes.
	for _, e := range retiredRedis {
		if e.stopHealth != nil {
			e.stopHealth()
			e.stopHealth = nil
		}
	}
	for name, e := range nextRedis {
		if e.cfg.ActiveConnHealthCheck && e.stopHealth == nil {
			hctx, cancel := context.WithCancel(m.ctx)
			redis.StartHealthCheckForClient(hctx, e.client, name, m.HealthCheckInterval)
			e.stopHealth = cancel
		}
	}

	m.redis = nextRedis
	m.filters = nextFilters
	m.ready.Store(true)
	return nil
}

// Close releases every resource held by the active policy. Call it after the gRPC
// server has stopped.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()

	all := make([]engine.Filter, 0, len(m.filters))
	for _, f := range m.filters {
		all = append(all, f)
	}
	closeFilters(all)
	for _, e := range m.redis {
		if e.stopHealth != nil {
			e.stopHealth()
		}
		_ = e.client.Close()
	}
	m.filters = map[string]engine.Filter{}
	m.redis = map[string]*redisEntry{}
}

// filterKey identifies a filter definition. Options are serialised with sorted map
// keys, and the identity of the Redis client the filter binds to is included so a
// filter is rebuilt whenever its Redis service is.
func filterKey(fc config.FilterConfig, nextRedis map[string]*redisEntry) (string, error) {
	opts, err := yaml.Marshal(fc.Options)
	if err != nil {
		return "", fmt.Errorf("invalid options: %w", err)
	}
	key := fc.Type + "\x00" + string(opts)
	if svc, ok := fc.Options["redis_service"].(string); ok {
		if e, ok := nextRedis[svc]; ok {
			key += fmt.Sprintf("\x00%p", e)
		}
	}
	return key, nil
}

func closeFilters(fs []engine.Filter) {
	for _, f := range fs {
		if c, ok := f.(io.Closer); ok {
			if err := c.Close(); err != nil {
				mylogger.Warn("Failed to close retired filter", zap.Error(err))
			}
		}
	}
}
