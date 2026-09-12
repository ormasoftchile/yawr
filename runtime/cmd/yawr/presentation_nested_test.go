package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/presentationview"
	"github.com/ormasoftchile/yawr/runtime/internal/runstore"
)

func TestPresentationWrapperAndChildFrozenIdentity(t *testing.T) {
	dir := t.TempDir()
	files, _ := filepath.Glob(filepath.Join(findRepoRoot(t), "examples", "code-presentation", "*.yaml"))
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, filepath.Base(file)), string(data))
	}
	writeFile(t, filepath.Join(dir, "echo.runbook.yaml"), `apiVersion: yawr.runbook/v1
id: wrapper-body
name: Wrapper body
toolRefs: [{name: leaf, path: leaf.tool.yaml}]
inputs:
  text: {type: string, required: true}
outputs:
  code: {type: string, value: "${text}"}
flow:
  - step:
      id: nested
      type: tool
      tool: {name: leaf, action: query, args: {text: "print child = 1"}}
`)
	writeFile(t, filepath.Join(dir, "leaf.tool.yaml"), `apiVersion: yawr.tool/v1
meta: {name: leaf, version: "1.0.0"}
transport: {mode: native, command: must-not-execute}
actions:
  - name: query
    classification: read-only
    args:
      text: {type: string, required: true, presentation: {version: 1, kind: code, language: kql}}
    outputs:
      code: {type: string, presentation: {version: 1, kind: code, language: kql}}
    execute: {kind: runbook, path: leaf.runbook.yaml}
`)
	writeFile(t, filepath.Join(dir, "leaf.runbook.yaml"), `apiVersion: yawr.runbook/v1
id: leaf-body
name: Leaf body
inputs:
  text: {type: string, required: true}
outputs:
  code: {type: string, value: "${text}"}
flow:
  - step: {id: noop, type: noop}
`)
	t.Chdir(dir)
	runs := filepath.Join(dir, "runs")
	runCaptureStdout(t, []string{"root.runbook.yaml", "--profile", "profile.yaml", "--run-dir", runs, "--output", "json"})
	entries, _ := os.ReadDir(runs)
	runID := ""
	for _, entry := range entries {
		if entry.IsDir() {
			runID = entry.Name()
		}
	}
	sources, _ := filepath.Glob(filepath.Join(dir, "*.yaml"))
	for _, file := range sources {
		if err := os.Remove(file); err != nil {
			t.Fatal(err)
		}
	}
	store := runstore.NewDirRunStore(runs)
	defer store.Close()
	doc, err := presentationview.Inspect(context.Background(), store, runID)
	if err != nil {
		t.Fatal(err)
	}
	wrappers, children := 0, 0
	identities := map[string]bool{}
	for _, occurrence := range doc.PresentationState.Occurrences {
		e := occurrence.Details.CodePresentation
		if e.Origin != "frozen" || e.PlanSnapshotDigest != doc.PresentationState.PlanSnapshotDigest {
			t.Fatal("lost frozen binding")
		}
		id := occurrence.Identity["qualified_node_id"].(string)
		if identities[id] {
			t.Fatal("wrapper/child occurrence alias")
		}
		identities[id] = true
		if occurrence.OutputValueStatus["code"] != "available" {
			t.Fatal("safe output not available")
		}
		switch e.ToolID {
		case "code":
			wrappers++
			if occurrence.Output["code"] == "print child = 1" {
				t.Fatal("child overwrote wrapper")
			}
		case "leaf":
			children++
			if e.Action != "query" || e.Outputs[0].Presentation.Language != "kql" || occurrence.Output["code"] != "print child = 1" {
				t.Fatal("child borrowed wrapper identity")
			}
		default:
			t.Fatalf("unexpected tool %q", e.ToolID)
		}
	}
	if wrappers != 3 || children != 3 {
		t.Fatalf("frozen wrappers/children = %d/%d", wrappers, children)
	}
}
