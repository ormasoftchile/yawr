package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	"github.com/ormasoftchile/yawr/runtime/internal/runstore"
	internaltrace "github.com/ormasoftchile/yawr/runtime/internal/trace"
	"github.com/ormasoftchile/yawr/runtime/pkg/presentation"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/render/graphjson"
	"github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

func TestPresentationDirectCLIProcess(t *testing.T) {
	if os.Getenv("YAWR_PRESENTATION_DIRECT_CHILD") != "1" {
		return
	}
	for index, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{os.Args[0]}, os.Args[index+1:]...)
			main()
			return
		}
	}
	t.Fatal("missing CLI arguments")
}

func presentationDirectCLI(t *testing.T, dir string, args ...string) ([]byte, string, error) {
	t.Helper()
	executable := os.Getenv("YAWR_PRESENTATION_CLI")
	if executable == "" {
		var err error
		executable, err = os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		args = append([]string{"-test.run=^TestPresentationDirectCLIProcess$", "--"}, args...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "YAWR_PRESENTATION_DIRECT_CHILD=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.String(), err
}

func TestPresentationDirectCLIPackageFrozenInspection(t *testing.T) {
	for _, name := range []string{"code", "args", "inputs", "outputs", "default", "presentation", "tools", "actions"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			files, err := filepath.Glob(filepath.Join(findRepoRoot(t), "examples", "code-presentation", "*.yaml"))
			if err != nil || len(files) != 4 {
				t.Fatalf("fixture files: %v %v", files, err)
			}
			argName, outputName, sqlAction := "text", "code", "sql"
			if name != "code" {
				argName, outputName, sqlAction = name, name, name
			}
			for _, file := range files {
				data, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				text := strings.ReplaceAll(string(data), "name: code", "name: "+name)
				if name != "code" {
					switch filepath.Base(file) {
					case "code.tool.yaml":
						text = strings.ReplaceAll(text, "name: sql", "name: "+sqlAction)
						text = strings.ReplaceAll(text, "      text:", "      "+argName+":")
						text = strings.ReplaceAll(text, "      code:", "      "+outputName+":")
					case "root.runbook.yaml":
						text = strings.ReplaceAll(text, "action: sql", "action: "+sqlAction)
						text = strings.ReplaceAll(text, "          text:", "          "+argName+":")
					case "echo.runbook.yaml":
						text = strings.ReplaceAll(text, "  text:", "  "+argName+":")
						text = strings.ReplaceAll(text, "  code:", "  "+outputName+":")
						text = strings.ReplaceAll(text, "${text}", "${"+argName+"}")
					}
				}
				writeFile(t, filepath.Join(dir, filepath.Base(file)), text)
			}
			staticRuns := filepath.Join(dir, "static-runs")
			staticOutput, staticStderr, err := presentationDirectCLI(t, dir, "run", "root.runbook.yaml", "--profile", "profile.yaml",
				"--run-dir", staticRuns, "--output", "json")
			if err != nil {
				t.Fatalf("static legal-name run: %v %s %s", err, staticStderr, staticOutput)
			}
			staticPlans, err := filepath.Glob(filepath.Join(staticRuns, "*", "plan.v1.json"))
			if err != nil || len(staticPlans) != 1 {
				t.Fatalf("static plan files: %v %v", staticPlans, err)
			}
			staticData, err := os.ReadFile(staticPlans[0])
			if err != nil {
				t.Fatal(err)
			}
			var staticPlan plansnapshot.SnapshotV1
			if err := json.Unmarshal(staticData, &staticPlan); err != nil || staticPlan.SchemaVersion != plansnapshot.SchemaVersionV3 ||
				staticPlan.Tools[name] == nil || staticPlan.Tools[name].Actions[sqlAction] == nil {
				t.Fatalf("static writer did not preserve legal names in v2: %v", err)
			}
			writeFile(t, filepath.Join(dir, "yawr-package.yaml"), fmt.Sprintf(`apiVersion: yawr.tool-package/v1
meta: {name: presentation-fixture, version: "1.0.0"}
exports:
  tools:
    - {id: %s, path: code.tool.yaml}
  runbooks:
    - {id: child, path: root.runbook.yaml}
`, name))
			writeFile(t, filepath.Join(dir, ".yawr", "config.yaml"), "apiVersion: yawr.config/v1\nrequires:\n  - {package: presentation-fixture, version: '^1.0.0', path: '.'}\n")
			writeFile(t, filepath.Join(dir, "parent.runbook.yaml"), `apiVersion: yawr.runbook/v1
id: dynamic-presentation
name: Dynamic presentation
flow:
  - step:
      id: child
      type: include
      include: {runbook_ref: "presentation-fixture/child", resolve_from: catalog}
`)
			runs := filepath.Join(dir, "runs")
			output, stderr, err := presentationDirectCLI(t, dir, "run", "parent.runbook.yaml", "--profile", "profile.yaml",
				"--run-dir", runs, "--trace", filepath.Join(dir, "projected.jsonl"), "--output", "json")
			if err != nil {
				t.Fatalf("direct run: %v %s %s", err, stderr, output)
			}
			var summary jsonSummary
			if err := json.Unmarshal(output, &summary); err != nil || summary.Status != "completed" {
				t.Fatalf("CLI output polluted or run failed: %s %v", output, err)
			}
			plans, err := filepath.Glob(filepath.Join(runs, "*", "plan.v1.json"))
			if err != nil || len(plans) != 1 {
				t.Fatalf("durable plan: %v %v", plans, err)
			}
			runID := filepath.Base(filepath.Dir(plans[0]))
			store := runstore.NewDirRunStore(runs)
			defer store.Close()
			ctx := context.Background()
			plan, err := store.LoadPlan(ctx, runID)
			if err != nil || len(plan.Tools) != 0 {
				t.Fatalf("expected empty root tools: %v", err)
			}
			state, err := store.LoadState(ctx, runID)
			if err != nil || len(state.DynamicIncludes) != 1 {
				t.Fatalf("dynamic commit: %v", err)
			}
			for _, resolution := range state.DynamicIncludes {
				closure := resolution.Pin.ExecutableClosure
				if !bytes.Contains(closure, []byte(plansnapshot.FlowClosureSchemaV3)) {
					t.Fatalf("closure not v2: %s", closure)
				}
				tools, err := plansnapshot.RestoreFlowTools(closure)
				if err != nil || tools[name] == nil || tools[name].Actions[sqlAction] == nil {
					t.Fatalf("original frozen action missing: %v", err)
				}
				action := tools[name].Actions[sqlAction]
				if action.Args[argName].Presentation.Language != "sql" || action.Outputs[outputName].Presentation.Language != "sql" {
					t.Fatal("frozen descriptors changed")
				}
			}
			events, err := internaltrace.NewJSONLReader(store.TracePath(runID)).ReadAllStrict(ctx)
			if err != nil || len(events) < 3 {
				t.Fatalf("durable producer did not retain complete trace: %v", err)
			}
			if events[0].Kind != "package/resolved" || events[1].Kind != "catalog/frozen" || events[2].Kind != "plan.validated" {
				t.Fatalf("wrong pre-start prefix: %v", events[:3])
			}
			projected, err := internaltrace.NewJSONLReader(filepath.Join(dir, "projected.jsonl")).ReadAllStrict(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, stream := range [][]trace.TraceEvent{events, projected} {
				for index := range stream {
					var payload any
					decoder := json.NewDecoder(bytes.NewReader(stream[index].Payload))
					decoder.UseNumber()
					if err := decoder.Decode(&payload); err != nil {
						t.Fatal(err)
					}
					stream[index].Payload, err = json.Marshal(payload)
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			if err != nil || !reflect.DeepEqual(events, projected) {
				t.Fatalf("live/durable linkage differs or duplicates: %v", err)
			}
			previewArgs := []string{"preview", "--format", "graphjson", "--run-dir", runs, "--run-id", runID}
			before, stderr, err := presentationDirectCLI(t, dir, previewArgs...)
			if err != nil {
				t.Fatalf("saved preview before deletion: %v %s", err, stderr)
			}
			assertDirectFrozenPreview(t, before, name, argName, outputName, sqlAction)
			for _, file := range append(files, filepath.Join(dir, "parent.runbook.yaml")) {
				if err := os.Remove(filepath.Join(dir, filepath.Base(file))); err != nil {
					t.Fatal(err)
				}
			}
			writeFile(t, filepath.Join(dir, "yawr-package.yaml"), "apiVersion: yawr.tool-package/v1\nmeta: {name: replacement, version: '9.0.0'}\n")
			after, stderr, err := presentationDirectCLI(t, dir, previewArgs...)
			if err != nil {
				t.Fatalf("saved preview after deletion: %v %s", err, stderr)
			}
			assertDirectFrozenPreview(t, after, name, argName, outputName, sqlAction)
			if !bytes.Equal(before, after) {
				t.Fatal("source deletion/catalog replacement changed saved preview")
			}
			tracePath := store.TracePath(runID)
			complete, err := os.ReadFile(tracePath)
			if err != nil {
				t.Fatal(err)
			}
			gap := complete[bytes.IndexByte(complete, '\n')+1:]
			if err := os.WriteFile(tracePath, gap, 0600); err != nil {
				t.Fatal(err)
			}
			_, stderr, err = presentationDirectCLI(t, dir, previewArgs...)
			if err == nil || !strings.Contains(stderr, "not contiguous") {
				t.Fatalf("retained trace gap accepted: %v %s", err, stderr)
			}
			if err := os.WriteFile(tracePath, complete, 0600); err != nil {
				t.Fatal(err)
			}
			if name == "code" {
				lines := bytes.Split(complete, []byte{'\n'})
				var first map[string]json.RawMessage
				if err := json.Unmarshal(lines[0], &first); err != nil {
					t.Fatal(err)
				}
				first["event_id"], _ = json.Marshal(events[1].EventID)
				lines[0], err = json.Marshal(first)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(tracePath, bytes.Join(lines, []byte{'\n'}), 0600); err != nil {
					t.Fatal(err)
				}
				_, stderr, err = presentationDirectCLI(t, dir, previewArgs...)
				if err == nil || !strings.Contains(stderr, "event id") {
					t.Fatalf("conflicting trace identity accepted: %v %s", err, stderr)
				}
				if err := os.WriteFile(tracePath, complete, 0600); err != nil {
					t.Fatal(err)
				}
				checkpoints, err := filepath.Glob(filepath.Join(runs, runID, "snapshots", "checkpoint-*.json"))
				if err != nil || len(checkpoints) == 0 {
					t.Fatalf("checkpoint files: %v %v", checkpoints, err)
				}
				checkpointPath := checkpoints[len(checkpoints)-1]
				checkpoint, err := os.ReadFile(checkpointPath)
				if err != nil {
					t.Fatal(err)
				}
				var stateFields map[string]json.RawMessage
				if err := json.Unmarshal(checkpoint, &stateFields); err != nil {
					t.Fatal(err)
				}
				stateFields["PlanSnapshotDigest"], _ = json.Marshal("sha256:" + strings.Repeat("0", 64))
				mixed, err := json.Marshal(stateFields)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(checkpointPath, mixed, 0600); err != nil {
					t.Fatal(err)
				}
				_, stderr, err = presentationDirectCLI(t, dir, previewArgs...)
				if err == nil || (!strings.Contains(stderr, "digest") && !strings.Contains(stderr, "mixed checkpoint")) {
					t.Fatalf("mixed checkpoint generation accepted: %v %s", err, stderr)
				}
				if err := os.WriteFile(checkpointPath, checkpoint, 0600); err != nil {
					t.Fatal(err)
				}
				recovered, stderr, err := presentationDirectCLI(t, dir, previewArgs...)
				if err != nil || !bytes.Equal(recovered, before) {
					t.Fatalf("restored complete evidence differs: %v %s", err, stderr)
				}
			}
			t.Logf("direct CLI tool=%s run=%s events=%d closure=v2 frozen preview identical after source deletion; gap rejected", name, runID, len(events))
		})
	}
}

func assertDirectFrozenPreview(t *testing.T, data []byte, toolName, argName, outputName, sqlAction string) {
	t.Helper()
	var doc graphjson.Document
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.PresentationState == nil || len(doc.PresentationState.Occurrences) != 3 {
		t.Fatalf("missing terminal outputs: %s", data)
	}
	languages := map[string]bool{}
	frozenNodes := 0
	for _, node := range doc.Nodes {
		details, _ := json.Marshal(node.Data["details"])
		var decoded struct {
			Presentation *presentation.Envelope `json:"code_presentation"`
		}
		if err := json.Unmarshal(details, &decoded); err != nil {
			t.Fatal(err)
		}
		if envelope := decoded.Presentation; envelope != nil {
			frozenNodes++
			if envelope.Origin != "frozen" || envelope.ToolID != toolName ||
				envelope.PlanSnapshotDigest != doc.PresentationState.PlanSnapshotDigest {
				t.Fatalf("frozen graph definition missing: %+v", envelope)
			}
		}
	}
	if frozenNodes != 3 {
		t.Fatalf("frozen graph descriptors = %d, want 3", frozenNodes)
	}
	for _, occurrence := range doc.PresentationState.Occurrences {
		envelope := occurrence.Details.CodePresentation
		if envelope == nil || envelope.Origin != "frozen" || envelope.ToolID != toolName ||
			envelope.PlanSnapshotDigest != doc.PresentationState.PlanSnapshotDigest {
			t.Fatalf("frozen binding missing: %+v", envelope)
		}
		if len(envelope.Arguments) != 1 || len(envelope.Outputs) != 1 ||
			envelope.Arguments[0].Name != argName || envelope.Outputs[0].Name != outputName ||
			envelope.Arguments[0].Presentation == nil || envelope.Outputs[0].Presentation == nil {
			t.Fatalf("definition fields missing: %+v", envelope)
		}
		language := envelope.Outputs[0].Presentation.Language
		languages[language] = true
		if language == "sql" && envelope.Action != sqlAction {
			t.Fatal("original frozen SQL action lost")
		}
		if occurrence.OutputValueStatus[outputName] != "available" {
			t.Fatalf("unapproved output: %+v", occurrence)
		}
		expected := map[string]string{
			"sql":        "SELECT name\nFROM synthetic_table\nWHERE enabled = 1;",
			"kql":        "print message = \"No provider is called\"\n| project message",
			"powershell": "Write-Output \"Stored as data, never executed\"",
		}
		if text, ok := occurrence.Output[outputName].(string); !ok || text != expected[language] {
			t.Fatalf("approved original output changed for %s: %q", language, text)
		}
	}
	if !languages["sql"] || !languages["kql"] || !languages["powershell"] {
		t.Fatalf("languages = %v", languages)
	}
}
