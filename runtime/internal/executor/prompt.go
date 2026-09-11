package executor

import (
	"context"
	"fmt"
	"os"

	internalinput "github.com/ormasoftchile/yawr/runtime/internal/input"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/input"
)

// PromptSpec is the step spec for prompt steps.
type PromptSpec struct {
	VarName   string
	Schema    map[string]any
	Sensitive bool
	Metadata  map[string]string
}

// StepKind returns the discriminator for prompt steps.
func (s *PromptSpec) StepKind() string { return "prompt" }

// PromptExecutor resolves a single input value via an input provider.
type PromptExecutor struct {
	provider input.InputProvider
}

// NewPromptExecutor constructs a PromptExecutor.
func NewPromptExecutor(provider input.InputProvider) *PromptExecutor {
	return &PromptExecutor{provider: provider}
}

func (e *PromptExecutor) Execute(ctx context.Context, step engine.ResolvedStep, vars map[string]any) (*engine.StepResult, error) {
	spec, ok := step.Spec.(*PromptSpec)
	if !ok || spec == nil {
		return nil, fmt.Errorf("prompt executor: invalid spec for step %s", step.ID)
	}

	provider := e.provider
	if provider == nil {
		provider = internalinput.NewPromptProvider(os.Stdin, os.Stderr)
	}

	resp, err := provider.Provide(ctx, input.InputRequest{
		StepID:    step.ID,
		VarName:   spec.VarName,
		Schema:    spec.Schema,
		Sensitive: spec.Sensitive,
		Metadata:  spec.Metadata,
	})
	if err != nil {
		result := newResult(step, engine.StepStatusFailed)
		result.Error = err
		result.Output["error"] = err.Error()
		return result, nil
	}
	if resp == nil {
		err := internalinput.ErrNoProvider
		result := newResult(step, engine.StepStatusFailed)
		result.Error = err
		result.Output["error"] = err.Error()
		return result, nil
	}

	result := newResult(step, engine.StepStatusCompleted)
	result.Output[spec.VarName] = resp.Value
	result.Vars[spec.VarName] = resp.Value
	return result, nil
}
