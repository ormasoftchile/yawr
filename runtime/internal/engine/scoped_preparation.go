package engine

import (
	"context"
	"errors"

	internalplanner "github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	enginepkg "github.com/ormasoftchile/yawr/runtime/pkg/engine"
)

func withFrozenPlanContext(ctx context.Context, plan *enginepkg.ExecutionPlan) context.Context {
	if plan == nil {
		return ctx
	}
	ctx = enginepkg.WithPlanTools(ctx, plan.Tools)
	ctx = enginepkg.WithToolScopes(ctx, plan.ToolScopes)
	ctx = enginepkg.WithPlanProfile(ctx, plan.Metadata.Profile)
	return enginepkg.WithPlanStepPresentation(ctx, plan.Steps)
}

func validateScopedSubstitutionBindings(plan *enginepkg.ExecutionPlan) error {
	if plan.ToolScopes == nil {
		return nil
	}
	for _, definition := range plan.ToolScopes.Export().Definitions {
		if definition.Declaration == nil {
			return errors.New("engine: scoped definition requires its captured declaration")
		}
		for _, action := range definition.Declaration.Actions {
			if action == nil || action.FrozenSubstitution == nil {
				continue
			}
			frozen := action.FrozenSubstitution
			flow, err := plansnapshot.RestoreScopedFlowClosure(frozen.ExecutableClosure, plan.ToolScopes, frozen.TargetScopeID)
			if err != nil {
				return err
			}
			if err := internalplanner.ValidateScopedTypedBoundFlow(flow, frozen.Bindings, plan.ToolScopes, frozen.TargetScopeID); err != nil {
				return err
			}
		}
	}
	return nil
}
