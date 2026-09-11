package testutil

import (
	"context"
	"fmt"
	"sync"

	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/planner"
)

// Compile-time interface guard.
var _ planner.RunbookLoader = (*FakeRunbookLoader)(nil)

// FakeRunbookLoader implements planner.RunbookLoader for tests.
// Register runbooks by path; returns an error for unknown paths.
type FakeRunbookLoader struct {
	// Runbooks maps path → *parser.ParsedRunbook.
	Runbooks map[string]*parser.ParsedRunbook
	// LoadCalls records paths that were passed to Load (for assertions).
	LoadCalls []string

	mu sync.Mutex
}

// NewFakeRunbookLoader returns a FakeRunbookLoader with an empty runbook map.
func NewFakeRunbookLoader() *FakeRunbookLoader {
	return &FakeRunbookLoader{
		Runbooks: make(map[string]*parser.ParsedRunbook),
	}
}

// Load returns the registered ParsedRunbook for path, or an error if none is registered.
func (f *FakeRunbookLoader) Load(_ context.Context, path string) (*parser.ParsedRunbook, error) {
	f.mu.Lock()
	f.LoadCalls = append(f.LoadCalls, path)
	rb, ok := f.Runbooks[path]
	f.mu.Unlock()

	if !ok {
		return nil, fmt.Errorf("runbook not found: %s", path)
	}
	return rb, nil
}

// Register adds rb to the loader under path so that Load(path) returns it.
func (f *FakeRunbookLoader) Register(path string, rb *parser.ParsedRunbook) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Runbooks[path] = rb
}
