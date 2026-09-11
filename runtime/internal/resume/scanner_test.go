package resume

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	internaltrace "github.com/ormasoftchile/yawr/runtime/internal/trace"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	tracepkg "github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

func TestScanTrace_FindsLastCheckpoint(t *testing.T) {
	dir := makeScannerDir(t)
	tracePath := filepath.Join(dir, "trace.jsonl")
	writeTraceEvents(t, tracePath, []tracepkg.TraceEvent{
		makeTraceEvent(1, tracepkg.EventKindStepCompleted, payload(map[string]any{"step_id": "step-1"})),
		makeTraceEvent(2, tracepkg.EventKind("checkpoint"), payload(map[string]any{"snapshot_file": "step-0001.json"})),
		makeTraceEvent(3, tracepkg.EventKindStepCompleted, payload(map[string]any{"step_id": "step-2"})),
		makeTraceEvent(4, tracepkg.EventKind("checkpoint"), payload(map[string]any{"snapshot_file": "step-0002.json"})),
	})

	store := &fakeRunStore{tracePath: tracePath}
	rc, err := ScanTrace(context.Background(), store, "run-1", engine.RunState{})
	if err != nil {
		t.Fatalf("ScanTrace: %v", err)
	}
	if rc.LastCheckpointSeq != 4 {
		t.Fatalf("expected last checkpoint seq 4, got %d", rc.LastCheckpointSeq)
	}
	if !rc.CompletedStepIDs["step-2"] {
		t.Fatalf("expected completed step-2")
	}
}

func TestScanTrace_OrphanedToolCall(t *testing.T) {
	dir := makeScannerDir(t)
	tracePath := filepath.Join(dir, "trace.jsonl")
	writeTraceEvents(t, tracePath, []tracepkg.TraceEvent{
		makeTraceEvent(1, tracepkg.EventKindToolInvoked, payload(map[string]any{"correlation_id": "c1"})),
	})

	store := &fakeRunStore{tracePath: tracePath}
	rc, err := ScanTrace(context.Background(), store, "run-1", engine.RunState{})
	if err != nil {
		t.Fatalf("ScanTrace: %v", err)
	}
	if len(rc.OrphanedToolCalls) != 1 || rc.OrphanedToolCalls[0] != "c1" {
		t.Fatalf("unexpected orphaned calls: %#v", rc.OrphanedToolCalls)
	}
}

func TestScanTrace_NoCheckpoint(t *testing.T) {
	dir := makeScannerDir(t)
	tracePath := filepath.Join(dir, "trace.jsonl")
	writeTraceEvents(t, tracePath, []tracepkg.TraceEvent{
		makeTraceEvent(1, tracepkg.EventKindStepCompleted, payload(map[string]any{"step_id": "step-1"})),
	})

	store := &fakeRunStore{tracePath: tracePath}
	rc, err := ScanTrace(context.Background(), store, "run-1", engine.RunState{})
	if err != nil {
		t.Fatalf("ScanTrace: %v", err)
	}
	if rc.LastCheckpointSeq != 0 {
		t.Fatalf("expected no checkpoint seq, got %d", rc.LastCheckpointSeq)
	}
}

func TestScanTrace_CorruptedLineSkipped(t *testing.T) {
	dir := makeScannerDir(t)
	tracePath := filepath.Join(dir, "trace.jsonl")
	writer, err := internaltrace.NewJSONLWriter(tracePath)
	if err != nil {
		t.Fatalf("NewJSONLWriter: %v", err)
	}
	if err := writer.Append(makeTraceEvent(1, tracepkg.EventKindStepCompleted, payload(map[string]any{"step_id": "step-1"}))); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	f, err := os.OpenFile(tracePath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := f.WriteString("{bad-json}\n"); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	store := &fakeRunStore{tracePath: tracePath}
	rc, err := ScanTrace(context.Background(), store, "run-1", engine.RunState{})
	if err != nil {
		t.Fatalf("ScanTrace: %v", err)
	}
	if !rc.CompletedStepIDs["step-1"] {
		t.Fatalf("expected completed step-1")
	}
}

type fakeRunStore struct {
	tracePath string
}

func (f *fakeRunStore) SaveState(_ context.Context, _ engine.RunState) error { return nil }
func (f *fakeRunStore) LoadState(_ context.Context, _ string) (engine.RunState, error) {
	return engine.RunState{}, nil
}
func (f *fakeRunStore) WriteTrace(_ context.Context, _ string, _ engine.Event) error { return nil }
func (f *fakeRunStore) Close() error                                                 { return nil }
func (f *fakeRunStore) TracePath(_ string) string                                    { return f.tracePath }

func makeTraceEvent(seq int64, kind tracepkg.EventKind, payload json.RawMessage) tracepkg.TraceEvent {
	return tracepkg.TraceEvent{
		EventID:   fmt.Sprintf("evt-%d", seq),
		RunID:     "run-1",
		RunbookID: "rb-1",
		Timestamp: "2026-04-23T10:20:30.123456Z",
		Kind:      kind,
		Sequence:  seq,
		Payload:   payload,
	}
}

func payload(data map[string]any) json.RawMessage {
	raw, _ := json.Marshal(data)
	return raw
}

func writeTraceEvents(t *testing.T, path string, events []tracepkg.TraceEvent) {
	t.Helper()
	writer, err := internaltrace.NewJSONLWriter(path)
	if err != nil {
		t.Fatalf("NewJSONLWriter: %v", err)
	}
	for _, ev := range events {
		if err := writer.Append(ev); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func makeScannerDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(".", "resume-scan-test-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
