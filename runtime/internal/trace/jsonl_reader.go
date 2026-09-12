package trace

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	tracepkg "github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

// JSONLReader reads trace events from a JSONL file.
type JSONLReader struct {
	path string
	mu   sync.Mutex
}

// NewJSONLReader constructs a reader for the given trace file path.
func NewJSONLReader(path string) *JSONLReader {
	return &JSONLReader{path: path}
}

// ReadAll returns all events from the trace file, skipping malformed lines.
func (r *JSONLReader) ReadAll(ctx context.Context) ([]tracepkg.TraceEvent, error) {
	return r.ReadFiltered(ctx, tracepkg.TraceFilter{})
}

func (r *JSONLReader) ReadAllStrict(ctx context.Context) ([]tracepkg.TraceEvent, error) {
	return r.ReadAllStrictBounded(ctx, 0)
}

func (r *JSONLReader) ReadAllStrictBounded(ctx context.Context, maxBytes int64) ([]tracepkg.TraceEvent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	file, err := os.Open(r.path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var events []tracepkg.TraceEvent
	lastSequence := make(map[string]int64)
	eventIDs := make(map[string]string)
	var reader io.Reader = file
	if maxBytes > 0 {
		info, err := file.Stat()
		if err != nil {
			return nil, err
		}
		if info.Size() > maxBytes {
			return nil, fmt.Errorf("trace: size limit exceeded")
		}
		reader = io.LimitReader(file, maxBytes+1)
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNumber := 0
	totalBytes := int64(0)
	for scanner.Scan() {
		totalBytes += int64(len(scanner.Bytes()) + 1)
		if maxBytes > 0 && totalBytes > maxBytes {
			return nil, fmt.Errorf("trace: size limit exceeded")
		}
		lineNumber++
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			return nil, fmt.Errorf("trace: strict JSONL line %d is empty", lineNumber)
		}
		var record jsonlRecord
		if err := json.Unmarshal(line, &record); err != nil {
			return nil, fmt.Errorf("trace: strict JSONL line %d is malformed: %w", lineNumber, err)
		}
		if record.Seq < 1 || record.RunID == "" || record.EventID == "" || record.Kind == "" ||
			record.TS == "" || len(record.Payload) == 0 {
			return nil, fmt.Errorf("trace: strict JSONL line %d has incomplete identity", lineNumber)
		}
		if _, err := time.Parse(time.RFC3339Nano, record.TS); err != nil {
			return nil, fmt.Errorf("trace: strict JSONL line %d has invalid timestamp: %w", lineNumber, err)
		}
		digest, err := recordDigest(record)
		if err != nil {
			return nil, fmt.Errorf("trace: strict JSONL line %d has invalid payload: %w", lineNumber, err)
		}
		if prior, found := eventIDs[record.EventID]; found {
			if prior != digest {
				return nil, fmt.Errorf("trace: event id %q conflicts with existing content", record.EventID)
			}
			return nil, fmt.Errorf("trace: event id %q is duplicated", record.EventID)
		}
		expected := lastSequence[record.RunID] + 1
		if record.Seq != expected {
			return nil, fmt.Errorf(
				"trace: run %q sequence %d is not contiguous after %d", record.RunID, record.Seq, lastSequence[record.RunID],
			)
		}
		eventIDs[record.EventID] = digest
		lastSequence[record.RunID] = record.Seq
		events = append(events, tracepkg.TraceEvent{
			EventID: record.EventID, RunID: record.RunID, RunbookID: record.RunbookID,
			Timestamp: record.TS, Kind: record.Kind, Sequence: record.Seq,
			Payload: record.Payload, Signature: record.Signature,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return events, nil
}

// ReadSince returns events with sequence > afterSeq.
func (r *JSONLReader) ReadSince(ctx context.Context, afterSeq int64) ([]tracepkg.TraceEvent, error) {
	return r.ReadFiltered(ctx, tracepkg.TraceFilter{AfterSeq: afterSeq})
}

// ReadFiltered reads events matching the given filter.
// Malformed JSON lines are silently skipped (crash safety contract).
func (r *JSONLReader) ReadFiltered(ctx context.Context, filter tracepkg.TraceFilter) ([]tracepkg.TraceEvent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	f, err := os.Open(r.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var events []tracepkg.TraceEvent
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		if ctx.Err() != nil {
			return events, ctx.Err()
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var payload struct {
			Seq       int64              `json:"seq"`
			TS        string             `json:"ts"`
			Kind      tracepkg.EventKind `json:"kind"`
			RunID     string             `json:"run_id"`
			RunbookID string             `json:"runbook_id,omitempty"`
			EventID   string             `json:"event_id,omitempty"`
			Payload   json.RawMessage    `json:"payload"`
			Signature string             `json:"sig,omitempty"`
		}
		if err := json.Unmarshal(line, &payload); err != nil {
			continue
		}

		event := tracepkg.TraceEvent{
			EventID:   payload.EventID,
			RunID:     payload.RunID,
			RunbookID: payload.RunbookID,
			Timestamp: payload.TS,
			Kind:      payload.Kind,
			Sequence:  payload.Seq,
			Payload:   payload.Payload,
			Signature: payload.Signature,
		}

		if !filter.Matches(event) {
			continue
		}

		if filter.StepID != "" {
			var fields map[string]any
			if err := json.Unmarshal(event.Payload, &fields); err != nil {
				continue
			}
			if stepID, _ := fields["step_id"].(string); stepID != filter.StepID {
				continue
			}
		}

		events = append(events, event)
	}

	return events, scanner.Err()
}
