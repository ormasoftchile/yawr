package tool

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

func TestFileDispatchStore_BasicLifecycle(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "yawr-file-store-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	store, err := NewFileDispatchStore(tempDir)
	if err != nil {
		t.Fatalf("NewFileDispatchStore failed: %v", err)
	}

	ctx := context.Background()
	req := engine.DispatchRequest{
		Classification:   "mutating",
		EndpointIdentity: "tool:native",
		RenderedRequest: map[string]any{
			"tool":            "deployer",
			"action":          "apply",
			"idempotency_key": "job-100",
			"args":            map[string]any{"cluster": "prod-1"},
		},
	}

	// 1. Prepare
	state, err := store.PrepareDispatch(ctx, req)
	if err != nil {
		t.Fatalf("PrepareDispatch failed: %v", err)
	}
	if state.Status != engine.DispatchStatusPrepared {
		t.Fatalf("expected prepared status, got %s", state.Status)
	}
	if state.OccurrenceID == "" {
		t.Fatal("expected non-empty occurrence ID")
	}

	// 2. Settle
	settleResult := map[string]any{"status": "deployed", "nodes": 3}
	if err := store.SettleDispatch(ctx, state.OccurrenceID, settleResult); err != nil {
		t.Fatalf("SettleDispatch failed: %v", err)
	}

	// 3. Replay with new store instance pointing at same directory (Simulate Restart)
	restartedStore, err := NewFileDispatchStore(tempDir)
	if err != nil {
		t.Fatalf("failed to open restarted store: %v", err)
	}

	replayedState, err := restartedStore.PrepareDispatch(ctx, req)
	if err != nil {
		t.Fatalf("replayed PrepareDispatch failed: %v", err)
	}
	if replayedState.Status != engine.DispatchStatusSettled {
		t.Fatalf("expected settled status on replay, got %s", replayedState.Status)
	}
	if replayedState.OccurrenceID != state.OccurrenceID {
		t.Errorf("expected same occurrence ID on replay: want %s, got %s", state.OccurrenceID, replayedState.OccurrenceID)
	}
}

func TestFileDispatchStore_InterruptedCrashRecovery(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "yawr-file-store-crash-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	store, err := NewFileDispatchStore(tempDir)
	if err != nil {
		t.Fatalf("NewFileDispatchStore failed: %v", err)
	}

	ctx := context.Background()
	req := engine.DispatchRequest{
		Classification:   "mutating",
		EndpointIdentity: "tool:native",
		RenderedRequest: map[string]any{
			"tool":            "deployer",
			"action":          "apply",
			"idempotency_key": "crash-job-200",
			"args":            map[string]any{"cluster": "prod-crash"},
		},
	}

	// Process 1 prepares intent and "crashes" before settling
	state1, err := store.PrepareDispatch(ctx, req)
	if err != nil {
		t.Fatalf("PrepareDispatch failed: %v", err)
	}
	if state1.Status != engine.DispatchStatusPrepared {
		t.Fatalf("expected prepared status, got %s", state1.Status)
	}

	// Process 2 restarts with a new store instance pointing to same directory
	restartedStore, err := NewFileDispatchStore(tempDir)
	if err != nil {
		t.Fatalf("failed to open restarted store: %v", err)
	}

	// Replay must detect unconfirmed prior crash, mark indeterminate, and return ErrIndeterminate
	state2, err := restartedStore.PrepareDispatch(ctx, req)
	if err == nil {
		t.Fatal("expected ErrIndeterminate error on crashed replay, got nil")
	}
	if !errors.Is(err, engine.ErrIndeterminate) {
		t.Fatalf("expected errors.Is ErrIndeterminate, got: %v", err)
	}
	if state2.Status != engine.DispatchStatusIndeterminate {
		t.Fatalf("expected state status indeterminate, got %s", state2.Status)
	}

	// Third attempt also stays indeterminate
	_, err3 := restartedStore.PrepareDispatch(ctx, req)
	if err3 == nil || !errors.Is(err3, engine.ErrIndeterminate) {
		t.Fatalf("subsequent replay should remain indeterminate, got: %v", err3)
	}
}

func TestFileDispatchStore_IdempotencyConflict(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "yawr-file-store-conflict-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	store, err := NewFileDispatchStore(tempDir)
	if err != nil {
		t.Fatalf("NewFileDispatchStore failed: %v", err)
	}

	ctx := context.Background()
	req1 := engine.DispatchRequest{
		Classification:   "mutating",
		EndpointIdentity: "tool:native",
		RenderedRequest: map[string]any{
			"tool":            "git-patch",
			"action":          "apply",
			"idempotency_key": "fixed-idempotency-key-555",
			"args":            map[string]any{"patch": "diff A"},
		},
	}

	// 1. Prepare and settle first request
	state1, err := store.PrepareDispatch(ctx, req1)
	if err != nil {
		t.Fatalf("PrepareDispatch req1 failed: %v", err)
	}
	if err := store.SettleDispatch(ctx, state1.OccurrenceID, map[string]any{"ok": true}); err != nil {
		t.Fatalf("SettleDispatch failed: %v", err)
	}

	// 2. Second request reuses the same idempotency key with DIFFERENT arguments
	req2 := engine.DispatchRequest{
		Classification:   "mutating",
		EndpointIdentity: "tool:native",
		RenderedRequest: map[string]any{
			"tool":            "git-patch",
			"action":          "apply",
			"idempotency_key": "fixed-idempotency-key-555",
			"args":            map[string]any{"patch": "DIFFERENT PATCH B"},
		},
	}

	_, err2 := store.PrepareDispatch(ctx, req2)
	if err2 == nil {
		t.Fatal("expected error when reusing idempotency key with different arguments, got nil")
	}
	if !errors.Is(err2, toolpkg.ErrIdempotencyConflict) {
		t.Fatalf("expected errors.Is ErrIdempotencyConflict, got: %v", err2)
	}
}

func TestFileDispatchStore_ConcurrentContention(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "yawr-file-store-concurrent-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	const goroutines = 8
	var wg sync.WaitGroup
	wg.Add(goroutines)

	results := make([]engine.DispatchState, goroutines)
	errorsList := make([]error, goroutines)

	req := engine.DispatchRequest{
		Classification:   "mutating",
		EndpointIdentity: "tool:native",
		RenderedRequest: map[string]any{
			"tool":            "service",
			"action":          "restart",
			"idempotency_key": "contended-restart-key",
			"args":            map[string]any{"svc": "gateway"},
		},
	}

	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			store, err := NewFileDispatchStore(tempDir)
			if err != nil {
				errorsList[idx] = err
				return
			}
			release, err := store.LockKey(context.Background(), "contended-restart-key")
			if err != nil {
				errorsList[idx] = err
				return
			}
			defer release()

			state, err := store.PrepareDispatch(context.Background(), req)
			results[idx] = state
			errorsList[idx] = err
			if err == nil && state.Status == engine.DispatchStatusPrepared {
				// Winner settles
				_ = store.SettleDispatch(context.Background(), state.OccurrenceID, map[string]any{"restarted": true})
			}
		}(i)
	}

	wg.Wait()

	// Verify that at least one prepared successfully and all callers either got prepared or settled replay
	preparedCount := 0
	settledCount := 0
	for i := 0; i < goroutines; i++ {
		if errorsList[i] != nil {
			t.Errorf("goroutine %d encountered unexpected error: %v", i, errorsList[i])
		}
		if results[i].Status == engine.DispatchStatusPrepared {
			preparedCount++
		} else if results[i].Status == engine.DispatchStatusSettled {
			settledCount++
		}
	}

	if preparedCount < 1 {
		t.Errorf("expected at least 1 winner to prepare, got %d", preparedCount)
	}
	if preparedCount+settledCount != goroutines {
		t.Errorf("expected all goroutines to finish as prepared or settled, got prepared=%d, settled=%d", preparedCount, settledCount)
	}
}
