package engine

import (
	"context"
	"errors"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

type TransitionValidator interface {
	ValidateHandoff(context.Context, HandoffRequest) error
	ValidateCompletion(context.Context) error
}

// HandoffRequest is the evaluated, transport-neutral control signal emitted by
// a handoff step. Only a session coordinator may resolve and start its target.
type HandoffRequest struct {
	TargetRunbook      string                               `json:"target_runbook"`
	ReasonCode         string                               `json:"reason_code"`
	ReasonSummary      string                               `json:"reason_summary"`
	Context            map[string]any                       `json:"context,omitempty"`
	Facts              map[string]any                       `json:"facts,omitempty"`
	ContextBindings    map[string]string                    `json:"context_bindings,omitempty"`
	FactBindings       map[string]string                    `json:"fact_bindings,omitempty"`
	QualifiedNodeID    string                               `json:"qualified_node_id"`
	CallPath           []DebugCallFrame                     `json:"call_path,omitempty"`
	StructuralPath     []schema.DynamicIncludeFrameIdentity `json:"structural_path,omitempty"`
	StepID             string                               `json:"step_id"`
	FrameID            string                               `json:"frame_id,omitempty"`
	FrameStepIndex     int                                  `json:"frame_step_index,omitempty"`
	Invocation         int                                  `json:"invocation"`
	RetryAttempt       int                                  `json:"retry_attempt"`
	OccurrenceSequence int64                                `json:"occurrence_sequence"`
}

type HandoffRequestError struct {
	Request HandoffRequest
}

func (err *HandoffRequestError) Error() string { return "engine: handoff requested" }

func NewHandoffRequestError(request HandoffRequest) error {
	return &HandoffRequestError{Request: request}
}

func HandoffRequestFromError(err error) (HandoffRequest, bool) {
	var requestError *HandoffRequestError
	if !errors.As(err, &requestError) {
		return HandoffRequest{}, false
	}
	return requestError.Request, true
}
