package conformance

import (
	"context"
	"errors"
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/gxl/parser"
)

// ParseRunner executes GXL parse vectors against the P2 parser.
type ParseRunner struct{}

// Run parses the vector input and compares the result with the vector expectation.
func (ParseRunner) Run(_ context.Context, v Vector) Result {
	_, err := parser.Parse(v.Input)
	if !v.Expected.WantsError() {
		if err != nil {
			return Result{Verdict: VerdictFail, Err: fmt.Errorf("expected parse_ok, got %w", err)}
		}
		return Result{Verdict: VerdictPass}
	}
	if err == nil {
		return Result{Verdict: VerdictFail, Err: fmt.Errorf("expected %s/%s, got parse_ok", v.Expected.ErrorClass, v.Expected.ErrorCode)}
	}
	if gotClass := errorClass(err); gotClass != v.Expected.ErrorClass {
		return Result{Verdict: VerdictFail, Err: fmt.Errorf("expected class %s, got %s: %w", v.Expected.ErrorClass, gotClass, err)}
	}
	if gotCode := errorCode(err); gotCode != v.Expected.ErrorCode {
		return Result{Verdict: VerdictFail, Err: fmt.Errorf("expected code %s, got %s: %w", v.Expected.ErrorCode, gotCode, err)}
	}
	return Result{Verdict: VerdictPass}
}

func errorClass(err error) string {
	var e *errkit.Error
	if errors.As(err, &e) {
		return e.Class()
	}
	return ""
}

func errorCode(err error) string {
	var e *errkit.Error
	if errors.As(err, &e) {
		return e.Code()
	}
	return ""
}
