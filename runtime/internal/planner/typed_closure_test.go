package planner

import (
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestTypedWholeBoundParallelClosure(t *testing.T) {
	capture := func(name string) schema.FlowNode {
		return schema.FlowNode{Step: &schema.Step{ID: "produce", Type: schema.StepTypeNoop, Capture: map[string]string{name: "stdout"}}}
	}
	include := func(nodes []schema.FlowNode) schema.FlowNode {
		return schema.FlowNode{Step: &schema.Step{ID: "include", Type: schema.StepTypeInclude, IncludeSpec: &schema.IncludeSpec{
			Include: schema.IncludeConfig{Runbook: "child"}, ResolvedSteps: nodes,
		}}}
	}
	for _, test := range []struct {
		name        string
		left, right []schema.FlowNode
		reject      bool
	}{
		{"direct-conflict", []schema.FlowNode{capture("count")}, []schema.FlowNode{capture("count")}, true},
		{"bound-child-conflict", []schema.FlowNode{include([]schema.FlowNode{capture("count")})}, []schema.FlowNode{capture("count")}, true},
		{"disjoint", []schema.FlowNode{capture("count")}, []schema.FlowNode{capture("other")}, false},
		{"dynamic-unprovable", []schema.FlowNode{{Step: &schema.Step{ID: "dynamic", Type: schema.StepTypeInclude, IncludeSpec: &schema.IncludeSpec{Include: schema.IncludeConfig{RunbookRef: "${target}"}}}}}, []schema.FlowNode{capture("other")}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			parallel := &schema.ParallelNode{ID: "parallel", Branches: []schema.ParallelBranch{{Steps: test.left}, {Steps: test.right}}}
			plan := &engine.ExecutionPlan{Bindings: []schema.Binding{{Name: "count", Mutable: true}, {Name: "other", Mutable: true}}, Steps: []engine.ResolvedStep{{Kind: "parallel", Spec: parallel}}}
			err := ValidateTypedBoundClosure(plan)
			if (err != nil) != test.reject {
				t.Fatalf("rejection=%v: %v", test.reject, err)
			}
			plan.Steps = []engine.ResolvedStep{{Kind: "include", Spec: &schema.IncludeSpec{ResolvedSteps: []schema.FlowNode{{Parallel: parallel}}}}}
			if err := ValidateTypedBoundClosure(plan); (err != nil) != test.reject {
				t.Fatalf("nested ambient write admission: %v", err)
			}
		})
	}
}
