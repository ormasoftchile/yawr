package plansnapshot

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/toolscope"
)

// Scoped closures reference the enclosing set, not a recursively embedded
// copy. The enclosing snapshot digest covers complete generated tool bodies.
func EncodeScopedFlowClosure(nodes []schema.FlowNode, scopes *toolscope.Set, rootScopeID string) (json.RawMessage, error) {
	if scopes == nil || !scopes.HasScope(rootScopeID) {
		return nil, errors.New("plan snapshot: scoped closure requires complete scopes")
	}
	if err := planner.ValidateScopedFlow(nodes, scopes, rootScopeID); err != nil {
		return nil, err
	}
	nodesSnapshot, err := snapshotFlowNodes(nodes)
	if err != nil {
		return nil, err
	}
	closure := flowClosureV1{SchemaVersion: FlowClosureSchemaV4, Nodes: nodesSnapshot, RootScopeID: rootScopeID, ScopeContextDigest: scopes.ContextDigest()}
	closure.ClosureDigest, err = flowClosureDigest(closure)
	if err != nil {
		return nil, err
	}
	return json.Marshal(closure)
}

func RestoreScopedFlowClosure(encoded json.RawMessage, scopes *toolscope.Set, rootScopeID string) ([]schema.FlowNode, error) {
	if scopes == nil || !scopes.HasScope(rootScopeID) {
		return nil, errors.New("plan snapshot: scoped closure requires immutable scopes")
	}
	if _, err := schema.DecodeScopedFlowClosure(encoded); err != nil {
		return nil, err
	}
	var closure flowClosureV1
	if err := decodeStrictJSON(encoded, &closure); err != nil {
		return nil, err
	}
	if closure.SchemaVersion != FlowClosureSchemaV4 || closure.RootScopeID != rootScopeID || closure.ScopeContextDigest != scopes.ContextDigest() || len(closure.Tools) != 0 {
		return nil, errors.New("plan snapshot: scoped closure version or ownership mismatch")
	}
	digest, err := flowClosureDigest(closure)
	if err != nil {
		return nil, err
	}
	if closure.ClosureDigest == "" || closure.ClosureDigest != digest {
		return nil, errors.New("plan snapshot: scoped closure digest mismatch")
	}
	nodes, err := restoreFlowNodes(closure.Nodes)
	if err != nil {
		return nil, err
	}
	if err := planner.ValidateScopedFlow(nodes, scopes, rootScopeID); err != nil {
		return nil, err
	}
	if err := validateScopedInvocation(nodes, closure.Invocation); err != nil {
		return nil, err
	}
	return nodes, nil
}

func EncodeScopedInvocationFlowClosure(nodes []schema.FlowNode, invocation *schema.RunbookInvocation, scopes *toolscope.Set, rootScopeID string) (json.RawMessage, error) {
	if err := validateScopedInvocation(nodes, invocation); err != nil {
		return nil, err
	}
	encoded, err := EncodeScopedFlowClosure(nodes, scopes, rootScopeID)
	if err != nil {
		return nil, err
	}
	var closure flowClosureV1
	if err := decodeStrictJSON(encoded, &closure); err != nil {
		return nil, err
	}
	closure.Invocation = schema.CloneInvocation(invocation)
	closure.ClosureDigest, err = flowClosureDigest(closure)
	if err != nil {
		return nil, err
	}
	return json.Marshal(closure)
}

func ScopedInvocationFromClosure(encoded json.RawMessage, scopes *toolscope.Set, rootScopeID string) (*schema.RunbookInvocation, error) {
	if _, err := RestoreScopedFlowClosure(encoded, scopes, rootScopeID); err != nil {
		return nil, err
	}
	closure, err := schema.DecodeScopedFlowClosure(encoded)
	if err != nil {
		return nil, err
	}
	return schema.CloneInvocation(closure.Invocation), nil
}

func validateScopedInvocation(nodes []schema.FlowNode, invocation *schema.RunbookInvocation) error {
	if invocation == nil {
		return nil
	}
	if invocation.Results != schema.HasResults(nodes) {
		return errors.New("plan snapshot: invocation results do not match declaring flow")
	}
	// Inputs are owned by the enclosing target/tool. Validate local ordering
	// here without mistaking an external input reference for a missing binding.
	local := make(map[string]bool)
	for _, binding := range invocation.Bindings {
		local[binding.Name] = true
	}
	inputs := make(map[string]*schema.Input)
	for _, binding := range invocation.Bindings {
		references, err := schema.TypedTreeReferences(binding.Value)
		if err != nil {
			return err
		}
		for _, name := range references {
			if !local[name] {
				inputs[name] = &schema.Input{Type: "any"}
			}
		}
	}
	return schema.ValidateTypedRunbook(&schema.Runbook{Inputs: inputs, Bindings: invocation.Bindings, Outputs: invocation.Outputs, Flow: nodes})
}

// RestoreScopedDynamicTarget restores eligible content solely from the durable
// set, even if the original runbook and package sources no longer exist.
func RestoreScopedDynamicTarget(scopes *toolscope.Set, qualifiedID string) (toolscope.Target, []schema.FlowNode, error) {
	target, err := scopes.Target(qualifiedID)
	if err != nil {
		return toolscope.Target{}, nil, err
	}
	nodes, err := RestoreScopedFlowClosure(target.ExecutableClosure, scopes, target.ScopeID)
	if err != nil {
		return toolscope.Target{}, nil, err
	}
	if err := validateFlowResumeSafety(nodes, qualifiedID, true); err != nil {
		return toolscope.Target{}, nil, err
	}
	if err := schema.ValidateTypedRunbook(&schema.Runbook{
		Inputs: target.Inputs, Bindings: target.Bindings, Outputs: target.Outputs, Flow: nodes,
	}); err != nil {
		return toolscope.Target{}, nil, err
	}
	return target, nodes, nil
}

func invocationDeclarationsMatch(invocation *schema.RunbookInvocation, bindings []schema.Binding, outputs map[string]*schema.Output) bool {
	if invocation == nil {
		return len(bindings) == 0 && len(outputs) == 0
	}
	expected := &schema.RunbookInvocation{Bindings: bindings, Outputs: outputs, Results: invocation.Results}
	actualJSON, err := json.Marshal(invocation)
	if err != nil {
		return false
	}
	expectedJSON, err := json.Marshal(expected)
	return err == nil && bytes.Equal(actualJSON, expectedJSON)
}

func validateScopedArtifacts(plan *engine.ExecutionPlan) error {
	for _, pin := range plan.Metadata.DynamicIncludes {
		if err := ValidateDynamicIncludePin(pin, plan.ToolScopes); err != nil {
			return err
		}
	}
	if plan.ToolScopes == nil {
		return nil
	}
	if len(plan.Tools) != 0 {
		return errors.New("plan snapshot: scoped plans cannot carry name-only tool definitions")
	}
	for qualifiedID := range plan.ToolScopes.Export().DynamicTargets {
		if _, _, err := RestoreScopedDynamicTarget(plan.ToolScopes, qualifiedID); err != nil {
			return err
		}
	}
	for _, definition := range plan.ToolScopes.Export().Definitions {
		if definition.Declaration != nil {
			for _, action := range definition.Declaration.Actions {
				if action == nil {
					return errors.New("plan snapshot: nil scoped action")
				}
				if action.FrozenSubstitution != nil || action.Execute.IsSubstitution() {
					if err := ValidateFrozenToolSubstitution(action.FrozenSubstitution, plan.ToolScopes); err != nil {
						return err
					}
				}
			}
		}
		for _, action := range definition.Runtime.Actions {
			if action == nil {
				return errors.New("plan snapshot: nil scoped runtime action")
			}
			retained := action.SchemaAction()
			if retained != nil && (retained.FrozenSubstitution != nil || retained.Execute.IsSubstitution()) {
				if err := ValidateFrozenToolSubstitution(retained.FrozenSubstitution, plan.ToolScopes); err != nil {
					return err
				}
			}
			if action.Execute.IsSubstitution() && (retained == nil || retained.FrozenSubstitution == nil) {
				return errors.New("plan snapshot: scoped substitution has no retained closure")
			}
		}
	}
	return nil
}
