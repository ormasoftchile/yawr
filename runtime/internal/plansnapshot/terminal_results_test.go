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

func TestTerminalResultsPlanAndClosureRoundTrip(t *testing.T) {
	end := &schema.EndSpec{PublishResults: true, Outcome: &schema.OutcomeDeclaration{Category: "escalated", Code: "Exact_Code"}}
	plan := &engine.ExecutionPlan{RunbookPath: "terminal.yaml", Metadata: engine.PlanMetadata{RunbookID: "terminal"},
		Outputs: map[string]*schema.Output{"status": {Type: "string", Value: "escalated"}},
		Steps:   []engine.ResolvedStep{{ID: "end", Kind: "end", Spec: end}, {ID: "tail", Kind: "noop", Spec: &schema.NoopSpec{}}}}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatal(err)
	}
	snapshot, err := plansnapshot.FromExecutionPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.SchemaVersion != plansnapshot.SchemaVersionV3 {
		t.Fatal("terminal-only plan not versioned")
	}
	restored, err := plansnapshot.Restore(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	spec := restored.Steps[0].Spec.(*schema.EndSpec)
	if !spec.PublishResults || spec.Outcome.Code != end.Outcome.Code {
		t.Fatal("terminal plan changed")
	}
	body, _ := json.Marshal(snapshot)
	if err := plansnapshot.Preflight(bytes.Replace(body, []byte(plansnapshot.SchemaVersionV3), []byte("execution-plan/v2"), 1)); err == nil {
		t.Fatal("terminal plan downgrade accepted")
	}
	flow := []schema.FlowNode{{Step: &schema.Step{ID: "end", Type: schema.StepTypeEnd, EndSpec: end}}}
	closure, err := plansnapshot.EncodeFlowClosure(flow)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(closure, []byte(plansnapshot.FlowClosureSchemaV3)) {
		t.Fatal("terminal closure not versioned")
	}
	nodes, err := plansnapshot.RestoreFlowClosure(closure)
	if err != nil || !nodes[0].Step.EndSpec.PublishResults {
		t.Fatalf("terminal closure changed: %v", err)
	}
	if _, err := plansnapshot.RestoreFlowClosure(bytes.Replace(closure, []byte(plansnapshot.FlowClosureSchemaV3), []byte("execution-flow-closure/v2"), 1)); err == nil {
		t.Fatal("terminal closure downgrade accepted")
	}
}
