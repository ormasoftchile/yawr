package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/presentationview"
	"github.com/ormasoftchile/yawr/runtime/internal/runstore"
)

func TestPresentationNativeProcess(t *testing.T) {
	if os.Getenv("YAWR_PRESENTATION_TEST_CHILD") != "1" {
		return
	}
	fmt.Print("SELECT native")
	os.Exit(0)
}

func TestPresentationNativeDeclaredOutput(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "native.tool.yaml"), fmt.Sprintf(`apiVersion: yawr.tool/v1
meta: {name: native, version: "1.0.0"}
transport:
  mode: native
  command: '%s'
  env: {YAWR_PRESENTATION_TEST_CHILD: "1"}
actions:
  - name: run
    classification: read-only
    argv: ["-test.run=^TestPresentationNativeProcess$"]
    args:
      text: {type: string, presentation: {version: 1, kind: code, language: sql}}
    outputs:
      code: {type: string, from: stdout, presentation: {version: 1, kind: code, language: sql}}
`, strings.ReplaceAll(executable, "'", "''")))
	writeFile(t, filepath.Join(root, "root.runbook.yaml"), `apiVersion: yawr.runbook/v1
id: native
name: Native presentation
toolRefs: [{name: native, path: native.tool.yaml}]
flow:
  - step:
      id: native
      type: tool
      tool: {name: native, action: run, args: {text: "SELECT authored"}}
`)
	writeFile(t, filepath.Join(root, "profile.yaml"), `apiVersion: yawr.runtime-profile/v1
id: native
context: test
attendance: unattended
approval:
  scope: {allow_read: true, allow_mutating: false, allow_destructive: false}
transport: {allow_subprocess_in_test: false}
`)
	runs := filepath.Join(root, "runs")
	runCaptureStdout(t, []string{"root.runbook.yaml", "--profile", "profile.yaml", "--run-dir", runs, "--output", "json"})
	entries, _ := os.ReadDir(runs)
	runID := ""
	for _, e := range entries {
		if e.IsDir() {
			runID = e.Name()
		}
	}
	if runID == "" {
		t.Fatal("native run not retained")
	}
	store := runstore.NewDirRunStore(runs)
	defer store.Close()
	doc, err := presentationview.Inspect(context.Background(), store, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.PresentationState.Occurrences) != 1 {
		t.Fatal("native terminal metadata absent")
	}
	o := doc.PresentationState.Occurrences[0]
	if o.Output["code"] != "SELECT native" || o.OutputValueStatus["code"] != "available" || len(o.Output) != 1 {
		t.Fatalf("declared native slot or safety projection wrong: %#v", o)
	}
	if o.Details.CodePresentation.Arguments[0].Presentation.Language != "sql" {
		t.Fatal("native input descriptor missing")
	}
}
