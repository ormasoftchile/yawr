package sessioncoordinator

import (
	"encoding/json"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/session"
)

func TestPresentationGraphBindingIntegrity(t *testing.T) {
	plan := &engine.ExecutionPlan{
		RunbookPath: "root.runbook.yaml",
		Metadata:    engine.PlanMetadata{RunbookID: "root", RunbookName: "Root"},
		Steps:       []engine.ResolvedStep{{ID: "query", Kind: "tool", Spec: &schema.ToolCallSpec{Tool: schema.ToolInvocation{Name: "db", Action: "run"}}}},
		Tools: map[string]*schema.ToolDef{"db": {Name: "db", Actions: map[string]*schema.ToolAction{"run": {
			Outputs: map[string]*schema.ArgDef{"code": {Type: "string", Presentation: &schema.PresentationDescriptor{Version: 1, Kind: "code", Language: "sql"}}},
		}}}},
	}
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
	for _, field := range []string{"plan_snapshot_digest", "tool_id", "tool_digest", "action", "origin", "version", "language"} {
		t.Run(field, func(t *testing.T) {
			var document boundHandoffGraph
			if err := decodeHandoffJSON(bound, &document); err != nil {
				t.Fatal(err)
			}
			e := document.Nodes[0].Data["details"].(map[string]any)["code_presentation"].(map[string]any)
			if field == "language" {
				e["outputs"].([]any)[0].(map[string]any)["presentation"].(map[string]any)["language"] = "powershell"
			} else if field == "version" {
				e[field] = 2
			} else {
				e[field] = "changed"
			}
			structuralErr := validateHandoffGraphContentHash(document.Document)
			if (structuralErr == nil) != (field == "plan_snapshot_digest") {
				t.Fatalf("incorrect structural exclusion: %v", structuralErr)
			}
			tampered, _ := json.Marshal(document)
			if err := validateBoundHandoffGraph(plan, snapshot.Digest, tampered); err == nil {
				t.Fatal("tampering bypassed full bound hash")
			}
			document.BoundContentHash = session.DigestJSON(document.Document)
			tampered, _ = json.Marshal(document)
			if err := validateBoundHandoffGraph(plan, snapshot.Digest, tampered); err == nil {
				t.Fatal("rehashing bypassed snapshot equality or structural identity")
			}
		})
	}
}
