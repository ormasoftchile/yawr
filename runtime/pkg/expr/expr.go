package expr

// Evaluator resolves string template expressions in step field values.
// Implementations must be safe for concurrent use.
type Evaluator interface {
	Eval(tmpl string, vars map[string]any) (string, error)
}

// ValueEvaluator evaluates a GXL expression without string interpolation.
// It is an optional capability for typed collections and output expressions.
type ValueEvaluator interface {
	EvalValue(expression string, vars map[string]any) (any, error)
}

// ConditionEvaluator evaluates boolean condition expressions.
// Implementations must be safe for concurrent use.
type ConditionEvaluator interface {
	EvalBool(condition string, vars map[string]any) (bool, error)
}
