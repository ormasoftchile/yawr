package eventbus

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	eventbuspkg "github.com/ormasoftchile/yawr/runtime/pkg/eventbus"
)

var (
	ErrEventTimeout   = eventbuspkg.ErrEventTimeout
	ErrEventCancelled = eventbuspkg.ErrEventCancelled
)

// Dispatcher correlates inbound external events with waiting steps.
type Dispatcher struct {
	mu      sync.Mutex
	waiters map[string][]waiterEntry
	pending map[string][]eventbuspkg.InboundEvent
}

type waiterEntry struct {
	runID  string
	stepID string
	filter eventbuspkg.EventFilter
	ch     chan *eventbuspkg.InboundEvent
}

// Ensure Dispatcher implements eventbus.EventDispatcher.
var _ eventbuspkg.EventDispatcher = (*Dispatcher)(nil)

// NewDispatcher constructs a Dispatcher.
func NewDispatcher() *Dispatcher {
	return &Dispatcher{
		waiters: make(map[string][]waiterEntry),
		pending: make(map[string][]eventbuspkg.InboundEvent),
	}
}

// Register registers a waiter for the given run/event.
func (d *Dispatcher) Register(ctx context.Context, runID, eventID string) (<-chan eventbuspkg.InboundEvent, error) {
	if eventID == "" {
		return nil, errors.New("eventbus: eventID is required")
	}
	internal := make(chan *eventbuspkg.InboundEvent, 1)
	out := make(chan eventbuspkg.InboundEvent, 1)
	entry := waiterEntry{
		runID:  runID,
		stepID: runID + ":" + eventID,
		filter: eventbuspkg.EventFilter{ID: eventID},
		ch:     internal,
	}
	d.mu.Lock()
	d.waiters[eventID] = append(d.waiters[eventID], entry)
	d.mu.Unlock()

	go func() {
		select {
		case ev := <-internal:
			if ev != nil {
				out <- *ev
			}
		case <-ctx.Done():
			d.Unregister(runID, eventID)
		}
		close(out)
	}()
	return out, nil
}

// Dispatch delivers an inbound event to the first waiter for that eventID.
func (d *Dispatcher) Dispatch(ev eventbuspkg.InboundEvent) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	channel := dispatchChannel(ev)
	waiters := d.waiters[channel]
	for i, entry := range waiters {
		if matchesFilter(ev, entry.filter) {
			d.waiters[channel] = append(waiters[:i], waiters[i+1:]...)
			entry.ch <- &ev
			return nil
		}
	}
	d.pending[channel] = append(d.pending[channel], ev)
	return nil
}

// Unregister removes a waiter for the given run/event.
func (d *Dispatcher) Unregister(runID, eventID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	waiters := d.waiters[eventID]
	kept := waiters[:0]
	for _, entry := range waiters {
		if entry.runID == runID {
			select {
			case entry.ch <- nil:
			default:
			}
			continue
		}
		kept = append(kept, entry)
	}
	d.waiters[eventID] = kept
}

// Wait blocks until a matching event arrives, timeout elapses, or ctx is cancelled.
func (d *Dispatcher) Wait(ctx context.Context, stepID string, filter eventbuspkg.EventFilter, timeout time.Duration) (*eventbuspkg.InboundEvent, error) {
	channel := filter.ID
	d.mu.Lock()
	if channel == "" {
		for ch, events := range d.pending {
			for i, ev := range events {
				if matchesFilter(ev, filter) {
					d.pending[ch] = append(events[:i], events[i+1:]...)
					d.mu.Unlock()
					return &ev, nil
				}
			}
		}
	} else {
		events := d.pending[channel]
		for i, ev := range events {
			if matchesFilter(ev, filter) {
				d.pending[channel] = append(events[:i], events[i+1:]...)
				d.mu.Unlock()
				return &ev, nil
			}
		}
	}

	delivery := make(chan *eventbuspkg.InboundEvent, 1)
	entry := waiterEntry{stepID: stepID, filter: filter, ch: delivery}
	d.waiters[channel] = append(d.waiters[channel], entry)
	d.mu.Unlock()

	var timer <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timer = t.C
	}

	select {
	case ev := <-delivery:
		if ev == nil {
			return nil, ErrEventCancelled
		}
		return ev, nil
	case <-timer:
		d.removeWaiter(channel, delivery)
		return nil, ErrEventTimeout
	case <-ctx.Done():
		d.removeWaiter(channel, delivery)
		return nil, ctx.Err()
	}
}

// Cancel cancels any outstanding Wait for the given stepID.
func (d *Dispatcher) Cancel(stepID string, _ string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for channel, entries := range d.waiters {
		kept := entries[:0]
		for _, entry := range entries {
			if entry.stepID == stepID {
				select {
				case entry.ch <- nil:
				default:
				}
				continue
			}
			kept = append(kept, entry)
		}
		d.waiters[channel] = kept
	}
}

func (d *Dispatcher) removeWaiter(channel string, ch chan *eventbuspkg.InboundEvent) {
	d.mu.Lock()
	defer d.mu.Unlock()
	entries := d.waiters[channel]
	kept := entries[:0]
	for _, entry := range entries {
		if entry.ch == ch {
			continue
		}
		kept = append(kept, entry)
	}
	d.waiters[channel] = kept
}

func matchesFilter(ev eventbuspkg.InboundEvent, filter eventbuspkg.EventFilter) bool {
	if filter.Source != "" && ev.Source != filter.Source {
		return false
	}
	if filter.ID != "" && ev.EventID != filter.ID {
		return false
	}
	for k, v := range filter.Payload {
		actual, ok := ev.Payload[k]
		if !ok {
			return false
		}
		if fmt.Sprintf("%v", actual) != v {
			return false
		}
	}
	return true
}

func dispatchChannel(ev eventbuspkg.InboundEvent) string {
	if ev.Channel != "" {
		return ev.Channel
	}
	return ev.EventID
}
