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

func TestPresentationSnapshotConditionalVersion(t *testing.T) {
	plan := &engine.ExecutionPlan{RunbookPath: "root.runbook.yaml", Metadata: engine.PlanMetadata{RunbookID: "root", RunbookName: "Root"}, Steps: []engine.ResolvedStep{{ID: "n", Kind: "noop", Spec: &schema.NoopSpec{}}}, Tools: map[string]*schema.ToolDef{"db": {Name: "db", Actions: map[string]*schema.ToolAction{"run": {Args: map[string]*schema.ArgDef{"text": {Type: "string"}}}}}}}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatal(err)
	}
	legacy, err := plansnapshot.FromExecutionPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.SchemaVersion != plansnapshot.SchemaVersionV1 {
		t.Fatal("metadata-free plan upgraded")
	}
	before, _ := json.Marshal(legacy)
	plan.Tools["db"].Actions["run"].Args["text"].Presentation = &schema.PresentationDescriptor{Version: 1, Kind: "code", Language: "sql"}
	modern, err := plansnapshot.FromExecutionPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	if modern.SchemaVersion != plansnapshot.SchemaVersionV2 {
		t.Fatal("metadata lost by writer")
	}
	data, _ := json.Marshal(modern)
	var decoded plansnapshot.SnapshotV1
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	restored, err := plansnapshot.Restore(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Tools["db"].Actions["run"].Args["text"].Presentation.Language != "sql" {
		t.Fatal("roundtrip lost descriptor")
	}
	forged := strings.Replace(string(data), "execution-plan/v2", "yawr.execution-plan/v1", 1)
	if json.Unmarshal([]byte(forged), &decoded) == nil {
		t.Fatal("v1 accepted metadata")
	}
	if json.Unmarshal([]byte(`{"schema_version":"yawr.execution-plan/v1","schema_version":"execution-plan/v2"}`), &decoded) == nil {
		t.Fatal("duplicate version accepted")
	}
	if json.Unmarshal(append(data, []byte("{}")...), &decoded) == nil {
		t.Fatal("trailing object accepted")
	}
	var old plansnapshot.SnapshotV1
	if err := json.Unmarshal(before, &old); err != nil {
		t.Fatal(err)
	}
	if _, err := plansnapshot.Restore(old); err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(old)
	if string(before) != string(after) {
		t.Fatal("v1 bytes changed")
	}
}
