package executor

import (
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/expr"
)

func outcomeForStatus(status engine.StepStatus) engine.StepOutcome {
	switch status {
	case engine.StepStatusCompleted:
		return engine.StepOutcomeSuccess
	case engine.StepStatusSkipped:
		return engine.StepOutcomeSkipped
	case engine.StepStatusWaiting:
		return engine.StepOutcomeWaiting
	case engine.StepStatusDenied:
		return engine.StepOutcomeDenied
	case engine.StepStatusFailed:
		return engine.StepOutcomeFailed
	default:
		return engine.StepOutcomeFailed
	}
}

func newResult(step engine.ResolvedStep, status engine.StepStatus) *engine.StepResult {
	return &engine.StepResult{
		StepID:  step.ID,
		Status:  status,
		Outcome: outcomeForStatus(status),
		Output:  map[string]any{},
		Vars:    map[string]any{},
	}
}

func resolveTemplate(e expr.Evaluator, tmpl string, vars map[string]any) (string, error) {
	if e == nil {
		return tmpl, nil
	}
	return e.Eval(tmpl, vars)
}

func resolveStringSlice(e expr.Evaluator, values []string, vars map[string]any) ([]string, error) {
	if e == nil {
		out := make([]string, len(values))
		copy(out, values)
		return out, nil
	}
	out := make([]string, 0, len(values))
	for _, v := range values {
		resolved, err := e.Eval(v, vars)
		if err != nil {
			return nil, err
		}
		out = append(out, resolved)
	}
	return out, nil
}

func evalCondition(cond expr.ConditionEvaluator, condition string, vars map[string]any) (bool, error) {
	if cond == nil {
		return true, nil
	}
	return cond.EvalBool(condition, vars)
}

type captureResolver interface {
	ResolveCapturePath(source string, output map[string]any) (any, bool, error)
}

func resolveCapture(e expr.Evaluator, source string, output map[string]any) (any, bool, error) {
	if resolver, ok := e.(captureResolver); ok {
		return resolver.ResolveCapturePath(source, output)
	}
	if source == "exitCode" {
		source = "exit_code"
	}
	value, ok := output[source]
	return value, ok, nil
}

func copyVars(vars map[string]any) map[string]any {
	if vars == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(vars))
	for k, v := range vars {
		out[k] = cloneRuntimeValue(v)
	}
	return out
}

func cloneRuntimeValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return copyVars(typed)
	case []any:
		cloned := make([]any, len(typed))
		for index, item := range typed {
			cloned[index] = cloneRuntimeValue(item)
		}
		return cloned
	case map[string]string:
		cloned := make(map[string]string, len(typed))
		for key, item := range typed {
			cloned[key] = item
		}
		return cloned
	case []string:
		return append([]string(nil), typed...)
	default:
		return value
	}
}

func requireSpec[T any](step engine.ResolvedStep, spec *T) (*T, error) {
	if spec == nil {
		return nil, fmt.Errorf("executor: step %s has nil spec", step.ID)
	}
	return spec, nil
}
