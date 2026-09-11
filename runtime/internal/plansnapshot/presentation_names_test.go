package plansnapshot_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestPresentationSchemaNamesRoundTripCurrentSnapshot(t *testing.T) {
	for _, name := range []string{"args", "inputs", "outputs", "default", "presentation", "tools", "actions"} {
		field := &schema.ArgDef{Type: "string", Presentation: &schema.PresentationDescriptor{Version: 1, Kind: "code", Language: "sql"}}
		plan := &engine.ExecutionPlan{
			RunbookPath: "root.runbook.yaml",
			Metadata:    engine.PlanMetadata{RunbookID: "root", RunbookName: "Root"},
			Steps:       []engine.ResolvedStep{{ID: "n", Kind: "noop", Spec: &schema.NoopSpec{}}},
			Tools: map[string]*schema.ToolDef{name: {
				Name: name,
				Actions: map[string]*schema.ToolAction{name: {Args: map[string]*schema.ArgDef{name: field}}},
			}},
		}
		if err := planner.ValidateExecutionPlan(plan); err != nil {
			t.Fatal(err)
		}
		snapshot, err := plansnapshot.FromExecutionPlan(plan)
		if err != nil || snapshot.SchemaVersion != plansnapshot.SchemaVersionV3 {
			t.Fatalf("%s: version=%q err=%v", name, snapshot.SchemaVersion, err)
		}
		restored, err := plansnapshot.Restore(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		field.Presentation.Language = "kql"
		if restored.Tools[name].Actions[name].Args[name].Presentation.Language != "sql" {
			t.Fatal("snapshot shares descriptor with caller")
		}
	}
}

func TestPresentationSchemaOpaqueValuesAndCurrentClosures(t *testing.T) {
	descriptor := &schema.PresentationDescriptor{Version: 1, Kind: "code", Language: "sql"}
	tools := map[string]*schema.ToolDef{"args": {
		Name: "args",
		Actions: map[string]*schema.ToolAction{"outputs": {
			Args: map[string]*schema.ArgDef{"default": {Type: "string", Presentation: descriptor}},
		}},
	}}
	closure, err := plansnapshot.EncodeFlowClosure([]schema.FlowNode{}, tools)
	if err != nil || !strings.Contains(string(closure), plansnapshot.FlowClosureSchemaV3) {
		t.Fatalf("closure not current: %s %v", closure, err)
	}
	frozen, err := plansnapshot.RestoreFlowTools(closure)
	if err != nil || !plansnapshot.HasPresentation(frozen) {
		t.Fatalf("frozen tools lost: %v", err)
	}
	descriptor.Language = "kql"
	if frozen["args"].Actions["outputs"].Args["default"].Presentation.Language != "sql" {
		t.Fatal("closure shares mutable tool")
	}
	for _, obsolete := range []string{"yawr.execution-flow-closure/v1", "execution-flow-closure/v2"} {
		var envelope map[string]any
		if err := json.Unmarshal(closure, &envelope); err != nil {
			t.Fatal(err)
		}
		envelope["schema_version"] = obsolete
		data, _ := json.Marshal(envelope)
		if plansnapshot.ValidateFlowClosure(data) == nil {
			t.Fatalf("obsolete closure %q accepted", obsolete)
		}
	}
}
