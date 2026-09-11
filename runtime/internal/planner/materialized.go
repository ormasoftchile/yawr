package planner

import (
	"errors"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func HasDeferredStaticIncludes(plan *engine.ExecutionPlan) bool {
	if plan == nil {
		return false
	}
	for index := range plan.Steps {
		if resolvedSpecHasDeferredInclude(plan.Steps[index].Spec) {
			return true
		}
	}
	return false
}

func resolvedSpecHasDeferredInclude(spec engine.StepSpec) bool {
	switch typed := spec.(type) {
	case *schema.IncludeSpec:
		if typed == nil {
			return false
		}
		return typed.LazyRunbookPath != "" || flowHasDeferredInclude(typed.ResolvedSteps)
	case *schema.BranchSpec:
		if typed != nil {
			for _, branch := range typed.Branches {
				if flowHasDeferredInclude(branch.Steps) {
					return true
				}
			}
		}
	case *schema.IterateNode:
		return typed != nil && flowHasDeferredInclude(typed.Steps)
	case *schema.ParallelNode:
		if typed != nil {
			for _, branch := range typed.Branches {
				if flowHasDeferredInclude(branch.Steps) {
					return true
				}
			}
		}
	case *schema.CompensateSpec:
		return typed != nil && flowHasDeferredInclude(typed.Compensate.Steps)
	}
	return false
}

func flowHasDeferredInclude(nodes []schema.FlowNode) bool {
	for index := range nodes {
		node := &nodes[index]
		if node.Step != nil && resolvedSpecHasDeferredInclude(specForStep(node.Step)) ||
			node.Iterate != nil && resolvedSpecHasDeferredInclude(node.Iterate) ||
			node.Parallel != nil && resolvedSpecHasDeferredInclude(node.Parallel) {
			return true
		}
	}
	return false
}

// FinalizeMaterializedPlan rebuilds the flat execution index after deferred
// static includes have been replaced by immutable executable closures.
func FinalizeMaterializedPlan(plan *engine.ExecutionPlan) error {
	if plan == nil || plan.Validation == nil {
		return errors.New("planner: validated execution plan is required")
	}
	builder := materializedPlanBuilder{plan: plan}
	for index := range plan.Steps {
		step := plan.Steps[index]
		if step.ParentID != "" || step.Depth != 0 {
			continue
		}
		step.Depth = 0
		step.NestDepth = 0
		step.DisplayOrder = builder.nextOrder()
		builder.steps = append(builder.steps, step)
		builder.appendSpecChildren(step.Spec, step.ID, step.Kind, step.Origin, 1, 1)
	}
	if len(plan.Steps) > 0 && len(builder.steps) == 0 {
		return errors.New("planner: materialized plan has no root steps")
	}
	plan.Steps = builder.steps
	return ValidateExecutionPlan(plan)
}

type materializedPlanBuilder struct {
	plan  *engine.ExecutionPlan
	steps []engine.ResolvedStep
	order int
}

func (builder *materializedPlanBuilder) nextOrder() int {
	order := builder.order
	builder.order++
	return order
}

func (builder *materializedPlanBuilder) appendSpecChildren(
	spec engine.StepSpec,
	parentID string,
	parentKind string,
	origin string,
	depth int,
	nestDepth int,
) {
	switch typed := spec.(type) {
	case *schema.IncludeSpec:
		if typed == nil || typed.Include.IsDynamic() && len(typed.ResolvedSteps) == 0 {
			return
		}
		childOrigin := typed.ResolvedRunbookPath
		if childOrigin == "" {
			childOrigin = origin
		}
		builder.appendFlow(typed.ResolvedSteps, parentID, "include", "", childOrigin, depth, nestDepth)
	case *schema.BranchSpec:
		if typed != nil {
			for _, branch := range typed.Branches {
				builder.appendFlow(branch.Steps, parentID, "branch", branch.Label, origin, depth, nestDepth)
			}
		}
	case *schema.IterateNode:
		if typed != nil {
			builder.appendFlow(typed.Steps, parentID, "iterate", "", origin, depth, nestDepth)
		}
	case *schema.ParallelNode:
		if typed != nil {
			for _, branch := range typed.Branches {
				builder.appendFlow(branch.Steps, parentID, "parallel", branch.Label, origin, depth, nestDepth)
			}
		}
	case *schema.CompensateSpec:
		if typed != nil {
			builder.appendFlow(typed.Compensate.Steps, parentID, "compensate", "", origin, depth, nestDepth)
		}
	}
}

func (builder *materializedPlanBuilder) appendFlow(
	nodes []schema.FlowNode,
	parentID string,
	parentKind string,
	branchLabel string,
	origin string,
	depth int,
	nestDepth int,
) {
	for index := range nodes {
		node := &nodes[index]
		var step engine.ResolvedStep
		switch {
		case node.Step != nil:
			authored := node.Step
			step = engine.ResolvedStep{
				ID: authored.ID, Name: displayName(authored), Subtitle: authored.Subtitle, Kind: string(authored.Type),
				Spec: specForStep(authored), Capture: authored.Capture, CaptureDefaults: authored.CaptureDefaults,
				Depth: depth, NestDepth: nestDepth, DisplayOrder: builder.nextOrder(), Origin: origin,
				OnError: authored.OnError, ContinueOnFail: authored.ContinueOnFail,
				Timeout: authored.Timeout, Delay: authored.Delay, When: authored.When,
				Retry: authored.Retry, Scope: authored.Scope, Export: authored.Export,
				Contract: authored.Contract, RequiredEvidence: authored.RequiredEvidence,
				ParentID: parentID, ParentKind: parentKind, BranchLabel: branchLabel,
			}
		case node.Iterate != nil:
			step = engine.ResolvedStep{
				ID: node.Iterate.ID, Kind: "iterate", Spec: node.Iterate,
				Depth: depth, NestDepth: nestDepth, DisplayOrder: builder.nextOrder(), Origin: origin,
				ParentID: parentID, ParentKind: parentKind, BranchLabel: branchLabel,
			}
		case node.Parallel != nil:
			step = engine.ResolvedStep{
				ID: node.Parallel.ID, Kind: "parallel", Spec: node.Parallel,
				Depth: depth, NestDepth: nestDepth, DisplayOrder: builder.nextOrder(), Origin: origin,
				ParentID: parentID, ParentKind: parentKind, BranchLabel: branchLabel,
			}
		default:
			continue
		}
		builder.steps = append(builder.steps, step)
		builder.appendSpecChildren(step.Spec, step.ID, step.Kind, origin, depth+1, nestDepth+1)
	}
}
