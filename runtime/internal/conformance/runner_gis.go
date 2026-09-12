package conformance

import (
	"context"
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/pkg/gis"
	gxleval "github.com/ormasoftchile/yawr/runtime/pkg/gxl/eval"
)

// GISRunner executes GIS interpolation vectors against the P4 interpolator.
type GISRunner struct{}

// Run interpolates the input template and compares it to the vector expectation.
func (GISRunner) Run(_ context.Context, v Vector) Result {
	scope, err := gxleval.FromAny(v.Variables)
	if err != nil {
		return Result{Verdict: VerdictFail, Err: fmt.Errorf("variables are not PJVM: %w", err)}
	}
	got, err := gis.Interpolate(v.Input, scope)
	if err != nil {
		return expectedErrorResult(v, err, "interpolate")
	}
	if v.Expected.WantsError() {
		return Result{Verdict: VerdictFail, Err: fmt.Errorf("expected %s/%s, got value %q", v.Expected.ErrorClass, v.Expected.ErrorCode, got)}
	}
	want, ok := v.Expected.Value.(string)
	if !ok && v.Expected.HasValue {
		want = fmt.Sprint(v.Expected.Value)
	}
	if !v.Expected.HasValue {
		return Result{Verdict: VerdictFail, Err: fmt.Errorf("expected value missing from vector")}
	}
	if got != want {
		return Result{Verdict: VerdictFail, Err: fmt.Errorf("expected %q, got %q", want, got)}
	}
	return Result{Verdict: VerdictPass}
}
