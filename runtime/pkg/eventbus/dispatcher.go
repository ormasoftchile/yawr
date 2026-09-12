package eventbus

import (
	"context"
	"errors"
	"time"
)

var (
	ErrEventTimeout   = errors.New("eventbus: event wait timed out")
	ErrEventCancelled = errors.New("eventbus: event wait cancelled")
)

// EventDispatcher manages the wait_for_event synchronisation primitive.
// It correlates inbound external events with waiting steps.
type EventDispatcher interface {
	// Dispatch delivers an external event to any matching waiting step.
	Dispatch(ev InboundEvent) error

	// Wait registers a step as waiting for an event matching filter.
	// Blocks until a matching event arrives or timeout elapses.
	// Returns the matched event, or an error if cancelled/timed out.
	Wait(ctx context.Context, stepID string, filter EventFilter, timeout time.Duration) (*InboundEvent, error)

	// Cancel cancels any outstanding Wait for the given stepID.
	Cancel(stepID string, reason string)
}

// InboundEvent is an external event delivered to a waiting step.
type InboundEvent struct {
	EventID       string
	Source        string
	Channel       string
	Payload       map[string]any
	FilterMatched string
}

// EventFilter defines matching criteria for inbound events.
type EventFilter struct {
	Source  string
	ID      string
	Payload map[string]string // key-value pairs that must match in inbound payload
}
