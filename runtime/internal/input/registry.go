package input

import (
	"context"
	"errors"
	"sort"
	"sync"

	inputpkg "github.com/ormasoftchile/yawr/runtime/pkg/input"
)

type providerEntry struct {
	priority int
	provider inputpkg.InputProvider
}

// defaultRegistry implements input.InputRegistry.
type defaultRegistry struct {
	mu              sync.RWMutex
	providers       []providerEntry
	defaultProvider inputpkg.InputProvider
}

// NewRegistry constructs a default InputRegistry.
func NewRegistry() inputpkg.InputRegistry {
	return &defaultRegistry{}
}

// Register adds a provider with a priority.
func (r *defaultRegistry) Register(priority int, p inputpkg.InputProvider) {
	if p == nil {
		return
	}
	r.mu.Lock()
	r.providers = append(r.providers, providerEntry{priority: priority, provider: p})
	sort.SliceStable(r.providers, func(i, j int) bool {
		return r.providers[i].priority < r.providers[j].priority
	})
	r.mu.Unlock()
}

// Resolve tries providers in priority order.
func (r *defaultRegistry) Resolve(ctx context.Context, req inputpkg.InputRequest) (*inputpkg.InputResponse, error) {
	r.mu.RLock()
	providers := append([]providerEntry(nil), r.providers...)
	defaultProvider := r.defaultProvider
	r.mu.RUnlock()

	for _, entry := range providers {
		resp, err := entry.provider.Provide(ctx, req)
		if err != nil {
			if errors.Is(err, ErrNoProvider) {
				continue
			}
			return nil, err
		}
		if resp != nil {
			return resp, nil
		}
	}
	if defaultProvider != nil {
		resp, err := defaultProvider.Provide(ctx, req)
		if err != nil {
			if errors.Is(err, ErrNoProvider) {
				return nil, ErrNoProvider
			}
			return nil, err
		}
		if resp != nil {
			return resp, nil
		}
	}
	return nil, ErrNoProvider
}

// SetDefault sets the default provider.
func (r *defaultRegistry) SetDefault(p inputpkg.InputProvider) {
	r.mu.Lock()
	r.defaultProvider = p
	r.mu.Unlock()
}
