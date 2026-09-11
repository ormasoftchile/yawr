package executor

import (
	"context"
	"errors"
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/governance"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// ApproveExecutor handles explicit approval steps.
type ApproveExecutor struct {
	gate governance.ApprovalGate
}

// NewApproveExecutor constructs an ApproveExecutor.
func NewApproveExecutor(gate governance.ApprovalGate) *ApproveExecutor {
	return &ApproveExecutor{gate: gate}
}

func (e *ApproveExecutor) Execute(ctx context.Context, step engine.ResolvedStep, vars map[string]any) (*engine.StepResult, error) {
	_ = vars
	spec, ok := step.Spec.(*schema.ApproveSpec)
	if !ok || spec == nil {
		return nil, fmt.Errorf("approve executor: invalid spec for step %s", step.ID)
	}
	if e.gate == nil {
		return nil, fmt.Errorf("approve executor: ApprovalGate is required")
	}

	record, err := e.gate.RequestApproval(ctx, step.ID, "explicit approval step")
	if err != nil {
		result := newResult(step, engine.StepStatusDenied)
		if spec.OnTimeout == "fail" || errors.Is(err, context.DeadlineExceeded) {
			result.Status = engine.StepStatusFailed
			result.Outcome = outcomeForStatus(result.Status)
		}
		result.Error = err
		result.Output["reason"] = err.Error()
		return result, nil
	}

	result := newResult(step, engine.StepStatusCompleted)
	result.Output["approver"] = record.Approver
	result.Output["token"] = record.Token
	return result, nil
}
