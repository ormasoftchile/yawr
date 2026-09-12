package executor

import (
	"context"

	"github.com/ormasoftchile/yawr/runtime/pkg/governance"
)

type runApprovalGateContextKey struct{}

func WithRunApprovalGate(ctx context.Context, gate governance.ApprovalGate) context.Context {
	if gate == nil {
		return ctx
	}
	return context.WithValue(ctx, runApprovalGateContextKey{}, gate)
}

func approvalGateFromContext(ctx context.Context) governance.ApprovalGate {
	return RunApprovalGateFromContext(ctx)
}

func RunApprovalGateFromContext(ctx context.Context) governance.ApprovalGate {
	gate, _ := ctx.Value(runApprovalGateContextKey{}).(governance.ApprovalGate)
	return gate
}
