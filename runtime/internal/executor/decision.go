package executor

import (
	"context"
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/expr"
	"github.com/ormasoftchile/yawr/runtime/pkg/input"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// DecisionExecutor prompts for a decision route.
type DecisionExecutor struct {
	input     input.PromptProvider
	evaluator expr.Evaluator
}

// NewDecisionExecutor constructs a DecisionExecutor.
func NewDecisionExecutor(in input.PromptProvider, eval expr.Evaluator) *DecisionExecutor {
	return &DecisionExecutor{input: in, evaluator: eval}
}

func (e *DecisionExecutor) Execute(ctx context.Context, step engine.ResolvedStep, vars map[string]any) (*engine.StepResult, error) {
	spec, ok := step.Spec.(*schema.DecisionSpec)
	if !ok || spec == nil {
		return nil, fmt.Errorf("decision executor: invalid spec for step %s", step.ID)
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
	routes := make([]input.Route, 0, len(spec.Routes))
	resolvedRoutes := make(map[string]schema.DecisionRoute, len(spec.Routes))
	for _, route := range spec.Routes {
		label, err := resolveTemplate(e.evaluator, route.Label, vars)
		if err != nil {
			return nil, err
		}
		hint, err := resolveTemplate(e.evaluator, route.Hint, vars)
		if err != nil {
			return nil, err
		}
		routes = append(routes, input.Route{Label: label, Hint: hint})
		resolvedRoutes[label] = route
	}

	resp, err := e.input.PromptDecision(ctx, input.DecisionRequest{
		StepID: step.ID,
		Prompt: prompt,
		Routes: routes,
	})
	if err != nil {
		result := newResult(step, engine.StepStatusFailed)
		result.Error = err
		result.Output["error"] = err.Error()
		return result, nil
	}

	result := newResult(step, engine.StepStatusCompleted)
	label := resp.Label
	if _, ok := resolvedRoutes[label]; !ok {
		result.Status = engine.StepStatusFailed
		result.Outcome = engine.StepOutcomeFailed
		result.Output["validation_errors"] = []string{"decision answer is not a declared route"}
		return result, nil
	}
	result.Output["route_label"] = label
	if route, ok := resolvedRoutes[label]; ok {
		if route.Goto != "" {
			result.Output["route_goto"] = route.Goto
		}
		if route.Runbook != "" {
			result.Output["route_runbook"] = route.Runbook
		}
	}
	if spec.Variable != "" {
		result.Vars[spec.Variable] = label
	}
	result.Output["interaction"] = map[string]any{
		"kind":   "decision",
		"prompt": prompt,
		"answer": map[string]any{"label": label},
	}
	return result, nil
}
