package testutil

import "github.com/ormasoftchile/yawr/runtime/pkg/expr"

// FakeExprEvaluator is a controllable Evaluator for use in tests.
type FakeExprEvaluator struct {
	Passthrough bool
	FixedResult string
}

var _ expr.Evaluator = (*FakeExprEvaluator)(nil)

func (f *FakeExprEvaluator) Eval(tmpl string, vars map[string]any) (string, error) {
	if f.FixedResult != "" {
		return f.FixedResult, nil
	}
	if f.Passthrough {
		return tmpl, nil
	}
	return "", nil
}
