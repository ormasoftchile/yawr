package sessioncoordinator

import (
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestToolDynamicProjectionRequiresExactStructuralOwner(t *testing.T) {
	plan := &engine.ExecutionPlan{Steps: []engine.ResolvedStep{
		{ID: "parallel", Kind: "parallel"},
		{ID: "route", Kind: "tool", ParentID: "parallel", Depth: 1, BranchLabel: "left"},
	}}
	pin := schema.LockedDynamicInclude{
		StepID: "include", QualifiedNodeID: "parallel/route/loop/include",
		TargetScopeID: "frozen-child", Invocation: 3, Revision: 7,
		StructuralPath: []schema.DynamicIncludeFrameIdentity{
			{QualifiedNodeID: "parallel", Kind: "parallel", BranchLabel: "left", Invocation: 1},
			{QualifiedNodeID: "parallel/route", Kind: "tool-substitution", Invocation: 2},
			{QualifiedNodeID: "parallel/route/loop", Kind: "iterate", IterationIndex: 4, Invocation: 3},
		},
	}
	local, matched, err := toolRelativeDynamicPin(plan, 1, pin)
	if err != nil || !matched || local.QualifiedNodeID != "loop/include" ||
		local.TargetScopeID != pin.TargetScopeID || local.Invocation != 3 || local.Revision != 7 ||
		len(local.StructuralPath) != 1 || local.StructuralPath[0].QualifiedNodeID != "loop" ||
		local.StructuralPath[0].IterationIndex != 4 || local.StructuralPath[0].Invocation != 3 {
		t.Fatalf("projection lost frozen occurrence identity: %#v, matched=%v, error=%v", local, matched, err)
	}
	local.StructuralPath[0].QualifiedNodeID = "changed"
	if pin.StructuralPath[2].QualifiedNodeID != "parallel/route/loop" {
		t.Fatal("projection mutated the durable pin")
	}
	for _, name := range []string{"wrong-branch", "wrong-frame-kind", "missing-owner", "escaping-child"} {
		t.Run(name, func(t *testing.T) {
			invalid := pin
			invalid.StructuralPath = append([]schema.DynamicIncludeFrameIdentity(nil), pin.StructuralPath...)
			switch name {
			case "wrong-branch":
				invalid.StructuralPath[0].BranchLabel = "right"
			case "wrong-frame-kind":
				invalid.StructuralPath[1].Kind = "include"
			case "missing-owner":
				invalid.StructuralPath = nil
			case "escaping-child":
				invalid.StructuralPath[2].QualifiedNodeID = "parallel/other/loop"
			}
			if _, _, err := toolRelativeDynamicPin(plan, 1, invalid); err == nil {
				t.Fatal("accepted a dynamic occurrence belonging to another structural owner")
			}
		})
	}
	pin.QualifiedNodeID = "parallel/route-other/include"
	if _, matched, err := toolRelativeDynamicPin(plan, 1, pin); matched || err != nil {
		t.Fatalf("another invocation matched by a partial name prefix: matched=%v, error=%v", matched, err)
	}
}
