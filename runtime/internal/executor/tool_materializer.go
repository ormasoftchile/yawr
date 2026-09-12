package executor

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"

	internalplanner "github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/expand"
	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/pkgsubst"
	plannerpkg "github.com/ormasoftchile/yawr/runtime/pkg/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

type toolSubstitutionMaterializer interface {
	MaterializeToolSubstitutions(context.Context, *engine.ExecutionPlan) error
}

func MaterializeToolSubstitutions(ctx context.Context, registry engine.ExecutorRegistry, plan *engine.ExecutionPlan) error {
	missing, err := validateFrozenToolSubstitutions(plan)
	if err != nil {
		return err
	}
	if !missing {
		return nil
	}
	if registry == nil {
		return errors.New("tool substitution materialization requires an executor registry")
	}
	materializer, ok := registry.Lookup("tool").(toolSubstitutionMaterializer)
	if !ok {
		return errors.New("tool substitution materialization requires a capable tool executor")
	}
	return materializer.MaterializeToolSubstitutions(ctx, plan)
}

func validateFrozenToolSubstitutions(plan *engine.ExecutionPlan) (bool, error) {
	if plan == nil {
		return false, nil
	}
	missing := false
	for _, definition := range plan.Tools {
		if definition == nil {
			continue
		}
		for _, action := range definition.Actions {
			if action != nil && action.Execute != nil && action.Execute.IsSubstitution() {
				if action.FrozenSubstitution == nil {
					missing = true
					continue
				}
				if err := plansnapshot.ValidateFrozenToolSubstitution(action.FrozenSubstitution); err != nil {
					return false, err
				}
				flow, err := plansnapshot.RestoreFlowClosure(action.FrozenSubstitution.ExecutableClosure)
				if err != nil {
					return false, err
				}
				// Saved definitions must carry the same dependency closure as
				// newly frozen ones; a frozen flag alone is not completeness.
				probe := frozenToolMaterializer{plan: plan, checkOnly: true}
				if err := probe.freezeNestedTools(context.Background(), flow, nil, nil); err != nil {
					missing = true
				}
			}
		}
	}
	return missing, nil
}

func (e *ToolExecutor) MaterializeToolSubstitutions(ctx context.Context, plan *engine.ExecutionPlan) error {
	if plan == nil {
		return errors.New("tool executor: plan is required")
	}
	lookup, ok := e.runtime.(tool.ToolDefLookup)
	if !ok {
		return errors.New("tool executor: tool definition lookup is required to freeze substitutions")
	}
	materializer := frozenToolMaterializer{plan: plan, parser: e.parser, lookup: lookup}
	owned := make(map[string]*schema.ToolDef, len(plan.Tools))
	for name, definition := range plan.Tools {
		owned[name] = schema.CloneToolDef(definition)
	}
	plan.Tools = owned
	toolNames := sortedSchemaToolNames(plan.Tools)
	for _, toolName := range toolNames {
		definition := plan.Tools[toolName]
		if definition == nil {
			continue
		}
		actionNames := sortedSchemaActionNames(definition.Actions)
		for _, actionName := range actionNames {
			action := definition.Actions[actionName]
			if action == nil || action.Execute == nil || !action.Execute.IsSubstitution() {
				continue
			}
			if err := materializer.freezeAction(ctx, toolName, actionName, nil, pkgsubst.EffectiveGovernanceFromTool(definition.Governance)); err != nil {
				return err
			}
		}
	}
	return nil
}

type frozenToolMaterializer struct {
	plan      *engine.ExecutionPlan
	parser    parser.Parser
	lookup    tool.ToolDefLookup
	checkOnly bool
}

func (materializer *frozenToolMaterializer) freezeAction(
	ctx context.Context,
	toolName string,
	actionName string,
	frames []pkgsubst.Frame,
	callerGovernance *pkgsubst.EffectiveGovernance,
) error {
	if definition := materializer.plan.Tools[toolName]; definition != nil {
		if action := definition.Actions[actionName]; action != nil && action.FrozenSubstitution != nil {
			frozen := action.FrozenSubstitution
			if err := plansnapshot.ValidateFrozenToolSubstitution(frozen); err != nil {
				return err
			}
			flow, err := plansnapshot.RestoreFlowClosure(frozen.ExecutableClosure)
			if err != nil {
				return err
			}
			sub := &schema.Runbook{ID: frozen.RunbookID, Name: frozen.RunbookName, Flow: flow,
				Inputs: frozen.Inputs, Outputs: frozen.Outputs, Governance: frozen.Governance}
			planned, errs := pkgsubst.PlanFrozen(action, actionName, frozen.PackageName, toolName,
				frozen.RunbookPath, sub, pkgsubst.PlanOptions{Frames: frames, CallerGovernance: callerGovernance})
			if len(errs) > 0 {
				return errs[0]
			}
			childFrames := append(append([]pkgsubst.Frame(nil), frames...), planned.Frame)
			return materializer.freezeNestedTools(ctx, flow, childFrames, planned.EffectiveGovernance)
		}
	}
	runtimeDefinition, found := materializer.lookup.LookupDef(toolName)
	if !found || runtimeDefinition == nil {
		return fmt.Errorf("tool substitution materialization: tool %q is unavailable", toolName)
	}
	runtimeAction := runtimeDefinition.Actions[actionName]
	if runtimeAction == nil || runtimeAction.SchemaAction() == nil {
		return fmt.Errorf("tool substitution materialization: action %s#%s has no schema definition", toolName, actionName)
	}
	definition, action, err := materializer.ensurePlanAction(runtimeDefinition, actionName)
	if err != nil {
		return err
	}
	if materializer.parser == nil {
		return errors.New("tool executor: substitution parser is required to freeze the plan")
	}
	planned, planningErrors := pkgsubst.Plan(
		runtimeAction.SchemaAction(), actionName, runtimeDefinition.PackageName, runtimeDefinition.Name,
		pkgsubst.PlanOptions{
			ToolFilePath: runtimeDefinition.SourcePath, PackageRoot: runtimeDefinition.PackageRoot,
			Frames: frames, CallerGovernance: callerGovernance, Parser: materializer.parser,
		},
	)
	if len(planningErrors) > 0 {
		return fmt.Errorf("tool substitution materialization: %s#%s: %w", toolName, actionName, planningErrors[0])
	}
	if planned == nil || planned.SubstituteRunbook == nil {
		return fmt.Errorf("tool substitution materialization: %s#%s produced no substitute", toolName, actionName)
	}
	if err := materializer.expandStaticIncludes(ctx, planned.SubstitutePath, planned.SubstituteRunbook); err != nil {
		return fmt.Errorf("tool substitution materialization: %s#%s: %w", toolName, actionName, err)
	}
	closure, err := plansnapshot.EncodeInvocationFlowClosure(planned.SubstituteRunbook.Flow, schema.InvocationForRunbook(planned.SubstituteRunbook.Bindings, planned.SubstituteRunbook.Outputs, planned.SubstituteRunbook.Flow))
	if err != nil {
		return fmt.Errorf("tool substitution materialization: %s#%s: %w", toolName, actionName, err)
	}
	contentHash, err := graphdoc.RunbookContentHash(planned.SubstituteRunbook)
	if err != nil {
		return fmt.Errorf("tool substitution materialization: %s#%s content hash: %w", toolName, actionName, err)
	}
	action.FrozenSubstitution = &schema.FrozenToolSubstitution{
		Bindings:    planned.SubstituteRunbook.Bindings,
		PackageName: runtimeDefinition.PackageName, RunbookPath: planned.SubstitutePath,
		RunbookID: planned.SubstituteRunbook.ID, RunbookName: planned.SubstituteRunbook.Name,
		RunbookContentHash: contentHash, ExecutableClosure: closure,
		Inputs: planned.SubstituteRunbook.Inputs, Outputs: planned.SubstituteRunbook.Outputs,
		Governance: planned.SubstituteRunbook.Governance,
	}
	definition.Actions[actionName] = action
	childFrames := append(append([]pkgsubst.Frame(nil), frames...), planned.Frame)
	return materializer.freezeNestedTools(ctx, planned.SubstituteRunbook.Flow, childFrames, planned.EffectiveGovernance)
}

type substitutionRunbookLoader struct{ parser parser.Parser }

func (loader substitutionRunbookLoader) Load(ctx context.Context, path string) (*parser.ParsedRunbook, error) {
	return loader.parser.Parse(ctx, path)
}

type substitutionToolRegistry struct{ materializer *frozenToolMaterializer }

func (registry substitutionToolRegistry) Lookup(
	_ context.Context,
	name string,
	actionName string,
) (*schema.ToolDef, error) {
	runtimeDefinition, found := registry.materializer.lookup.LookupDef(name)
	if !found || runtimeDefinition == nil {
		return nil, plannerpkg.ErrToolNotFound
	}
	if runtimeDefinition.Actions[actionName] == nil {
		return nil, plannerpkg.ErrActionNotFound
	}
	definition, _, err := registry.materializer.ensurePlanAction(runtimeDefinition, actionName)
	return definition, err
}

func (materializer *frozenToolMaterializer) expandStaticIncludes(
	ctx context.Context,
	source string,
	runbook *schema.Runbook,
) error {
	parsed := &parser.ParsedRunbook{Source: source, Runbook: runbook}
	plannerImpl := internalplanner.New(plannerpkg.Config{
		Loader:  substitutionRunbookLoader{parser: materializer.parser},
		Tools:   substitutionToolRegistry{materializer: materializer},
		BaseDir: filepath.Dir(source), ExpandPolicy: expand.Policy{Default: expand.ModeEager},
	})
	_, err := plannerImpl.Plan(ctx, parsed)
	return err
}

func (materializer *frozenToolMaterializer) freezeNestedTools(
	ctx context.Context,
	nodes []schema.FlowNode,
	frames []pkgsubst.Frame,
	callerGovernance *pkgsubst.EffectiveGovernance,
) error {
	for _, node := range nodes {
		switch {
		case node.Step != nil:
			if err := materializer.freezeNestedStep(ctx, node.Step, frames, callerGovernance); err != nil {
				return err
			}
		case node.Iterate != nil:
			if err := materializer.freezeNestedTools(ctx, node.Iterate.Steps, frames, callerGovernance); err != nil {
				return err
			}
		case node.Parallel != nil:
			for _, branch := range node.Parallel.Branches {
				if err := materializer.freezeNestedTools(ctx, branch.Steps, frames, callerGovernance); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (materializer *frozenToolMaterializer) freezeNestedStep(
	ctx context.Context,
	step *schema.Step,
	frames []pkgsubst.Frame,
	callerGovernance *pkgsubst.EffectiveGovernance,
) error {
	if step == nil {
		return nil
	}
	if step.Type == schema.StepTypeTool && step.ToolCall != nil {
		toolName := step.ToolCall.Tool.Name
		actionName := step.ToolCall.Tool.Action
		if actionName == "" {
			actionName = "run"
		}
		var action *schema.ToolAction
		if definition := materializer.plan.Tools[toolName]; definition != nil {
			action = definition.Actions[actionName]
		}
		if action == nil {
			if materializer.checkOnly {
				return fmt.Errorf("tool substitution materialization: missing nested action %s#%s", toolName, actionName)
			}
			runtimeDefinition, found := materializer.lookup.LookupDef(toolName)
			if !found || runtimeDefinition == nil || runtimeDefinition.Actions[actionName] == nil {
				return fmt.Errorf("tool substitution materialization: nested action %s#%s is unavailable", toolName, actionName)
			}
			var err error
			_, action, err = materializer.ensurePlanAction(runtimeDefinition, actionName)
			if err != nil {
				return err
			}
		}
		if !materializer.checkOnly && action.Execute != nil && action.Execute.IsSubstitution() {
			if err := materializer.freezeAction(ctx, toolName, actionName, frames, callerGovernance); err != nil {
				return err
			}
		}
	}
	if step.IncludeSpec != nil {
		if err := materializer.freezeNestedTools(ctx, step.IncludeSpec.ResolvedSteps, frames, callerGovernance); err != nil {
			return err
		}
	}
	if step.BranchSpec != nil {
		for _, branch := range step.BranchSpec.Branches {
			if err := materializer.freezeNestedTools(ctx, branch.Steps, frames, callerGovernance); err != nil {
				return err
			}
		}
	}
	if step.CompensateSpec != nil {
		return materializer.freezeNestedTools(ctx, step.CompensateSpec.Compensate.Steps, frames, callerGovernance)
	}
	return nil
}

func (materializer *frozenToolMaterializer) ensurePlanAction(
	runtimeDefinition *tool.ToolDef,
	actionName string,
) (*schema.ToolDef, *schema.ToolAction, error) {
	definition := materializer.plan.Tools[runtimeDefinition.Name]
	if definition == nil {
		definition = schema.CloneToolDef(&schema.ToolDef{Name: runtimeDefinition.Name, Governance: runtimeDefinition.Governance, Actions: make(map[string]*schema.ToolAction)})
		materializer.plan.Tools[runtimeDefinition.Name] = definition
	}
	if definition.Actions == nil {
		definition.Actions = make(map[string]*schema.ToolAction)
	}
	action := definition.Actions[actionName]
	if action == nil {
		runtimeAction := runtimeDefinition.Actions[actionName]
		if runtimeAction == nil || runtimeAction.SchemaAction() == nil {
			return nil, nil, fmt.Errorf("tool substitution materialization: action %s#%s has no schema definition", runtimeDefinition.Name, actionName)
		}
		action = schema.CloneToolAction(runtimeAction.SchemaAction())
		definition.Actions[actionName] = action
	}
	return definition, action, nil
}

func sortedSchemaToolNames(definitions map[string]*schema.ToolDef) []string {
	names := make([]string, 0, len(definitions))
	for name := range definitions {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortedSchemaActionNames(actions map[string]*schema.ToolAction) []string {
	names := make([]string, 0, len(actions))
	for name := range actions {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
