package executor

import (
	"context"
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// CompensateExecutor registers compensation steps.
type CompensateExecutor struct{}

// NewCompensateExecutor constructs a CompensateExecutor.
func NewCompensateExecutor() *CompensateExecutor {
	return &CompensateExecutor{}
}

func (e *CompensateExecutor) Execute(ctx context.Context, step engine.ResolvedStep, vars map[string]any) (*engine.StepResult, error) {
	_ = ctx
	_ = vars
	spec, ok := step.Spec.(*schema.CompensateSpec)
	if !ok || spec == nil {
		return nil, fmt.Errorf("compensate executor: invalid spec for step %s", step.ID)
	}

	result := newResult(step, engine.StepStatusCompleted)
	key := "__compensation_" + step.ID
	result.Vars[key] = map[string]any{
		"on":    spec.Compensate.On,
		"steps": spec.Compensate.Steps,
	}
	result.Output["compensation_registered"] = true
	return result, nil
}
