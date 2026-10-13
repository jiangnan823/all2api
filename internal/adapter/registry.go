package adapter

import (
	"fmt"
	"sync"
)

// Registry holds all registered platform adapters.
// Build-time registration works through init() in each platform package —
// if a platform is excluded via build tag, its init() never runs,
// so it never registers. No runtime plugin loading needed.
type Registry struct {
	mu    sync.RWMutex
	byName map[string]Adapter
	byCap  map[CapType][]string // cap → list of adapter names supporting it
}

var Default = NewRegistry()

func NewRegistry() *Registry {
	return &Registry{
		byName: make(map[string]Adapter),
		byCap:  make(map[CapType][]string),
	}
}

// Register adds an adapter to the registry. Called from init() in each platform package.
func (r *Registry) Register(a Adapter) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	name := a.Name()
	if _, exists := r.byName[name]; exists {
		return fmt.Errorf("adapter %q already registered", name)
	}
	r.byName[name] = a

	caps := a.Capabilities()
	for bit := CapType(1); bit <= CapSSERebuild; bit <<= 1 {
		if caps.Has(bit) {
			r.byCap[bit] = append(r.byCap[bit], name)
		}
	}
	return nil
}

// Get returns the adapter for the given name, or nil if not found.
func (r *Registry) Get(name string) Adapter {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byName[name]
}

// GetAll returns all registered adapters (not modified during iteration).
func (r *Registry) GetAll() []Adapter {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]Adapter, 0, len(r.byName))
	for _, a := range r.byName {
		result = append(result, a)
	}
	return result
}

// ByCap returns adapter names that support the given capability.
func (r *Registry) ByCap(cap CapType) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, len(r.byCap[cap]))
	copy(names, r.byCap[cap])
	return names
}
