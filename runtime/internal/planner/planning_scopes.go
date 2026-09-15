package planner

import (
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/flowwalk"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/toolscope"
)

func planningDocumentScope(ctx flowwalk.Ctx) string {
	if ctx.Runbook == nil || ctx.Runbook.Runbook == nil {
		return ""
	}
	return ctx.Runbook.Runbook.LexicalScopeID
}

// ValidateScopedFlowBeforePlanning fills binding IDs only from the immutable
// set. Source ownership must already be annotated by the captured-source loader.
func ValidateScopedFlowBeforePlanning(nodes []schema.FlowNode, scopes *toolscope.Set, owner string) error {
	if scopes == nil || !scopes.HasScope(owner) {
		return fmt.Errorf("tool scope: planning requires a frozen document owner")
	}
	var visit func([]schema.FlowNode) error
	var spec func(engine.StepSpec) error
	spec = func(value engine.StepSpec) error {
		switch value := value.(type) {
		case *schema.BranchSpec:
			for _, arm := range value.Branches {
				if err := visit(arm.Steps); err != nil {
					return err
				}
			}
		case *schema.IterateNode:
			return visit(value.Steps)
		case *schema.ParallelNode:
			for _, branch := range value.Branches {
				if err := visit(branch.Steps); err != nil {
					return err
				}
			}
		case *schema.CompensateSpec:
			return visit(value.Compensate.Steps)
		}
		return nil
	}
	visit = func(nodes []schema.FlowNode) error {
		for _, node := range nodes {
			if step := node.Step; step != nil {
				if step.LexicalScopeID != owner {
					return fmt.Errorf("tool scope: step %q does not belong to parsed document", step.ID)
				}
				if step.ToolCall != nil {
					bound, err := scopes.Resolve(owner, step.ToolCall.Tool.Name, step.ToolCall.Tool.Action)
					if err != nil {
						return err
					}
					if step.ToolBindingID != "" && step.ToolBindingID != bound.BindingID {
						return fmt.Errorf("tool scope: authored binding mismatch")
					}
					step.ToolBindingID = bound.BindingID
				} else if step.ToolBindingID != "" {
					return fmt.Errorf("tool scope: non-tool step carries binding")
				}
				if err := validateIncludeEdge(scopes, owner, step.ID, specForStep(step)); err != nil {
					return err
				}
				if err := spec(specForStep(step)); err != nil {
					return err
				}
			}
			if node.Iterate != nil {
				if err := spec(node.Iterate); err != nil {
					return err
				}
			}
			if node.Parallel != nil {
				if err := spec(node.Parallel); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return visit(nodes)
}

func stepToolDefinition(plan *engine.ExecutionPlan, step engine.ResolvedStep, call *schema.ToolCallSpec) *schema.ToolDef {
	if plan.ToolScopes == nil {
		return plan.Tools[call.Tool.Name]
	}
	bound, err := plan.ToolScopes.Resolve(step.LexicalScopeID, call.Tool.Name, call.Tool.Action)
	if err != nil || bound.BindingID != step.ToolBindingID {
		return nil
	}
	return bound.Definition.Declaration
}

func validationToolDefinitions(plan *engine.ExecutionPlan) map[string]*schema.ToolDef {
	if plan.ToolScopes == nil {
		return plan.Tools
	}
	tables := plan.ToolScopes.Export()
	definitions := make(map[string]*schema.ToolDef, len(tables.Bindings))
	for id, binding := range tables.Bindings {
		definitions[id] = tables.Definitions[binding.DefinitionID].Declaration
	}
	return definitions
}
