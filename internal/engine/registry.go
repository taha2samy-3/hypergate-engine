package engine

import (
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/taha2samy/hypergate/internal/config"
)

// Snapshot is an immutable, compiled view of the active policy: the config the
// router matches against and the filter chains its routes point to. Both are
// swapped together so a request can never route to a chain that is not compiled.
//
// Snapshots are reference counted. Every ext_proc stream holds the snapshot it
// started with until it ends, so a reload never changes the chain of an
// in-flight request, and resources retired by a reload are only released once
// the last stream using them has finished.
type Snapshot struct {
	Config *config.Config
	Chains map[string]Chain
	// FilterNames labels each chain position for metrics: the configured name,
	// or "<index>:<type>".
	FilterNames map[string][]string

	refs      atomic.Int64
	retired   atomic.Bool
	cleanup   func()
	cleanOnce sync.Once
}

// NewSnapshot creates a snapshot for the given config and compiled chains.
func NewSnapshot(cfg *config.Config, chains map[string]Chain) *Snapshot {
	if chains == nil {
		chains = make(map[string]Chain)
	}
	names := map[string][]string{}
	if cfg != nil {
		for chain, filters := range cfg.Chains {
			labels := make([]string, len(filters))
			for i, f := range filters {
				if f.Name != "" {
					labels[i] = f.Name
				} else {
					labels[i] = strconv.Itoa(i) + ":" + f.Type
				}
			}
			names[chain] = labels
		}
	}
	return &Snapshot{Config: cfg, Chains: chains, FilterNames: names}
}

// FilterName returns the metrics label of the filter at index in chain, or
// "engine" when the engine itself made the decision (index < 0).
func (s *Snapshot) FilterName(chain string, index int) string {
	if index < 0 {
		return "engine"
	}
	if names := s.FilterNames[chain]; index < len(names) {
		return names[index]
	}
	return strconv.Itoa(index)
}

// Release drops a reference taken with ChainRegistry.Acquire.
func (s *Snapshot) Release() {
	if s.refs.Add(-1) == 0 && s.retired.Load() {
		s.runCleanup()
	}
}

func (s *Snapshot) retire(cleanup func()) {
	s.cleanup = cleanup
	s.retired.Store(true)
	if s.refs.Load() == 0 {
		s.runCleanup()
	}
}

func (s *Snapshot) runCleanup() {
	s.cleanOnce.Do(func() {
		if s.cleanup != nil {
			s.cleanup()
		}
	})
}

// ChainRegistry holds the current Snapshot.
type ChainRegistry struct {
	mu      sync.RWMutex
	current *Snapshot
}

func NewChainRegistry() *ChainRegistry {
	return &ChainRegistry{current: NewSnapshot(nil, nil)}
}

// Acquire returns the current snapshot with its reference count incremented.
// The caller must call Release when done with it.
func (r *ChainRegistry) Acquire() *Snapshot {
	r.mu.RLock()
	s := r.current
	s.refs.Add(1)
	r.mu.RUnlock()
	return s
}

// Swap installs next as the current snapshot. cleanup (may be nil) runs once the
// previous snapshot is no longer referenced by any in-flight stream.
func (r *ChainRegistry) Swap(next *Snapshot, cleanup func()) {
	r.mu.Lock()
	prev := r.current
	r.current = next
	r.mu.Unlock()
	prev.retire(cleanup)
}

// Current returns the current snapshot without taking a reference. It is meant for
// inspection (tests, reload bookkeeping), not for serving requests.
func (r *ChainRegistry) Current() *Snapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.current
}

func (r *ChainRegistry) Register(name string, chain Chain) {
	r.mu.Lock()
	defer r.mu.Unlock()
	chains := make(map[string]Chain, len(r.current.Chains)+1)
	for k, v := range r.current.Chains {
		chains[k] = v
	}
	chains[name] = chain
	prev := r.current
	r.current = NewSnapshot(prev.Config, chains)
	prev.retire(nil)
}

func (r *ChainRegistry) Get(name string) (Chain, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	chain, exists := r.current.Chains[name]
	return chain, exists
}

// ReplaceAll swaps the chains while keeping the current config.
func (r *ChainRegistry) ReplaceAll(newChains map[string]Chain) {
	r.Swap(NewSnapshot(r.Current().Config, newChains), nil)
}
