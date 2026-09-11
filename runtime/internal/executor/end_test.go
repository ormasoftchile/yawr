package executor

import (
	"context"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestEndExecutor_WithOutcome(t *testing.T) {
	exec := NewEndExecutor()
	step := engine.ResolvedStep{ID: "end", Kind: "end", Spec: &schema.EndSpec{Outcome: &schema.OutcomeDeclaration{
		Category: "resolved",
		Code:     "task_succeeded",
	}}}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Output["terminal"] != true {
		t.Fatalf("expected terminal output")
	}
	if res.Vars["__run_outcome_category"] != "resolved" {
		t.Fatalf("expected outcome category, got %v", res.Vars["__run_outcome_category"])
	}
}

func TestEndExecutor_NoOutcome(t *testing.T) {
	exec := NewEndExecutor()
	step := engine.ResolvedStep{ID: "end", Kind: "end", Spec: &schema.EndSpec{}}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Output["terminal"] != true {
		t.Fatalf("expected terminal output")
	}
	if _, ok := res.Vars["__run_outcome_category"]; ok {
		t.Fatalf("did not expect outcome vars")
	}
}
