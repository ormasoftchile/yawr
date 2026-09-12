package executor

import (
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/pkg/capture"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/expr"
	"github.com/ormasoftchile/yawr/runtime/pkg/gis"
	"github.com/ormasoftchile/yawr/runtime/pkg/pjvm"
)

func mergeChildVars(result *engine.StepResult, children []*engine.StepResult) {
	for _, child := range children {
		if child != nil {
			for name, value := range child.Vars {
				result.Vars[name] = value
			}
		}
	}
}

func resolveValueExpression(evaluator expr.Evaluator, expression string, vars map[string]any) (any, error) {
	typed, ok := evaluator.(expr.ValueEvaluator)
	if !ok {
		return nil, fmt.Errorf("typed expression requires a ValueEvaluator")
	}
	return typed.EvalValue(expression, vars)
}

func resolveTypedValue(evaluator expr.Evaluator, value any, vars map[string]any) (any, error) {
	return resolveTypedValueAt(evaluator, value, vars, 0)
}

func resolveTypedValueAt(evaluator expr.Evaluator, value any, vars map[string]any, depth int) (any, error) {
	pure, bounded := evaluator.(*pureEvaluator)
	if bounded {
		pure.remaining--
		if depth > 128 || pure.remaining < 0 {
			return nil, fmt.Errorf("typed construction budget exceeded")
		}
	}
	switch typed := value.(type) {
	case string:
		parse := gis.Parse
		if bounded {
			parse = gis.ParsePure
		}
		template, err := parse(typed)
		if err != nil {
			return nil, err
		}
		if len(template.Segments) == 1 {
			if expression, ok := template.Segments[0].(*gis.Expr); ok {
				return resolveValueExpression(evaluator, expression.Expression, vars)
			}
		}
		return resolveTemplate(evaluator, typed, vars)
	case map[string]any:
		resolved := make(map[string]any, len(typed))
		for key, item := range typed {
			result, err := resolveTypedValueAt(evaluator, item, vars, depth+1)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", key, err)
			}
			resolved[key] = result
		}
		return resolved, nil
	case []any:
		resolved := make([]any, len(typed))
		for index, item := range typed {
			result, err := resolveTypedValueAt(evaluator, item, vars, depth+1)
			if err != nil {
				return nil, fmt.Errorf("[%d]: %w", index, err)
			}
			resolved[index] = result
		}
		return resolved, nil
	default:
		normalized, err := pjvm.FromAny(value)
		if err != nil {
			return nil, err
		}
		return capture.ToAny(normalized), nil
	}
}
