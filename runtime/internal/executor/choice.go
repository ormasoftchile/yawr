package executor

import (
	"context"
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/expr"
	"github.com/ormasoftchile/yawr/runtime/pkg/input"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// ChoiceExecutor prompts the user to choose among options.
type ChoiceExecutor struct {
	input     input.PromptProvider
	evaluator expr.Evaluator
}

// NewChoiceExecutor constructs a ChoiceExecutor.
func NewChoiceExecutor(in input.PromptProvider, eval expr.Evaluator) *ChoiceExecutor {
	return &ChoiceExecutor{input: in, evaluator: eval}
}

func (e *ChoiceExecutor) Execute(ctx context.Context, step engine.ResolvedStep, vars map[string]any) (*engine.StepResult, error) {
	spec, ok := step.Spec.(*schema.ChoiceSpec)
	if !ok || spec == nil {
		return nil, fmt.Errorf("choice executor: invalid spec for step %s", step.ID)
	}
	if e.input == nil {
		result := newResult(step, engine.StepStatusSkipped)
		result.Output["warning"] = "input provider not configured"
		return result, nil
	}

	prompt, err := resolveTemplate(e.evaluator, spec.Prompt, vars)
	if err != nil {
		return nil, err
	}
	options := make([]input.Option, 0, len(spec.Options))
	for _, opt := range spec.Options {
		label, err := resolveTemplate(e.evaluator, opt.Label, vars)
		if err != nil {
			return nil, err
		}
		hint, err := resolveTemplate(e.evaluator, opt.Hint, vars)
		if err != nil {
			return nil, err
		}
		options = append(options, input.Option{Label: label, Value: opt.Value, Hint: hint})
	}
	def, err := resolveTemplate(e.evaluator, spec.Default, vars)
	if err != nil {
		return nil, err
	}

	resp, err := e.input.PromptChoice(ctx, input.ChoiceRequest{
		StepID:   step.ID,
		Prompt:   prompt,
		Options:  options,
		Default:  def,
		Multiple: spec.Multiple,
		Min:      spec.MinSelections,
		Max:      spec.MaxSelections,
	})
	if err != nil {
		result := newResult(step, engine.StepStatusFailed)
		result.Error = err
		result.Output["error"] = err.Error()
		return result, nil
	}

	selected := resp.Selected
	if len(selected) == 0 && def != "" {
		selected = []string{def}
	}
	result := newResult(step, engine.StepStatusCompleted)
	allowed := make(map[string]bool, len(options))
	for _, option := range options {
		allowed[option.Value] = true
	}
	invalid := !spec.Multiple && len(selected) > 1 || spec.MinSelections > 0 && len(selected) < spec.MinSelections || spec.MaxSelections > 0 && len(selected) > spec.MaxSelections
	for _, value := range selected {
		invalid = invalid || !allowed[value]
	}
	if invalid {
		result.Status = engine.StepStatusFailed
		result.Outcome = engine.StepOutcomeFailed
		result.Output["validation_errors"] = []string{"choice answer does not match the declared options or selection limits"}
		return result, nil
	}
	if spec.Multiple {
		result.Vars[spec.Variable] = selected
	} else if len(selected) > 0 {
		result.Vars[spec.Variable] = selected[0]
	} else {
		result.Vars[spec.Variable] = ""
	}
	result.Output["interaction"] = map[string]any{
		"kind":   "choice",
		"prompt": prompt,
		"answer": map[string]any{"selected": selected},
	}
	return result, nil
}
