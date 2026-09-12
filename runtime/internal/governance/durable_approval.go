package governance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	governancepkg "github.com/ormasoftchile/yawr/runtime/pkg/governance"
)

type durableApprovalGate struct {
	delegate governancepkg.ApprovalGate
}

type durableInteractionProvider interface {
	CommitsInteractionsDurably() bool
}

func NewDurableApprovalGate(gate governancepkg.ApprovalGate) governancepkg.ApprovalGate {
	if gate == nil {
		return nil
	}
	if _, automated := gate.(*NoOpApprovalGate); automated {
		return gate
	}
	if durable, ok := gate.(durableInteractionProvider); ok && durable.CommitsInteractionsDurably() {
		return gate
	}
	return &durableApprovalGate{delegate: gate}
}

func (gate *durableApprovalGate) RequestApproval(
	ctx context.Context,
	stepID string,
	reason string,
) (governancepkg.ApprovalRecord, error) {
	committer := engine.InteractionCommitterFromContext(ctx)
	if committer == nil {
		return gate.delegate.RequestApproval(ctx, stepID, reason)
	}
	tracker := engine.InteractionInvocationTrackerFromContext(ctx)
	if tracker == nil {
		return governancepkg.ApprovalRecord{}, errors.New("durable approval: interaction invocation tracker is required")
	}
	boundary, ok := engine.DispatchExecutionBoundaryFromContext(ctx)
	if !ok {
		boundary.StepID = stepID
		boundary.CallPath = engine.DebugCallPathFromContext(ctx)
		boundary.QualifiedNodeID = engine.DebugNodeID(boundary.CallPath, stepID)
	}
	request, err := json.Marshal(struct {
		Kind   string `json:"kind"`
		Reason string `json:"reason"`
	}{Kind: "approval", Reason: reason})
	if err != nil {
		return governancepkg.ApprovalRecord{}, err
	}
	committed, err := committer.PrepareInteraction(ctx, engine.InteractionState{
		SchemaVersion: engine.InteractionStateSchemaV1, TurnID: uuid.NewString(),
		NodeID: boundary.QualifiedNodeID, StepID: stepID,
		FrameID: boundary.FrameID, FrameStepIndex: boundary.FrameStepIndex, Kind: "approval",
		Ordinal:       tracker.NextOccurrence(boundary.QualifiedNodeID, "approval", boundary.FrameID, boundary.FrameStepIndex),
		Status:        engine.InteractionStatusPending,
		RequestDigest: engine.InteractionPayloadDigest(request), Request: request,
	})
	if err != nil {
		return governancepkg.ApprovalRecord{}, err
	}
	if committed.Status == engine.InteractionStatusAnswered {
		var answer struct {
			Record governancepkg.ApprovalRecord `json:"record"`
		}
		if err := json.Unmarshal(committed.Answer, &answer); err != nil {
			return governancepkg.ApprovalRecord{}, fmt.Errorf("durable approval: decode answer: %w", err)
		}
		if err := validateApprovalRecord(answer.Record); err != nil {
			return governancepkg.ApprovalRecord{}, err
		}
		return answer.Record, nil
	}
	record, err := gate.delegate.RequestApproval(ctx, stepID, reason)
	if err != nil {
		return governancepkg.ApprovalRecord{}, err
	}
	if err := validateApprovalRecord(record); err != nil {
		return governancepkg.ApprovalRecord{}, err
	}
	answer, err := json.Marshal(struct {
		Record governancepkg.ApprovalRecord `json:"record"`
	}{Record: record})
	if err != nil {
		return governancepkg.ApprovalRecord{}, err
	}
	if _, err := committer.AcceptInteraction(ctx, committed.TurnID, engine.InteractionPayloadDigest(answer), answer); err != nil {
		return governancepkg.ApprovalRecord{}, err
	}
	return record, nil
}

func validateApprovalRecord(record governancepkg.ApprovalRecord) error {
	approver := strings.TrimSpace(record.Approver)
	if approver == "" || len([]byte(approver)) > 256 || record.Token == "" || len(record.Token) > 4096 {
		return errors.New("durable approval: invalid approval record")
	}
	if _, err := time.Parse(time.RFC3339Nano, record.ApprovedAt); err != nil {
		return errors.New("durable approval: invalid approval timestamp")
	}
	return nil
}
