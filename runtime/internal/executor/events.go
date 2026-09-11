package executor

import (
	"context"

	"github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

// EventEmitter is the lightweight callback executors use to emit lifecycle
// events that are not tied to a single ResolvedStep, such as iterate
// iteration_started/completed events. It is provided by the engine via
// context.Context so executors can remain decoupled from engine internals.
//
// The underlying type and context key live in pkg/trace so that packages
// outside the executor layer (e.g. internal/tool transports) can emit events
// using the same context slot without creating an import cycle.
type EventEmitter = trace.EventEmitter

// WithEventEmitter returns a child context carrying the given emitter.
// The engine attaches an emitter before invoking each executor's Execute.
func WithEventEmitter(ctx context.Context, e EventEmitter) context.Context {
	return trace.WithEventEmitter(ctx, e)
}

// EmitterFromContext returns the emitter stored in ctx, or nil if none is
// present. Callers should nil-check before invoking.
func EmitterFromContext(ctx context.Context) EventEmitter {
	return trace.EmitterFromContext(ctx)
}
