package testutil

import (
	"context"
	"sync"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

// ToolInvokeCall records a tool runtime invocation.
type ToolInvokeCall struct {
	ToolName string
	Action   string
	Args     map[string]any
	At       time.Time
}

// FakeToolRuntime is a controllable ToolRuntime for tests.
type FakeToolRuntime struct {
	Results map[string]*tool.ToolResult
	Errors  map[string]error
	Calls   []ToolInvokeCall
	// Defs, when non-nil for a tool name, makes LookupDef succeed --
	// enabling tests to exercise the ToolDefLookup-gated paths (execute.kind:
	// runbook substitution dispatch, ENUM-008 arg-binding checks).
	Defs map[string]*tool.ToolDef

	mu sync.Mutex
}

// Ensure FakeToolRuntime implements tool.ToolRuntime and tool.ToolDefLookup.
var _ tool.ToolRuntime = (*FakeToolRuntime)(nil)
var _ tool.ToolDefLookup = (*FakeToolRuntime)(nil)

// NewFakeToolRuntime constructs an empty FakeToolRuntime.
func NewFakeToolRuntime() *FakeToolRuntime {
	return &FakeToolRuntime{
		Results: make(map[string]*tool.ToolResult),
		Errors:  make(map[string]error),
	}
}

// Invoke records the call and returns configured results/errors.
func (f *FakeToolRuntime) Invoke(_ context.Context, toolName string, action string, args map[string]any) (*tool.ToolResult, error) {
	key := toolName + "/" + action
	f.mu.Lock()
	f.Calls = append(f.Calls, ToolInvokeCall{
		ToolName: toolName,
		Action:   action,
		Args:     args,
		At:       time.Now(),
	})
	res := f.Results[key]
	err := f.Errors[key]
	f.mu.Unlock()

	if err != nil {
		return nil, err
	}
	if res == nil {
		return &tool.ToolResult{ExitCode: 0}, nil
	}
	return res, nil
}

// RegisterResult registers a ToolResult for tool/action.
func (f *FakeToolRuntime) RegisterResult(toolName, action string, result *tool.ToolResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Results[toolName+"/"+action] = result
}

// RegisterError registers an error for tool/action.
func (f *FakeToolRuntime) RegisterError(toolName, action string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Errors[toolName+"/"+action] = err
}

// RegisterDef registers a *tool.ToolDef so LookupDef(toolName) succeeds.
func (f *FakeToolRuntime) RegisterDef(toolName string, def *tool.ToolDef) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Defs == nil {
		f.Defs = make(map[string]*tool.ToolDef)
	}
	f.Defs[toolName] = def
}

// LookupDef implements tool.ToolDefLookup.
func (f *FakeToolRuntime) LookupDef(name string) (*tool.ToolDef, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	def, ok := f.Defs[name]
	return def, ok
}
