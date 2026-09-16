package sessioncoordinator

import (
	"encoding/json"
	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/presentation"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/render/graphjson"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func frozenGraphDetails(index int, plan *engine.ExecutionPlan) (*graphdoc.StepDetails, error) {
	d := graphdoc.DetailsForResolvedStep(plan.Steps[index])
	if _, parallel := plan.Steps[index].Spec.(*schema.ParallelNode); !parallel {
		d.ProjectExpressions()
	}
	if d == nil || d.Kind != "tool" {
		return d, nil
	}
	if plan.ToolScopes != nil {
		definition, err := engine.FrozenToolDefinition(plan, plan.Steps[index])
		if err != nil {
			return nil, err
		}
		d.SetCodePresentation(presentation.ForAction(d.Tool, d.Action, "frozen", "", definition))
		return d, nil
	}
	definition := plan.Tools[d.Tool]
	for current := index; plan.Steps[current].ParentID != ""; {
		parent, err := handoffPlanParentIndex(plan, current)
		if err != nil {
			break
		}
		matched := false
		for _, pin := range plan.Metadata.DynamicIncludes {
			site, err := dynamicRevisionParentIndex(plan, pin)
			if err != nil || site != parent || pin.Revision < 1 || pin.QualifiedNodeID == "" {
				continue
			}
			tools, err := plansnapshot.RestoreFlowTools(pin.ExecutableClosure)
			if err == nil && tools[d.Tool] != nil {
				definition = tools[d.Tool]
				matched = true
			}
		}
		if matched {
			break
		}
		current = parent
	}
	if plansnapshot.HasPresentation(definition) {
		d.SetCodePresentation(presentation.ForAction(d.Tool, d.Action, "frozen", "", definition))
	}
	return d, nil
}

// InspectionDocument is a read-only projection. It never finalizes or rewrites
// the plan and never reloads current source definitions.
func InspectionDocument(plan *engine.ExecutionPlan, digest string, state ...engine.RunState) (*graphdoc.Document, error) {
	return inspectionDocument(plan, digest, nil, state...)
}

// LiveExecutionGraph projects frozen definitions and all resolved invocations,
// without rereading source or mutating the running plan.
func LiveExecutionGraph(state engine.RunState, digest string) (*graphdoc.Document, []ExecutionGraphBinding, error) {
	bindings := []ExecutionGraphBinding{}
	doc, err := inspectionDocument(state.Plan, digest, &bindings, state)
	return doc, bindings, err
}

func inspectionDocument(plan *engine.ExecutionPlan, digest string, bindings *[]ExecutionGraphBinding, state ...engine.RunState) (*graphdoc.Document, error) {
	if len(state) > 0 && len(state[0].DynamicIncludes) > 0 {
		checkpoint := state[0]
		checkpoint.Plan = plan
		var err error
		plan, _, err = dynamicRevisionPlan(checkpoint)
		if err != nil {
			return nil, err
		}
	}
	encoded, err := executionPlanGraphWithBindings(plan, nil, bindings)
	if err != nil {
		return nil, err
	}
	var wire graphjson.Document
	if err = json.Unmarshal(encoded, &wire); err != nil {
		return nil, err
	}
	doc, err := graphDocumentFromHandoffGraph(wire)
	if err != nil {
		return nil, err
	}
	for i := range doc.Nodes {
		d := doc.Nodes[i].Details
		if d == nil || d.Kind != "tool" {
			continue
		}
		if d.CodePresentation != nil {
			d.CodePresentation.PlanSnapshotDigest = digest
		} else {
			d.SetCodePresentation(presentation.ForAction(d.Tool, d.Action, "frozen", digest, plan.Tools[d.Tool]))
		}
	}
	doc.Hash, err = doc.ContentHash()
	return doc, err
}
