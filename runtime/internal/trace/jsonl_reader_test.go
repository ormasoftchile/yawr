package trace

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tracepkg "github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

func TestJSONLReader_ReadAll(t *testing.T) {
	dir := makeWorkDir(t)
	path := filepath.Join(dir, "trace.jsonl")

	writer, err := NewJSONLWriter(path)
	if err != nil {
		t.Fatalf("NewJSONLWriter: %v", err)
	}
	if err := writer.Append(makeEvent(1)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := writer.Append(makeEvent(2)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reader := NewJSONLReader(path)
	events, err := reader.ReadAll(context.Background())
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].Sequence != 1 || events[1].Sequence != 2 {
		t.Fatalf("unexpected sequences: %#v", events)
	}
}

func TestJSONLReader_ReadSince(t *testing.T) {
	dir := makeWorkDir(t)
	path := filepath.Join(dir, "trace.jsonl")

	writer, err := NewJSONLWriter(path)
	if err != nil {
		t.Fatalf("NewJSONLWriter: %v", err)
	}
	for i := int64(1); i <= 3; i++ {
		if err := writer.Append(makeEvent(i)); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reader := NewJSONLReader(path)
	events, err := reader.ReadSince(context.Background(), 1)
	if err != nil {
		t.Fatalf("ReadSince: %v", err)
	}
	if len(events) != 2 || events[0].Sequence != 2 || events[1].Sequence != 3 {
		t.Fatalf("unexpected events: %#v", events)
	}
}

func TestJSONLReader_SkipMalformed(t *testing.T) {
	dir := makeWorkDir(t)
	path := filepath.Join(dir, "trace.jsonl")

	writer, err := NewJSONLWriter(path)
	if err != nil {
		t.Fatalf("NewJSONLWriter: %v", err)
	}
	if err := writer.Append(makeEvent(1)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := f.WriteString("{not-json}\n"); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	writer, err = NewJSONLWriter(path)
	if err != nil {
		t.Fatalf("NewJSONLWriter: %v", err)
	}
	if err := writer.Append(makeEvent(2)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reader := NewJSONLReader(path)
	events, err := reader.ReadAll(context.Background())
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
}

func TestJSONLReaderStrictRejectsMalformedGapAndIdentityConflict(t *testing.T) {
	for _, test := range []struct {
		name  string
		lines []string
		want  string
	}{
		{name: "malformed", lines: []string{string(mustJSONRecord(t, makeEvent(1))), "{not-json}"}, want: "malformed"},
		{name: "sequence gap", lines: []string{string(mustJSONRecord(t, makeEvent(1))), string(mustJSONRecord(t, makeEvent(3)))}, want: "not contiguous"},
		{name: "event identity conflict", lines: []string{
			string(mustJSONRecord(t, makeEvent(1))),
			string(mustJSONRecord(t, tracepkg.TraceEvent{
				EventID: "evt-1", RunID: "other-run", RunbookID: "runbook", Sequence: 1,
				Timestamp: "2026-04-23T10:20:30.123456Z", Kind: tracepkg.EventKindStepCompleted,
				Payload: json.RawMessage(`{"ok":false}`),
			})),
		}, want: "conflicts"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(makeWorkDir(t), "strict.jsonl")
			if err := os.WriteFile(path, []byte(strings.Join(test.lines, "\n")+"\n"), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			if _, err := NewJSONLReader(path).ReadAllStrict(context.Background()); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ReadAllStrict error = %v, want %q", err, test.want)
			}
		})
	}
}

func mustJSONRecord(t *testing.T, event tracepkg.TraceEvent) []byte {
	t.Helper()
	encoded, err := json.Marshal(jsonlRecord{
		Seq: event.Sequence, TS: event.Timestamp, Kind: event.Kind, RunID: event.RunID,
		RunbookID: event.RunbookID, EventID: event.EventID, Payload: event.Payload, Signature: event.Signature,
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return encoded
}

func TestJSONLReader_FilterByKind(t *testing.T) {
	dir := makeWorkDir(t)
	path := filepath.Join(dir, "trace.jsonl")

	writer, err := NewJSONLWriter(path)
	if err != nil {
		t.Fatalf("NewJSONLWriter: %v", err)
	}
	start := makeEvent(1)
	start.Kind = tracepkg.EventKindStepStarted
	if err := writer.Append(start); err != nil {
		t.Fatalf("Append: %v", err)
	}
	done := makeEvent(2)
	done.Kind = tracepkg.EventKindStepCompleted
	if err := writer.Append(done); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reader := NewJSONLReader(path)
	events, err := reader.ReadFiltered(context.Background(), tracepkg.TraceFilter{
		Kinds: []tracepkg.EventKind{tracepkg.EventKindStepCompleted},
	})
	if err != nil {
		t.Fatalf("ReadFiltered: %v", err)
	}
	if len(events) != 1 || events[0].Kind != tracepkg.EventKindStepCompleted {
		t.Fatalf("unexpected events: %#v", events)
	}
}

func TestJSONLReader_FilterByStepID(t *testing.T) {
	dir := makeWorkDir(t)
	path := filepath.Join(dir, "trace.jsonl")

	writer, err := NewJSONLWriter(path)
	if err != nil {
		t.Fatalf("NewJSONLWriter: %v", err)
	}
	payloadA, _ := json.Marshal(map[string]any{"step_id": "step-a"})
	payloadB, _ := json.Marshal(map[string]any{"step_id": "step-b"})
	eventA := tracepkg.TraceEvent{
		EventID:   "evt-a",
		RunID:     "run",
		RunbookID: "rb",
		Timestamp: "2026-04-23T10:20:30.123456Z",
		Kind:      tracepkg.EventKindStepCompleted,
		Sequence:  1,
		Payload:   payloadA,
	}
	eventB := tracepkg.TraceEvent{
		EventID:   "evt-b",
		RunID:     "run",
		RunbookID: "rb",
		Timestamp: "2026-04-23T10:20:30.123456Z",
		Kind:      tracepkg.EventKindStepCompleted,
		Sequence:  2,
		Payload:   payloadB,
	}
	if err := writer.Append(eventA); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := writer.Append(eventB); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reader := NewJSONLReader(path)
	events, err := reader.ReadFiltered(context.Background(), tracepkg.TraceFilter{
		Kinds:  []tracepkg.EventKind{tracepkg.EventKindStepCompleted},
		StepID: "step-b",
	})
	if err != nil {
		t.Fatalf("ReadFiltered: %v", err)
	}
	if len(events) != 1 || events[0].Sequence != 2 {
		t.Fatalf("unexpected events: %#v", events)
	}
}

func TestJSONLReader_EmptyFile(t *testing.T) {
	dir := makeWorkDir(t)
	path := filepath.Join(dir, "trace.jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	reader := NewJSONLReader(path)
	events, err := reader.ReadAll(context.Background())
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected no events, got %d", len(events))
	}
}

func TestJSONLReader_CancelledContext(t *testing.T) {
	dir := makeWorkDir(t)
	path := filepath.Join(dir, "trace.jsonl")

	writer, err := NewJSONLWriter(path)
	if err != nil {
		t.Fatalf("NewJSONLWriter: %v", err)
	}
	if err := writer.Append(makeEvent(1)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	reader := NewJSONLReader(path)
	_, err = reader.ReadAll(ctx)
	if err == nil {
		t.Fatalf("expected cancellation error")
	}
}
