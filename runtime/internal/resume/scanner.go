package resume

import (
	"context"
	"encoding/json"
	"errors"
	"os"

	internaltrace "github.com/ormasoftchile/yawr/runtime/internal/trace"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	tracepkg "github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

// ResumeContext holds metadata gathered from scanning the trace file.
type ResumeContext struct {
	// LastCheckpointSeq is the sequence number of the last checkpoint event.
	LastCheckpointSeq int64

	// OrphanedToolCalls is a list of tool/invoked events with no matching tool/completed.
	OrphanedToolCalls []string

	// CompletedStepIDs is the set of step IDs that have step/completed events.
	CompletedStepIDs map[string]bool

	// LastSeq is the highest sequence number seen.
	LastSeq int64
}

// TraceScanner scans trace events to build resume context.
type TraceScanner struct {
	reader tracepkg.TraceReader
}

// NewTraceScanner constructs a TraceScanner.
func NewTraceScanner(reader tracepkg.TraceReader) *TraceScanner {
	return &TraceScanner{reader: reader}
}

// Scan reads trace events and produces a ResumeContext.
func (s *TraceScanner) Scan(ctx context.Context) (*ResumeContext, error) {
	if s == nil || s.reader == nil {
		return nil, errors.New("resume: trace reader is required")
	}
	events, err := s.reader.ReadAll(ctx)
	if err != nil {
		return nil, err
	}
	return buildResumeContext(events)
}

// ScanTrace reads the trace file for the given run and builds a ResumeContext.
func ScanTrace(ctx context.Context, store engine.RunStore, runID string, state engine.RunState) (*ResumeContext, error) {
	if store == nil {
		return nil, errors.New("resume: RunStore is required")
	}
	pathProvider, ok := store.(interface{ TracePath(string) string })
	if !ok {
		return nil, errors.New("resume: RunStore does not expose trace path")
	}
	reader := internaltrace.NewJSONLReader(pathProvider.TracePath(runID))
	scanner := NewTraceScanner(reader)
	result, err := scanner.Scan(ctx)
	if errors.Is(err, os.ErrNotExist) && state.Status == engine.RunStatusPending && state.CurrentStepIndex < 0 &&
		state.CheckpointSequence == 0 && state.CommittedTraceSequence == 0 && len(state.StepResults) == 0 && len(state.Dispatches) == 0 && len(state.ExecutionFrames) == 0 {
		return buildResumeContext(nil)
	}
	return result, err
}

func buildResumeContext(events []tracepkg.TraceEvent) (*ResumeContext, error) {
	rc := &ResumeContext{
		CompletedStepIDs: make(map[string]bool),
	}
	invoked := make(map[string]bool)

	for _, ev := range events {
		if ev.Sequence > rc.LastSeq {
			rc.LastSeq = ev.Sequence
		}
		switch ev.Kind {
		case tracepkg.EventKind("checkpoint"):
			rc.LastCheckpointSeq = ev.Sequence
		case tracepkg.EventKindStepCompleted:
			stepID := extractStepID(ev.Payload)
			if stepID != "" {
				rc.CompletedStepIDs[stepID] = true
			}
		case tracepkg.EventKindToolInvoked:
			correlationID := extractCorrelationID(ev.Payload)
			if correlationID != "" {
				invoked[correlationID] = true
			}
		case tracepkg.EventKindToolCompleted:
			correlationID := extractCorrelationID(ev.Payload)
			if correlationID != "" {
				delete(invoked, correlationID)
			}
		}
	}

	for id := range invoked {
		rc.OrphanedToolCalls = append(rc.OrphanedToolCalls, id)
	}

	return rc, nil
}

func extractStepID(payload json.RawMessage) string {
	var data map[string]any
	if err := json.Unmarshal(payload, &data); err != nil {
		return ""
	}
	if stepID, ok := data["step_id"].(string); ok {
		return stepID
	}
	return ""
}

func extractCorrelationID(payload json.RawMessage) string {
	var data map[string]any
	if err := json.Unmarshal(payload, &data); err != nil {
		return ""
	}
	if id, ok := data["correlation_id"].(string); ok {
		return id
	}
	return ""
}
