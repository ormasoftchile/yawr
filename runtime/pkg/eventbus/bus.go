package eventbus

import "context"

// Event is a runtime event flowing through the in-process event bus.
// This mirrors the engine.Event type but is kept independent to avoid import cycles.
type Event struct {
	RunID   string
	Kind    string
	Payload map[string]any
}

// EventBus is the in-process fan-out event bus.
// Publishers are non-blocking; if a subscriber channel is full, the event is dropped.
type EventBus interface {
	// Publish broadcasts ev to all matching subscribers.
	// MUST NOT block; drops event if any subscriber channel is full.
	Publish(ev Event)

	// Subscribe registers a new subscriber and returns a channel that receives events.
	// opts.KindPrefix filters events; empty string matches all kinds.
	Subscribe(ctx context.Context, opts SubscribeOptions) (<-chan Event, error)

	// Unsubscribe removes a subscriber. The channel returned by Subscribe is closed.
	Unsubscribe(ch <-chan Event)
}

// SubscribeOptions configures an event subscription.
type SubscribeOptions struct {
	// KindPrefix filters events to those whose Kind starts with this prefix.
	// Empty string matches all events.
	KindPrefix string

	// RunID filters events to a specific run. Empty matches all runs.
	RunID string

	// BufferSize is the subscriber channel buffer. Default: 100.
	BufferSize int
}
