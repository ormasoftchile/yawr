package sessioncoordinator

import (
	"encoding/json"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/session"
)

func TestExpressionFrozenGraphAndBinding(t *testing.T) {
	plan := &engine.ExecutionPlan{RunbookPath: "source-no-longer-exists.runbook.yaml", Metadata: engine.PlanMetadata{RunbookID: "frozen", RunbookName: "Frozen"}, Steps: []engine.ResolvedStep{
		{ID: "gate", Kind: "noop", When: "vars.count >= 2", Spec: &schema.NoopSpec{}},
		{ID: "message", Kind: "display", Spec: &schema.DisplaySpec{Display: schema.DisplayConfig{Content: "Hi ${vars.name}!"}}},
	}}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatal(err)
	}

	graph, err := finalizeExecutionPlanGraph(plan, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := snapshotBlob(plan)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := bindHandoffGraph(plan, snapshot.Digest, graph)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateBoundHandoffGraph(plan, snapshot.Digest, bound); err != nil {
		t.Fatal(err)
	}
	doc, err := InspectionDocument(plan, snapshot.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Nodes) != 2 || doc.Nodes[0].Details.ExpressionPresentation.Values[0].Path != "/common/when" || doc.Nodes[1].Details.ExpressionPresentation.Values[0].Path != "/content" {
		t.Fatal("non-tool frozen details missing")
	}
	frozen, err := plansnapshot.FromExecutionPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(frozen)
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]any
	if json.Unmarshal(encoded, &envelope) != nil {
		t.Fatal("snapshot encoding")
	}
	if envelope["schema_version"] != "execution-plan/v3" {
		t.Fatal("expression metadata changed snapshot version")
	}
	restored, err := plansnapshot.Restore(frozen)
	if err != nil {
		t.Fatal(err)
	}
	plan.Steps[1].Spec.(*schema.DisplaySpec).Display.Content = "changed current text"
	retained, err := InspectionDocument(restored, snapshot.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if retained.Nodes[1].Details.Content != "Hi ${vars.name}!" || retained.Nodes[1].Details.ExpressionPresentation == nil {
		t.Fatal("frozen authored definition changed")
	}
	plan = restored
	var tampered boundHandoffGraph
	if decodeHandoffJSON(bound, &tampered) != nil {
		t.Fatal("bound graph")
	}
	details := tampered.Nodes[0].Data["details"].(map[string]any)
	expression := details["expression_presentation"].(map[string]any)
	expression["grammar_version"] = "future"
	if validateHandoffGraphContentHash(tampered.Document) == nil {
		t.Fatal("expression grammar excluded from structural hash")
	}
	tampered.BoundContentHash = session.DigestJSON(tampered.Document)
	bytes, _ := json.Marshal(tampered)
	if validateBoundHandoffGraph(plan, snapshot.Digest, bytes) == nil {
		t.Fatal("expression tampering accepted")
	}
}
