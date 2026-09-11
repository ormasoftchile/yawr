package trace

import "context"

// EventEmitter is a lightweight callback for emitting lifecycle events.
// The kind is a string matching one of the EventKind constants; payload
// is a free-form map of fields. Implementations are provided by the engine
// via context; callers nil-check before invoking.
type EventEmitter func(kind string, payload map[string]any)

type emitterKey struct{}

// WithEventEmitter returns a child context carrying the given emitter.
// The engine attaches an emitter before invoking each executor or transport.
func WithEventEmitter(ctx context.Context, e EventEmitter) context.Context {
	if e == nil {
		return ctx
	}
	return context.WithValue(ctx, emitterKey{}, e)
}

// EmitterFromContext returns the EventEmitter stored in ctx, or nil if none
// is present. Callers must nil-check before invoking.
func EmitterFromContext(ctx context.Context) EventEmitter {
	if ctx == nil {
		return nil
	}
	e, _ := ctx.Value(emitterKey{}).(EventEmitter)
	return e
}
