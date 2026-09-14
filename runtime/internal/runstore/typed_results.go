package runstore

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/internal/executor"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
)

const runStateSchemaV3 = "run-state/v3"

// Publications have one canonical stored value; frame/step references do not
// replicate the report into every ancestor's checkpoint envelope.
func typedStateValues(state engine.RunState) map[string]any {
	values := make(map[string]any)
	if state.BindingScope != nil {
		values["scope:root"] = state.BindingScope
	}
	add := func(record *engine.RunResults) {
		if record != nil {
			values["results:"+record.PublicationID] = record
		}
	}
	add(state.Results)
	for id, frame := range state.ExecutionFrames {
		if frame == nil {
			continue
		}
		if frame == nil {
			continue
		}
		if frame.BindingScope != nil {
			values["scope:"+id] = frame.BindingScope
		}
		add(frame.RunResults)
		for _, result := range frame.Results {
			if result != nil {
				add(result.Results)
			}
		}
	}
	for _, result := range state.StepResults {
		if result != nil {
			add(result.Results)
		}
	}
	return values
}

func publicationID(record *engine.RunResults) string {
	if record == nil {
		return ""
	}
	return record.PublicationID
}

func decodeTypedState(value any, destination any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	return decoder.Decode(destination)
}

func restoreTypedState(state *engine.RunState, values map[string]any, rootID string, frameSnapshots map[string]*executionFrameSnapshotV1, stepSnapshots map[string]*stepResultSnapshotV2) error {
	publications := make(map[string]*engine.RunResults)
	for key, value := range values {
		if len(key) < len("results:") || key[:len("results:")] != "results:" {
			continue
		}
		var record engine.RunResults
		if err := decodeTypedState(value, &record); err != nil {
			return err
		}
		if err := record.Validate(); err != nil {
			return err
		}
		if key != "results:"+record.PublicationID || record.PlanSnapshotDigest != state.PlanSnapshotDigest ||
			record.CheckpointSequence > state.CheckpointSequence {
			return fmt.Errorf("runstore: results identity does not match checkpoint")
		}
		publications[record.PublicationID] = &record
	}
	get := func(id string) (*engine.RunResults, error) {
		if id == "" {
			return nil, nil
		}
		record := publications[id]
		if record == nil {
			return nil, fmt.Errorf("runstore: missing publication %q", id)
		}
		return engine.CloneRunResults(record)
	}
	scope := func(key string) (*engine.BindingScopeState, error) {
		value, exists := values[key]
		if !exists {
			return nil, nil
		}
		var scope engine.BindingScopeState
		if err := decodeTypedState(value, &scope); err != nil {
			return nil, err
		}
		if !scope.Initialized || scope.Invocation == nil || scope.DeclarationDigest != engine.InvocationDigest(scope.Invocation) {
			return nil, fmt.Errorf("runstore: initialized declaration digest mismatch")
		}
		return &scope, nil
	}
	var err error
	state.Results, err = get(rootID)
	if err != nil {
		return err
	}
	state.BindingScope, err = scope("scope:root")
	if err != nil {
		return err
	}
	for id, frame := range state.ExecutionFrames {
		frame.BindingScope, err = scope("scope:" + id)
		if err != nil {
			return err
		}
		frame.RunResults, err = get(frameSnapshots[id].RunResultsID)
		if err != nil {
			return err
		}
		for index, result := range frame.Results {
			if result != nil {
				result.Results, err = get(frameSnapshots[id].DurableResults[index].ResultsID)
				if err != nil {
					return err
				}
			}
		}
	}
	for id, result := range state.StepResults {
		if result != nil {
			result.Results, err = get(stepSnapshots[id].ResultsID)
			if err != nil {
				return err
			}
		}
	}
	return validateTypedPublications(*state)
}

func validateTypedPublications(state engine.RunState) error {
	check := func(record *engine.RunResults, scope *engine.BindingScopeState, frame *engine.ExecutionFrameState) error {
		if record == nil {
			return nil
		}
		if err := record.Validate(); err != nil {
			return err
		}
		if scope == nil || !scope.Initialized || scope.Invocation == nil || !scope.Invocation.Results ||
			scope.DeclarationDigest != engine.InvocationDigest(scope.Invocation) {
			return fmt.Errorf("runstore: publication has no initialized declaration")
		}
		identity, _ := json.Marshal(struct {
			RunID  string               `json:"run_id"`
			Plan   string               `json:"plan_snapshot_digest"`
			Origin engine.ResultsOrigin `json:"origin"`
		}{state.RunID, record.PlanSnapshotDigest, record.Origin})
		if engine.InteractionPayloadDigest(identity) != record.PublicationID {
			return fmt.Errorf("runstore: publication identity mismatch")
		}
		if frame == nil {
			if record.Origin.FrameID != "" || record.Origin.Invocation != 1 || state.Status != engine.RunStatusCompleted {
				return fmt.Errorf("runstore: root publication has invalid origin/status")
			}
		} else if record.Origin.FrameID != frame.FrameID || record.Origin.Invocation != frame.Invocation ||
			frame.Status != engine.ExecutionFrameStatusCompleted || frame.StepCount < 1 || len(frame.StepIDs) != frame.StepCount ||
			frame.NextStepIndex != frame.StepCount || !validFramePublicationOrigin(frame, record) {
			return fmt.Errorf("runstore: frame publication origin/cursor mismatch")
		}
		for name, output := range record.Outputs {
			declaration := scope.Invocation.Outputs[name]
			if declaration == nil || declaration.Type != output.Type {
				return fmt.Errorf("runstore: outputs.%s declaration mismatch", name)
			}
			if err := executor.ValidateStrictValue(output.Value, declaration.Type, declaration.Enum); err != nil {
				return fmt.Errorf("runstore: outputs.%s: %w", name, err)
			}
		}
		for name, declaration := range scope.Invocation.Outputs {
			if _, exists := record.Outputs[name]; !exists && declaration != nil && !declaration.Optional {
				return fmt.Errorf("runstore: required output %s missing", name)
			}
		}
		return nil
	}
	if err := check(state.Results, state.BindingScope, nil); err != nil {
		return err
	}
	for _, result := range state.StepResults {
		if result != nil && result.Results != nil && (state.Results == nil || result.Results.Digest != state.Results.Digest ||
			result.Results.Origin.NodeID != engine.DebugNodeID(nil, result.StepID)) {
			return fmt.Errorf("runstore: root step publication reference differs from owner")
		}
	}
	for _, frame := range state.ExecutionFrames {
		if frame != nil {
			if err := check(frame.RunResults, frame.BindingScope, frame); err != nil {
				return err
			}
			for _, result := range frame.Results {
				if result != nil && result.Results != nil && (frame.RunResults == nil || result.Results.Digest != frame.RunResults.Digest ||
					result.Results.Origin.NodeID != engine.DebugNodeID(frame.CallPath, result.StepID)) {
					return fmt.Errorf("runstore: frame step publication reference differs from owner")
				}
			}
		}
	}
	return nil
}

func hasTypedReferences(snapshot runStateSnapshotV1) bool {
	if len(snapshot.TypedState) != 0 || snapshot.ResultsID != "" {
		return true
	}
	for _, result := range snapshot.DurableStepResults {
		if result != nil && (result.ResultsID != "" || result.PublicOutputs != nil || result.RequiredFailure || result.TerminalResults) {
			return true
		}
	}
	for _, frame := range snapshot.DurableExecutionFrames {
		if frame == nil {
			continue
		}
		if frame.RunResultsID != "" {
			return true
		}
		for _, result := range frame.DurableResults {
			if result != nil && (result.ResultsID != "" || result.PublicOutputs != nil || result.RequiredFailure || result.TerminalResults) {
				return true
			}
		}
	}
	for _, resolution := range snapshot.DynamicIncludes {
		if resolution == nil {
			continue
		}
		if len(resolution.Pin.ResolvedBindings) != 0 {
			return true
		}
		for _, output := range resolution.Pin.ResolvedOutputs {
			if output != nil && output.ValueTreePresent {
				return true
			}
		}
	}
	return false
}

func validateTypedPlanOrigin(state engine.RunState, plan *engine.ExecutionPlan) error {
	invocation := engine.ResultsInvocation(plan)
	if state.BindingScope != nil && (state.BindingScope.FrameID != "" ||
		state.BindingScope.DeclarationDigest != engine.InvocationDigest(invocation)) {
		return fmt.Errorf("runstore: root initialized declaration differs from frozen plan")
	}
	if !engine.ValidRootPublicationOrigin(plan, state) {
		return fmt.Errorf("runstore: root result origin differs from frozen plan")
	}

	return validateFrameDeclarations(state, plan)
}

func validFramePublicationOrigin(frame *engine.ExecutionFrameState, record *engine.RunResults) bool {
	for _, result := range frame.Results {
		if result == nil || result.Results == nil || result.Results.Digest != record.Digest ||
			result.Status != engine.StepStatusCompleted ||
			record.Origin.NodeID != engine.DebugNodeID(frame.CallPath, result.StepID) {
			continue
		}
		terminal, _ := result.Output["terminal"].(bool)
		return result.StepID == frame.StepIDs[frame.StepCount-1] || terminal && result.TerminalResults
	}
	return false
}
