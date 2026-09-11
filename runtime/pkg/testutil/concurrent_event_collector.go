package testutil

import (
	"encoding/json"
	"sync"

	"github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

// ConcurrentEventCollector collects trace.TraceEvent values safely across goroutines.
// Use in parallel step tests to verify event ordering and completeness.
type ConcurrentEventCollector struct {
	events []trace.TraceEvent
	mu     sync.Mutex
}

// Collect appends event in a thread-safe manner.
func (c *ConcurrentEventCollector) Collect(event trace.TraceEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, event)
}

// Events returns a snapshot copy of all collected events.
func (c *ConcurrentEventCollector) Events() []trace.TraceEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]trace.TraceEvent, len(c.events))
	copy(out, c.events)
	return out
}

// EventsForStep returns a snapshot of events whose payload contains a "step_id"
// field matching stepID.
func (c *ConcurrentEventCollector) EventsForStep(stepID string) []trace.TraceEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []trace.TraceEvent
	for _, e := range c.events {
		if stepIDFromPayload(e.Payload) == stepID {
			out = append(out, e)
		}
	}
	return out
}

// Count returns the total number of collected events.
func (c *ConcurrentEventCollector) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.events)
}

// Reset clears all collected events.
func (c *ConcurrentEventCollector) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = c.events[:0]
}

// stepIDFromPayload extracts the "step_id" field from a trace event payload.
func stepIDFromPayload(payload json.RawMessage) string {
	var m struct {
		StepID string `json:"step_id"`
	}
	_ = json.Unmarshal(payload, &m)
	return m.StepID
}
