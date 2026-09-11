package planner

import (
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestValidateExecutionPlanRejectsHandoffUnderConcurrentAncestor(t *testing.T) {
	for _, test := range []struct {
		name   string
		parent engine.ResolvedStep
	}{
		{
			name: "parallel include closure",
			parent: engine.ResolvedStep{
				ID: "parallel", Kind: "parallel", Depth: 0, Spec: &schema.ParallelNode{ID: "parallel"},
			},
		},
		{
			name: "concurrent iterate closure",
			parent: engine.ResolvedStep{
				ID: "iterate", Kind: "iterate", Depth: 0,
				Spec: &schema.IterateNode{ID: "iterate", Concurrency: 2},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan := &engine.ExecutionPlan{
				RunbookPath: "root.runbook.yaml",
				Steps: []engine.ResolvedStep{
					test.parent,
					{ID: "include", Kind: "include", Depth: 1, ParentID: test.parent.ID, ParentKind: test.parent.Kind, Spec: &schema.IncludeSpec{}},
					{ID: "handoff", Kind: "handoff", Depth: 2, ParentID: "include", ParentKind: "include", Spec: &schema.HandoffSpec{Handoff: schema.HandoffConfig{
						Runbook: "target.runbook.yaml", Reason: schema.HandoffReason{Code: "next", Summary: "Continue"},
					}}},
				},
				Metadata: engine.PlanMetadata{RunbookID: "root", RunbookName: "Root"},
			}
			err := ValidateExecutionPlan(plan)
			if err == nil || !strings.Contains(err.Error(), "handoff") || !strings.Contains(err.Error(), "concurrent") {
				t.Fatalf("ValidateExecutionPlan error = %v, want concurrent handoff refusal", err)
			}
		})
	}
}
