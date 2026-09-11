package executor

import (
	"context"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/testutil"
)

func TestApproveExecutor_Approved(t *testing.T) {
	gate := testutil.NewFakeApprovalGate(true)
	exec := NewApproveExecutor(gate)
	step := engine.ResolvedStep{ID: "approve", Kind: "approve", Spec: &schema.ApproveSpec{}}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != engine.StepStatusCompleted {
		t.Fatalf("expected completed, got %s", res.Status)
	}
	if res.Output["approver"] == "" {
		t.Fatalf("expected approver output")
	}
}

func TestApproveExecutor_Rejected(t *testing.T) {
	gate := testutil.NewFakeApprovalGate(false)
	exec := NewApproveExecutor(gate)
	step := engine.ResolvedStep{ID: "approve", Kind: "approve", Spec: &schema.ApproveSpec{}}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != engine.StepStatusDenied {
		t.Fatalf("expected denied, got %s", res.Status)
	}
}
