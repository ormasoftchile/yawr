package serve

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/preview/runstate"
	servepkg "github.com/ormasoftchile/yawr/runtime/pkg/serve"
)

type eventSubscriber struct {
	runID string
	ch    chan servepkg.RunEvent
}

type eventHistory struct {
	events []servepkg.RunEvent
	limit  int
}

func (h *eventHistory) add(ev servepkg.RunEvent) {
	h.events = append(h.events, ev)
	if h.limit > 0 && len(h.events) > h.limit {
		h.events = h.events[len(h.events)-h.limit:]
	}
}

func (h *eventHistory) since(seq int64) []servepkg.RunEvent {
	var out []servepkg.RunEvent
	for _, ev := range h.events {
		if ev.Sequence > seq {
			out = append(out, ev)
		}
	}
	return out
}

// EventBridge fans out events to WS/SSE subscribers and stores a replay buffer.
type EventBridge struct {
	mu          sync.RWMutex
	subscribers map[*eventSubscriber]struct{}
	history     map[string]*eventHistory
	historySize int
	closed      bool
}

// NewEventBridge constructs an EventBridge with the given history size.
func NewEventBridge(historySize int) *EventBridge {
	return &EventBridge{
		subscribers: make(map[*eventSubscriber]struct{}),
		history:     make(map[string]*eventHistory),
		historySize: historySize,
	}
}

// Subscribe registers a subscriber for the given runID (empty for all).
func (b *EventBridge) Subscribe(runID string, bufferSize int) *eventSubscriber {
	if bufferSize <= 0 {
		bufferSize = 1
	}
	sub := &eventSubscriber{
		runID: runID,
		ch:    make(chan servepkg.RunEvent, bufferSize),
	}
	b.mu.Lock()
	if b.closed {
		close(sub.ch)
		b.mu.Unlock()
		return sub
	}
	b.subscribers[sub] = struct{}{}
	b.mu.Unlock()
	return sub
}

// Unsubscribe removes a subscriber and closes its channel.
func (b *EventBridge) Unsubscribe(sub *eventSubscriber) {
	if sub == nil {
		return
	}
	b.mu.Lock()
	if _, ok := b.subscribers[sub]; ok {
		delete(b.subscribers, sub)
		close(sub.ch)
	}
	b.mu.Unlock()
}

// Broadcast sends ev to all matching subscribers and stores it for replay.
func (b *EventBridge) Broadcast(ev servepkg.RunEvent) {
	ev.Payload = runstate.PreviewEventPayload(ev.Payload)
	b.mu.RLock()
	if b.closed {
		b.mu.RUnlock()
		return
	}
	b.mu.RUnlock()

	b.mu.Lock()
	h, ok := b.history[ev.RunID]
	if !ok {
		h = &eventHistory{limit: b.historySize}
		b.history[ev.RunID] = h
	}
	h.add(ev)
	b.mu.Unlock()

	b.mu.RLock()
	for sub := range b.subscribers {
		if sub.runID != "" && sub.runID != ev.RunID {
			continue
		}
		select {
		case sub.ch <- ev:
		default:
		}
	}
	b.mu.RUnlock()
}

// Replay returns buffered events for runID with sequence greater than since.
func (b *EventBridge) Replay(runID string, since int64) []servepkg.RunEvent {
	if runID == "" {
		return nil
	}
	b.mu.RLock()
	h := b.history[runID]
	if h == nil {
		b.mu.RUnlock()
		return nil
	}
	out := h.since(since)
	b.mu.RUnlock()
	return out
}

// WaitForSubscriber blocks until at least one subscriber is registered or the context
// or timeout expires. It is primarily useful in tests to synchronize before broadcasting.
func (b *EventBridge) WaitForSubscriber(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		b.mu.RLock()
		count := len(b.subscribers)
		b.mu.RUnlock()
		if count > 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("timeout waiting for subscriber")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}
func (b *EventBridge) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	for sub := range b.subscribers {
		close(sub.ch)
	}
	b.subscribers = make(map[*eventSubscriber]struct{})
	b.mu.Unlock()
}
