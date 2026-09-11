package main

import (
	"context"
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/pkg/flowwalk"
	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/pkgsubst"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

// validateSubstitutionsPlanTime implements Barbara's B1 blocker fix
// (design/yawr/sections/06-tool-runtime.tex §Action Substitution: "checked
// statically at plan time", quoted in the gate review): every statically
// reachable step.tool node -- including branch arms, iterate/parallel
// bodies, and compensate bodies that may be unreachable given runtime
// conditions or dry-run mode -- whose bound action is execute.kind:
// runbook MUST have its substitution contract (input/output signature,
// cycle, nesting depth, governance widening) validated before the run
// starts, not lazily the first time the step executes
// (internal/executor/tool.go's pkgsubst.Plan call, which remains as
// defense in depth but is no longer the only enforcement point).
//
// It reuses flowwalk.Walker (the same structural traversal the planner and
// preview command already use) so include closures are traversed the same
// way everywhere in the codebase, always recursing into every include
// (never deferring for "lazy" expand, since a static contract violation
// inside a lazily-expanded include is still a plan-time failure per the
// review) and visits every step regardless of its when: condition --
// unreachable-by-condition steps are validated exactly like reachable
// ones, matching the review's explicit "validity is not conditional on
// execution path" requirement.
func validateSubstitutionsPlanTime(ctx context.Context, parserImpl parser.Parser, rb *parser.ParsedRunbook, toolDefs map[string]toolpkg.ToolDef) []error {
	if rb == nil || rb.Runbook == nil {
		return nil
	}
	v := &substitutionPlanVisitor{
		ctx:        ctx,
		parserImpl: parserImpl,
		toolDefs:   toolDefs,
	}
	w := &flowwalk.Walker{Loader: &cliLoader{p: parserImpl}}
	if err := w.Walk(ctx, rb, v); err != nil {
		v.errs = append(v.errs, err)
	}
	return v.errs
}

// substitutionPlanVisitor collects every step.tool invocation reached by
// the walk and, for each one bound to a substituted (execute.kind:
// runbook) action, plans it via pkgsubst.Plan.
type substitutionPlanVisitor struct {
	flowwalk.Base
	ctx        context.Context
	parserImpl parser.Parser
	toolDefs   map[string]toolpkg.ToolDef
	errs       []error
}

// BeforeInclude loads every statically-known include eagerly: B1/§5's
// requirement that the include closure be fully enumerable and validated
// at plan time means a lazily-expanded include's substitution contracts
// still cannot be deferred to execution time. Dynamic includes have no
// statically-known path and are skipped — their substitution contracts
// (if any) are checked when the child is resolved at execution time.
func (v *substitutionPlanVisitor) BeforeInclude(_ flowwalk.Ctx, step *schema.Step) (bool, error) {
	if step.IncludeSpec != nil && step.IncludeSpec.Include.IsDynamic() {
		return false, nil
	}
	return true, nil
}

// EnterInclude always recurses once the child is loaded, for the same
// reason.
func (v *substitutionPlanVisitor) EnterInclude(_ flowwalk.Ctx, _ *schema.Step, childRb *parser.ParsedRunbook) (bool, error) {
	return childRb != nil, nil
}

func (v *substitutionPlanVisitor) EnterStep(ctx flowwalk.Ctx, step *schema.Step) error {
	if step == nil || step.Type != schema.StepTypeTool || step.ToolCall == nil {
		return nil
	}
	inv := step.ToolCall.Tool
	toolDef, ok := v.toolDefs[inv.Name]
	if !ok {
		// An unresolved tool name is a binding failure surfaced elsewhere
		// (internal/planner's PLAN-010/ErrToolNotFound path); B1 only
		// needs to plan substitutions for successfully bound tools.
		return nil
	}
	action, ok := toolDef.Actions[inv.Action]
	if !ok || action.Execute == nil || !action.Execute.IsSubstitution() {
		return nil
	}
	schemaAction := action.SchemaAction()
	if schemaAction == nil {
		v.errs = append(v.errs, fmt.Errorf(
			"plan-time substitution check: step %s: action %s#%s has no retained schema definition", step.ID, toolDef.Name, inv.Action))
		return nil
	}
	callerGov := pkgsubst.EffectiveGovernanceFromTool(toolDef.Governance)
	v.planSubstitutionChain(step.ID, schemaAction, inv.Action, toolDef, callerGov, nil)
	return nil
}

// planSubstitutionChain calls pkgsubst.Plan for one substitution frame and,
// if it succeeds, recurses into the substitute runbook's own flow so
// nested substitutions (and cycles/excess depth across the whole chain,
// PKG-015/PKG-028) are caught statically too -- exactly the same
// registry-wide tool binding the runtime itself uses when actually running
// a nested substitution (internal/executor/tool.go's SubStepRunner path),
// so plan-time and run-time agree on what "reachable" means.
func (v *substitutionPlanVisitor) planSubstitutionChain(stepID string, action *schema.ToolAction, actionName string, toolDef toolpkg.ToolDef, callerGov *pkgsubst.EffectiveGovernance, frames []pkgsubst.Frame) {
	result, errs := pkgsubst.Plan(action, actionName, toolDef.PackageName, toolDef.Name, pkgsubst.PlanOptions{
		ToolFilePath:     toolDef.SourcePath,
		PackageRoot:      toolDef.PackageRoot,
		Frames:           frames,
		CallerGovernance: callerGov,
		Parser:           v.parserImpl,
	})
	if len(errs) > 0 {
		for _, e := range errs {
			v.errs = append(v.errs, fmt.Errorf("plan-time substitution check: step %s: %w", stepID, e))
		}
		return
	}
	if result == nil {
		return
	}
	childFrames := append(append([]pkgsubst.Frame(nil), frames...), result.Frame)
	v.planSubstitutionsInSubtree(stepID, result.SubstituteRunbook.Flow, childFrames, result.EffectiveGovernance)
}

// planSubstitutionsInSubtree walks a substitute runbook's own flow
// (already parsed by the same structural parser as any other runbook, so
// this is safe to traverse via typed schema.FlowNode directly -- unlike
// pkg/pkgcatalog's digest closure, which must read raw bytes that may not
// even be valid runbooks yet) looking for further nested substitution
// actions, planning each transitively.
func (v *substitutionPlanVisitor) planSubstitutionsInSubtree(stepID string, nodes []schema.FlowNode, frames []pkgsubst.Frame, callerGov *pkgsubst.EffectiveGovernance) {
	for i := range nodes {
		n := &nodes[i]
		switch {
		case n.Step != nil:
			v.planSubstitutionStep(stepID, n.Step, frames, callerGov)
		case n.Iterate != nil:
			v.planSubstitutionsInSubtree(stepID, n.Iterate.Steps, frames, callerGov)
		case n.Parallel != nil:
			for j := range n.Parallel.Branches {
				v.planSubstitutionsInSubtree(stepID, n.Parallel.Branches[j].Steps, frames, callerGov)
			}
		}
	}
}

func (v *substitutionPlanVisitor) planSubstitutionStep(parentStepID string, step *schema.Step, frames []pkgsubst.Frame, callerGov *pkgsubst.EffectiveGovernance) {
	if step == nil {
		return
	}
	if step.BranchSpec != nil {
		for i := range step.BranchSpec.Branches {
			v.planSubstitutionsInSubtree(parentStepID, step.BranchSpec.Branches[i].Steps, frames, callerGov)
		}
	}
	if step.CompensateSpec != nil {
		v.planSubstitutionsInSubtree(parentStepID, step.CompensateSpec.Compensate.Steps, frames, callerGov)
	}
	if step.Type != schema.StepTypeTool || step.ToolCall == nil {
		return
	}
	inv := step.ToolCall.Tool
	toolDef, ok := v.toolDefs[inv.Name]
	if !ok {
		return
	}
	action, ok := toolDef.Actions[inv.Action]
	if !ok || action.Execute == nil || !action.Execute.IsSubstitution() {
		return
	}
	schemaAction := action.SchemaAction()
	if schemaAction == nil {
		v.errs = append(v.errs, fmt.Errorf(
			"plan-time substitution check: step %s: action %s#%s has no retained schema definition", parentStepID, toolDef.Name, inv.Action))
		return
	}
	v.planSubstitutionChain(parentStepID, schemaAction, inv.Action, toolDef, callerGov, frames)
}
