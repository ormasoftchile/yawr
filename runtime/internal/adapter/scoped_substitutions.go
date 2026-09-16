package adapter

import (
	"context"
	"errors"
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/pkgsubst"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/tool"
	"github.com/ormasoftchile/yawr/runtime/pkg/toolscope"
)

func materializeScopedDefinitions(ctx context.Context, loader *ScopedRunbookLoader, snapshot *toolscope.Snapshot) error {
	for _, source := range loader.closure.Documents {
		scopeID := loader.discoveryScopes[source.ID]
		for _, binding := range source.Bindings {
			bindingID := snapshot.Scopes[scopeID].Bindings[binding.Name]
			id := snapshot.Bindings[bindingID].DefinitionID
			definition := snapshot.Definitions[id]
			for actionName, action := range definition.Declaration.Actions {
				if action == nil {
					return fmt.Errorf("scoped preparation: missing action %q", actionName)
				}
				if !action.Execute.IsSubstitution() {
					continue
				}
				var targetScope string
				for _, edge := range source.Substitutions {
					if edge.StepID == binding.Name+"."+actionName {
						if targetScope != "" {
							return fmt.Errorf("scoped preparation: ambiguous substitution source")
						}
						targetScope = loader.discoveryScopes[edge.Target]
					}
				}
				target := loader.documents[targetScope]
				if target == nil {
					return fmt.Errorf("scoped preparation: %s.%s has no captured substitution source", binding.Name, actionName)
				}
				child, err := loader.LoadScope(ctx, target.Path, targetScope)
				if err != nil {
					return err
				}
				issues := schema.ValidateToolActionEnumDefaults(definition.Runtime.Name, actionName, action)
				if len(issues) > 0 {
					return errors.Join(issues...)
				}
				_, issues = pkgsubst.PlanFrozen(action, actionName, definition.Runtime.PackageName, definition.Runtime.Name,
					target.Path, child.Runbook, pkgsubst.PlanOptions{CallerGovernance: pkgsubst.EffectiveGovernanceFromTool(definition.Runtime.Governance)})
				if len(issues) != 0 {
					return errors.Join(issues...)
				}
				body, err := plansnapshot.EncodeScopedInvocationFlowClosure(child.Runbook.Flow,
					schema.InvocationForRunbook(child.Runbook.Bindings, child.Runbook.Outputs, child.Runbook.Flow), loader.scopes, targetScope)
				if err != nil {
					return err
				}
				hash, err := graphdoc.RunbookContentHash(child.Runbook)
				if err != nil {
					return err
				}
				action.FrozenSubstitution = &schema.FrozenToolSubstitution{
					SchemaVersion: plansnapshot.FrozenToolSubstitutionSchemaV2, TargetScopeID: targetScope,
					PackageName: definition.Runtime.PackageName, RunbookPath: target.Path,
					RunbookID: child.Runbook.ID, RunbookName: child.Runbook.Name, RunbookContentHash: hash,
					ExecutableClosure: body, Inputs: child.Runbook.Inputs, Bindings: child.Runbook.Bindings,
					Outputs: child.Runbook.Outputs, Governance: child.Runbook.Governance,
				}
				definition.Runtime.Actions[actionName].WithSchemaAction(schema.CloneToolAction(action))
			}
			actual, err := tool.DefinitionID(definition)
			if err != nil {
				return err
			}
			if actual != id {
				return fmt.Errorf("scoped preparation: substitution materialization changed binding identity")
			}
			snapshot.Definitions[id] = definition
		}
	}
	return nil
}

func validateScopedBodies(scopes *toolscope.Set) error {
	for _, definition := range scopes.Export().Definitions {
		for name, action := range definition.Declaration.Actions {
			if action.Execute.IsSubstitution() {
				if err := plansnapshot.ValidateFrozenToolSubstitution(action.FrozenSubstitution, scopes); err != nil {
					return err
				}
				if err := validateScopedSubstitution(definition, name, scopes, nil,
					pkgsubst.EffectiveGovernanceFromTool(definition.Runtime.Governance)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validateScopedSubstitution(definition tool.BoundDefinition, name string, scopes *toolscope.Set,
	frames []string, governance *pkgsubst.EffectiveGovernance) error {
	action := definition.Declaration.Actions[name]
	if action == nil || !action.Execute.IsSubstitution() {
		return nil
	}
	id, err := tool.DefinitionID(definition)
	if err != nil {
		return err
	}
	frame := id + "#" + name
	for _, active := range frames {
		if active == frame {
			return errkit.New("PKG-015", fmt.Sprintf("scoped substitution cycle at %s#%s", definition.Runtime.Name, name))
		}
	}
	if len(frames) >= pkgsubst.MaxDepth {
		return errkit.New("PKG-028", "scoped substitution nesting exceeds maximum depth")
	}
	frozen := action.FrozenSubstitution
	if err := plansnapshot.ValidateFrozenToolSubstitution(frozen, scopes); err != nil {
		return err
	}
	flow, err := plansnapshot.RestoreScopedFlowClosure(frozen.ExecutableClosure, scopes, frozen.TargetScopeID)
	if err != nil {
		return err
	}
	runbook := &schema.Runbook{ID: frozen.RunbookID, Name: frozen.RunbookName, Flow: flow,
		Inputs: frozen.Inputs, Bindings: frozen.Bindings, Outputs: frozen.Outputs, Governance: frozen.Governance}
	issues := schema.ValidateToolActionEnumDefaults(definition.Runtime.Name, name, action)
	if len(issues) > 0 {
		return errors.Join(issues...)
	}
	planned, issues := pkgsubst.PlanFrozen(action, name, definition.Runtime.PackageName, definition.Runtime.Name,
		frozen.RunbookPath, runbook, pkgsubst.PlanOptions{CallerGovernance: governance})
	if len(issues) != 0 {
		return errors.Join(issues...)
	}
	next := append(append([]string(nil), frames...), frame)
	return walkScopedSubstitutions(flow, scopes, next, planned.EffectiveGovernance)
}

func walkScopedSubstitutions(nodes []schema.FlowNode, scopes *toolscope.Set,
	frames []string, governance *pkgsubst.EffectiveGovernance) error {
	walk := func(nodes []schema.FlowNode) error { return walkScopedSubstitutions(nodes, scopes, frames, governance) }
	for _, node := range nodes {
		if step := node.Step; step != nil {
			if call := step.ToolCall; call != nil {
				bound, err := scopes.Resolve(step.LexicalScopeID, call.Tool.Name, call.Tool.Action)
				if err != nil {
					return err
				}
				if err := validateScopedSubstitution(bound.Definition, call.Tool.Action, scopes, frames, governance); err != nil {
					return err
				}
			}
			if step.IncludeSpec != nil {
				if err := walk(step.IncludeSpec.ResolvedSteps); err != nil {
					return err
				}
			}
			if step.BranchSpec != nil {
				for _, branch := range step.BranchSpec.Branches {
					if err := walk(branch.Steps); err != nil {
						return err
					}
				}
			}
			if step.CompensateSpec != nil {
				if err := walk(step.CompensateSpec.Compensate.Steps); err != nil {
					return err
				}
			}
		}
		if node.Iterate != nil {
			if err := walk(node.Iterate.Steps); err != nil {
				return err
			}
		}
		if node.Parallel != nil {
			for _, branch := range node.Parallel.Branches {
				if err := walk(branch.Steps); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
