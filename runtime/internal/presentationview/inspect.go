package presentationview

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/internal/sessioncoordinator"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/presentation"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/runstate"
)

type eventReader interface {
	PresentationEvents(context.Context, string, int64) ([]engine.Event, error)
}

func Inspect(ctx context.Context, store engine.DurableRunStore, runID string) (*graphdoc.Document, error) {
	plan, err := store.LoadPlan(ctx, runID)
	if err != nil {
		return nil, err
	}
	digest, ok := store.PlanDigest(runID)
	if !ok || digest == "" {
		return nil, errors.New("inspection snapshot digest unavailable")
	}
	state, err := store.LoadState(ctx, runID)
	if err != nil {
		return nil, err
	}
	if state.PlanSnapshotDigest != digest {
		return nil, errors.New("inspection mixed checkpoint generation")
	}
	doc, err := sessioncoordinator.InspectionDocument(plan, digest, state)
	if err != nil {
		return nil, err
	}
	projection := &graphdoc.PresentationState{RunID: runID, PlanSnapshotDigest: digest, CheckpointSequence: uint64(state.CheckpointSequence), Occurrences: []graphdoc.PresentationOccurrence{}}
	events := []engine.Event{}
	if reader, ok := store.(eventReader); ok {
		events, err = reader.PresentationEvents(ctx, runID, state.CommittedTraceSequence)
		if err != nil {
			return nil, err
		}
	}
	seen := map[string]bool{}
	events = append(events, state.PendingTraceEvents...)
	details := map[string]*graphdoc.StepDetails{}
	for _, node := range doc.Nodes {
		id := node.ID
		if node.QualifiedID != "" {
			id = node.QualifiedID
		}
		details[id] = node.Details
	}
	for _, event := range events {
		if event.EventID == "" || seen[event.EventID] || event.Sequence > state.CommittedTraceSequence {
			continue
		}
		seen[event.EventID] = true
		if event.Kind != "step/completed" && event.Kind != "step/failed" {
			continue
		}
		p := runstate.PreviewEventPayload(event.Payload)
		var envelope presentation.Envelope
		data, err := json.Marshal(p["code_presentation"])
		if err != nil || json.Unmarshal(data, &envelope) != nil || envelope.Version != 1 || envelope.Origin != "frozen" || envelope.PlanSnapshotDigest != digest {
			continue
		}
		nodeID, _ := p["qualified_node_id"].(string)
		if nodeID == "" {
			continue
		}
		identity := map[string]any{"qualified_node_id": nodeID, "event_id": event.EventID}
		validIdentity := true
		for _, key := range []string{"frame_id", "frame_step_index", "invocation", "retry_attempt", "occurrence_sequence", "dispatch_occurrence_id"} {
			if value, ok := p[key]; ok {
				if key != "frame_id" && key != "dispatch_occurrence_id" {
					data, _ := json.Marshal(value)
					var number float64
					if json.Unmarshal(data, &number) != nil || number < 0 || number > 9007199254740991 || math.Trunc(number) != number {
						validIdentity = false
						break
					}
				}
				identity[key] = value
			}
		}
		if !validIdentity {
			continue
		}
		d := &graphdoc.StepDetails{Kind: "tool", Tool: envelope.ToolID, Action: envelope.Action, CodePresentation: &envelope}
		if authored := details[nodeID]; authored != nil && authored.Tool == envelope.ToolID && authored.Action == envelope.Action {
			copy := *authored
			copy.CodePresentation = &envelope
			d = &copy
		}
		output := map[string]any{}
		statuses := map[string]string{}
		raw, _ := p["output"].(map[string]any)
		statusBytes, _ := json.Marshal(p["output_value_status"])
		var retained map[string]string
		json.Unmarshal(statusBytes, &retained)
		for _, field := range envelope.Outputs {
			if field.Presentation == nil {
				continue
			}
			status := "unavailable"
			if s, ok := retained[field.Name]; ok {
				switch s {
				case "absent", "redacted", "unavailable":
					status = s
				case "available", "truncated":
					if value, ok := raw[field.Name].(string); ok && !strings.Contains(value, "<redacted>") && field.ValueType == "string" {
						status = s
						output[field.Name] = value
					}
				}
			}
			statuses[field.Name] = status
		}
		projection.Occurrences = append(projection.Occurrences, graphdoc.PresentationOccurrence{Identity: identity, Details: d, Output: output, OutputValueStatus: statuses})
	}
	rechecked, err := store.LoadState(ctx, runID)
	if err != nil {
		return nil, err
	}
	if rechecked.CheckpointSequence != state.CheckpointSequence || rechecked.PlanSnapshotDigest != digest {
		return nil, errors.New("inspection checkpoint changed")
	}
	doc.PresentationState = projection
	return doc, nil
}
