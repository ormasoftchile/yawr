package governance

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	governancepkg "github.com/ormasoftchile/yawr/runtime/pkg/governance"
)

type approvalGateStub struct {
	called   int
	prepared *bool
}

func (gate *approvalGateStub) RequestApproval(context.Context, string, string) (governancepkg.ApprovalRecord, error) {
	if gate.prepared == nil || !*gate.prepared {
		return governancepkg.ApprovalRecord{}, errors.New("approval ran before pending commit")
	}
	gate.called++
	return governancepkg.ApprovalRecord{
		Approver: "operator", ApprovedAt: "2026-08-31T05:00:00Z", Token: "token",
	}, nil
}

type approvalCommitterStub struct {
	prepared bool
	accepted bool
	restored *engine.InteractionState
}

func (committer *approvalCommitterStub) PrepareInteraction(_ context.Context, proposed engine.InteractionState) (engine.InteractionState, error) {
	committer.prepared = true
	if committer.restored != nil {
		return *committer.restored, nil
	}
	return proposed, nil
}
func (committer *approvalCommitterStub) AcceptInteraction(
	_ context.Context, _ string, _ string, answer json.RawMessage,
) (engine.InteractionState, error) {
	committer.accepted = true
	return engine.InteractionState{Status: engine.InteractionStatusAnswered, Answer: answer}, nil
}

func TestDurableApprovalGateCommitsBeforePromptAndReturn(t *testing.T) {
	committer := &approvalCommitterStub{}
	gate := &approvalGateStub{prepared: &committer.prepared}
	durable := NewDurableApprovalGate(gate)
	ctx := engine.WithInteractionInvocationTracker(
		engine.WithInteractionCommitter(context.Background(), committer),
		engine.NewInteractionInvocationTracker(),
	)
	record, err := durable.RequestApproval(ctx, "approve", "verify")
	if err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}
	if gate.called != 1 || !committer.accepted || record.Approver != "operator" {
		t.Fatalf("called=%d accepted=%v record=%#v", gate.called, committer.accepted, record)
	}
}

func TestDurableApprovalGateRejectsMalformedRestoredRecord(t *testing.T) {
	answer := json.RawMessage(`{"record":{"approver":"","approved_at":"invalid","token":""}}`)
	committer := &approvalCommitterStub{restored: &engine.InteractionState{
		Status: engine.InteractionStatusAnswered, Answer: answer,
	}}
	ctx := engine.WithInteractionInvocationTracker(
		engine.WithInteractionCommitter(context.Background(), committer),
		engine.NewInteractionInvocationTracker(),
	)
	if _, err := NewDurableApprovalGate(&approvalGateStub{}).RequestApproval(ctx, "approve", "verify"); err == nil {
		t.Fatal("malformed restored approval was accepted")
	}
}
