package plansnapshot

import (
	"encoding/json"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestScopedSubplanBoundaryPreservesGraphParents(t *testing.T) {
	original, owner := scopedFixture(t)
	bound, err := original.ToolScopes.Resolve(owner, "query", "inspect")
	if err != nil {
		t.Fatal(err)
	}
	call := &schema.Step{ID: "inspect", Type: schema.StepTypeTool, LexicalScopeID: owner, ToolBindingID: bound.BindingID,
		ToolCall: &schema.ToolCallSpec{Tool: schema.ToolInvocation{Name: "query", Action: "inspect"}}}
	branch := &schema.BranchSpec{Branches: []schema.BranchArm{{Else: true, Steps: []schema.FlowNode{{Step: call}}}}}
	plan := &engine.ExecutionPlan{
		RunbookPath: "substeps", ToolScopes: original.ToolScopes, RootScopeID: owner,
		Metadata: engine.PlanMetadata{RunbookID: "substeps", CatalogDigest: original.Metadata.CatalogDigest},
		Steps: []engine.ResolvedStep{
			{ID: "nested", Kind: "branch", LexicalScopeID: owner, ParentID: "choice", ParentKind: "branch", Spec: branch},
			{ID: call.ID, Kind: "tool", LexicalScopeID: owner, ToolBindingID: bound.BindingID, ParentID: "nested", ParentKind: "branch", Depth: 1, Spec: call.ToolCall},
		},
	}
	if err := planner.ValidateExecutionPlan(plan); err == nil {
		t.Fatal("undeclared external parent accepted")
	}
	plan.ScopeBoundary = &engine.ToolScopeBoundary{ParentID: "choice", ParentKind: "branch", ScopeID: owner}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatal(err)
	}
	if err := planner.FinalizeMaterializedPlan(plan); err != nil {
		t.Fatal(err)
	}
	snapshot, err := FromExecutionPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var decoded SnapshotV1
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	restored, err := Restore(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if restored.ScopeBoundary == nil || restored.ScopeBoundary.ParentID != "choice" ||
		restored.Steps[0].ParentID != "choice" || restored.Steps[1].ParentID != "nested" ||
		restored.Steps[1].LexicalScopeID != owner {
		t.Fatal("subplan serialization rewrote graph linkage or lexical owner")
	}
	for _, test := range []struct {
		name   string
		mutate func(*engine.ExecutionPlan)
	}{
		{"wrong-boundary-scope", func(p *engine.ExecutionPlan) { p.ScopeBoundary.ScopeID = original.RootScopeID }},
		{"wrong-external-parent", func(p *engine.ExecutionPlan) { p.Steps[0].ParentID = "unrelated" }},
		{"wrong-external-kind", func(p *engine.ExecutionPlan) { p.Steps[0].ParentKind = "include" }},
		{"nested-external-parent", func(p *engine.ExecutionPlan) { p.Steps[1].ParentID = "choice" }},
		{"nested-owner-switch", func(p *engine.ExecutionPlan) { p.Steps[1].LexicalScopeID = original.RootScopeID }},
		{"nested-kind-mismatch", func(p *engine.ExecutionPlan) { p.Steps[1].ParentKind = "include" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			p, err := Restore(decoded)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(p)
			if err := planner.ValidateExecutionPlan(p); err == nil {
				t.Fatal("invalid boundary/reference accepted")
			}
		})
	}
	restored.ScopeBoundary.ParentID = "new-external-parent"
	if _, err := FromExecutionPlan(restored); err == nil {
		t.Fatal("boundary mutation bypassed validation/digest")
	}
	legacy := &engine.ExecutionPlan{ScopeBoundary: &engine.ToolScopeBoundary{ParentID: "choice", ParentKind: "branch"}}
	if err := planner.ValidateExecutionPlan(legacy); err == nil {
		t.Fatal("legacy plan acquired scoped boundary meaning")
	}
}
