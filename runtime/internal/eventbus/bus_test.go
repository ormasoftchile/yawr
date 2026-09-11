package eventbus

import (
	"context"
	"sync"
	"testing"
	"time"

	eventbuspkg "github.com/ormasoftchile/yawr/runtime/pkg/eventbus"
)

func TestBus_Subscribe(t *testing.T) {
	bus := NewBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := bus.Subscribe(ctx, eventbuspkg.SubscribeOptions{KindPrefix: "step/"})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	bus.Publish(eventbuspkg.Event{RunID: "run-1", Kind: "step/started"})
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("expected event")
	}
}

func TestBus_FanOut(t *testing.T) {
	bus := NewBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a, _ := bus.Subscribe(ctx, eventbuspkg.SubscribeOptions{})
	b, _ := bus.Subscribe(ctx, eventbuspkg.SubscribeOptions{})
	bus.Publish(eventbuspkg.Event{RunID: "run-1", Kind: "run/started"})

	select {
	case <-a:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for subscriber A")
	}
	select {
	case <-b:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for subscriber B")
	}
}

func TestBus_Unsubscribe(t *testing.T) {
	bus := NewBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, _ := bus.Subscribe(ctx, eventbuspkg.SubscribeOptions{})
	bus.Unsubscribe(ch)

	_, ok := <-ch
	if ok {
		t.Fatalf("expected channel closed after unsubscribe")
	}
}

func TestBus_ConcurrentPublish(t *testing.T) {
	bus := NewBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, _ := bus.Subscribe(ctx, eventbuspkg.SubscribeOptions{BufferSize: 500})
	const total = 200

	var wg sync.WaitGroup
	wg.Add(total)
	for i := 0; i < total; i++ {
		go func() {
			defer wg.Done()
			bus.Publish(eventbuspkg.Event{RunID: "run-1", Kind: "step/started"})
		}()
	}
	wg.Wait()

	received := 0
	for {
		select {
		case <-ch:
			received++
		default:
			if received == 0 {
				t.Fatalf("expected events to be published")
			}
			return
		}
	}
}
