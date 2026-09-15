package engine

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

const DynamicIncludeResolutionStateSchemaV1 = "yawr.dynamic-include-resolution/v1"
const DynamicIncludeResolutionStateSchemaV2 = "yawr.dynamic-include-resolution/v2"

// ValidateDynamicIncludeResolutionVersion prevents a legacy state envelope
// from silently acquiring scope-aware pin meaning.
func ValidateDynamicIncludeResolutionVersion(state DynamicIncludeResolutionState) error {
	switch state.SchemaVersion {
	case DynamicIncludeResolutionStateSchemaV1:
		if state.Pin.SchemaVersion != "" || state.Pin.TargetScopeID != "" {
			return fmt.Errorf("engine: scoped pin requires dynamic resolution v2")
		}
	case DynamicIncludeResolutionStateSchemaV2:
		if state.Pin.SchemaVersion != "yawr.dynamic-include-pin/v2" || state.Pin.TargetScopeID == "" {
			return fmt.Errorf("engine: dynamic resolution v2 requires scoped pin")
		}
	default:
		return fmt.Errorf("engine: unsupported dynamic resolution version %q", state.SchemaVersion)
	}
	return nil
}

const dynamicIncludeNotFoundOutcomeSchemaV1 = "yawr.dynamic-include-not-found-outcome/v1"

type DynamicIncludeResolutionStatus string

const (
	DynamicIncludeResolutionStatusActive    DynamicIncludeResolutionStatus = "active"
	DynamicIncludeResolutionStatusCompleted DynamicIncludeResolutionStatus = "completed"
	DynamicIncludeResolutionStatusFailed    DynamicIncludeResolutionStatus = "failed"
)

func DynamicIncludeNotFoundResultDigest(result *StepResult) (string, bool) {
	if result == nil || result.StepID == "" || result.Status != StepStatusSkipped ||
		result.Outcome != StepOutcomeSkipped || result.Output["skip_reason"] != "include_not_found" ||
		result.Vars["runbook_found"] != false || result.Error != nil {
		return "", false
	}
	payload, err := json.Marshal(struct {
		SchemaVersion string      `json:"schema_version"`
		StepID        string      `json:"step_id"`
		Status        StepStatus  `json:"status"`
		Outcome       StepOutcome `json:"outcome"`
		SkipReason    string      `json:"skip_reason"`
		RunbookFound  bool        `json:"runbook_found"`
	}{
		SchemaVersion: dynamicIncludeNotFoundOutcomeSchemaV1,
		StepID:        result.StepID,
		Status:        result.Status,
		Outcome:       result.Outcome,
		SkipReason:    "include_not_found",
		RunbookFound:  false,
	})
	if err != nil {
		return "", false
	}
	return InteractionPayloadDigest(payload), true
}

// DynamicIncludeResolutionState pins one rendered include occurrence before
// approval, publication, or child execution.
type DynamicIncludeResolutionState struct {
	SchemaVersion   string                         `json:"schema_version"`
	ResolutionID    string                         `json:"resolution_id"`
	WriterEpoch     uint64                         `json:"writer_epoch"`
	QualifiedNodeID string                         `json:"qualified_node_id"`
	CallPath        []DebugCallFrame               `json:"call_path,omitempty"`
	StepID          string                         `json:"step_id"`
	FrameID         string                         `json:"frame_id,omitempty"`
	FrameStepIndex  int                            `json:"frame_step_index,omitempty"`
	Invocation      int                            `json:"invocation"`
	Revision        int64                          `json:"revision"`
	Pin             schema.LockedDynamicInclude    `json:"pin"`
	Status          DynamicIncludeResolutionStatus `json:"status"`
	CommittedAt     string                         `json:"committed_at"`
	CompletedAt     string                         `json:"completed_at,omitempty"`
}

type DynamicIncludeResolutionCommitter interface {
	LookupDynamicIncludeResolution(ctx context.Context, renderedRef string) (DynamicIncludeResolutionState, bool, error)
	CommitDynamicIncludeResolution(ctx context.Context, pin schema.LockedDynamicInclude) (DynamicIncludeResolutionState, error)
}

type dynamicIncludeResolutionCommitterContextKey struct{}
type dynamicIncludeStructuralPathContextKey struct{}

func WithDynamicIncludeStructuralPath(
	ctx context.Context,
	path []schema.DynamicIncludeFrameIdentity,
) context.Context {
	if len(path) == 0 {
		return ctx
	}
	return context.WithValue(
		ctx, dynamicIncludeStructuralPathContextKey{}, append([]schema.DynamicIncludeFrameIdentity(nil), path...),
	)
}

func DynamicIncludeStructuralPathFromContext(ctx context.Context) []schema.DynamicIncludeFrameIdentity {
	path, _ := ctx.Value(dynamicIncludeStructuralPathContextKey{}).([]schema.DynamicIncludeFrameIdentity)
	return append([]schema.DynamicIncludeFrameIdentity(nil), path...)
}

func WithDynamicIncludeResolutionCommitter(ctx context.Context, committer DynamicIncludeResolutionCommitter) context.Context {
	if committer == nil {
		return ctx
	}
	return context.WithValue(ctx, dynamicIncludeResolutionCommitterContextKey{}, committer)
}

func LookupDynamicIncludeResolution(ctx context.Context, renderedRef string) (DynamicIncludeResolutionState, bool, error) {
	committer, _ := ctx.Value(dynamicIncludeResolutionCommitterContextKey{}).(DynamicIncludeResolutionCommitter)
	if committer == nil {
		return DynamicIncludeResolutionState{}, false, nil
	}
	return committer.LookupDynamicIncludeResolution(ctx, renderedRef)
}

func CommitDynamicIncludeResolution(
	ctx context.Context,
	pin schema.LockedDynamicInclude,
) (DynamicIncludeResolutionState, error) {
	committer, _ := ctx.Value(dynamicIncludeResolutionCommitterContextKey{}).(DynamicIncludeResolutionCommitter)
	if committer == nil {
		return DynamicIncludeResolutionState{}, nil
	}
	return committer.CommitDynamicIncludeResolution(ctx, pin)
}
