package testutil

import (
	"context"
	"fmt"
	"sync"

	"github.com/ormasoftchile/yawr/runtime/pkg/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// Compile-time interface guard.
var _ planner.ToolRegistry = (*FakeToolRegistry)(nil)

// ToolLookupCall records a single Lookup invocation.
type ToolLookupCall struct {
	Name   string
	Action string
}

// FakeToolRegistry implements planner.ToolRegistry for tests.
// Register tool definitions by "name/action" key; returns an error for unknown refs.
type FakeToolRegistry struct {
	// Tools maps "name/action" → *schema.ToolDef.
	Tools map[string]*schema.ToolDef
	// LookupCalls records all (name, action) pairs passed to Lookup (for assertions).
	LookupCalls []ToolLookupCall

	mu sync.Mutex
}

// NewFakeToolRegistry returns a FakeToolRegistry with an empty tool map.
func NewFakeToolRegistry() *FakeToolRegistry {
	return &FakeToolRegistry{
		Tools: make(map[string]*schema.ToolDef),
	}
}

// Lookup returns the registered ToolDef for (name, action), or an error if none is registered.
func (f *FakeToolRegistry) Lookup(_ context.Context, name string, action string) (*schema.ToolDef, error) {
	key := name + "/" + action

	f.mu.Lock()
	f.LookupCalls = append(f.LookupCalls, ToolLookupCall{Name: name, Action: action})
	def, ok := f.Tools[key]
	f.mu.Unlock()

	if !ok {
		return nil, fmt.Errorf("tool not found: %s/%s", name, action)
	}
	return def, nil
}

// Register adds def to the registry under "name/action" so that Lookup(name, action) returns it.
func (f *FakeToolRegistry) Register(name, action string, def *schema.ToolDef) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Tools[name+"/"+action] = def
}
