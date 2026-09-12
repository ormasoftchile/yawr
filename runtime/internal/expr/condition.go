package expr

import pkgExpr "github.com/ormasoftchile/yawr/runtime/pkg/expr"

// SimpleConditionEvaluator evaluates boolean conditions via the native GXL engine.
type SimpleConditionEvaluator struct{}

// NewSimpleConditionEvaluator returns a SimpleConditionEvaluator.
func NewSimpleConditionEvaluator(_ pkgExpr.Evaluator) *SimpleConditionEvaluator {
	return &SimpleConditionEvaluator{}
}

func (e *SimpleConditionEvaluator) EvalBool(condition string, vars map[string]any) (bool, error) {
	selectedEngine(Operation{Kind: EvalCondition, Source: condition})
	return evalBoolNative(condition, vars)
}
