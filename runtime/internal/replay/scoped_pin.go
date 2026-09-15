package replay

import (
	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/toolscope"
)

func restoreReplayPin(pin schema.LockedDynamicInclude, scopes *toolscope.Set) ([]schema.FlowNode, error) {
	if scopes == nil {
		return plansnapshot.RestoreFlowClosure(pin.ExecutableClosure)
	}
	if err := plansnapshot.ValidateDynamicIncludePin(pin, scopes); err != nil {
		return nil, err
	}
	return plansnapshot.RestoreScopedFlowClosure(pin.ExecutableClosure, scopes, pin.TargetScopeID)
}
