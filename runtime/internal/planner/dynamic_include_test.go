package planner_test

import (
	"context"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	plannerPkg "github.com/ormasoftchile/yawr/runtime/pkg/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// makeDynamicRunbook returns a ParsedRunbook with one dynamic include step,
// optionally followed by a CLI step (when afterStepID is non-empty).
func makeDynamicRunbook(source, runbookID, includeStepID, afterStepID string) *parser.ParsedRunbook {
	flow := []schema.FlowNode{
		{Step: &schema.Step{
			ID:   includeStepID,
			Type: schema.StepTypeInclude,
			IncludeSpec: &schema.IncludeSpec{
				Include: schema.IncludeConfig{
					RunbookRef:  "${suggested_tsg_id}",
					ResolveFrom: schema.ResolveFromCatalog,
				},
			},
		}},
	}
	if afterStepID != "" {
		flow = append(flow, schema.FlowNode{Step: &schema.Step{
			ID:   afterStepID,
			Type: schema.StepTypeCLI,
			CLI:  &schema.CLISpec{Command: "echo after"},
		}})
	}
	return &parser.ParsedRunbook{
		Source: source,
		Runbook: &schema.Runbook{
			ID:   runbookID,
			Name: runbookID,
			Flow: flow,
		},
	}
}

// TestDynamic_PlansWithoutError verifies that a dynamic include site plans
// without error and emits exactly one placeholder ResolvedStep carrying the
// original IncludeConfig (with IsDynamic()=true).
func TestDynamic_PlansWithoutError(t *testing.T) {
	rb := makeDynamicRunbook("/abs/parent.yaml", "parent", "inc0", "")
	p := planner.New(plannerPkg.Config{
		Loader: &failingLoader{t: t}, // must not be called for dynamic sites
		Tools:  &fakeRegistry{tools: map[string]*schema.ToolDef{}},
	})

	plan, err := p.Plan(context.Background(), rb)
	if err != nil {
		t.Fatalf("Plan failed: %v", err)
	}
	if len(plan.Steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(plan.Steps))
	}

	spec, ok := plan.Steps[0].Spec.(*schema.IncludeSpec)
	if !ok {
		t.Fatalf("step spec is not *IncludeSpec: %T", plan.Steps[0].Spec)
	}
	if !spec.Include.IsDynamic() {
		t.Error("step spec must be dynamic (IsDynamic() == true)")
	}
	if spec.Include.RunbookRef != "${suggested_tsg_id}" {
		t.Errorf("RunbookRef mismatch: got %q", spec.Include.RunbookRef)
	}
	if spec.LazyRunbookPath != "" {
		t.Errorf("LazyRunbookPath must be empty for dynamic sites; got %q", spec.LazyRunbookPath)
	}
	if len(spec.ResolvedSteps) != 0 {
		t.Errorf("ResolvedSteps must be empty for dynamic sites; got %d", len(spec.ResolvedSteps))
	}
}

// TestDynamic_SiblingDepthAfterDynamic is the depth-pop regression test.
// A dynamic include (like lazy) must NOT push execDepth, so a sibling step
// that follows the dynamic include must be emitted at depth 0 (the root
// depth), not at depth 1 (which would incorrectly nest it inside the
// dynamic include placeholder).
func TestDynamic_SiblingDepthAfterDynamic(t *testing.T) {
	rb := makeDynamicRunbook("/abs/parent.yaml", "parent", "inc0", "after-step")
	p := planner.New(plannerPkg.Config{
		Loader: &failingLoader{t: t},
		Tools:  &fakeRegistry{tools: map[string]*schema.ToolDef{}},
	})

	plan, err := p.Plan(context.Background(), rb)
	if err != nil {
		t.Fatalf("Plan failed: %v", err)
	}
	if len(plan.Steps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(plan.Steps))
	}

	incStep := plan.Steps[0]
	if incStep.ID != "inc0" {
		t.Fatalf("step 0 should be inc0, got %q", incStep.ID)
	}
	if incStep.Depth != 0 {
		t.Errorf("dynamic include step depth: got %d, want 0", incStep.Depth)
	}

	afterStep := plan.Steps[1]
	if afterStep.ID != "after-step" {
		t.Fatalf("step 1 should be after-step, got %q", afterStep.ID)
	}
	if afterStep.Depth != 0 {
		t.Errorf("sibling after dynamic include depth: got %d, want 0 (depth-pop regression)", afterStep.Depth)
	}
}

// TestDynamic_NestedInsideBranch verifies that a dynamic include nested
// inside a branch arm plans correctly and the sibling step in the same arm
// gets the arm's depth (1), not the branch parent's depth (0). This covers
// the nested-container + dynamic-include combination.
func TestDynamic_NestedInsideBranch(t *testing.T) {
	rb := &parser.ParsedRunbook{
		Source: "/abs/parent.yaml",
		Runbook: &schema.Runbook{
			ID:   "parent",
			Name: "parent",
			Flow: []schema.FlowNode{
				{Step: &schema.Step{
					ID:   "branch0",
					Type: schema.StepTypeBranch,
					BranchSpec: &schema.BranchSpec{
						Branches: []schema.BranchArm{
							{
								Condition: "true",
								Steps: []schema.FlowNode{
									{Step: &schema.Step{
										ID:   "dyn-inc",
										Type: schema.StepTypeInclude,
										IncludeSpec: &schema.IncludeSpec{
											Include: schema.IncludeConfig{
												RunbookRef:  "${suggested_tsg_id}",
												ResolveFrom: schema.ResolveFromCatalog,
											},
										},
									}},
									{Step: &schema.Step{
										ID:   "arm-after",
										Type: schema.StepTypeCLI,
										CLI:  &schema.CLISpec{Command: "echo arm-after"},
									}},
								},
							},
						},
					},
				}},
			},
		},
	}

	p := planner.New(plannerPkg.Config{
		Loader: &failingLoader{t: t},
		Tools:  &fakeRegistry{tools: map[string]*schema.ToolDef{}},
	})

	plan, err := p.Plan(context.Background(), rb)
	if err != nil {
		t.Fatalf("Plan failed: %v", err)
	}

	stepsByID := make(map[string]*engine.ResolvedStep)
	for i := range plan.Steps {
		s := &plan.Steps[i]
		stepsByID[s.ID] = s
	}

	dynInc, ok := stepsByID["dyn-inc"]
	if !ok {
		t.Fatal("dynamic include step 'dyn-inc' not found in plan")
	}
	if dynInc.Depth != 1 {
		t.Errorf("dyn-inc depth: got %d, want 1 (inside branch arm)", dynInc.Depth)
	}

	armAfter, ok := stepsByID["arm-after"]
	if !ok {
		t.Fatal("step 'arm-after' not found in plan")
	}
	if armAfter.Depth != 1 {
		t.Errorf("arm-after depth: got %d, want 1 (depth-pop regression for dynamic in branch)", armAfter.Depth)
	}
}
