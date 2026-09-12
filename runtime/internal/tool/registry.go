package tool

import (
	"sync"

	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

// MapRegistry is a simple in-memory registry backed by a map.
type MapRegistry struct {
	mu    sync.RWMutex
	tools map[string]toolpkg.ToolDef
}

// NewMapRegistry constructs a registry from a slice of ToolDefs.
func NewMapRegistry(defs []toolpkg.ToolDef) *MapRegistry {
	r := &MapRegistry{tools: make(map[string]toolpkg.ToolDef)}
	for _, def := range defs {
		r.tools[def.Name] = def
	}
	return r
}

// Lookup returns the tool definition by name.
func (r *MapRegistry) Lookup(name string) (*toolpkg.ToolDef, bool) {
	r.mu.RLock()
	def, ok := r.tools[name]
	r.mu.RUnlock()
	if !ok {
		return nil, false
	}
	out := def
	return &out, true
}

// All returns all tool definitions in the registry.
func (r *MapRegistry) All() []toolpkg.ToolDef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]toolpkg.ToolDef, 0, len(r.tools))
	for _, def := range r.tools {
		out = append(out, def)
	}
	return out
}

// Register adds a new tool definition to the registry.
// If a tool with the same name is already registered, it is silently skipped
// (idempotent — child runbooks sharing toolRefs with the parent won't error).
func (r *MapRegistry) Register(def toolpkg.ToolDef) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[def.Name]; exists {
		return nil // already registered — skip silently
	}
	r.tools[def.Name] = def
	return nil
}

// NewBuiltinRegistry returns a registry pre-populated with builtin stub tools.
func NewBuiltinRegistry() *MapRegistry {
	defs := []toolpkg.ToolDef{
		stubToolDef("slack-notify"),
		stubToolDef("pagerduty-notify"),
		stubToolDef("alertmanager-notify"),
		stubToolDef("aws"),
		stubToolDef("okta"),
		stubToolDef("palo-alto"),
		stubToolDef("splunk"),
		stubToolDef("email-notify"),
	}
	return NewMapRegistry(defs)
}

func stubToolDef(name string) toolpkg.ToolDef {
	return toolpkg.ToolDef{
		Name:      name,
		Source:    "builtin://" + name,
		Transport: toolpkg.TransportStdio,
		Command:   "yawr-stub",
		Args:      []string{"--tool", name},
	}
}
