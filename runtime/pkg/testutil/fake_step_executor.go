// Package testutil provides test infrastructure for yawr.
// Fakes, controllers, and golden-trace helpers for unit and integration tests.
package testutil

import (
	"context"
	"sync"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
)

// StepHandler is the function signature for per-step handlers in FakeStepExecutor.
type StepHandler func(ctx context.Context, step engine.ResolvedStep, vars map[string]any) (*engine.StepResult, error)

// ExecuteCall records a single invocation of FakeStepExecutor.Execute.
type ExecuteCall struct {
	StepID string
	Step   engine.ResolvedStep
	Vars   map[string]any
	At     time.Time
}

// FakeStepExecutor is a controllable StepExecutor for use in unit tests.
// It implements engine.StepExecutor. Register handlers per step ID to control
// execution outcome.
type FakeStepExecutor struct {
	// Handlers maps step ID → handler function. If not set, uses DefaultHandler.
	Handlers map[string]StepHandler
	// DefaultHandler is used when no specific handler is registered.
	DefaultHandler StepHandler
	// Calls records all Execute invocations in order.
	Calls []ExecuteCall
	// mu protects Calls for concurrent use.
	mu sync.Mutex
}

// Ensure FakeStepExecutor implements engine.StepExecutor at compile time.
var _ engine.StepExecutor = (*FakeStepExecutor)(nil)

// NewFakeStepExecutor returns a FakeStepExecutor with an empty handler map and a
// default handler that returns a successful empty result.
func NewFakeStepExecutor() *FakeStepExecutor {
	f := &FakeStepExecutor{
		Handlers: make(map[string]StepHandler),
	}
	f.DefaultHandler = func(_ context.Context, _ engine.ResolvedStep, _ map[string]any) (*engine.StepResult, error) {
		return &engine.StepResult{Vars: map[string]any{}, Outcome: engine.StepOutcomeSuccess}, nil
	}
	return f
}

// Execute records the call and dispatches to the registered handler for step.ID,
// falling back to DefaultHandler when none is registered.
func (f *FakeStepExecutor) Execute(ctx context.Context, step engine.ResolvedStep, vars map[string]any) (*engine.StepResult, error) {
	f.mu.Lock()
	f.Calls = append(f.Calls, ExecuteCall{
		StepID: step.ID,
		Step:   step,
		Vars:   vars,
		At:     time.Now(),
	})
	handler := f.Handlers[step.ID]
	f.mu.Unlock()

	if handler == nil {
		handler = f.DefaultHandler
	}
	if handler == nil {
		return &engine.StepResult{Outcome: engine.StepOutcomeSuccess}, nil
	}
	return handler(ctx, step, vars)
}

// RegisterSuccess registers a handler for stepID that always returns a successful
// result with the given vars map.
func (f *FakeStepExecutor) RegisterSuccess(stepID string, output map[string]any) {
	f.Handlers[stepID] = func(_ context.Context, _ engine.ResolvedStep, _ map[string]any) (*engine.StepResult, error) {
		return &engine.StepResult{Vars: output, Outcome: engine.StepOutcomeSuccess}, nil
	}
}

// RegisterFailure registers a handler for stepID that always returns err.
func (f *FakeStepExecutor) RegisterFailure(stepID string, err error) {
	f.Handlers[stepID] = func(_ context.Context, _ engine.ResolvedStep, _ map[string]any) (*engine.StepResult, error) {
		return &engine.StepResult{Outcome: engine.StepOutcomeFailed}, err
	}
}

// RegisterDelay wraps handler with a time.Sleep for use in race/timeout tests.
func (f *FakeStepExecutor) RegisterDelay(stepID string, d time.Duration, handler StepHandler) {
	f.Handlers[stepID] = func(ctx context.Context, step engine.ResolvedStep, vars map[string]any) (*engine.StepResult, error) {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return &engine.StepResult{Outcome: engine.StepOutcomeFailed}, ctx.Err()
		}
		return handler(ctx, step, vars)
	}
}

// CallCount returns the total number of Execute calls recorded.
func (f *FakeStepExecutor) CallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Calls)
}

// WasCalled reports whether Execute was ever called for the given stepID.
func (f *FakeStepExecutor) WasCalled(stepID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.Calls {
		if c.StepID == stepID {
			return true
		}
	}
	return false
}
