package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/internal/debugprotect"
	"github.com/ormasoftchile/yawr/runtime/internal/executor"
	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	enginepkg "github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func planInvocation(plan *enginepkg.ExecutionPlan) *schema.RunbookInvocation {
	invocation := &schema.RunbookInvocation{Bindings: plan.Bindings, Outputs: plan.Outputs}
	for _, step := range plan.Steps {
		if step.Depth == 0 && step.Kind == "results" {
			invocation.Results = true
		}

	}
	if len(invocation.Bindings) == 0 && !invocation.Results {
		return nil
	}
	return invocation
}

// ValidateResultsDelivery applies accumulated invocation protection without
// exporting secret state to an HTTP adapter.
func (h *runHandle) ValidateResultsDelivery(body json.RawMessage) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return debugprotect.ValidateHandoffJSON(h.effectiveDebugProtection(h.debugProtection), body)
}

func initializeScope(invocation *schema.RunbookInvocation, vars map[string]any) (*enginepkg.BindingScopeState, map[string]any, error) {
	if invocation == nil || len(invocation.Bindings) == 0 && !invocation.Results {
		return nil, vars, nil
	}
	initialized, err := executor.InitializeBindings(invocation.Bindings, vars)
	if err != nil {
		return nil, nil, err
	}
	return &enginepkg.BindingScopeState{
		Initialized: true, DeclarationDigest: enginepkg.InvocationDigest(invocation),
		Invocation: schema.CloneInvocation(invocation),
	}, initialized, nil
}

func (h *runHandle) initializeRootBindings(ctx context.Context) error {
	if h.run.BindingScope != nil {
		return nil
	}
	invocation := planInvocation(h.run.Plan)
	if invocation == nil {
		return nil
	}
	scope, vars, err := initializeScope(invocation, h.run.Vars)
	if err != nil {
		return typedErrorAt(err, enginepkg.ResultsOrigin{NodeID: "bindings", Invocation: 1})
	}
	previous := h.run.Vars
	h.run.BindingScope, h.run.Vars = scope, vars
	if _, err := h.persistCompletedCheckpoint(ctx); err != nil {
		if errors.Is(err, enginepkg.ErrCheckpointCommit) {
			h.run.BindingScope, h.run.Vars = nil, previous
		}
		return err
	}
	return nil
}

func requiredResultFailed(result *enginepkg.StepResult) bool {
	if result == nil {
		return false
	}
	return result.RequiredFailure || result.Status == enginepkg.StepStatusFailed ||
		result.Status == enginepkg.StepStatusDenied || result.Status == enginepkg.StepStatusIndeterminate ||
		result.Status == enginepkg.StepStatusWaiting || result.Status == enginepkg.StepStatusRunning ||
		result.Status == enginepkg.StepStatusPending
}

func (h *runHandle) preparePublication(ctx context.Context, result *enginepkg.StepResult, frame *enginepkg.ExecutionFrameState) error {
	if result.Results == nil {
		return nil
	}
	if frame == nil && h.engine.cfg.TransitionValidator != nil {
		if err := h.engine.cfg.TransitionValidator.ValidateCompletion(ctx); err != nil {
			return err
		}
	}
	scope := h.run.BindingScope
	origin := enginepkg.ResultsOrigin{NodeID: enginepkg.DebugNodeID(h.debugCallPath, result.StepID), Invocation: 1}
	prior := h.run.StepResults
	if frame != nil {
		scope = frame.BindingScope
		prior = frame.Results
		origin = enginepkg.ResultsOrigin{NodeID: enginepkg.DebugNodeID(frame.CallPath, result.StepID), FrameID: frame.FrameID, Invocation: frame.Invocation}
	}
	if scope == nil || !scope.Initialized || !scope.Invocation.Results {
		return errors.New("results: declaring invocation is not initialized")
	}
	if frame != nil && (scope.FrameID != frame.FrameID || frame.NextStepIndex != frame.StepCount-1) {
		return errors.New("results: publication must be the last operation of its own invocation")
	}
	for _, settled := range prior {
		if requiredResultFailed(settled) {
			return errors.New("results: required work is not successfully settled")
		}
	}
	// Descendant failures cannot disappear behind a tolerated container result.
	for _, child := range h.run.ExecutionFrames {
		if frame != nil && !descendsFrom(h.run.ExecutionFrames, child, frame.FrameID) {
			continue
		}
		if child.Status != enginepkg.ExecutionFrameStatusCompleted {
			return errors.New("results: required descendant scope is not successfully settled")
		}
		for _, settled := range child.Results {
			if requiredResultFailed(settled) {
				return errors.New("results: required descendant work is not successfully settled")
			}
		}
	}
	if len(h.run.Interactions) != 0 {
		for _, interaction := range h.run.Interactions {
			if interaction != nil {
				return errors.New("results: interaction has not settled")
			}
		}
	}
	digest := ""
	if store, ok := h.store.(interface{ PlanDigest(string) (string, bool) }); ok {
		digest, _ = store.PlanDigest(h.run.ID)
	}
	if digest == "" {
		snapshot, err := plansnapshot.FromExecutionPlan(h.run.Plan)
		if err != nil {
			return err
		}
		digest = snapshot.SnapshotDigest
	}
	record, err := enginepkg.CloneRunResults(result.Results)
	if err != nil {
		return err
	}
	record.PlanSnapshotDigest, record.Origin = digest, origin
	record.CheckpointSequence = h.run.CheckpointSequence + 1
	identity, _ := json.Marshal(struct {
		RunID  string                  `json:"run_id"`
		Plan   string                  `json:"plan_snapshot_digest"`
		Origin enginepkg.ResultsOrigin `json:"origin"`
	}{h.run.ID, digest, origin})
	record.PublicationID = enginepkg.InteractionPayloadDigest(identity)
	if err := record.Seal(); err != nil {
		return err
	}
	body, err := enginepkg.CanonicalResultsJSON(record, true)
	if err != nil {
		return err
	}
	protection := enginepkg.MergeDebugProtection(h.effectiveDebugProtection(h.debugProtection), debugprotect.ProtectionFromContext(ctx))
	if err := debugprotect.ValidateHandoffJSON(protection, body); err != nil {
		return errors.New("results: canonical publication contains protected content")
	}
	if err := validatePublicationDeclarations(record, scope); err != nil {
		return err
	}
	result.Results = record
	return nil
}

func descendsFrom(frames map[string]*enginepkg.ExecutionFrameState, child *enginepkg.ExecutionFrameState, parent string) bool {
	for child != nil && child.ParentFrameID != "" {
		if child.ParentFrameID == parent {
			return true
		}
		child = frames[child.ParentFrameID]
	}
	return false
}

func validatePublicationDeclarations(record *enginepkg.RunResults, scope *enginepkg.BindingScopeState) error {
	if err := record.Validate(); err != nil {
		return err
	}
	for name, value := range record.Outputs {
		declaration := scope.Invocation.Outputs[name]
		if declaration == nil || declaration.Type != value.Type {
			return fmt.Errorf("results.outputs.%s: declaration mismatch", name)
		}
		if err := executor.ValidateStrictValue(value.Value, declaration.Type, declaration.Enum); err != nil {
			return fmt.Errorf("results.outputs.%s: %w", name, err)
		}
	}
	for name, declaration := range scope.Invocation.Outputs {
		if _, exists := record.Outputs[name]; !exists && declaration != nil && !declaration.Optional {
			return fmt.Errorf("results.outputs.%s: required value missing", name)
		}
	}
	return nil
}

func cloneResults(record *enginepkg.RunResults) *enginepkg.RunResults {
	copy, _ := enginepkg.CloneRunResults(record)
	return copy
}

func lastTopLevelStep(plan *enginepkg.ExecutionPlan) int {
	for index := len(plan.Steps) - 1; index >= 0; index-- {
		if plan.Steps[index].Depth == 0 {
			return index
		}
	}
	return -1
}

func publicationMetadata(record *enginepkg.RunResults) map[string]any {
	return map[string]any{"schema_version": record.SchemaVersion, "publication_id": record.PublicationID,
		"digest": record.Digest, "origin": record.Origin, "checkpoint_sequence": record.CheckpointSequence}
}

func (h *runHandle) frameOwnerSettled(frame *enginepkg.ExecutionFrameState) bool {
	if frame.ParentFrameID == "" {
		return h.run.StepResults[frame.ParentStepID] != nil
	}

	parent := h.run.ExecutionFrames[frame.ParentFrameID]
	if parent == nil {
		return true
	}
	for _, result := range parent.Results {
		if result != nil && result.StepID == frame.ParentStepID {
			return true
		}
	}
	return false
}

func (h *runHandle) hasUnreturnedPublication() bool {
	for _, frame := range h.run.ExecutionFrames {
		if frame != nil && frame.RunResults != nil && !h.frameOwnerSettled(frame) {
			return true
		}
	}
	return false
}

func validateRestoredTypedState(plan *enginepkg.ExecutionPlan, state enginepkg.RunState) error {
	expected := planInvocation(plan)
	if expected != nil && state.BindingScope == nil && state.Status == enginepkg.RunStatusPending && state.CurrentStepIndex < 0 &&
		len(state.StepResults) == 0 && len(state.ExecutionFrames) == 0 && len(state.Dispatches) == 0 && state.Results == nil {
		return nil
	}
	if expected != nil {
		if state.BindingScope == nil || !state.BindingScope.Initialized || state.BindingScope.FrameID != "" ||
			state.BindingScope.DeclarationDigest != enginepkg.InvocationDigest(expected) {
			return errors.New("root invocation initialized declaration differs from frozen plan")
		}
	}
	if state.Results != nil {
		if expected == nil || !expected.Results {
			return errors.New("root publication has no frozen declaration")
		}
		if err := validatePublicationDeclarations(state.Results, state.BindingScope); err != nil {
			return err
		}
		last := lastTopLevelStep(plan)
		if last < 0 || plan.Steps[last].Kind != "results" ||
			state.Results.Origin.NodeID != enginepkg.DebugNodeID(nil, plan.Steps[last].ID) ||
			state.Results.Origin.FrameID != "" || state.Results.Origin.Invocation != 1 {
			return errors.New("root publication origin differs from frozen Results step")
		}
	}
	for _, frame := range state.ExecutionFrames {
		if frame != nil && frame.RunResults != nil {
			if frame.BindingScope == nil || frame.BindingScope.FrameID != frame.FrameID {
				return errors.New("frame publication scope mismatch")
			}
			if err := validatePublicationDeclarations(frame.RunResults, frame.BindingScope); err != nil {
				return err
			}
		}
	}
	return nil
}
