package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestRun_Enum008_CallerVarRejected covers R1
// (barbara-enum-mvp-implementation-gate.md): a --var value bound to a
// declared, enum-constrained runbook input that is not a declared member
// must be rejected by the real `yawr run` CLI entry point (cmd/yawr/run.go
// builds its own runVars and calls internal/engine directly -- it never
// calls pkg/run.Start), with a non-zero exit code and an ENUM-008 error on
// stderr. This is the exact live-probe scenario from the rejected gate
// review (`yawr run enum-input.runbook.yaml --var env_name=not-a-member`).
func TestRun_Enum008_CallerVarRejected(t *testing.T) {
	dir := makeWorkDir(t)
	runbookPath := filepath.Join(dir, "enum-input.runbook.yaml")
	tracePath := filepath.Join(dir, "trace.jsonl")

	writeFile(t, runbookPath, `$schema: "https://schemas.yawr.dev/yawr.runbook/v1.json"
apiVersion: yawr.runbook/v1
id: r-enum-caller-var
name: enum caller var CLI test
inputs:
  env_name:
    type: string
    required: true
    enum: ["prod", "staging"]
flow:
  - step:
      id: show
      type: display
      title: Show env
      content: "Env: ${env_name}"
  - step:
      id: end
      type: end
      outcome: {category: success, code: ok}
`)

	stderr := captureStderr(t, func() int {
		return runRun([]string{
			runbookPath,
			"--trace", tracePath,
			"--output", "quiet",
			"--var", "env_name=not-a-member",
		})
	})

	code := runLast
	if code == exitSuccess {
		t.Fatalf("expected a non-zero exit code for an off-enum --var caller binding, got %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stderr, "ENUM-008") {
		t.Fatalf("expected ENUM-008 in stderr, got: %s", stderr)
	}
}

// TestRun_Enum008_CallerVarAccepted is the corresponding happy path: a
// declared-member --var value must not be rejected and the run must
// complete successfully end to end through the CLI.
func TestRun_Enum008_CallerVarAccepted(t *testing.T) {
	dir := makeWorkDir(t)
	runbookPath := filepath.Join(dir, "enum-input-ok.runbook.yaml")
	tracePath := filepath.Join(dir, "trace.jsonl")

	writeFile(t, runbookPath, `$schema: "https://schemas.yawr.dev/yawr.runbook/v1.json"
apiVersion: yawr.runbook/v1
id: r-enum-caller-var-ok
name: enum caller var CLI test ok
inputs:
  env_name:
    type: string
    required: true
    enum: ["prod", "staging"]
flow:
  - step:
      id: show
      type: display
      title: Show env
      content: "Env: ${env_name}"
  - step:
      id: end
      type: end
      outcome: {category: success, code: ok}
`)

	code := runRun([]string{
		runbookPath,
		"--trace", tracePath,
		"--output", "quiet",
		"--var", "env_name=prod",
	})
	if code != exitSuccess {
		t.Fatalf("expected exit code %d for a declared-member --var value, got %d", exitSuccess, code)
	}
}

// TestRun_EnumW001_SurfacedOnStderr covers R4
// (barbara-enum-mvp-implementation-gate.md): a case-only-distinct enum
// declaration (ENUM-W001, non-fatal per AR-ENUM-12) must be printed to
// stderr by the real `yawr run` CLI path -- previously it reached only
// ParsedRunbook.Warnings, which had no consumer outside a test -- and the
// run must still succeed (the warning MUST NOT fail the run).
func TestRun_EnumW001_SurfacedOnStderr(t *testing.T) {
	dir := makeWorkDir(t)
	runbookPath := filepath.Join(dir, "enum-w001.runbook.yaml")
	tracePath := filepath.Join(dir, "trace.jsonl")

	writeFile(t, runbookPath, `$schema: "https://schemas.yawr.dev/yawr.runbook/v1.json"
apiVersion: yawr.runbook/v1
id: r-enum-w001
name: enum w001 CLI test
inputs:
  env_name:
    type: string
    required: false
    enum: ["Prod", "prod"]
flow:
  - step:
      id: end
      type: end
      outcome: {category: success, code: ok}
`)

	stderr := captureStderr(t, func() int {
		return runRun([]string{
			runbookPath,
			"--trace", tracePath,
			"--output", "quiet",
		})
	})

	if runLast != exitSuccess {
		t.Fatalf("ENUM-W001 is non-fatal and MUST NOT fail the run; got exit code %d (stderr: %s)", runLast, stderr)
	}
	if !strings.Contains(stderr, "ENUM-W001") {
		t.Fatalf("expected ENUM-W001 to be surfaced on stderr, got: %s", stderr)
	}
}
