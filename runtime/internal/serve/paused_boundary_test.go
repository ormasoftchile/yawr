package serve

import (
	"context"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
)

func TestAdvanceRunPreservesPausedBoundaryOnEOF(t *testing.T) {
	handle := newFakeRunHandle("paused-run")
	handle.eofStatus = engine.RunStatusPausedAtBoundary
	registry := NewRunRegistry()
	entry := &RunEntry{ID: "paused-run", Handle: handle, State: engine.RunStatusRunning}
	registry.Add(entry)
	server := &Server{registry: registry}
	server.wg.Add(1)
	server.advanceRun(context.Background(), entry)
	stored, ok := registry.Get(entry.ID)
	if !ok || stored.State != engine.RunStatusPausedAtBoundary || !stored.CompletedAt.IsZero() {
		t.Fatalf("paused registry entry = %#v", stored)
	}
}

func TestPumpEventsPreservesPausedBoundaryOnClose(t *testing.T) {
	handle := newFakeRunHandle("paused-run")
	handle.eofStatus = engine.RunStatusPausedAtBoundary
	_, _ = handle.Next(context.Background())
	registry := NewRunRegistry()
	entry := &RunEntry{ID: "paused-run", Handle: handle, State: engine.RunStatusPausedAtBoundary}
	registry.Add(entry)
	server := &Server{registry: registry, bridge: NewEventBridge(16)}
	server.wg.Add(1)
	go server.pumpEvents(context.Background(), entry)
	handle.closeEvents()
	server.wg.Wait()
	stored, ok := registry.Get(entry.ID)
	if !ok || stored.State != engine.RunStatusPausedAtBoundary || !stored.CompletedAt.IsZero() {
		t.Fatalf("paused entry = %#v", stored)
	}
}
