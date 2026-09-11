package resume

import (
	"context"
	"errors"
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

var ErrCursorUnsupported = errors.New("resume: execution cursor is unsupported")

// ResumeFromTrace loads checkpoint state and rebuilds run state for resumption.
func ResumeFromTrace(ctx context.Context, store engine.RunStore, runID string, plan *engine.ExecutionPlan, opts engine.RunOptions) (*engine.Run, *ResumeContext, error) {
	if store == nil {
		return nil, nil, errors.New("resume: RunStore is required")
	}
	if plan == nil {
		return nil, nil, errors.New("resume: execution plan is required")
	}
	state, err := store.LoadState(ctx, runID)
	if err != nil {
		return nil, nil, err
	}
	return ResumeFromState(ctx, store, runID, plan, state, opts)
}

// ResumeFromState rebuilds a run from one checkpoint snapshot that the caller
// has already validated. It never reloads state and therefore cannot cross a
// safety boundary using a different checkpoint than the one that was checked.
func ResumeFromState(
	ctx context.Context,
	store engine.RunStore,
	runID string,
	plan *engine.ExecutionPlan,
	state engine.RunState,
	opts engine.RunOptions,
) (*engine.Run, *ResumeContext, error) {
	if store == nil {
		return nil, nil, errors.New("resume: RunStore is required")
	}
	if plan == nil {
		return nil, nil, errors.New("resume: execution plan is required")
	}
	if isTerminalState(state.Status) {
		return nil, nil, errors.New("resume: run already completed")
	}
	rc, err := ScanTrace(ctx, store, runID, state)
	if err != nil {
		return nil, nil, err
	}
	run, err := RebuildRun(runID, plan, state, rc, opts)
	if err != nil {
		return nil, nil, err
	}
	return run, rc, nil
}

// RebuildRun reconstructs a Run from checkpoint state and trace context.
func RebuildRun(
	runID string,
	plan *engine.ExecutionPlan,
	state engine.RunState,
	rc *ResumeContext,
	opts engine.RunOptions,
) (*engine.Run, error) {
	currentStepIndex, err := resumeStepIndex(plan, state)
	if err != nil {
		return nil, err
	}
	persistedMode := state.Mode
	if persistedMode == "" {
		persistedMode = engine.RunModeReal
	}
	requestedMode := opts.Mode
	if requestedMode == "" {
		requestedMode = persistedMode
	}
	if requestedMode != persistedMode &&
		!(persistedMode == engine.RunModeRouteTest && requestedMode == engine.RunModeReal) {
		return nil, fmt.Errorf("resume: requested mode %s conflicts with persisted mode %s", requestedMode, persistedMode)
	}
	run := &engine.Run{
		ID:                     runID,
		BindingScope:           engine.CloneBindingScope(state.BindingScope),
		Status:                 engine.RunStatusRunning,
		Plan:                   plan,
		Vars:                   make(map[string]any),
		StepResults:            make(map[string]*engine.StepResult),
		Interactions:           make(map[string]*engine.InteractionState),
		Dispatches:             make(map[string]*engine.DispatchState),
		ExecutionFrames:        make(map[string]*engine.ExecutionFrameState),
		DynamicIncludes:        make(map[string]*engine.DynamicIncludeResolutionState),
		PendingHandoff:         cloneHandoffRequest(state.PendingHandoff),
		CurrentStepIndex:       currentStepIndex,
		CheckpointSequence:     state.CheckpointSequence,
		CommittedTraceSequence: state.CommittedTraceSequence,
		PendingTraceEvents:     append([]engine.Event(nil), state.PendingTraceEvents...),
		WriterEpoch:            state.WriterEpoch,
		StartedAt:              state.StartedAt,
		Actor:                  opts.Actor,
		Mode:                   requestedMode,
	}
	run.Results, err = state.CloneResults()
	if err != nil {
		return nil, err
	}
	if run.Results != nil {
		run.Status, run.CompletedAt = state.Status, state.CompletedAt
	}
	if rc != nil {
		run.Sequence = rc.LastSeq
	}
	if state.CommittedTraceSequence > run.Sequence {
		run.Sequence = state.CommittedTraceSequence
	}

	for k, v := range state.Vars {
		run.Vars[k] = v
	}
	for stepID, result := range state.StepResults {
		run.StepResults[stepID] = result
	}
	for turnID, interaction := range state.Interactions {
		run.Interactions[turnID] = interaction
	}
	for occurrenceID, dispatch := range state.Dispatches {
		run.Dispatches[occurrenceID] = dispatch
	}
	for frameID, frame := range state.ExecutionFrames {
		run.ExecutionFrames[frameID] = frame
	}
	for resolutionID, resolution := range state.DynamicIncludes {
		run.DynamicIncludes[resolutionID] = resolution
	}

	if rc != nil && state.CursorSet == nil {
		for stepID := range rc.CompletedStepIDs {
			if _, restored := run.StepResults[stepID]; restored {
				continue
			}
			run.StepResults[stepID] = &engine.StepResult{
				StepID: stepID,
				Status: engine.StepStatusCompleted,
			}
		}
	}

	return run, nil
}

func cloneHandoffRequest(request *engine.HandoffRequest) *engine.HandoffRequest {
	if request == nil {
		return nil
	}
	cloned := *request
	cloned.CallPath = append([]engine.DebugCallFrame(nil), request.CallPath...)
	cloned.StructuralPath = append([]schema.DynamicIncludeFrameIdentity(nil), request.StructuralPath...)
	cloned.Context = cloneHandoffMap(request.Context)
	cloned.Facts = cloneHandoffMap(request.Facts)
	cloned.ContextBindings = cloneHandoffBindings(request.ContextBindings)
	cloned.FactBindings = cloneHandoffBindings(request.FactBindings)
	return &cloned
}

func cloneHandoffBindings(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func cloneHandoffMap(values map[string]any) map[string]any {
	if values == nil {
		return nil
	}
	cloned := make(map[string]any, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func resumeStepIndex(plan *engine.ExecutionPlan, state engine.RunState) (int, error) {
	if state.CursorSet == nil {
		if state.CheckpointSequence > 0 {
			return 0, fmt.Errorf("%w: sequenced checkpoint has no cursor", ErrCursorUnsupported)
		}
		return findStepIndex(plan, state.CurrentStep), nil
	}
	if state.CursorSet.SchemaVersion != engine.ExecutionCursorSchemaV1 {
		return 0, fmt.Errorf("%w: schema version %q", ErrCursorUnsupported, state.CursorSet.SchemaVersion)
	}
	if len(state.CursorSet.Cursors) == 0 {
		return 0, fmt.Errorf("%w: cursor count %d", ErrCursorUnsupported, len(state.CursorSet.Cursors))
	}
	if len(state.CursorSet.Cursors) > 1 || len(state.CursorSet.Cursors[0].CallPath) > 0 {
		return resumeStepIndexFromExecutionFrames(plan, state)
	}
	if len(state.CursorSet.Cursors) != 1 {
		return 0, fmt.Errorf("%w: cursor count %d", ErrCursorUnsupported, len(state.CursorSet.Cursors))
	}
	cursor := state.CursorSet.Cursors[0]
	if cursor.Phase != engine.ExecutionPhaseBefore || len(cursor.CallPath) != 0 || cursor.Invocation < 1 || cursor.RetryAttempt != 1 {
		return 0, fmt.Errorf("%w: cursor boundary %#v", ErrCursorUnsupported, cursor)
	}
	if cursor.StepIndex < 0 || cursor.StepIndex > len(plan.Steps) {
		return 0, fmt.Errorf("%w: step index %d", ErrCursorUnsupported, cursor.StepIndex)
	}
	if cursor.AtEnd {
		if cursor.StepIndex != len(plan.Steps) || cursor.StepID != "" || cursor.QualifiedNodeID != "" {
			return 0, fmt.Errorf("%w: invalid end cursor %#v", ErrCursorUnsupported, cursor)
		}
		return len(plan.Steps) - 1, nil
	}
	if cursor.StepIndex >= len(plan.Steps) {
		return 0, fmt.Errorf("%w: nonterminal step index %d", ErrCursorUnsupported, cursor.StepIndex)
	}
	step := plan.Steps[cursor.StepIndex]
	if step.Depth != 0 {
		return 0, fmt.Errorf("%w: cursor references nested plan step %q", ErrCursorUnsupported, step.ID)
	}
	if step.ID != cursor.StepID || engine.DebugNodeID(nil, step.ID) != cursor.QualifiedNodeID {
		return 0, fmt.Errorf("%w: cursor does not match plan step %d", ErrCursorUnsupported, cursor.StepIndex)
	}
	return cursor.StepIndex - 1, nil
}

func resumeStepIndexFromExecutionFrames(plan *engine.ExecutionPlan, state engine.RunState) (int, error) {
	if plan == nil || state.CursorSet == nil {
		return 0, fmt.Errorf("%w: execution frame cursor has no plan", ErrCursorUnsupported)
	}
	rootIndex := -1
	rootStepID := ""
	seenFrames := make(map[string]bool, len(state.CursorSet.Cursors))
	for _, cursor := range state.CursorSet.Cursors {
		if cursor.AtEnd || cursor.Phase != engine.ExecutionPhaseBefore || cursor.FrameID == "" ||
			len(cursor.CallPath) == 0 || cursor.Invocation < 1 || cursor.RetryAttempt < 1 || seenFrames[cursor.FrameID] {
			return 0, fmt.Errorf("%w: cursor boundary %#v", ErrCursorUnsupported, cursor)
		}
		seenFrames[cursor.FrameID] = true
		if cursor.StepIndex < 0 || cursor.StepIndex >= len(plan.Steps) {
			return 0, fmt.Errorf("%w: root step index %d", ErrCursorUnsupported, cursor.StepIndex)
		}
		frame := state.ExecutionFrames[cursor.FrameID]
		if frame == nil {
			return 0, fmt.Errorf("%w: cursor has no execution frame", ErrCursorUnsupported)
		}
		candidateRootID := cursor.CallPath[0].StepID
		resumeRootID := candidateRootID
		rootStep := plan.Steps[cursor.StepIndex]
		if rootStep.Depth != 0 || rootStep.ID != candidateRootID {
			ownerFound := false
			for _, step := range plan.Steps {
				if step.Depth == 0 && step.ID == candidateRootID && step.Kind == "compensate" {
					ownerFound = true
					break
				}
			}
			failed := state.StepResults[rootStep.ID]
			if frame.Kind != "compensate" || frame.ParentFrameID != "" || frame.ParentStepID != candidateRootID ||
				!ownerFound || rootStep.Depth != 0 || failed == nil || failed.Status != engine.StepStatusFailed {
				return 0, fmt.Errorf("%w: cursor root does not match plan", ErrCursorUnsupported)
			}
			resumeRootID = rootStep.ID
		}
		if rootStep.Depth != 0 {
			return 0, fmt.Errorf("%w: cursor root does not match plan", ErrCursorUnsupported)
		}
		if rootIndex == -1 {
			rootIndex, rootStepID = cursor.StepIndex, resumeRootID
		} else if cursor.StepIndex != rootIndex || resumeRootID != rootStepID {
			return 0, fmt.Errorf("%w: cursor set spans multiple root steps", ErrCursorUnsupported)
		}
		if frame.Status != engine.ExecutionFrameStatusActive ||
			frame.NextStepIndex < 0 || frame.NextStepIndex >= frame.StepCount || frame.NextStepIndex >= len(frame.StepIDs) ||
			frame.StepIDs[frame.NextStepIndex] != cursor.StepID || frame.BranchLabel != cursor.BranchLabel ||
			frame.IterationIndex != cursor.IterationIndex ||
			!sameDebugCallPath(frame.CallPath, cursor.CallPath) ||
			engine.DebugNodeID(cursor.CallPath, cursor.StepID) != cursor.QualifiedNodeID {
			return 0, fmt.Errorf("%w: cursor does not match active execution frame", ErrCursorUnsupported)
		}
		for _, candidate := range state.ExecutionFrames {
			if candidate != nil && candidate.Status == engine.ExecutionFrameStatusActive && candidate.ParentFrameID == cursor.FrameID {
				return 0, fmt.Errorf("%w: cursor references a non-leaf execution frame", ErrCursorUnsupported)
			}
		}
	}
	if rootIndex < 0 {
		return 0, fmt.Errorf("%w: no frame-backed cursor", ErrCursorUnsupported)
	}
	return rootIndex - 1, nil
}

func sameDebugCallPath(left, right []engine.DebugCallFrame) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func findStepIndex(plan *engine.ExecutionPlan, stepID string) int {
	if plan == nil {
		return -1
	}
	for i, step := range plan.Steps {
		if step.ID == stepID {
			return i
		}
	}
	return -1
}

func isTerminalState(state engine.RunStatus) bool {
	switch state {
	case engine.RunStatusCompleted, engine.RunStatusFailed, engine.RunStatusCancelled:
		return true
	default:
		return false
	}
}
