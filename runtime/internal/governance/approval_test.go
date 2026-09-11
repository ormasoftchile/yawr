package governance

import (
	"context"
	"testing"
)

func TestNoOpApprovalGate_AlwaysApproves(t *testing.T) {
	gate := NewNoOpApprovalGate()

	record, err := gate.RequestApproval(context.Background(), "step-1", "test reason")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if record.Approver != "noop-gate" {
		t.Errorf("expected Approver=noop-gate, got %s", record.Approver)
	}
	if record.Token == "" {
		t.Errorf("expected non-empty Token")
	}
	if record.ApprovedAt == "" {
		t.Errorf("expected non-empty ApprovedAt")
	}
}

func TestNoOpApprovalGate_ContextCancelled(t *testing.T) {
	gate := NewNoOpApprovalGate()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	_, err := gate.RequestApproval(ctx, "step-1", "test reason")
	if err == nil {
		t.Errorf("expected error due to cancelled context")
	}
}
