package main

// dryrun_declared_outputs_test.go — a dry-run must cover tool-bearing
// runbooks.
//
// A dry-run crosses no transport and expands no substituted action, so an
// action's declared outputs: contract is never satisfied by a real payload.
// Without a stand-in for that contract, every legal capture reading
// outputs.<name> fails with GCP-RESOLVE-002 and the runbook is reported as
// failed — including yawr's own shipped structured-values example, which
// passes under `yawr run`. That left no static gate for any runbook that
// calls a tool.
//
// These tests pin both shapes the example exercises: flat captures of
// declared array outputs (root.runbook.yaml) and nested outputs.<a>.<b>
// captures whose leaf types the declaration does not describe
// (ordering-root.runbook.yaml).

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// dryRunCaptureStdout runs runDryRun with args and returns everything written
// to os.Stdout during the call, along with the exit code.
func dryRunCaptureStdout(t *testing.T, args []string) (string, int) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	os.Stdout = w
	code := runDryRun(args)
	_ = w.Close()
	os.Stdout = old
	runLast = code

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	return buf.String(), code
}

func TestDryRun_CoversCapturesFromSubstitutedActions(t *testing.T) {
	for _, test := range []struct {
		runbook string
		steps   int
	}{
		{runbook: "root.runbook.yaml", steps: 2},
		{runbook: "ordering-root.runbook.yaml", steps: 6},
	} {
		t.Run(test.runbook, func(t *testing.T) {
			dir := copyStructuredValuesExample(t)
			t.Chdir(dir)
			artifacts := t.TempDir()

			out, code := dryRunCaptureStdout(t, []string{
				test.runbook, "--package-map", "package-map.yaml",
				"--run-dir", filepath.Join(artifacts, "runs"), "--output", "json",
			})
			if strings.Contains(out, "GCP-RESOLVE-002") {
				t.Fatalf("dry-run rejected a capture the real run resolves: %s", out)
			}
			if code != exitSuccess {
				t.Fatalf("dry-run exit = %d, want %d: %s", code, exitSuccess, out)
			}
			var summary jsonSummary
			if err := json.Unmarshal([]byte(out), &summary); err != nil {
				t.Fatalf("unmarshal JSON summary: %v (raw: %s)", err, out)
			}
			if summary.Status != "completed" || len(summary.Steps) != test.steps {
				t.Fatalf("dry-run did not cover every step: %s", out)
			}
		})
	}
}

// The stand-in must not become a dispatch: dry-run still executes nothing, so
// each tool step reports the marker rather than a real payload.
func TestDryRun_SynthesizedOutputsStillDispatchNothing(t *testing.T) {
	dir := copyStructuredValuesExample(t)
	t.Chdir(dir)
	artifacts := t.TempDir()

	out, _ := dryRunCaptureStdout(t, []string{
		"root.runbook.yaml", "--package-map", "package-map.yaml",
		"--run-dir", filepath.Join(artifacts, "runs"), "--output", "json",
	})
	var summary jsonSummary
	if err := json.Unmarshal([]byte(out), &summary); err != nil {
		t.Fatalf("unmarshal JSON summary: %v (raw: %s)", err, out)
	}
	relay := summary.Steps[0]
	if relay.Output["dry_run"] != true || relay.Output["would_execute"] != "tool" {
		t.Fatalf("tool step was not simulated: %#v", relay.Output)
	}
	// The declared contract is present and typed, so captures resolve, but it
	// carries the declaration's zero values rather than a produced payload.
	records, ok := relay.Output["records"].([]any)
	if !ok || len(records) != 0 {
		t.Fatalf("declared array output not stood in for as an empty array: %#v", relay.Output)
	}
	labels, ok := relay.Output["labels"].([]any)
	if !ok || len(labels) != 0 {
		t.Fatalf("declared array output not stood in for as an empty array: %#v", relay.Output)
	}
}
