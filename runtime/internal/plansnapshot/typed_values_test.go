package plansnapshot_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestTypedBindingPlanAndClosurePresenceVersions(t *testing.T) {
	plan := &engine.ExecutionPlan{
		RunbookPath: "typed.runbook.yaml",
		Metadata:    engine.PlanMetadata{RunbookID: "typed", RunbookName: "Typed"},
		Bindings:    []schema.Binding{{Name: "value", Type: "any", Value: nil, ValuePresent: true}},
		Outputs:     map[string]*schema.Output{"result": {Type: "any", ValueTreePresent: true, ValueTree: nil}},
		Steps:       []engine.ResolvedStep{{ID: "results", Kind: "results", Spec: &schema.ResultsSpec{}}},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatal(err)
	}
	snapshot, err := plansnapshot.FromExecutionPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.SchemaVersion != plansnapshot.SchemaVersionV3 {
		t.Fatalf("wrong typed version %s", snapshot.SchemaVersion)
	}
	restored, err := plansnapshot.Restore(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !restored.Bindings[0].ValuePresent || restored.Bindings[0].Value != nil || !restored.Outputs["result"].ValueTreePresent {
		t.Fatal("explicit null metadata lost")
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	forged := bytes.Replace(data, []byte(plansnapshot.SchemaVersionV3), []byte("execution-plan/v2"), 1)
	if err := plansnapshot.Preflight(forged); err == nil {
		t.Fatal("typed features admitted under old version")
	}
	flow := []schema.FlowNode{
		{Step: &schema.Step{ID: "assign", Type: schema.StepTypeAssign, AssignSpec: &schema.AssignSpec{Assign: []schema.Assignment{{Name: "value", Value: nil, ValuePresent: true}}}}},
		{Step: &schema.Step{ID: "results", Type: schema.StepTypeResults, ResultsSpec: &schema.ResultsSpec{}}},
	}
	closure, err := plansnapshot.EncodeFlowClosure(flow)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(closure, []byte(plansnapshot.FlowClosureSchemaV3)) {
		t.Fatal("typed closure version missing")
	}
	nodes, err := plansnapshot.RestoreFlowClosure(closure)
	if err != nil {
		t.Fatal(err)
	}
	if !nodes[0].Step.AssignSpec.Assign[0].ValuePresent {
		t.Fatal("assignment null presence lost")
	}
	forged = bytes.Replace(closure, []byte(plansnapshot.FlowClosureSchemaV3), []byte("execution-flow-closure/v2"), 1)
	if _, err := plansnapshot.RestoreFlowClosure(forged); err == nil {
		t.Fatal("downgraded typed closure admitted")
	}
}

func TestFlowClosurePreservesTypedCollections(t *testing.T) {
	flow := []schema.FlowNode{{Iterate: &schema.IterateNode{
		ID: "loop", Over: "items", Collect: map[string]string{"labels": "${item.id}"},
		CollectValues: map[string]any{
			"records": map[string]any{"configuration": "${item}", "rows": "${rows}", "empty": []any{}, "active": true, "kind": "results", "bindings": []any{"ordinary data"}},
		},
		Steps: []schema.FlowNode{{Step: &schema.Step{ID: "child", Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{}}}},
	}}}
	encoded, err := plansnapshot.EncodeFlowClosure(flow)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := plansnapshot.RestoreFlowClosure(encoded)
	if err != nil {
		t.Fatal(err)
	}
	records, ok := restored[0].Iterate.CollectValues["records"].(map[string]any)
	if !ok || records["configuration"] != "${item}" || records["rows"] != "${rows}" || records["active"] != true {
		t.Fatalf("typed collection lost on restore: %#v", records)
	}
	again, err := plansnapshot.EncodeFlowClosure(restored)
	if err != nil || !bytes.Equal(encoded, again) {
		t.Fatalf("typed closure changed after restore: %v", err)
	}
}
