package executor

import (
	"context"

	igov "github.com/ormasoftchile/yawr/runtime/internal/governance"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	tracepkg "github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

// WithEntryGovernance seeds ctx with the entry runbook's governance so that
// executeDynamic can compose child governance against the root policy rather
// than against nil. Must be called before eng.Start so every dynamic include
// execution sees the correct parent governance (DEF-006).
//
// When gov is nil (no governance: block on the entry runbook) the context is
// returned unchanged and the behaviour is the same as before the fix.
func WithEntryGovernance(ctx context.Context, gov *schema.GovernanceConfig) context.Context {
	if gov == nil {
		return ctx
	}
	egp := &tracepkg.EffectiveGovernancePayload{
		RequireApproval: gov.RequireApproval,
		AllowCommands:   gov.AllowCommands,
		DenyCommands:    gov.DenyCommands,
		DenyEnvVars:     gov.DenyEnvVars,
		// Redact and Capabilities are not present on schema.GovernanceConfig.
	}
	ctx = withDynGov(ctx, egp)
	ctx = withGovPolicy(ctx, igov.BuildPolicy(gov))
	return ctx
}
