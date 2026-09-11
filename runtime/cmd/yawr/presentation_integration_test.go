package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/presentationview"
	"github.com/ormasoftchile/yawr/runtime/internal/runstore"
)

func TestPresentationRealRunFrozenInspection(t *testing.T) {
	source := filepath.Join(findRepoRoot(t), "examples", "code-presentation")
	dir := t.TempDir()
	files, err := filepath.Glob(filepath.Join(source, "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, filepath.Base(file)), string(data))
	}
	t.Chdir(dir)
	runs := filepath.Join(dir, "runs")
	output := runCaptureStdout(t, []string{"root.runbook.yaml", "--profile", "profile.yaml", "--run-dir", runs, "--output", "json"})
	var summary jsonSummary
	if err := json.Unmarshal([]byte(output), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Status != "completed" || len(summary.Steps) != 3 {
		t.Fatalf("real offline run failed: %s", output)
	}
	entries, err := os.ReadDir(runs)
	if err != nil {
		t.Fatal(err)
	}
	runID := ""
	for _, entry := range entries {
		if entry.IsDir() {
			runID = entry.Name()
		}
	}
	if runID == "" {
		t.Fatal("no durable run")
	}
	store := runstore.NewDirRunStore(runs)
	defer store.Close()
	data, err := os.ReadFile(store.PlanPath(runID))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"schema_version":"execution-plan/v2"`) {
		t.Fatal("metadata run not frozen as v2")
	}
	for _, file := range files {
		if err := os.Remove(filepath.Join(dir, filepath.Base(file))); err != nil {
			t.Fatal(err)
		}
	}
	doc, err := presentationview.Inspect(context.Background(), store, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.PresentationState.Occurrences) != 3 {
		encoded, _ := json.Marshal(doc.PresentationState)
		t.Fatalf("missing real terminal occurrences: %s", encoded)
	}
	for _, o := range doc.PresentationState.Occurrences {
		if o.Details.CodePresentation.Origin != "frozen" || o.Details.CodePresentation.ToolID == "" || o.OutputValueStatus["code"] != "available" {
			t.Fatalf("metadata or safe output unavailable: %#v", o)
		}
		if _, ok := o.Output["code"].(string); !ok {
			t.Fatal("output missing")
		}
	}
}
