package tool

import "sync"

// MapRegistry is a concurrent in-memory ToolRegistry backed by a map.
type MapRegistry struct {
	mu    sync.RWMutex
	tools map[string]ToolDef
}

// NewRegistry constructs an in-memory ToolRegistry from zero or more ToolDefs.
func NewRegistry(defs ...ToolDef) *MapRegistry {
	r := &MapRegistry{tools: make(map[string]ToolDef, len(defs))}
	for _, def := range defs {
		r.tools[def.Name] = def
	}
	return r
}

// Lookup returns the tool definition by name.
func (r *MapRegistry) Lookup(name string) (*ToolDef, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	def, ok := r.tools[name]
	if !ok {
		return nil, false
	}
	out := def
	return &out, true
}

// All returns all tool definitions in the registry.
func (r *MapRegistry) All() []ToolDef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ToolDef, 0, len(r.tools))
	for _, def := range r.tools {
		out = append(out, def)
	}
	return out
}

// Register adds a new tool definition to the registry.
// If a tool with the same name is already registered, it is silently skipped.
func (r *MapRegistry) Register(def ToolDef) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[def.Name]; exists {
		return nil
	}
	r.tools[def.Name] = def
	return nil
}
