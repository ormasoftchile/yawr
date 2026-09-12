package executor

import (
	"context"
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// EndExecutor marks the run as terminal.
type EndExecutor struct{}

// NewEndExecutor constructs an EndExecutor.
func NewEndExecutor() *EndExecutor {
	return &EndExecutor{}
}

func (e *EndExecutor) Execute(ctx context.Context, step engine.ResolvedStep, vars map[string]any) (*engine.StepResult, error) {
	_ = ctx
	_ = vars
	spec, ok := step.Spec.(*schema.EndSpec)
	if !ok || spec == nil {
		return nil, fmt.Errorf("end executor: invalid spec for step %s", step.ID)
	}

	result := newResult(step, engine.StepStatusCompleted)
	result.Output["terminal"] = true
	if spec.Outcome != nil {
		if spec.Outcome.Category != "" {
			result.Output["outcome_category"] = spec.Outcome.Category
			result.Vars["__run_outcome_category"] = spec.Outcome.Category
		}
		if spec.Outcome.Code != "" {
			result.Output["outcome_code"] = spec.Outcome.Code
			result.Vars["__run_outcome_code"] = spec.Outcome.Code
		}
	}
	return result, nil
}
