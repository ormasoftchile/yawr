package expr

// TemplateEvaluator resolves runtime string templates through the native GIS engine.
// It is stateless and safe for concurrent use.
type TemplateEvaluator struct{}

func (e *TemplateEvaluator) Eval(tmpl string, vars map[string]any) (string, error) {
	selectedEngine(Operation{Kind: Interpolate, Source: tmpl})
	return interpolateNative(tmpl, vars)
}

func (e *TemplateEvaluator) EvalValue(expression string, vars map[string]any) (any, error) {
	return evalValueNative(expression, vars)
}
