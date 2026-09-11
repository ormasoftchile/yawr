package governance

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/ormasoftchile/yawr/runtime/pkg/governance"
)

// NoOpApprovalGate always approves immediately without human interaction.
// Returns a random UUID token for audit trail.
type NoOpApprovalGate struct{}

// NewNoOpApprovalGate constructs a no-op approval gate.
func NewNoOpApprovalGate() *NoOpApprovalGate {
	return &NoOpApprovalGate{}
}

// RequestApproval always returns approval with a generated token.
func (g *NoOpApprovalGate) RequestApproval(ctx context.Context, stepID string, reason string) (governance.ApprovalRecord, error) {
	if err := ctx.Err(); err != nil {
		return governance.ApprovalRecord{}, err
	}

	return governance.ApprovalRecord{
		Approver:   "noop-gate",
		ApprovedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Token:      uuid.New().String(),
	}, nil
}

// TerminalApprovalGate prompts for approval on stdin/stdout.
type TerminalApprovalGate struct {
	in  io.Reader
	out io.Writer
}

// NewTerminalApprovalGate constructs a terminal approval gate.
func NewTerminalApprovalGate(in io.Reader, out io.Writer) *TerminalApprovalGate {
	return &TerminalApprovalGate{
		in:  in,
		out: out,
	}
}

// RequestApproval prompts the user and waits for input.
func (g *TerminalApprovalGate) RequestApproval(ctx context.Context, stepID string, reason string) (governance.ApprovalRecord, error) {
	// Check context before blocking
	if err := ctx.Err(); err != nil {
		return governance.ApprovalRecord{}, err
	}

	// Prompt user
	fmt.Fprintf(g.out, "Approval required for step %s: %s\n", stepID, reason)
	fmt.Fprintf(g.out, "Enter approver identity (e.g., email): ")

	// Read response (this blocks - real implementation would use context-aware I/O)
	var approver string
	if _, err := fmt.Fscanln(g.in, &approver); err != nil {
		return governance.ApprovalRecord{}, fmt.Errorf("failed to read approval: %w", err)
	}

	if approver == "" {
		return governance.ApprovalRecord{}, fmt.Errorf("approval rejected: no approver provided")
	}

	return governance.ApprovalRecord{
		Approver:   approver,
		ApprovedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Token:      uuid.New().String(),
	}, nil
}
