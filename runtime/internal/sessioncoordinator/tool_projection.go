package sessioncoordinator

import (
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/internal/adapter"
	internalplanner "github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func frozenToolProjection(plan *engine.ExecutionPlan, step engine.ResolvedStep) (*engine.ExecutionPlan, error) {
	call, ok := step.Spec.(*schema.ToolCallSpec)
	if plan.ToolScopes == nil || !ok || call == nil {
		return nil, nil
	}
	definition, err := engine.FrozenToolDefinition(plan, step)
	if err != nil {
		return nil, err
	}
	actionName := call.Tool.Action
	if actionName == "" {
		actionName = "run"
	}
	action := definition.Actions[actionName]
	if action == nil || action.FrozenSubstitution == nil {
		return nil, nil
	}
	frozen := action.FrozenSubstitution
	flow, err := plansnapshot.RestoreScopedFlowClosure(frozen.ExecutableClosure, plan.ToolScopes, frozen.TargetScopeID)
	if err != nil {
		return nil, err
	}
	metadata := plan.Metadata
	metadata.DynamicIncludes = nil
	metadata.GraphContentHash = ""
	metadata.RunbookID, metadata.RunbookName = frozen.RunbookID, frozen.RunbookName
	metadata.RunbookContentHash = frozen.RunbookContentHash
	projection := &engine.ExecutionPlan{
		RunbookPath: frozen.RunbookPath, RootScopeID: frozen.TargetScopeID,
		ToolScopes: plan.ToolScopes, Metadata: metadata,
		Inputs: frozen.Inputs, Bindings: frozen.Bindings, Outputs: frozen.Outputs,
	}
	for _, node := range flow {
		childStep, ok := adapter.ResolveFlowNode(node)
		if !ok {
			return nil, fmt.Errorf("session coordinator: frozen tool graph contains an invalid flow node")
		}
		if node.Step == nil {
			childStep.LexicalScopeID = frozen.TargetScopeID
		}
		projection.Steps = append(projection.Steps, childStep)
	}
	if err := internalplanner.ValidateExecutionPlan(projection); err != nil {
		return nil, err
	}
	if err := internalplanner.FinalizeMaterializedPlan(projection); err != nil {
		return nil, err
	}
	return projection, nil
}
