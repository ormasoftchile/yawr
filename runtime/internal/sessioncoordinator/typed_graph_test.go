package sessioncoordinator

import (
	"encoding/json"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/render/graphjson"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestTypedFrozenGraphBindingAndDowngrade(t *testing.T) {
	plan := &engine.ExecutionPlan{RunbookPath: "source-no-longer-exists.yaml", Metadata: engine.PlanMetadata{RunbookID: "typed", RunbookName: "Typed"},
		Bindings: []schema.Binding{{Name: "value", Type: "any", Mutable: true, Value: nil, ValuePresent: true}},
		Outputs:  map[string]*schema.Output{"result": {Type: "object", ValueTree: map[string]any{"value": "${value}"}, ValueTreePresent: true}},
		Steps: []engine.ResolvedStep{
			{ID: "write", Kind: "assign", Spec: &schema.AssignSpec{Assign: []schema.Assignment{{Name: "value", Value: false, ValuePresent: true}}}},
			{ID: "publish", Kind: "results", Spec: &schema.ResultsSpec{}},
		}}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatal(err)
	}
	data, err := finalizeExecutionPlanGraph(plan, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := snapshotBlob(plan)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := bindHandoffGraph(plan, snapshot.Digest, data)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateBoundHandoffGraph(plan, snapshot.Digest, bound); err != nil {
		t.Fatal(err)
	}
	frozen, err := plansnapshot.FromExecutionPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := plansnapshot.Restore(frozen)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := InspectionDocument(restored, snapshot.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if doc.SchemaVersion != "3" || len(doc.Frames) != 1 || doc.Frames[0].Invocation == nil {
		t.Fatal("frozen metadata/version lost")
	}
	inv := doc.Frames[0].Invocation
	if !inv.Results || !inv.Bindings[0].ValuePresent || inv.Bindings[0].Value != nil || !inv.Outputs["result"].ValueTreePresent {
		t.Fatal("frozen typed declarations changed")
	}
	if doc.Nodes[1].ID != "publish" || doc.Nodes[1].Title != "Results" || doc.Nodes[0].Details.Role != "technical" {
		t.Fatal("frozen identity/operator metadata changed")
	}
	var graph graphjson.Document
	if json.Unmarshal(data, &graph) != nil {
		t.Fatal("graph encoding")
	}
	if graph.Nodes[1].Type != "terminal" {
		t.Fatal("Results is not a terminal invocation boundary")
	}
	graph.SchemaVersion = "1"
	if _, err := graphDocumentFromHandoffGraph(graph); err == nil {
		t.Fatal("typed metadata accepted under old graph envelope")
	}
}
