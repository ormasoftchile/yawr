package executor

import (
	"context"
	"testing"

	internalexpr "github.com/ormasoftchile/yawr/runtime/internal/expr"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestAssertExecutor_AllPass(t *testing.T) {
	exec := NewAssertExecutor(&internalexpr.TemplateEvaluator{})
	step := engine.ResolvedStep{ID: "assert", Kind: "assert", Spec: &schema.AssertSpec{Assert: []schema.Assertion{
		{Type: "eq", Subject: "${msg}", Expected: "hello"},
		{Type: "contains", Subject: "${msg}", Expected: "ell"},
	}}}

	res, err := exec.Execute(context.Background(), step, map[string]any{"msg": "hello"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != engine.StepStatusCompleted {
		t.Fatalf("expected completed, got %s", res.Status)
	}
}

func TestAssertExecutor_OneFails(t *testing.T) {
	exec := NewAssertExecutor(&internalexpr.TemplateEvaluator{})
	step := engine.ResolvedStep{ID: "assert", Kind: "assert", Spec: &schema.AssertSpec{Assert: []schema.Assertion{
		{Type: "eq", Subject: "${msg}", Expected: "world"},
	}}}

	res, err := exec.Execute(context.Background(), step, map[string]any{"msg": "hello"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != engine.StepStatusFailed {
		t.Fatalf("expected failed, got %s", res.Status)
	}

	failures, ok := res.Output["failures"].([]map[string]any)
	if !ok || len(failures) == 0 {
		t.Fatal("expected failures in output")
	}
	if failures[0]["type"] != "eq" {
		t.Fatalf("expected failure type eq, got %v", failures[0]["type"])
	}
	if failures[0]["subject"] != "hello" {
		t.Fatalf("expected failure subject hello, got %v", failures[0]["subject"])
	}
	if failures[0]["expected"] != "world" {
		t.Fatalf("expected failure expected world, got %v", failures[0]["expected"])
	}
}

func TestAssertExecutor_RegexMatch(t *testing.T) {
	exec := NewAssertExecutor(nil)
	step := engine.ResolvedStep{ID: "assert", Kind: "assert", Spec: &schema.AssertSpec{Assert: []schema.Assertion{
		{Type: "matches", Subject: "foo-123", Expected: "^foo-\\d+$"},
	}}}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != engine.StepStatusCompleted {
		t.Fatalf("expected completed, got %s", res.Status)
	}
}

func TestAssertExecutor_Contains(t *testing.T) {
	exec := NewAssertExecutor(nil)
	step := engine.ResolvedStep{ID: "assert", Kind: "assert", Spec: &schema.AssertSpec{Assert: []schema.Assertion{
		{Type: "contains", Subject: "hello world", Expected: "world"},
	}}}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != engine.StepStatusCompleted {
		t.Fatalf("expected completed, got %s", res.Status)
	}
}

func TestAssertExecutor_Exists(t *testing.T) {
	exec := NewAssertExecutor(nil)
	step := engine.ResolvedStep{ID: "assert", Kind: "assert", Spec: &schema.AssertSpec{Assert: []schema.Assertion{
		{Type: "exists", Subject: "value"},
	}}}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != engine.StepStatusCompleted {
		t.Fatalf("expected completed, got %s", res.Status)
	}
}
