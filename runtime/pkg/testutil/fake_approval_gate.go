package testutil

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/governance"
)

// FakeApprovalGate is a controllable ApprovalGate for use in unit tests.
// It implements governance.ApprovalGate with thread-safe call recording.
type FakeApprovalGate struct {
	// Approve controls whether RequestApproval returns success or error.
	Approve bool

	// Approver is the identity to include in the ApprovalRecord.
	Approver string

	// Error is returned when Approve is false.
	Error error

	// Calls records all RequestApproval invocations.
	Calls []ApprovalCall
	mu    sync.Mutex
}

// ApprovalCall records a single invocation of RequestApproval.
type ApprovalCall struct {
	StepID string
	Reason string
	At     time.Time
}

// Ensure FakeApprovalGate implements governance.ApprovalGate at compile time.
var _ governance.ApprovalGate = (*FakeApprovalGate)(nil)

// NewFakeApprovalGate returns a FakeApprovalGate configured for approve/reject mode.
func NewFakeApprovalGate(approve bool) *FakeApprovalGate {
	return &FakeApprovalGate{
		Approve:  approve,
		Approver: "test-approver",
		Error:    errors.New("approval rejected"),
	}
}

// RequestApproval simulates an approval gate request.
// Thread-safe. Respects context cancellation.
func (f *FakeApprovalGate) RequestApproval(ctx context.Context, stepID string, reason string) (governance.ApprovalRecord, error) {
	f.mu.Lock()
	f.Calls = append(f.Calls, ApprovalCall{StepID: stepID, Reason: reason, At: time.Now()})
	f.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return governance.ApprovalRecord{}, err
	}
	if !f.Approve {
		return governance.ApprovalRecord{}, f.Error
	}
	return governance.ApprovalRecord{
		Approver:   f.Approver,
		ApprovedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Token:      fmt.Sprintf("fake-token-%d", time.Now().UnixNano()),
	}, nil
}
