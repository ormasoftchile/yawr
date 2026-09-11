package eventbus

import (
	"context"
	"testing"
	"time"

	eventbuspkg "github.com/ormasoftchile/yawr/runtime/pkg/eventbus"
)

func TestDispatcher_RegisterDispatch(t *testing.T) {
	dispatcher := NewDispatcher()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := dispatcher.Register(ctx, "run-1", "event-1")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := dispatcher.Dispatch(eventbuspkg.InboundEvent{EventID: "event-1", Channel: "event-1"}); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	select {
	case ev := <-ch:
		if ev.EventID != "event-1" {
			t.Fatalf("expected event-1, got %q", ev.EventID)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for event")
	}
}

func TestDispatcher_FirstWaiterWins(t *testing.T) {
	dispatcher := NewDispatcher()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	first, _ := dispatcher.Register(ctx, "run-1", "event-2")
	second, _ := dispatcher.Register(ctx, "run-1", "event-2")
	_ = dispatcher.Dispatch(eventbuspkg.InboundEvent{EventID: "event-2", Channel: "event-2"})

	select {
	case <-first:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for first waiter")
	}
	select {
	case <-second:
		t.Fatalf("expected second waiter to remain pending")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestDispatcher_ContextCancel(t *testing.T) {
	dispatcher := NewDispatcher()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ch, err := dispatcher.Register(ctx, "run-2", "event-3")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatalf("expected channel to be closed on cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for cancel")
	}
}

func TestDispatcher_Unregister(t *testing.T) {
	dispatcher := NewDispatcher()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := dispatcher.Register(ctx, "run-3", "event-4")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	dispatcher.Unregister("run-3", "event-4")

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatalf("expected channel closed after unregister")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for unregister")
	}
}
