package testutil

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/eventbus"
)

// ErrEventTimeout is returned by Wait when no matching event arrives before the deadline.
var ErrEventTimeout = errors.New("testutil: event wait timed out")

// ErrEventCancelled is returned by Wait when DrainAll/Cancel is called or ctx is cancelled.
var ErrEventCancelled = errors.New("testutil: event wait cancelled")

// waiterEntry holds the channel used to deliver an event to a single waiter.
type waiterEntry struct {
	stepID string
	filter eventbus.EventFilter
	ch     chan *eventbus.InboundEvent
}

// FakeEventDispatcher is a controllable EventDispatcher for unit tests.
// It implements eventbus.EventDispatcher. Inject events with Dispatch(),
// observe waiting steps with WaitersCount().
// Consume semantics: the first waiter on a channel receives each event.
type FakeEventDispatcher struct {
	// pending queues events by channel name (consume semantics).
	pending map[string][]eventbus.InboundEvent
	// waiters tracks goroutines blocked in Wait().
	waiters map[string][]waiterEntry
	mu      sync.Mutex
}

// Ensure FakeEventDispatcher implements eventbus.EventDispatcher at compile time.
var _ eventbus.EventDispatcher = (*FakeEventDispatcher)(nil)

// NewFakeEventDispatcher returns an initialised FakeEventDispatcher.
func NewFakeEventDispatcher() *FakeEventDispatcher {
	return &FakeEventDispatcher{
		pending: make(map[string][]eventbus.InboundEvent),
		waiters: make(map[string][]waiterEntry),
	}
}

// matchesFilter reports whether ev satisfies all non-empty criteria in f.
func matchesFilter(ev eventbus.InboundEvent, f eventbus.EventFilter) bool {
	if f.Source != "" && ev.Source != f.Source {
		return false
	}
	if f.ID != "" && ev.EventID != f.ID {
		return false
	}
	for k, v := range f.Payload {
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

// Dispatch delivers ev to the first waiter whose filter matches on ev.Channel.
// If no matching waiter exists, the event is queued for future Wait calls
// (consume semantics: each event is delivered at most once).
func (d *FakeEventDispatcher) Dispatch(ev eventbus.InboundEvent) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	ch := ev.Channel
	entries := d.waiters[ch]
	for i, e := range entries {
		if matchesFilter(ev, e.filter) {
			d.waiters[ch] = append(entries[:i], entries[i+1:]...)
			e.ch <- &ev
			return nil
		}
	}
	// Also check catch-all waiters (registered by Wait without a channel).
	const catchAll = ""
	for i, e := range d.waiters[catchAll] {
		if matchesFilter(ev, e.filter) {
			d.waiters[catchAll] = append(d.waiters[catchAll][:i], d.waiters[catchAll][i+1:]...)
			e.ch <- &ev
			return nil
		}
	}
	// No waiter matched — queue the event.
	d.pending[ch] = append(d.pending[ch], ev)
	return nil
}

// Wait blocks until a matching event arrives, until timeout elapses, or until
// ctx is cancelled / Cancel/DrainAll is called.
//
// Since the channel is not part of the filter, Wait scans all pending queues
// and registers as a catch-all waiter. Use WaitOnChannel when the channel name
// is known at call-site.
func (d *FakeEventDispatcher) Wait(ctx context.Context, stepID string, filter eventbus.EventFilter, timeout time.Duration) (*eventbus.InboundEvent, error) {
	d.mu.Lock()
	// Scan all pending queues for a matching event.
	for channel, events := range d.pending {
		for i, ev := range events {
			if matchesFilter(ev, filter) {
				d.pending[channel] = append(events[:i], events[i+1:]...)
				d.mu.Unlock()
				return &ev, nil
			}
		}
	}

	delivery := make(chan *eventbus.InboundEvent, 1)
	entry := waiterEntry{stepID: stepID, filter: filter, ch: delivery}
	const catchAll = ""
	d.waiters[catchAll] = append(d.waiters[catchAll], entry)
	d.mu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case ev := <-delivery:
		if ev == nil {
			return nil, ErrEventCancelled
		}
		return ev, nil
	case <-timer.C:
		d.removeWaiter(catchAll, delivery)
		return nil, ErrEventTimeout
	case <-ctx.Done():
		d.removeWaiter(catchAll, delivery)
		return nil, ErrEventCancelled
	}
}

// Cancel cancels any outstanding Wait for the given stepID with ErrEventCancelled.
func (d *FakeEventDispatcher) Cancel(stepID string, _ string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	for ch, entries := range d.waiters {
		kept := entries[:0]
		for _, e := range entries {
			if e.stepID == stepID {
				select {
				case e.ch <- nil:
				default:
				}
			} else {
				kept = append(kept, e)
			}
		}
		d.waiters[ch] = kept
	}
}

// WaitOnChannel blocks until a matching event arrives on the named channel,
// until timeout elapses, or ctx is cancelled. Preferred when the channel name
// is known at call-site.
func (d *FakeEventDispatcher) WaitOnChannel(ctx context.Context, channel, stepID string, filter eventbus.EventFilter, timeout time.Duration) (*eventbus.InboundEvent, error) {
	d.mu.Lock()
	// Scan pending queue first.
	events := d.pending[channel]
	for i, ev := range events {
		if matchesFilter(ev, filter) {
			d.pending[channel] = append(events[:i], events[i+1:]...)
			d.mu.Unlock()
			return &ev, nil
		}
	}

	delivery := make(chan *eventbus.InboundEvent, 1)
	entry := waiterEntry{stepID: stepID, filter: filter, ch: delivery}
	d.waiters[channel] = append(d.waiters[channel], entry)
	d.mu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case ev := <-delivery:
		if ev == nil {
			return nil, ErrEventCancelled
		}
		return ev, nil
	case <-timer.C:
		d.removeWaiter(channel, delivery)
		return nil, ErrEventTimeout
	case <-ctx.Done():
		d.removeWaiter(channel, delivery)
		return nil, ErrEventCancelled
	}
}

// WaitersCount returns the number of goroutines currently blocked in Wait on
// the given channel.
func (d *FakeEventDispatcher) WaitersCount(channel string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.waiters[channel])
}

// DrainAll clears all pending events and unblocks all waiters with ErrEventCancelled.
func (d *FakeEventDispatcher) DrainAll() {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.pending = make(map[string][]eventbus.InboundEvent)
	for ch, entries := range d.waiters {
		for _, e := range entries {
			select {
			case e.ch <- nil:
			default:
			}
		}
		delete(d.waiters, ch)
	}
}

func (d *FakeEventDispatcher) removeWaiter(channel string, delivery chan *eventbus.InboundEvent) {
	d.mu.Lock()
	defer d.mu.Unlock()
	entries := d.waiters[channel]
	for i, e := range entries {
		if e.ch == delivery {
			d.waiters[channel] = append(entries[:i], entries[i+1:]...)
			return
		}
	}
}
