package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	evidencepkg "github.com/ormasoftchile/yawr/runtime/pkg/evidence"
	tracepkg "github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

func TestRPC_RunStart_Success(t *testing.T) {
	h := newTestServerHarness(t)
	resp := doRPC(t, h.server, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "run.start",
		"params": map[string]any{
			"runbookPath": "runbook.yaml",
		},
	})

	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	result := resp.Result.(map[string]any)
	if result["runID"] != "run-1" {
		t.Fatalf("expected runID run-1, got %v", result["runID"])
	}
}

func TestRPC_RunStart_InvalidParams(t *testing.T) {
	h := newTestServerHarness(t)
	resp := doRPC(t, h.server, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "run.start",
		"params":  map[string]any{},
	})

	if resp.Error == nil || resp.Error.Code != rpcInvalidParams {
		t.Fatalf("expected invalid params, got %+v", resp.Error)
	}
}

func TestRPC_RunStart_RunbookNotFound(t *testing.T) {
	h := newTestServerHarness(t)
	h.parser.err = os.ErrNotExist
	resp := doRPC(t, h.server, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "run.start",
		"params": map[string]any{
			"runbookPath": "missing.yaml",
		},
	})

	if resp.Error == nil || resp.Error.Code != rpcRunbookNotFound {
		t.Fatalf("expected runbook not found, got %+v", resp.Error)
	}
}

func TestRPC_RunNext_Advances(t *testing.T) {
	h := newTestServerHarness(t)
	h.handle.results = []*engine.StepResult{{
		StepID: "step-1",
		Status: engine.StepStatusCompleted,
		Output: map[string]any{"ok": true},
	}}
	start := doRPC(t, h.server, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "run.start",
		"params": map[string]any{
			"runbookPath": "runbook.yaml",
		},
	})
	if start.Error != nil {
		t.Fatalf("unexpected start error: %+v", start.Error)
	}

	resp := doRPC(t, h.server, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "run.next",
		"params": map[string]any{
			"runID": "run-1",
		},
	})
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	result := resp.Result.(map[string]any)
	if result["stepID"] != "step-1" {
		t.Fatalf("expected stepID step-1, got %v", result["stepID"])
	}
}

func TestRPC_RunNext_EOF(t *testing.T) {
	h := newTestServerHarness(t)
	_ = doRPC(t, h.server, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "run.start",
		"params": map[string]any{
			"runbookPath": "runbook.yaml",
		},
	})

	resp := doRPC(t, h.server, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "run.next",
		"params": map[string]any{
			"runID": "run-1",
		},
	})
	if resp.Error == nil || resp.Error.Code != rpcRunCompleted {
		t.Fatalf("expected run completed, got %+v", resp.Error)
	}
}

func TestRPC_RunNext_PreservesPausedBoundaryOnEOF(t *testing.T) {
	h := newTestServerHarness(t)
	h.handle.eofStatus = engine.RunStatusPausedAtBoundary
	_ = doRPC(t, h.server, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "run.start",
		"params": map[string]any{"runbookPath": "runbook.yaml"},
	})
	resp := doRPC(t, h.server, map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "run.next",
		"params": map[string]any{"runID": "run-1"},
	})
	if resp.Error != nil {
		t.Fatalf("paused run returned error: %+v", resp.Error)
	}
	result := resp.Result.(map[string]any)
	if result["state"] != string(engine.RunStatusPausedAtBoundary) {
		t.Fatalf("paused response = %#v", result)
	}
	entry, ok := h.server.registry.Get("run-1")
	if !ok || entry.State != engine.RunStatusPausedAtBoundary || !entry.CompletedAt.IsZero() {
		t.Fatalf("paused registry entry = %#v", entry)
	}
}

func TestRPC_RunNext_RunNotFound(t *testing.T) {
	h := newTestServerHarness(t)
	resp := doRPC(t, h.server, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "run.next",
		"params": map[string]any{
			"runID": "missing",
		},
	})
	if resp.Error == nil || resp.Error.Code != rpcRunNotFound {
		t.Fatalf("expected run not found, got %+v", resp.Error)
	}
}

func TestRPC_RunCancel_Stops(t *testing.T) {
	h := newTestServerHarness(t)
	_ = doRPC(t, h.server, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "run.start",
		"params": map[string]any{
			"runbookPath": "runbook.yaml",
		},
	})

	resp := doRPC(t, h.server, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "run.cancel",
		"params": map[string]any{
			"runID":  "run-1",
			"reason": "test",
		},
	})
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	if state := h.handle.State().Status; state != engine.RunStatusCancelled {
		t.Fatalf("expected cancelled state, got %s", state)
	}
}

func TestRPC_RunStatus_ReflectsState(t *testing.T) {
	h := newTestServerHarness(t)
	_ = doRPC(t, h.server, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "run.start",
		"params": map[string]any{
			"runbookPath": "runbook.yaml",
		},
	})

	resp := doRPC(t, h.server, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "run.status",
		"params": map[string]any{
			"runID": "run-1",
		},
	})
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	result := resp.Result.(map[string]any)
	if result["runID"] != "run-1" {
		t.Fatalf("expected runID run-1, got %v", result["runID"])
	}
}

func TestRPC_RunList_ShowsActiveRuns(t *testing.T) {
	h := newTestServerHarness(t)
	_ = doRPC(t, h.server, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "run.start",
		"params": map[string]any{
			"runbookPath": "runbook.yaml",
		},
	})

	resp := doRPC(t, h.server, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "run.list",
	})
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	list := resp.Result.([]any)
	if len(list) == 0 {
		t.Fatal("expected runs in list")
	}
}

func TestRPC_RunEvidence_ReturnsRecords(t *testing.T) {
	h := newTestServerHarness(t)
	payload := map[string]any{
		"step_id": "step-1",
		"evidence": []evidencepkg.EvidenceRecord{{
			Name:  "stdout",
			Kind:  evidencepkg.EvidenceKindText,
			Value: "ok",
		}},
	}
	_ = h.store.WriteTrace(context.Background(), "run-1", engine.Event{
		EventID:   "evt-1",
		RunID:     "run-1",
		RunbookID: "rb-1",
		Timestamp: "2026-04-23T10:20:30.123456Z",
		Kind:      string(tracepkg.EventKindStepCompleted),
		Sequence:  1,
		Payload:   payload,
	})

	resp := doRPC(t, h.server, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "run.evidence",
		"params": map[string]any{
			"runID": "run-1",
		},
	})
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	result := resp.Result.(map[string]any)
	evidence := result["evidence"].([]any)
	if len(evidence) != 1 {
		t.Fatalf("expected evidence set, got %#v", evidence)
	}
}

func TestRPC_RunResume_CreatesRun(t *testing.T) {
	h := newTestServerHarness(t)
	h.engine.resumeFunc = func(_ context.Context, _ string, _ engine.RunOptions) (engine.RunHandle, error) {
		return newFakeRunHandle("run-1"), nil
	}

	resp := doRPC(t, h.server, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "run.resume",
		"params": map[string]any{
			"runID": "run-1",
		},
	})
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	result := resp.Result.(map[string]any)
	if result["runID"] != "run-1" {
		t.Fatalf("expected runID run-1, got %v", result["runID"])
	}
}

func TestRPC_UnknownMethod(t *testing.T) {
	h := newTestServerHarness(t)
	resp := doRPC(t, h.server, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "run.unknown",
	})
	if resp.Error == nil || resp.Error.Code != rpcMethodNotFound {
		t.Fatalf("expected method not found, got %+v", resp.Error)
	}
}

func TestRPC_MalformedJSON(t *testing.T) {
	h := newTestServerHarness(t)
	body := []byte("{")
	req := httptest.NewRequest(http.MethodPost, "/rpc", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.server.handler.ServeHTTP(rec, req)

	var resp rpcResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != rpcParseError {
		t.Fatalf("expected parse error, got %+v", resp.Error)
	}
}

func TestRPC_MissingJsonrpcField(t *testing.T) {
	h := newTestServerHarness(t)
	resp := doRPC(t, h.server, map[string]any{
		"id":     1,
		"method": "run.start",
		"params": map[string]any{"runbookPath": "runbook.yaml"},
	})
	if resp.Error == nil || resp.Error.Code != rpcInvalidRequest {
		t.Fatalf("expected invalid request, got %+v", resp.Error)
	}
}

func doRPC(t *testing.T, srv *Server, payload map[string]any) rpcResponse {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/rpc", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status %d", rec.Code)
	}

	var resp rpcResponse
	decoder := json.NewDecoder(rec.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	resp.Result = normalizeResult(resp.Result)
	if resp.Error != nil {
		return resp
	}
	return resp
}

func normalizeResult(value any) any {
	switch result := value.(type) {
	case map[string]any:
		return result
	case []any:
		return result
	case nil:
		return nil
	default:
		return value
	}
}

func TestRPC_RunList_MergesActiveAndPersisted(t *testing.T) {
	h := newTestServerHarness(t)

	// Persist a completed run in store.
	completedState := engine.RunState{
		RunID:       "run-completed",
		RunbookPath: "completed.yaml",
		Status:      engine.RunStatusCompleted,
		StartedAt:   time.Now().Add(-1 * time.Hour),
	}
	if err := h.store.SaveState(context.Background(), completedState); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	// Start an active run in registry.
	_ = doRPC(t, h.server, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "run.start",
		"params":  map[string]any{"runbookPath": "runbook.yaml"},
	})

	resp := doRPC(t, h.server, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "run.list",
	})
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	list := resp.Result.([]any)
	if len(list) < 2 {
		t.Fatalf("expected at least 2 runs (active + persisted), got %d", len(list))
	}

	sources := map[string]bool{}
	for _, item := range list {
		run := item.(map[string]any)
		if src, ok := run["source"].(string); ok {
			sources[src] = true
		}
	}
	if !sources["active"] {
		t.Error("expected at least one active run in list")
	}
	if !sources["persisted"] {
		t.Error("expected at least one persisted run in list")
	}
}

func TestRPC_RunList_ActiveOverridesPersisted(t *testing.T) {
	h := newTestServerHarness(t)

	// Persist a state for run-1 as completed.
	oldState := engine.RunState{
		RunID:       "run-1",
		RunbookPath: "runbook.yaml",
		Status:      engine.RunStatusCompleted,
		StartedAt:   time.Now().Add(-1 * time.Hour),
	}
	if err := h.store.SaveState(context.Background(), oldState); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	// Start run-1 active in the registry (same ID).
	_ = doRPC(t, h.server, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "run.start",
		"params":  map[string]any{"runbookPath": "runbook.yaml"},
	})

	resp := doRPC(t, h.server, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "run.list",
	})
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	list := resp.Result.([]any)

	// run-1 should appear exactly once, as active.
	count := 0
	for _, item := range list {
		run := item.(map[string]any)
		if run["runID"] == "run-1" {
			count++
			if run["source"] != "active" {
				t.Errorf("expected run-1 source=active, got %v", run["source"])
			}
		}
	}
	if count != 1 {
		t.Fatalf("expected run-1 to appear exactly once, got %d", count)
	}
}

func TestRPC_RunList_StoreNilFallback(t *testing.T) {
	h := newTestServerHarness(t)
	// Remove store reference from server.
	h.server.store = nil

	_ = doRPC(t, h.server, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "run.start",
		"params":  map[string]any{"runbookPath": "runbook.yaml"},
	})

	resp := doRPC(t, h.server, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "run.list",
	})
	if resp.Error != nil {
		t.Fatalf("expected graceful degradation without store, got error: %+v", resp.Error)
	}
	list := resp.Result.([]any)
	if len(list) == 0 {
		t.Fatal("expected registry runs even without store")
	}
}

func TestRPC_RunGet_ActiveRun(t *testing.T) {
	h := newTestServerHarness(t)
	_ = doRPC(t, h.server, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "run.start",
		"params":  map[string]any{"runbookPath": "runbook.yaml"},
	})

	resp := doRPC(t, h.server, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "run.get",
		"params":  map[string]any{"runID": "run-1"},
	})
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	result := resp.Result.(map[string]any)
	if result["runID"] != "run-1" {
		t.Fatalf("expected runID run-1, got %v", result["runID"])
	}
	if result["source"] != "active" {
		t.Fatalf("expected source=active, got %v", result["source"])
	}
}

func TestRPC_RunGet_PersistedRun(t *testing.T) {
	h := newTestServerHarness(t)

	persistedState := engine.RunState{
		RunID:            "run-persisted",
		RunbookPath:      "persisted.yaml",
		Status:           engine.RunStatusCompleted,
		CurrentStep:      "step-3",
		CurrentStepIndex: 2,
		StartedAt:        time.Now().Add(-2 * time.Hour),
	}
	if err := h.store.SaveState(context.Background(), persistedState); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	resp := doRPC(t, h.server, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "run.get",
		"params":  map[string]any{"runID": "run-persisted"},
	})
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	result := resp.Result.(map[string]any)
	if result["runID"] != "run-persisted" {
		t.Fatalf("expected runID run-persisted, got %v", result["runID"])
	}
	if result["source"] != "persisted" {
		t.Fatalf("expected source=persisted, got %v", result["source"])
	}
	if result["state"] != "completed" {
		t.Fatalf("expected state=completed, got %v", result["state"])
	}
}

func TestRPC_RunGet_NotFound(t *testing.T) {
	h := newTestServerHarness(t)
	resp := doRPC(t, h.server, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "run.get",
		"params":  map[string]any{"runID": "nonexistent"},
	})
	if resp.Error == nil || resp.Error.Code != rpcRunNotFound {
		t.Fatalf("expected rpcRunNotFound, got %+v", resp.Error)
	}
}

func TestRPC_RunGet_InvalidParams(t *testing.T) {
	h := newTestServerHarness(t)
	resp := doRPC(t, h.server, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "run.get",
		"params":  map[string]any{"runID": ""},
	})
	if resp.Error == nil || resp.Error.Code != rpcInvalidParams {
		t.Fatalf("expected rpcInvalidParams, got %+v", resp.Error)
	}
}

func TestRPC_RunDelete(t *testing.T) {
	h := newTestServerHarness(t)

	completedState := engine.RunState{
		RunID:       "run-completed",
		RunbookPath: "done.yaml",
		Status:      engine.RunStatusCompleted,
		StartedAt:   time.Now().Add(-1 * time.Hour),
	}
	if err := h.store.SaveState(context.Background(), completedState); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	resp := doRPC(t, h.server, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "run.delete",
		"params":  map[string]any{"runID": "run-completed"},
	})
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	result := resp.Result.(map[string]any)
	if result["deleted"] != true {
		t.Fatalf("expected deleted=true, got %v", result["deleted"])
	}
}

func TestRPC_RunDelete_Running(t *testing.T) {
	h := newTestServerHarness(t)
	_ = doRPC(t, h.server, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "run.start",
		"params":  map[string]any{"runbookPath": "runbook.yaml"},
	})

	resp := doRPC(t, h.server, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "run.delete",
		"params":  map[string]any{"runID": "run-1"},
	})
	if resp.Error == nil || resp.Error.Code != rpcRunDeleteRunning {
		t.Fatalf("expected rpcRunDeleteRunning, got %+v", resp.Error)
	}
}

func TestRPC_RunDelete_NotFound(t *testing.T) {
	h := newTestServerHarness(t)
	resp := doRPC(t, h.server, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "run.delete",
		"params":  map[string]any{"runID": "nonexistent-run"},
	})
	if resp.Error == nil || resp.Error.Code != rpcRunNotFound {
		t.Fatalf("expected rpcRunNotFound, got %+v", resp.Error)
	}
}

func TestRPC_RunDelete_MissingRunID(t *testing.T) {
	h := newTestServerHarness(t)
	resp := doRPC(t, h.server, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "run.delete",
		"params":  map[string]any{"runID": ""},
	})
	if resp.Error == nil || resp.Error.Code != rpcInvalidParams {
		t.Fatalf("expected rpcInvalidParams, got %+v", resp.Error)
	}
}

func TestRPC_RunGet_Persisted_CompletedAt(t *testing.T) {
	t.Parallel()
	h := newTestServerHarness(t)

	completedAt := time.Date(2026, 4, 20, 9, 5, 32, 0, time.UTC)
	persistedState := engine.RunState{
		RunID:            "run-completed-ts",
		RunbookPath:      "check.yaml",
		Status:           engine.RunStatusCompleted,
		CurrentStep:      "step-verify",
		CurrentStepIndex: 3,
		StartedAt:        time.Date(2026, 4, 20, 9, 0, 0, 0, time.UTC),
		CompletedAt:      completedAt,
	}
	if err := h.store.SaveState(context.Background(), persistedState); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	resp := doRPC(t, h.server, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "run.get",
		"params":  map[string]any{"runID": "run-completed-ts"},
	})
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	result := resp.Result.(map[string]any)
	if result["source"] != "persisted" {
		t.Fatalf("expected source=persisted, got %v", result["source"])
	}
	got, ok := result["completedAt"].(string)
	if !ok || got == "" {
		t.Fatalf("expected completedAt to be present, got %v", result["completedAt"])
	}
	if got != completedAt.Format(time.RFC3339Nano) {
		t.Fatalf("expected completedAt=%s, got %s", completedAt.Format(time.RFC3339Nano), got)
	}
}

func TestRPC_RunList_CompletedAt_ActiveRun(t *testing.T) {
	t.Parallel()
	h := newTestServerHarness(t)

	// Start an active run, then complete it so CompletedAt is set on the registry entry.
	_ = doRPC(t, h.server, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "run.start",
		"params":  map[string]any{"runbookPath": "runbook.yaml"},
	})

	// Manually set CompletedAt on the live registry entry.
	completedAt := time.Date(2026, 4, 21, 12, 0, 0, 0, time.UTC)
	if !h.server.registry.WithEntry("run-1", func(e *RunEntry) {
		e.CompletedAt = completedAt
	}) {
		t.Fatal("expected run-1 in registry")
	}

	resp := doRPC(t, h.server, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "run.list",
	})
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	list := resp.Result.([]any)
	var found map[string]any
	for _, item := range list {
		run := item.(map[string]any)
		if run["runID"] == "run-1" {
			found = run
			break
		}
	}
	if found == nil {
		t.Fatal("run-1 not found in list")
	}
	got, ok := found["completedAt"].(string)
	if !ok || got == "" {
		t.Fatalf("expected completedAt in active run list item, got %v", found["completedAt"])
	}
	if got != completedAt.Format(time.RFC3339Nano) {
		t.Fatalf("expected completedAt=%s, got %s", completedAt.Format(time.RFC3339Nano), got)
	}
}

func TestRPC_RunList_CompletedAt_PersistedRun(t *testing.T) {
	t.Parallel()
	h := newTestServerHarness(t)

	completedAt := time.Date(2026, 4, 20, 9, 5, 32, 0, time.UTC)
	persistedState := engine.RunState{
		RunID:       "run-listed-completed",
		RunbookPath: "check.yaml",
		Status:      engine.RunStatusCompleted,
		StartedAt:   time.Date(2026, 4, 20, 9, 0, 0, 0, time.UTC),
		CompletedAt: completedAt,
	}
	if err := h.store.SaveState(context.Background(), persistedState); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	resp := doRPC(t, h.server, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "run.list",
	})
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	list := resp.Result.([]any)
	var found map[string]any
	for _, item := range list {
		run := item.(map[string]any)
		if run["runID"] == "run-listed-completed" {
			found = run
			break
		}
	}
	if found == nil {
		t.Fatal("run-listed-completed not found in list")
	}
	got, ok := found["completedAt"].(string)
	if !ok || got == "" {
		t.Fatalf("expected completedAt in persisted run list item, got %v", found["completedAt"])
	}
	if got != completedAt.Format(time.RFC3339Nano) {
		t.Fatalf("expected completedAt=%s, got %s", completedAt.Format(time.RFC3339Nano), got)
	}
}
