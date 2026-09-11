package conformance

import (
	"context"
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/pkg/gdp"
	gxlparser "github.com/ormasoftchile/yawr/runtime/pkg/gxl/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/pjvm"
)

// PathRunner executes GXL path vectors against the shared GDP resolver.
type PathRunner struct{}

// Run parses a GXL GDP path and compares resolution with the vector expectation.
func (PathRunner) Run(_ context.Context, v Vector) Result {
	expr, err := gxlparser.Parse(v.Input)
	if err != nil {
		return expectedErrorResult(v, err, "parse")
	}
	ref, ok := expr.Node.(*gxlparser.PathRef)
	if !ok {
		return Result{Verdict: VerdictFail, Err: fmt.Errorf("expected GXL path expression, got %T", expr.Node)}
	}
	tree, err := pjvm.FromAny(v.Variables)
	if err != nil {
		return Result{Verdict: VerdictFail, Err: fmt.Errorf("variables are not PJVM: %w", err)}
	}
	got, err := gdp.Resolve(tree, gxlPath(ref))
	if err != nil {
		return expectedErrorResult(v, err, "resolve")
	}
	if v.Expected.WantsError() {
		return Result{Verdict: VerdictFail, Err: fmt.Errorf("expected %s/%s, got value %s", v.Expected.ErrorClass, v.Expected.ErrorCode, got.String())}
	}
	if v.Expected.PJVMValue == nil {
		return Result{Verdict: VerdictFail, Err: fmt.Errorf("expected value missing from vector")}
	}
	if !got.Equal(*v.Expected.PJVMValue) {
		return Result{Verdict: VerdictFail, Err: fmt.Errorf("expected %s, got %s", v.Expected.PJVMValue.String(), got.String())}
	}
	return Result{Verdict: VerdictPass}
}

func gxlPath(ref *gxlparser.PathRef) *gdp.Path {
	out := &gdp.Path{Dialect: gdp.DialectGXL, Root: ref.Root, Span: convertSpan(ref.Span)}
	for _, seg := range ref.Segments {
		out.Segments = append(out.Segments, gdp.Segment{Span: convertSpan(seg.Span), Name: seg.Name, Index: seg.Index})
	}
	return out
}

func convertSpan(s gxlparser.Span) gdp.Span {
	return gdp.Span{Start: gdp.Pos{Line: s.Start.Line, Column: s.Start.Column, Offset: s.Start.Offset}, End: gdp.Pos{Line: s.End.Line, Column: s.End.Column, Offset: s.End.Offset}}
}

func expectedErrorResult(v Vector, err error, phase string) Result {
	if !v.Expected.WantsError() {
		return Result{Verdict: VerdictFail, Err: fmt.Errorf("expected value, got %s error: %w", phase, err)}
	}
	if gotClass := errorClass(err); gotClass != v.Expected.ErrorClass {
		return Result{Verdict: VerdictFail, Err: fmt.Errorf("expected class %s, got %s: %w", v.Expected.ErrorClass, gotClass, err)}
	}
	if gotCode := errorCode(err); gotCode != v.Expected.ErrorCode {
		return Result{Verdict: VerdictFail, Err: fmt.Errorf("expected code %s, got %s: %w", v.Expected.ErrorCode, gotCode, err)}
	}
	return Result{Verdict: VerdictPass}
}
