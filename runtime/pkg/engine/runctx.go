package engine

import (
	"context"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// runIDCtxKey is the context key used to attach a run ID to an executor's
// context. Implementations of input.PromptProvider that route prompts by
// run (e.g. an HTTP broker) read the value via RunIDFromContext.
type runIDCtxKey struct{}
type runWriterEpochCtxKey struct{}
type concurrentExecutionCtxKey struct{}

// WithRunID returns a derived context carrying the given run ID.
// The engine attaches this to every context passed into a step executor
// so that prompt providers can disambiguate concurrent runs.
func WithRunID(ctx context.Context, runID string) context.Context {
	if runID == "" {
		return ctx
	}
	return context.WithValue(ctx, runIDCtxKey{}, runID)
}

// RunIDFromContext returns the run ID stored in ctx, or the empty string
// when no run ID has been attached. Callers MUST handle the empty case.
func RunIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(runIDCtxKey{}).(string)
	return v
}

// WithRunWriterEpoch binds an audit mutation to the active run writer lease.
func WithRunWriterEpoch(ctx context.Context, writerEpoch uint64) context.Context {
	if writerEpoch == 0 {
		return ctx
	}
	return context.WithValue(ctx, runWriterEpochCtxKey{}, writerEpoch)
}

// RunWriterEpochFromContext returns the expected writer epoch for a mutation.
func RunWriterEpochFromContext(ctx context.Context) uint64 {
	if ctx == nil {
		return 0
	}
	epoch, _ := ctx.Value(runWriterEpochCtxKey{}).(uint64)
	return epoch
}

func WithConcurrentExecution(ctx context.Context) context.Context {
	return context.WithValue(ctx, concurrentExecutionCtxKey{}, true)
}

func ConcurrentExecutionFromContext(ctx context.Context) bool {
	concurrent, _ := ctx.Value(concurrentExecutionCtxKey{}).(bool)
	return concurrent
}

type planToolsCtxKey struct{}

// WithPlanTools returns a derived context carrying the immutable tool
// definitions selected for the root execution plan.
func WithPlanTools(ctx context.Context, tools map[string]*schema.ToolDef) context.Context {
	if len(tools) == 0 {
		return ctx
	}
	return context.WithValue(ctx, planToolsCtxKey{}, tools)
}

// PlanToolsFromContext returns the root plan's tool definitions, or nil when
// the context did not originate from a planned run.
func PlanToolsFromContext(ctx context.Context) map[string]*schema.ToolDef {
	if ctx == nil {
		return nil
	}
	tools, _ := ctx.Value(planToolsCtxKey{}).(map[string]*schema.ToolDef)
	return tools
}

// EventForwarder is a callback that forwards an event from a sub-engine
// (spawned by SubStepRunner) back to the parent run handle so it reaches
// the parent's Events() channel and any downstream consumers (SSE bridge,
// CLI tail, etc.). Without this, events emitted by sub-engines for nested
// branch/iterate sub-steps disappear and the UI shows those nodes gray.
type EventForwarder func(Event)

// eventForwarderCtxKey is the context key for an EventForwarder.
type eventForwarderCtxKey struct{}

// WithEventForwarder returns a derived context carrying fwd. The engine
// attaches a forwarder before calling step executors that may run sub-
// engines (branch, iterate, compensate). SubStepRunner implementations
// retrieve it via EventForwarderFromContext and wire it into the sub-
// engine's RunOptions.OnEvent.
func WithEventForwarder(ctx context.Context, fwd EventForwarder) context.Context {
	if fwd == nil {
		return ctx
	}
	return context.WithValue(ctx, eventForwarderCtxKey{}, fwd)
}

// EventForwarderFromContext returns the forwarder stored in ctx, or nil
// when none has been attached.
func EventForwarderFromContext(ctx context.Context) EventForwarder {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(eventForwarderCtxKey{}).(EventForwarder)
	return v
}
