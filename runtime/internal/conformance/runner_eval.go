package conformance

import (
	"context"
	"fmt"
	"regexp"

	gxleval "github.com/ormasoftchile/yawr/runtime/pkg/gxl/eval"
	"github.com/ormasoftchile/yawr/runtime/pkg/gxl/parser"
)

// EvalRunner executes GXL evaluation vectors against the P3 evaluator.
type EvalRunner struct{}

// Run parses, evaluates, and compares a GXL evaluation vector.
func (EvalRunner) Run(_ context.Context, v Vector) Result {
	expr, err := parser.Parse(v.Input)
	if err != nil {
		return expectedErrorResult(v, err, "parse")
	}
	scope, err := gxleval.FromAny(v.Variables)
	if err != nil {
		return Result{Verdict: VerdictFail, Err: fmt.Errorf("variables are not PJVM: %w", err)}
	}
	got, err := gxleval.Eval(expr, scope)
	if err != nil {
		return expectedErrorResult(v, err, "eval")
	}
	if v.Expected.WantsError() {
		return Result{Verdict: VerdictFail, Err: fmt.Errorf("expected %s/%s, got value %s", v.Expected.ErrorClass, v.Expected.ErrorCode, got.String())}
	}
	if v.Expected.Assert != "" {
		return assertEvalResult(v, got.String(), string(got.Kind()))
	}
	if v.Expected.PJVMValue == nil {
		return Result{Verdict: VerdictFail, Err: fmt.Errorf("expected value missing from vector")}
	}
	if !got.Equal(*v.Expected.PJVMValue) {
		return Result{Verdict: VerdictFail, Err: fmt.Errorf("expected %s, got %s", v.Expected.PJVMValue.String(), got.String())}
	}
	return Result{Verdict: VerdictPass}
}

func assertEvalResult(v Vector, got, gotType string) Result {
	if gotType != v.Expected.Type {
		return Result{Verdict: VerdictFail, Err: fmt.Errorf("expected type %s, got %s", v.Expected.Type, gotType)}
	}
	switch v.Expected.Assert {
	case "regex":
		matched, err := regexp.MatchString(v.Expected.Pattern, got)
		if err != nil {
			return Result{Verdict: VerdictFail, Err: fmt.Errorf("invalid expected regex %q: %w", v.Expected.Pattern, err)}
		}
		if !matched {
			return Result{Verdict: VerdictFail, Err: fmt.Errorf("expected %q to match %q", got, v.Expected.Pattern)}
		}
		return Result{Verdict: VerdictPass}
	default:
		return Result{Verdict: VerdictFail, Err: fmt.Errorf("unsupported assertion %q", v.Expected.Assert)}
	}
}
