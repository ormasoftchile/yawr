// Package engine provides the concrete implementation of the yawr runtime engine.
//
// The engine executes runbooks step by step according to an ExecutionPlan.
// It manages run state, dispatches to step executors, handles parallel branches,
// and emits trace events through the configured TraceWriter.
//
// # Architecture
//
// The engine follows a client-driven execution model where the caller controls
// step advancement via RunHandle.Next(). This design supports both interactive
// adapters (CLI, VS Code) and non-interactive replay.
//
// # Event Protocol
//
// The runtime emits events in this order:
//   - run/started: when a run begins
//   - step/started: before each step executes
//   - step/completed | step/failed | step/skipped: after each step
//   - event/received: when wait_for_event receives an external event
//   - step/resumed: after event reception completes
//   - run/completed | run/failed | run/cancelled: at terminal state
//
// # Parallel Execution
//
// Parallel branches execute concurrently in goroutines. Trace events are
// published live with per-occurrence causal order, without sorting concurrent branches.
// On any branch failure with fail-fast semantics, remaining branches are
// cancelled.
package engine
