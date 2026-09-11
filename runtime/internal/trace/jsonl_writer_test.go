package trace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	tracepkg "github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

var errTraceSync = errors.New("trace sync failed")

func TestJSONLWriter_WritesLines(t *testing.T) {
	dir := makeWorkDir(t)
	path := filepath.Join(dir, "trace.jsonl")

	writer, err := NewJSONLWriter(path)
	if err != nil {
		t.Fatalf("NewJSONLWriter: %v", err)
	}
	defer writer.Close()

	if err := writer.Append(makeEvent(1)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := writer.Append(makeEvent(2)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	lines := readLines(t, path)
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}
	for _, line := range lines {
		var payload map[string]any
		if err := json.Unmarshal([]byte(line), &payload); err != nil {
			t.Fatalf("invalid JSON line: %v", err)
		}
	}
}

func TestJSONLWriter_ThreadSafe(t *testing.T) {
	dir := makeWorkDir(t)
	path := filepath.Join(dir, "trace.jsonl")

	writer, err := NewJSONLWriter(path)
	if err != nil {
		t.Fatalf("NewJSONLWriter: %v", err)
	}
	defer writer.Close()

	const total = 50
	var wg sync.WaitGroup
	wg.Add(total)
	for i := 0; i < total; i++ {
		i := i
		go func() {
			defer wg.Done()
			_ = writer.Append(makeEvent(int64(i + 1)))
		}()
	}
	wg.Wait()
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	lines := readLines(t, path)
	if len(lines) != total {
		t.Fatalf("expected %d lines, got %d", total, len(lines))
	}
}

func TestJSONLWriter_AutoCreatesDir(t *testing.T) {
	dir := makeWorkDir(t)
	nested := filepath.Join(dir, "nested", "trace.jsonl")

	writer, err := NewJSONLWriter(nested)
	if err != nil {
		t.Fatalf("NewJSONLWriter: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(nested)); err != nil {
		t.Fatalf("expected directory to exist: %v", err)
	}
}

func TestJSONLWriter_CloseFlushes(t *testing.T) {
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
	if err := writer.Append(makeEvent(2)); err == nil {
		t.Fatalf("expected error after Close")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(contents) == 0 {
		t.Fatalf("expected data in trace file")
	}
}

func TestJSONLWriter_AppendFailsWhenDurableSyncFails(t *testing.T) {
	path := filepath.Join(makeWorkDir(t), "trace.jsonl")
	writer, err := NewJSONLWriter(path)
	if err != nil {
		t.Fatalf("NewJSONLWriter: %v", err)
	}
	writer.syncFile = func() error { return errTraceSync }
	if err := writer.Append(makeEvent(1)); !errors.Is(err, errTraceSync) {
		t.Fatalf("Append error = %v, want sync failure", err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile after sync failure: %v", err)
	}
	if len(contents) != 0 {
		t.Fatalf("failed append left %d bytes in trace", len(contents))
	}
	writer.syncFile = writer.file.Sync
	if err := writer.Append(makeEvent(1)); err != nil {
		t.Fatalf("retry Append: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestJSONLWriter_TruncatesIncompleteTailBeforeAppend(t *testing.T) {
	path := filepath.Join(makeWorkDir(t), "trace.jsonl")
	if err := os.WriteFile(path, []byte(`{"seq":1`), 0o600); err != nil {
		t.Fatalf("write incomplete tail: %v", err)
	}
	writer, err := NewJSONLWriter(path)
	if err != nil {
		t.Fatalf("NewJSONLWriter: %v", err)
	}
	if writer.LastSequence("run") != 0 {
		t.Fatalf("LastSequence before repair append = %d", writer.LastSequence("run"))
	}
	if err := writer.Append(makeEvent(1)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, err := NewJSONLReader(path).ReadAll(context.Background())
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(events) != 1 || events[0].Sequence != 1 || events[0].EventID != "evt-1" {
		t.Fatalf("events after tail repair = %#v", events)
	}
}

func TestJSONLWriter_IdempotentReplayRejectsIdentityConflicts(t *testing.T) {
	path := filepath.Join(makeWorkDir(t), "trace.jsonl")
	event := makeEvent(1)
	writer, err := NewJSONLWriter(path)
	if err != nil {
		t.Fatalf("NewJSONLWriter: %v", err)
	}
	if err := writer.Append(event); err != nil {
		t.Fatalf("first Append: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	writer, err = NewJSONLWriter(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer writer.Close()
	if err := writer.Append(event); err != nil {
		t.Fatalf("idempotent Append: %v", err)
	}
	changedPayload := event
	changedPayload.Payload = json.RawMessage(`{"ok":false}`)
	if err := writer.Append(changedPayload); err == nil {
		t.Fatal("writer accepted conflicting content for an existing event ID")
	}
	changedID := event
	changedID.EventID = "different-event"
	if err := writer.Append(changedID); err == nil {
		t.Fatal("writer accepted a different event at an existing run sequence")
	}
	if lines := readLines(t, path); len(lines) != 1 {
		t.Fatalf("idempotent replay wrote %d lines, want 1", len(lines))
	}
}

func makeWorkDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(".", "trace-test-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	raw := strings.Split(string(contents), "\n")
	lines := raw[:0]
	for _, line := range raw {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func makeEvent(seq int64) tracepkg.TraceEvent {
	return tracepkg.TraceEvent{
		EventID:   fmt.Sprintf("evt-%d", seq),
		RunID:     "run",
		RunbookID: "runbook",
		Timestamp: "2026-04-23T10:20:30.123456Z",
		Kind:      tracepkg.EventKindStepStarted,
		Sequence:  seq,
		Payload:   json.RawMessage(`{"ok":true}`),
	}
}
