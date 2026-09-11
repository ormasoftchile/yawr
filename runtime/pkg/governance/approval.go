package governance

import "context"

// ApprovalGate requests human approval before a step executes.
// Implementations are injected into the engine at construction time.
//
// Contract:
//   - RequestApproval blocks until approval is granted or denied
//   - Returns ApprovalRecord on approval, error on rejection or timeout
//   - The context carries cancellation (e.g., run cancelled while waiting)
//   - Implementations MUST NOT call back into the engine (no re-entrancy)
type ApprovalGate interface {
	RequestApproval(ctx context.Context, stepID string, reason string) (ApprovalRecord, error)
}
