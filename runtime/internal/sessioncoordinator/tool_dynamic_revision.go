package sessioncoordinator

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

var errDynamicRevisionParentUnavailable = errors.New("session coordinator: dynamic include revision parent is unavailable")

func toolRelativeDynamicPin(plan *engine.ExecutionPlan, index int, pin schema.LockedDynamicInclude) (schema.LockedDynamicInclude, bool, error) {
	path, parentID, err := planDynamicStructuralIdentity(plan, index)
	if err != nil {
		return schema.LockedDynamicInclude{}, false, err
	}
	prefix := parentID + "/"
	if !strings.HasPrefix(pin.QualifiedNodeID, prefix) {
		return schema.LockedDynamicInclude{}, false, nil
	}
	path = append(path, schema.DynamicIncludeFrameIdentity{QualifiedNodeID: parentID, Kind: "tool-substitution"})
	if len(pin.StructuralPath) < len(path) || !sameDynamicPlanStructuralPath(pin.StructuralPath[:len(path)], path) {
		return schema.LockedDynamicInclude{}, false, fmt.Errorf("session coordinator: dynamic tool occurrence has an incompatible structural owner")
	}
	local := pin
	local.QualifiedNodeID = strings.TrimPrefix(pin.QualifiedNodeID, prefix)
	local.StructuralPath = append([]schema.DynamicIncludeFrameIdentity(nil), pin.StructuralPath[len(path):]...)
	for index := range local.StructuralPath {
		if !strings.HasPrefix(local.StructuralPath[index].QualifiedNodeID, prefix) {
			return schema.LockedDynamicInclude{}, false, fmt.Errorf("session coordinator: dynamic tool occurrence escapes its structural owner")
		}
		local.StructuralPath[index].QualifiedNodeID = strings.TrimPrefix(local.StructuralPath[index].QualifiedNodeID, prefix)
	}
	return local, true, nil
}

func applyFrozenToolProjectionPins(plan *engine.ExecutionPlan, index int, projection *engine.ExecutionPlan, pins []schema.LockedDynamicInclude) error {
	for _, pin := range pins {
		local, matches, err := toolRelativeDynamicPin(plan, index, pin)
		if err != nil {
			return err
		}
		if matches {
			if err := applyDynamicRevisionPin(projection, local); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateDynamicToolRevisionPin(plan *engine.ExecutionPlan, pin schema.LockedDynamicInclude) error {
	for index, step := range plan.Steps {
		if step.Kind != "tool" {
			continue
		}
		local, matches, err := toolRelativeDynamicPin(plan, index, pin)
		if err != nil {
			return err
		}
		if !matches {
			continue
		}
		projection, err := frozenToolProjection(plan, step)
		if err != nil {
			return err
		}
		if projection == nil {
			return errDynamicRevisionParentUnavailable
		}
		if err := applyFrozenToolProjectionPins(plan, index, projection, plan.Metadata.DynamicIncludes); err != nil {
			return err
		}
		return applyDynamicRevisionPin(projection, local)
	}
	return errDynamicRevisionParentUnavailable
}
