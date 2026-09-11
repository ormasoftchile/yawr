package engine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

const ExecutionFrameStateSchemaV1 = "yawr.execution-frame-state/v1"

type ExecutionFrameStatus string

const (
	ExecutionFrameStatusActive        ExecutionFrameStatus = "active"
	ExecutionFrameStatusCompleted     ExecutionFrameStatus = "completed"
	ExecutionFrameStatusFailed        ExecutionFrameStatus = "failed"
	ExecutionFrameStatusIndeterminate ExecutionFrameStatus = "indeterminate"
)

// ExecutionFrameState checkpoints one invocation of a structural container.
// Results are keyed by the zero-based child index encoded in decimal.
type ExecutionFrameState struct {
	SchemaVersion         string                 `json:"schema_version"`
	FrameID               string                 `json:"frame_id"`
	ParentFrameID         string                 `json:"parent_frame_id,omitempty"`
	WriterEpoch           uint64                 `json:"writer_epoch"`
	ParentQualifiedNodeID string                 `json:"parent_qualified_node_id"`
	ParentStepID          string                 `json:"parent_step_id"`
	Kind                  string                 `json:"kind"`
	CallPath              []DebugCallFrame       `json:"call_path"`
	BranchLabel           string                 `json:"branch_label,omitempty"`
	IterationIndex        int                    `json:"iteration_index,omitempty"`
	Invocation            int                    `json:"invocation"`
	DefinitionDigest      string                 `json:"definition_digest"`
	StepCount             int                    `json:"step_count"`
	StepIDs               []string               `json:"step_ids"`
	NextStepIndex         int                    `json:"next_step_index"`
	WorkingVars           map[string]any         `json:"working_vars,omitempty"`
	BindingScope          *BindingScopeState     `json:"binding_scope,omitempty"`
	RunResults            *RunResults            `json:"run_results,omitempty"`
	Results               map[string]*StepResult `json:"results,omitempty"`
	Status                ExecutionFrameStatus   `json:"status"`
	StartedAt             string                 `json:"started_at"`
	CompletedAt           string                 `json:"completed_at,omitempty"`
}

// ExecutionFrameRequest identifies one structural container invocation.
type ExecutionFrameRequest struct {
	ParentFrameID         string
	ParentQualifiedNodeID string
	ParentStepID          string
	Kind                  string
	CallPath              []DebugCallFrame
	BranchLabel           string
	IterationIndex        int
	DefinitionDigest      string
	StepCount             int
	StepIDs               []string
	InitialVars           map[string]any
	RunbookInvocation     *schema.RunbookInvocation
	BindingScope          *BindingScopeState
}

// ExecutionFrameStepCommit advances one frame after a child result settles.
type ExecutionFrameStepCommit struct {
	FrameID     string
	StepIndex   int
	StepKind    string
	Result      *StepResult
	WorkingVars map[string]any
}

type ExecutionFrameCommitter interface {
	BeginExecutionFrame(ctx context.Context, request ExecutionFrameRequest) (ExecutionFrameState, error)
	CommitExecutionFrameStep(ctx context.Context, commit ExecutionFrameStepCommit) (ExecutionFrameState, error)
}

// ExecutionFrameBinding maps a sub-engine's local indices into its parent frame.
type ExecutionFrameBinding struct {
	FrameID    string
	StepOffset int
	Invocation int
}

type executionFrameCommitterContextKey struct{}
type executionFrameBindingContextKey struct{}

func WithExecutionFrameCommitter(ctx context.Context, committer ExecutionFrameCommitter) context.Context {
	if committer == nil {
		return ctx
	}
	return context.WithValue(ctx, executionFrameCommitterContextKey{}, committer)
}

func ExecutionFrameCommitterFromContext(ctx context.Context) ExecutionFrameCommitter {
	committer, _ := ctx.Value(executionFrameCommitterContextKey{}).(ExecutionFrameCommitter)
	return committer
}

func WithExecutionFrameBinding(ctx context.Context, binding ExecutionFrameBinding) context.Context {
	if binding.FrameID == "" {
		return ctx
	}
	return context.WithValue(ctx, executionFrameBindingContextKey{}, binding)
}

func ExecutionFrameBindingFromContext(ctx context.Context) (ExecutionFrameBinding, bool) {
	binding, ok := ctx.Value(executionFrameBindingContextKey{}).(ExecutionFrameBinding)
	return binding, ok
}

// ExecutionFrameDefinitionDigest binds resumed progress to the exact child list.
func ExecutionFrameDefinitionDigest(steps []ResolvedStep) (string, error) {
	definition := make([]struct {
		ID   string
		Kind string
		Spec StepSpec
	}, len(steps))
	for index, step := range steps {
		definition[index].ID = step.ID
		definition[index].Kind = step.Kind
		definition[index].Spec = step.Spec
	}
	encoded, err := json.Marshal(definition)
	if err != nil {
		return "", fmt.Errorf("engine: encode execution frame definition: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return fmt.Sprintf("sha256:%x", digest), nil
}

func ExecutionFrameStepIDs(steps []ResolvedStep) []string {
	stepIDs := make([]string, len(steps))
	for index, step := range steps {
		stepIDs[index] = step.ID
	}
	return stepIDs
}
