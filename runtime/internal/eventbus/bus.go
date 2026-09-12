package eventbus

import (
	"context"
	"sync"

	eventbuspkg "github.com/ormasoftchile/yawr/runtime/pkg/eventbus"
)

// Bus is an in-process EventBus implementation.
type Bus struct {
	mu          sync.RWMutex
	subscribers map[<-chan eventbuspkg.Event]subscriber
}

type subscriber struct {
	ch         chan eventbuspkg.Event
	kindPrefix string
	runID      string
}

// NewBus constructs a Bus.
func NewBus() *Bus {
	return &Bus{
		subscribers: make(map[<-chan eventbuspkg.Event]subscriber),
	}
}

// Publish broadcasts ev to all matching subscribers.
func (b *Bus) Publish(ev eventbuspkg.Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, sub := range b.subscribers {
		if sub.kindPrefix != "" && !hasPrefix(ev.Kind, sub.kindPrefix) {
			continue
		}
		if sub.runID != "" && sub.runID != ev.RunID {
			continue
		}
		select {
		case sub.ch <- ev:
		default:
		}
	}
}

// Subscribe registers a subscriber and returns its channel.
func (b *Bus) Subscribe(ctx context.Context, opts eventbuspkg.SubscribeOptions) (<-chan eventbuspkg.Event, error) {
	buffer := opts.BufferSize
	if buffer <= 0 {
		buffer = 100
	}
	ch := make(chan eventbuspkg.Event, buffer)
	ro := (<-chan eventbuspkg.Event)(ch)
	b.mu.Lock()
	b.subscribers[ro] = subscriber{
		ch:         ch,
		kindPrefix: opts.KindPrefix,
		runID:      opts.RunID,
	}
	b.mu.Unlock()

	go func() {
		<-ctx.Done()
		b.Unsubscribe(ro)
	}()
	return ro, nil
}

// Unsubscribe removes a subscriber and closes its channel.
func (b *Bus) Unsubscribe(ch <-chan eventbuspkg.Event) {
	b.mu.Lock()
	sub, ok := b.subscribers[ch]
	if ok {
		delete(b.subscribers, ch)
		close(sub.ch)
	}
	b.mu.Unlock()
}

func hasPrefix(s, prefix string) bool {
	if prefix == "" {
		return true
	}
	if len(prefix) > len(s) {
		return false
	}
	return s[:len(prefix)] == prefix
}
