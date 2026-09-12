package executor

import (
	"context"
	"fmt"
	"io"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/expr"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// DisplayExecutor executes a display step — renders template content to the
// configured output writer. No user input is required or collected.
type DisplayExecutor struct {
	evaluator expr.Evaluator
	out       io.Writer
}

// NewDisplayExecutor constructs a DisplayExecutor.
func NewDisplayExecutor(evaluator expr.Evaluator, out io.Writer) *DisplayExecutor {
	return &DisplayExecutor{evaluator: evaluator, out: out}
}

func (e *DisplayExecutor) Execute(ctx context.Context, step engine.ResolvedStep, vars map[string]any) (*engine.StepResult, error) {
	_ = ctx
	spec, ok := step.Spec.(*schema.DisplaySpec)
	if !ok || spec == nil {
		return nil, fmt.Errorf("display executor: invalid spec for step %s", step.ID)
	}

	rendered, err := resolveTemplate(e.evaluator, spec.Display.Content, vars)
	if err != nil {
		return nil, fmt.Errorf("display executor: content template %q: %w", step.ID, err)
	}

	if _, err := fmt.Fprintln(e.out, rendered); err != nil {
		return nil, fmt.Errorf("display executor: write output: %w", err)
	}

	result := newResult(step, engine.StepStatusCompleted)
	result.Output = map[string]any{"content": rendered}
	return result, nil
}
