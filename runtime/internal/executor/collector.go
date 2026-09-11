package executor

import (
	"context"
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/expr"
	"github.com/ormasoftchile/yawr/runtime/pkg/input"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// CollectorExecutor gathers form input from the user.
type CollectorExecutor struct {
	input     input.PromptProvider
	evaluator expr.Evaluator
	condition expr.ConditionEvaluator
}

// NewCollectorExecutor constructs a CollectorExecutor.
func NewCollectorExecutor(in input.PromptProvider, eval expr.Evaluator, cond expr.ConditionEvaluator) *CollectorExecutor {
	return &CollectorExecutor{input: in, evaluator: eval, condition: cond}
}

func (e *CollectorExecutor) Execute(ctx context.Context, step engine.ResolvedStep, vars map[string]any) (*engine.StepResult, error) {
	spec, ok := step.Spec.(*schema.CollectorSpec)
	if !ok || spec == nil {
		return nil, fmt.Errorf("collector executor: invalid spec for step %s", step.ID)
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

	var fields []input.FormField
	var ephemeral []string
	for _, field := range spec.Fields {
		ok, err := evalCondition(e.condition, field.When, vars)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		label, err := resolveTemplate(e.evaluator, field.Label, vars)
		if err != nil {
			return nil, err
		}
		hint, err := resolveTemplate(e.evaluator, field.Hint, vars)
		if err != nil {
			return nil, err
		}
		def := field.Default
		if s, ok := field.Default.(string); ok {
			def, err = resolveTemplate(e.evaluator, s, vars)
			if err != nil {
				return nil, err
			}
		}
		opts := make([]input.Option, 0, len(field.Options))
		for _, opt := range field.Options {
			optLabel, err := resolveTemplate(e.evaluator, opt.Label, vars)
			if err != nil {
				return nil, err
			}
			optHint, err := resolveTemplate(e.evaluator, opt.Hint, vars)
			if err != nil {
				return nil, err
			}
			opts = append(opts, input.Option{Label: optLabel, Value: opt.Value, Hint: optHint})
		}
		fields = append(fields, input.FormField{
			Name:       field.Name,
			Type:       string(field.Type),
			Label:      label,
			Required:   field.Required,
			Default:    def,
			Hint:       hint,
			Options:    opts,
			Multiple:   field.Multiple,
			Validation: collectorFormValidation(field.Validation),
			Ephemeral:  field.Ephemeral,
			FromStep:   field.FromStep,
		})
		if field.Ephemeral {
			ephemeral = append(ephemeral, field.Name)
		}
	}

	request := input.FormRequest{
		StepID: step.ID,
		Prompt: prompt,
		Fields: fields,
	}
	resp, err := e.input.PromptForm(ctx, request)
	if err != nil {
		result := newResult(step, engine.StepStatusFailed)
		result.Error = err
		result.Output["error"] = err.Error()
		return result, nil
	}

	failures := input.ValidateFormResponse(request, *resp)

	result := newResult(step, engine.StepStatusCompleted)
	if len(failures) > 0 {
		result.Status = engine.StepStatusFailed
		result.Outcome = outcomeForStatus(result.Status)
		result.Output["validation_errors"] = failures
		return result, nil
	}

	for k, v := range resp.Values {
		result.Vars[k] = v
	}
	if len(ephemeral) > 0 {
		result.Output["ephemeral_fields"] = ephemeral
	}
	// Echo the resolved prompt and submitted answer into Output so UI
	// renderers can replay the interaction after the step completes.
	// Ephemeral fields are scrubbed from the echoed answer.
	answer := make(map[string]any, len(resp.Values))
	for k, v := range resp.Values {
		skip := false
		for _, name := range ephemeral {
			if name == k {
				skip = true
				break
			}
		}
		if !skip {
			answer[k] = v
		}
	}
	result.Output["interaction"] = map[string]any{
		"kind":   "collector",
		"prompt": prompt,
		"answer": answer,
	}
	return result, nil
}

func collectorFormValidation(validation *schema.FieldValidation) *input.FormValidation {
	if validation == nil {
		return nil
	}
	return &input.FormValidation{
		MinLength: validation.MinLength,
		MaxLength: validation.MaxLength,
		Pattern:   validation.Pattern,
		Format:    validation.Format,
		Min:       validation.Min,
		Max:       validation.Max,
		Step:      validation.Step,
	}
}
