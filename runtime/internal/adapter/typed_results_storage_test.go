package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	internalengine "github.com/ormasoftchile/yawr/runtime/internal/engine"
	internalplanner "github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/internal/runstore"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestTypedResultsEngineStorageIntegrity(t *testing.T) {
	for _, test := range []string{"large-blob", "digest", "declaration", "downgrade-v1", "downgrade-v2", "plan-downgrade"} {
		t.Run(test, func(t *testing.T) {
			ctx := context.Background()
			dir := typedEngineDirectory(t)
			cfg, shutdown, err := BuildEngineConfig(ctx, WireOptions{Mode: "real", RunDir: filepath.Join(dir, "runs"), TraceFile: filepath.Join(dir, "trace.jsonl"), ToolScanDir: dir})
			if err != nil {
				t.Fatal(err)
			}
			payload := "original"
			if test == "large-blob" {
				payload = strings.Repeat("canonical-not-preview-", 8192)
			}
			plan := &engine.ExecutionPlan{RunID: "typed-storage", RunbookPath: filepath.Join(dir, "root.yaml"), Metadata: engine.PlanMetadata{RunbookID: "typed-storage"},
				Outputs: map[string]*schema.Output{"result": {Type: "string", ValueTreePresent: true, ValueTree: payload}},
				Steps:   []engine.ResolvedStep{{ID: "results", Kind: "results", Spec: &schema.ResultsSpec{}}},
			}
			if err := internalplanner.ValidateExecutionPlan(plan); err != nil {
				t.Fatal(err)
			}
			handle, err := internalengine.New(cfg).Start(ctx, plan, engine.RunOptions{Store: cfg.Store})
			if err != nil {
				t.Fatal(err)
			}
			for {
				_, err := handle.Next(ctx)
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			state := handle.State()
			if state.Results == nil {
				t.Fatal("missing publication")
			}
			shutdown()
			store := runstore.NewDirRunStore(filepath.Join(dir, "runs"))
			defer store.Close()
			snapshotPath := filepath.Join(store.RunDir(plan.RunID), "snapshots", fmt.Sprintf("checkpoint-%020d.json", state.CheckpointSequence))
			data, err := os.ReadFile(snapshotPath)
			if err != nil {
				t.Fatal(err)
			}
			var snapshot map[string]any
			if err := json.Unmarshal(data, &snapshot); err != nil {
				t.Fatal(err)
			}
			if snapshot["SchemaVersion"] != "run-state/v3" {
				t.Fatal("typed state not v3")
			}
			registry := snapshot["TypedState"].(map[string]any)
			record := registry["results:"+state.Results.PublicationID].(map[string]any)
			if test == "large-blob" {
				if record["blob"] == nil || record["inline"] != nil {
					t.Fatal("large canonical record not blob-backed")
				}
				loaded, err := store.LoadState(ctx, plan.RunID)
				if err != nil {
					t.Fatal(err)
				}
				if loaded.Results.Outputs["result"].Value != payload {
					t.Fatal("canonical report truncated")
				}
				if loaded.Results.Digest != state.Results.Digest {
					t.Fatal("blob roundtrip digest changed")
				}
				return
			}
			switch test {
			case "digest":
				record["inline"].(map[string]any)["outputs"].(map[string]any)["result"].(map[string]any)["value"] = "tampered"
			case "declaration":
				scope := registry["scope:root"].(map[string]any)["inline"].(map[string]any)
				invocation := scope["invocation"].(map[string]any)
				invocation["outputs"].(map[string]any)["result"].(map[string]any)["value_tree"] = "replacement"
				encoded, _ := json.Marshal(invocation)
				var declaration schema.RunbookInvocation
				if err := json.Unmarshal(encoded, &declaration); err != nil {
					t.Fatal(err)
				}
				scope["declaration_digest"] = engine.InvocationDigest(&declaration)
			case "downgrade-v1":
				snapshot["SchemaVersion"] = "yawr.run-state/v1"
			case "downgrade-v2":
				snapshot["SchemaVersion"] = "run-state/v2"
			case "plan-downgrade":
				planData, err := os.ReadFile(store.PlanPath(plan.RunID))
				if err != nil {
					t.Fatal(err)
				}
				planData = []byte(strings.Replace(string(planData), "execution-plan/v3", "execution-plan/v2", 1))
				if err := os.WriteFile(store.PlanPath(plan.RunID), planData, 0600); err != nil {
					t.Fatal(err)
				}
			}
			data, err = json.Marshal(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(snapshotPath, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := store.LoadState(ctx, plan.RunID); err == nil {
				t.Fatal("tampered canonical state accepted")
			}
		})
	}
}
