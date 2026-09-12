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

func TestPresentationSnapshotUsesCurrentVersionOnly(t *testing.T) {
	plan := &engine.ExecutionPlan{
		RunbookPath: "root.runbook.yaml",
		Metadata:    engine.PlanMetadata{RunbookID: "root", RunbookName: "Root"},
		Steps:       []engine.ResolvedStep{{ID: "n", Kind: "noop", Spec: &schema.NoopSpec{}}},
		Tools: map[string]*schema.ToolDef{"db": {
			Name: "db",
			Actions: map[string]*schema.ToolAction{"run": {
				Args: map[string]*schema.ArgDef{"text": {
					Type: "string", Presentation: &schema.PresentationDescriptor{Version: 1, Kind: "code", Language: "sql"},
				}},
			}},
		}},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatal(err)
	}
	snapshot, err := plansnapshot.FromExecutionPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.SchemaVersion != plansnapshot.SchemaVersionV3 {
		t.Fatalf("schema version = %q", snapshot.SchemaVersion)
	}
	data, _ := json.Marshal(snapshot)
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
	for _, obsolete := range []string{"yawr.execution-plan/v1", "execution-plan/v2"} {
		forged := strings.Replace(string(data), plansnapshot.SchemaVersionV3, obsolete, 1)
		if json.Unmarshal([]byte(forged), &decoded) == nil {
			t.Fatalf("obsolete schema %q accepted", obsolete)
		}
	}
	if json.Unmarshal([]byte(`{"schema_version":"execution-plan/v2","schema_version":"execution-plan/v3"}`), &decoded) == nil {
		t.Fatal("duplicate version accepted")
	}
	if json.Unmarshal(append(data, []byte("{}")...), &decoded) == nil {
		t.Fatal("trailing object accepted")
	}
}
