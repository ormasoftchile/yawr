package executor

import (
	"context"
	"fmt"
	"strings"

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
	spec, ok := step.Spec.(*schema.EndSpec)
	if !ok || spec == nil {
		return nil, fmt.Errorf("end executor: invalid spec for step %s", step.ID)
	}

	result := newResult(step, engine.StepStatusCompleted)
	result.TerminalResults = spec.PublishResults
	outcome := spec.Outcome
	if spec.PublishResults && outcome != nil {
		resolved := *outcome
		for _, field := range []struct {
			name  string
			value *string
		}{{"category", &resolved.Category}, {"code", &resolved.Code}} {
			if !strings.Contains(*field.value, "${") {
				continue
			}
			value, err := ResolvePureTree(*field.value, vars)
			text, ok := value.(string)
			if err != nil || !ok {
				result.Status, result.Outcome = engine.StepStatusFailed, engine.StepOutcomeFailed
				result.Error = typedDiagnostic("outcome."+field.name, "string", value, err)
				return result, nil
			}
			*field.value = text
		}
		outcome = &resolved
	}
	result.Output["terminal"] = true
	if outcome != nil {
		if outcome.Category != "" {
			result.Output["outcome_category"] = outcome.Category
			result.Vars["__run_outcome_category"] = outcome.Category
		}
		if outcome.Code != "" {
			result.Output["outcome_code"] = outcome.Code
			result.Vars["__run_outcome_code"] = outcome.Code
		}
	}
	return result, nil
}
