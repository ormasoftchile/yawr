package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/presentationview"
	"github.com/ormasoftchile/yawr/runtime/internal/runstore"
)

func TestPresentationIterationOccurrencesDoNotAlias(t *testing.T) {
	source := filepath.Join(findRepoRoot(t), "examples", "code-presentation")
	dir := t.TempDir()
	for _, name := range []string{"code.yawt", "echo.yawr", "profile.yaml"} {
		data, err := os.ReadFile(filepath.Join(source, name))
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, name), string(data))
	}
	writeFile(t, filepath.Join(dir, "root.yawr"), `apiVersion: yawr.runbook/v1
id: repeated-code
name: Repeated code
toolRefs: [{name: code, path: code.yawt}]
flow:
  - iterate:
      id: repeat
      over: "2"
      as: index
      steps:
        - step:
            id: query
            type: tool
            tool:
              name: code
              action: sql
              args: {text: 'SELECT ${index}'}
`)
	t.Chdir(dir)
	runs := filepath.Join(dir, "runs")
	runCaptureStdout(t, []string{"root.yawr", "--profile", "profile.yaml", "--run-dir", runs, "--output", "json"})
	entries, _ := os.ReadDir(runs)
	runID := ""
	for _, entry := range entries {
		if entry.IsDir() {
			runID = entry.Name()
		}
	}
	if runID == "" {
		t.Fatal("run not persisted")
	}
	store := runstore.NewDirRunStore(runs)
	defer store.Close()
	doc, err := presentationview.Inspect(context.Background(), store, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.PresentationState.Occurrences) != 2 {
		data, _ := json.Marshal(doc.PresentationState)
		t.Fatalf("actual repeated tool events missing: %s", data)
	}
	a, b := doc.PresentationState.Occurrences[0], doc.PresentationState.Occurrences[1]
	if a.OutputValueStatus["code"] != "available" || b.OutputValueStatus["code"] != "available" {
		t.Fatal("repeated output unavailable")
	}
	if a.Output["code"] == b.Output["code"] {
		t.Fatal("latest output aliased into both occurrences")
	}
	if a.Identity["event_id"] == b.Identity["event_id"] || a.Identity["frame_id"] == b.Identity["frame_id"] {
		t.Fatalf("frame occurrence identity missing: %#v %#v", a.Identity, b.Identity)
	}
}
