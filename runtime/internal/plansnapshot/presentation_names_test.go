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

func TestPresentationSchemaNames(t *testing.T) {
	for _, name := range []string{"args", "inputs", "outputs", "default", "presentation", "tools", "actions"} {
		for _, site := range []string{"args", "outputs"} {
			t.Run(name+"/"+site, func(t *testing.T) {
				field := &schema.ArgDef{Type: "string"}
				action := &schema.ToolAction{}
				if site == "args" {
					action.Args = map[string]*schema.ArgDef{name: field}
				} else {
					action.Outputs = map[string]*schema.ArgDef{name: field}
				}
				plan := &engine.ExecutionPlan{
					RunbookPath: "root.runbook.yaml", Metadata: engine.PlanMetadata{RunbookID: "root", RunbookName: "Root"},
					Steps: []engine.ResolvedStep{{ID: "n", Kind: "noop", Spec: &schema.NoopSpec{}}},
					Tools: map[string]*schema.ToolDef{name: {Name: name, Actions: map[string]*schema.ToolAction{name: action}}},
				}
				if err := planner.ValidateExecutionPlan(plan); err != nil {
					t.Fatal(err)
				}
				old, err := plansnapshot.FromExecutionPlan(plan)
				if err != nil || old.SchemaVersion != plansnapshot.SchemaVersionV1 {
					t.Fatalf("metadata-free v1: %v", err)
				}
				before, _ := json.Marshal(old)
				field.Presentation = &schema.PresentationDescriptor{Version: 1, Kind: "code", Language: "sql"}
				modern, err := plansnapshot.FromExecutionPlan(plan)
				if err != nil || modern.SchemaVersion != plansnapshot.SchemaVersionV2 {
					t.Fatalf("metadata writer: version=%s err=%v", modern.SchemaVersion, err)
				}
				data, _ := json.Marshal(modern)
				forged := strings.Replace(string(data), plansnapshot.SchemaVersionV2, plansnapshot.SchemaVersionV1, 1)
				if plansnapshot.Preflight([]byte(forged)) == nil {
					t.Fatal("forged v1 accepted")
				}
				modern.SchemaVersion = plansnapshot.SchemaVersionV1
				if _, err := plansnapshot.Restore(modern); err == nil {
					t.Fatal("typed forged v1 accepted")
				}
				modern.SchemaVersion = plansnapshot.SchemaVersionV2
				restored, err := plansnapshot.Restore(modern)
				if err != nil {
					t.Fatal(err)
				}
				field.Presentation.Language = "kql"
				fields := restored.Tools[name].Actions[name].Args
				if site == "outputs" {
					fields = restored.Tools[name].Actions[name].Outputs
				}
				if fields[name].Presentation.Language != "sql" {
					t.Fatal("snapshot shares descriptor with caller")
				}
				var legacy plansnapshot.SnapshotV1
				if err := json.Unmarshal(before, &legacy); err != nil {
					t.Fatal(err)
				}
				after, _ := json.Marshal(legacy)
				if string(before) != string(after) {
					t.Fatal("legacy encoding changed")
				}
			})
		}
	}
}

func TestPresentationSchemaOpaqueValuesAndEmbeddedClosures(t *testing.T) {
	descriptor := &schema.PresentationDescriptor{Version: 1, Kind: "code", Language: "sql"}
	tools := map[string]*schema.ToolDef{"args": {Name: "args", Actions: map[string]*schema.ToolAction{
		"outputs": {Args: map[string]*schema.ArgDef{"default": {Type: "string", Presentation: descriptor}}},
	}}}
	opaque, _ := json.Marshal(tools)
	var value any
	if err := json.Unmarshal(opaque, &value); err != nil {
		t.Fatal(err)
	}
	plain := map[string]*schema.ToolDef{"tools": {Name: "tools", Actions: map[string]*schema.ToolAction{
		"actions": {Args: map[string]*schema.ArgDef{"presentation": {Type: "object", Default: value}}},
	}}}
	if plansnapshot.HasPresentation(plain) {
		t.Fatal("arbitrary default JSON misclassified")
	}
	envelope, _ := json.Marshal(map[string]any{"schema_version": plansnapshot.SchemaVersionV1, "tools": plain,
		"inputs": value, "outputs": value, "steps": []any{map[string]any{"spec": map[string]any{"args": value}}},
	})
	if err := plansnapshot.Preflight(envelope); err != nil {
		t.Fatalf("opaque user data rejected: %v", err)
	}
	closure, err := plansnapshot.EncodeFlowClosure([]schema.FlowNode{}, tools)
	if err != nil || !strings.Contains(string(closure), plansnapshot.FlowClosureSchemaV2) {
		t.Fatalf("closure not v2: %s %v", closure, err)
	}
	frozen, err := plansnapshot.RestoreFlowTools(closure)
	if err != nil || !plansnapshot.HasPresentation(frozen) {
		t.Fatalf("frozen tools lost: %v", err)
	}
	descriptor.Language = "kql"
	if frozen["args"].Actions["outputs"].Args["default"].Presentation.Language != "sql" {
		t.Fatal("closure shares mutable tool")
	}
	plain["tools"].Actions["actions"].FrozenSubstitution = &schema.FrozenToolSubstitution{ExecutableClosure: closure}
	if !plansnapshot.HasPresentation(plain) {
		t.Fatal("embedded substitution descriptors missed")
	}
	for _, payload := range []map[string]any{
		{"tools": plain},
		{"Tools": plain},
		{"metadata": map[string]any{"DynamicIncludes": []any{map[string]any{"executable_closure": json.RawMessage(closure)}}}},
		{"metadata": map[string]any{"dynamicincludes": []any{map[string]any{"executable_closure": json.RawMessage(closure)}}}},
	} {
		payload["schema_version"] = plansnapshot.SchemaVersionV1
		data, _ := json.Marshal(payload)
		if plansnapshot.Preflight(data) == nil {
			t.Fatalf("embedded/case-folded v1 accepted: %s", data)
		}
	}
	plan := &engine.ExecutionPlan{RunbookPath: "root.runbook.yaml",
		Metadata: engine.PlanMetadata{RunbookID: "root", RunbookName: "Root"},
		Steps:    []engine.ResolvedStep{{ID: "n", Kind: "noop", Spec: &schema.NoopSpec{}}}}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatal(err)
	}
	plan.Metadata.DynamicIncludes = []schema.LockedDynamicInclude{{ExecutableClosure: closure}}
	snapshot, err := plansnapshot.FromExecutionPlan(plan)
	if err != nil || snapshot.SchemaVersion != plansnapshot.SchemaVersionV2 || len(snapshot.Tools) != 0 {
		t.Fatalf("empty-root embedded closure version: %s %v", snapshot.SchemaVersion, err)
	}
}
