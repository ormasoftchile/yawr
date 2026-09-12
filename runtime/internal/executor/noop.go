package executor

import (
	"context"
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/expr"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// NoopExecutor executes a noop step — it performs no external action but
// evaluates any capture expressions against the current vars context.
type NoopExecutor struct {
	evaluator expr.Evaluator
}

// NewNoopExecutor constructs a NoopExecutor with a template evaluator.
func NewNoopExecutor(evaluator expr.Evaluator) *NoopExecutor {
	return &NoopExecutor{evaluator: evaluator}
}

func (e *NoopExecutor) Execute(ctx context.Context, step engine.ResolvedStep, vars map[string]any) (*engine.StepResult, error) {
	_ = ctx
	spec, ok := step.Spec.(*schema.NoopSpec)
	if !ok || spec == nil {
		return nil, fmt.Errorf("noop executor: invalid spec for step %s", step.ID)
	}

	result := newResult(step, engine.StepStatusCompleted)

	// Capture entries on noop steps are string templates evaluated against
	// the current vars context.
	for dest, src := range step.Capture {
		val, err := resolveTemplate(e.evaluator, src, vars)
		if err != nil {
			return nil, fmt.Errorf("noop executor: capture %q: %w", dest, err)
		}
		result.Vars[dest] = val
	}

	return result, nil
}
