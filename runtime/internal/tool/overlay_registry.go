package tool

import (
	"sync"

	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

// OverlayRegistry layers a small set of authoritative overrides on top of a
// base registry (e.g. a directory scan performed once per workspace at
// wiring time), so a caller that has already resolved a specific binding
// for a tool name (e.g. the Tool Packages MVP catalog/toolRefs resolution
// in cmd/yawr) can force that binding to win at invocation time.
//
// This exists because MapRegistry.Register is deliberately
// idempotent-skip-if-exists (so parent/child runbooks sharing toolRefs
// don't error re-registering the same name) — which would otherwise
// silently let a same-named tool the base directory scan happened to find
// first shadow a project- or --package-map-resolved binding. Lookup and
// All prefer overrides; Register preserves the base's existing
// idempotent-skip contract; Override is the one operation that always
// replaces, regardless of prior state.
type OverlayRegistry struct {
	mu        sync.RWMutex
	base      toolpkg.ToolRegistry
	overrides map[string]toolpkg.ToolDef
}

// NewOverlayRegistry constructs an OverlayRegistry wrapping base. base may
// be nil (an overlay with no base, useful in tests).
func NewOverlayRegistry(base toolpkg.ToolRegistry) *OverlayRegistry {
	return &OverlayRegistry{base: base, overrides: make(map[string]toolpkg.ToolDef)}
}

// Lookup returns the override for name if one is set, otherwise falls
// through to the base registry.
func (r *OverlayRegistry) Lookup(name string) (*toolpkg.ToolDef, bool) {
	r.mu.RLock()
	def, ok := r.overrides[name]
	r.mu.RUnlock()
	if ok {
		out := def
		return &out, true
	}
	if r.base == nil {
		return nil, false
	}
	return r.base.Lookup(name)
}

// All returns every definition visible through this overlay: overrides,
// plus every base entry not shadowed by an override.
func (r *OverlayRegistry) All() []toolpkg.ToolDef {
	r.mu.RLock()
	overrides := make(map[string]toolpkg.ToolDef, len(r.overrides))
	for k, v := range r.overrides {
		overrides[k] = v
	}
	r.mu.RUnlock()

	var out []toolpkg.ToolDef
	if r.base != nil {
		for _, def := range r.base.All() {
			if _, shadowed := overrides[def.Name]; shadowed {
				continue
			}
			out = append(out, def)
		}
	}
	for _, def := range overrides {
		out = append(out, def)
	}
	return out
}

// Register adds def as an override only if name is not already known to
// either this overlay or the base registry — matching MapRegistry's
// existing idempotent-skip contract.
func (r *OverlayRegistry) Register(def toolpkg.ToolDef) error {
	if _, ok := r.Lookup(def.Name); ok {
		return nil
	}
	r.mu.Lock()
	r.overrides[def.Name] = def
	r.mu.Unlock()
	return nil
}

// Override force-registers def as an override, replacing any existing
// entry for the same name in either this overlay or the wrapped base
// registry.
func (r *OverlayRegistry) Override(def toolpkg.ToolDef) {
	r.mu.Lock()
	r.overrides[def.Name] = def
	r.mu.Unlock()
}
