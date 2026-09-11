package runstore

import (
	"context"
	"encoding/json"
	"errors"
	internaltrace "github.com/ormasoftchile/yawr/runtime/internal/trace"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"os"
)

func (s *DirRunStore) PresentationEvents(ctx context.Context, runID string, sequence int64) ([]engine.Event, error) {
	if err := validateRunID(runID); err != nil {
		return nil, err
	}
	events, err := internaltrace.NewJSONLReader(s.TracePath(runID)).ReadAllStrictBounded(ctx, 64<<20)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := []engine.Event{}
	for _, event := range events {
		if event.RunID != runID {
			return nil, errors.New("presentation trace identity mismatch")
		}
		if event.Sequence > sequence {
			continue
		}
		if event.Kind != "step/completed" && event.Kind != "step/failed" {
			continue
		}
		var payload map[string]any
		if json.Unmarshal(event.Payload, &payload) != nil {
			return nil, errors.New("presentation trace payload invalid")
		}
		out = append(out, engine.Event{EventID: event.EventID, RunID: event.RunID, RunbookID: event.RunbookID, Timestamp: event.Timestamp, Kind: string(event.Kind), Sequence: event.Sequence, Payload: payload})
		if len(out) > 4096 {
			return nil, errors.New("presentation occurrence limit")
		}
	}
	return out, nil
}
