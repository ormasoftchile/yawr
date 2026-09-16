package sessioncoordinator

import (
	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func restoreRevisionPin(plan *engine.ExecutionPlan, pin schema.LockedDynamicInclude) ([]schema.FlowNode, error) {
	if plan.ToolScopes != nil {
		if err := plansnapshot.ValidateDynamicIncludePin(pin, plan.ToolScopes); err != nil {
			return nil, err
		}
		return plansnapshot.RestoreScopedFlowClosure(pin.ExecutableClosure, plan.ToolScopes, pin.TargetScopeID)
	}
	if err := plansnapshot.ValidateDynamicIncludePin(pin); err != nil {
		return nil, err
	}
	return plansnapshot.RestoreFlowClosure(pin.ExecutableClosure)
}
