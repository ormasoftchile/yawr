package engine

import "context"

// StepExecutor executes a single resolved step and returns a result.
// Each step type has a corresponding StepExecutor implementation.
//
// Implementations MUST:
// - Return a non-nil StepResult on success (even for skipped steps)
// - Return an error only for infrastructure failures (not step logic failures)
// - Set StepResult.Status to StepStatusFailed for step logic failures
// - Be safe for concurrent use if the same executor handles parallel branches
type StepExecutor interface {
	// Execute runs the step and returns the outcome.
	// ctx carries deadline, cancellation, and trace context.
	// vars contains all variables accumulated from prior steps.
	// The executor MUST NOT modify vars; any new variables should be in StepResult.Vars.
	Execute(ctx context.Context, step ResolvedStep, vars map[string]any) (*StepResult, error)
}

// ExecutorRegistry maps step kinds to their executor implementations.
// The engine uses the registry to dispatch each step to the correct executor.
type ExecutorRegistry interface {
	// Register associates a step kind with an executor.
	// If an executor is already registered for the kind, it is replaced.
	Register(kind string, exec StepExecutor)

	// Lookup returns the executor for the given step kind.
	// Returns nil if no executor is registered for that kind.
	Lookup(kind string) StepExecutor
}

// ExecutionContext provides contextual information to step executors.
// Passed alongside context.Context to provide run-level state access.
type ExecutionContext struct {
	// RunID is the unique identifier of the current run.
	RunID string

	// RunbookPath is the path to the runbook being executed.
	RunbookPath string

	// StepID is the ID of the step being executed.
	StepID string

	// Actor is the identity that initiated this run.
	Actor string

	// Mode indicates whether this is a real, dry-run, or replay execution.
	Mode RunMode

	// Depth is the include nesting depth of the current step.
	Depth int
}
