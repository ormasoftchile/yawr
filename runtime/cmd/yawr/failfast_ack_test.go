package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	internalrunstore "github.com/ormasoftchile/yawr/runtime/internal/runstore"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
)

// ─── Part A: Profileless non-interactive fail-fast ────────────────────────────

// TestCLI_Profileless_NonInteractive_FailFast verifies that a profileless
// yawr run in a non-interactive (non-TTY) environment exits immediately with
// an actionable error, rather than blocking forever on the TerminalApprovalGate.
//
// Mechanism: interactiveTTYDetect is temporarily replaced with a stub that
// reports false (simulating a CI pipe), and runRun is called without --profile.
// The fail-fast check must fire before any approval gate is installed.
//
// If the check is removed, runRun would attempt to start the engine and block
// (or fail for unrelated reasons), so the exitValidation assertion would fail.
func TestCLI_Profileless_NonInteractive_FailFast(t *testing.T) {
	// Temporarily simulate a non-interactive environment.
	saved := interactiveTTYDetect
	interactiveTTYDetect = func() bool { return false }
	t.Cleanup(func() { interactiveTTYDetect = saved })

	dir := makeWorkDir(t)
	runbookPath := filepath.Join(dir, "runbook.yaml")
	writeFile(t, runbookPath, `$schema: "https://schemas.yawr.dev/yawr.runbook/v1.json"
apiVersion: yawr.runbook/v1
id: failfast-test
name: failfast test
flow:
  - step:
      id: end
      type: end
      outcome: {category: success, code: ok}
`)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	stderr := captureStderr(t, func() int {
		return runRun([]string{"runbook.yaml", "--trace", "trace.jsonl", "--output", "quiet"})
	})
	if runLast != exitValidation {
		t.Fatalf("expected exit code %d (exitValidation), got %d; stderr: %s",
			exitValidation, runLast, stderr)
	}
	if !bytes.Contains([]byte(stderr), []byte("non-interactive")) {
		t.Errorf("expected non-interactive error in stderr, got: %s", stderr)
	}
	if !bytes.Contains([]byte(stderr), []byte("--profile")) {
		t.Errorf("expected fix hint (--profile) in stderr, got: %s", stderr)
	}
}

// TestCLI_Profileless_Interactive_Allowed verifies that a profileless run in
// an interactive (TTY) environment is NOT refused by the fail-fast check.
// The run proceeds normally (even if it fails for other reasons).
func TestCLI_Profileless_Interactive_Allowed(t *testing.T) {
	// Simulate an interactive terminal (should already be true via TestMain,
	// but make this explicit for clarity).
	saved := interactiveTTYDetect
	interactiveTTYDetect = func() bool { return true }
	t.Cleanup(func() { interactiveTTYDetect = saved })

	dir := makeWorkDir(t)
	runbookPath := filepath.Join(dir, "runbook.yaml")
	writeFile(t, runbookPath, `$schema: "https://schemas.yawr.dev/yawr.runbook/v1.json"
apiVersion: yawr.runbook/v1
id: interactive-test
name: interactive test
flow:
  - step:
      id: end
      type: end
      outcome: {category: success, code: ok}
`)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	stderr := captureStderr(t, func() int {
		return runRun([]string{"runbook.yaml", "--trace", "trace.jsonl", "--output", "quiet"})
	})
	// The run may succeed or fail for other reasons, but it must NOT fail
	// with the non-interactive error — that would mean the fail-fast check
	// fired in an interactive context, which is wrong.
	if bytes.Contains([]byte(stderr), []byte("non-interactive execution requires")) {
		t.Errorf("fail-fast fired in interactive context; stderr: %s", stderr)
	}
}

// ─── Part B: --acknowledge-indeterminate ────────────────────────────────────

// TestCLI_AcknowledgeIndeterminate_BlockedWithoutFlag verifies that resuming
// a run whose persisted status is RunStatusIndeterminate is refused when
// --acknowledge-indeterminate is not supplied.
//
// The error must appear in stderr and the exit code must be exitRuntime
// (the engine's resume path propagates the error via fmt.Fprintln(os.Stderr, err)).
func TestCLI_AcknowledgeIndeterminate_BlockedWithoutFlag(t *testing.T) {
	dir := makeWorkDir(t)
	// Resolve absolute path before any chdir so seedIndeterminateRun uses
	// an absolute base regardless of CWD changes below.
	absDir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}

	runID := "test-indeterminate-run"
	seedIndeterminateRun(t, absDir, runID)

	// Write profile into the absolute dir before chdir.
	profilePath := filepath.Join(absDir, "unattended.profile.yaml")
	writeFile(t, profilePath, unattendedProfileYAML)

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(absDir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	stderr := captureStderr(t, func() int {
		return runRun([]string{
			"--resume", runID,
			"--run-dir", filepath.Join(absDir, ".runbook", "runs"),
			"--profile", profilePath,
			"--trace", "trace.jsonl",
			"--output", "quiet",
		})
	})
	if runLast != exitRuntime {
		t.Fatalf("expected exit code %d (exitRuntime), got %d; stderr: %s",
			exitRuntime, runLast, stderr)
	}
	// The engine must emit ErrIndeterminateAcknowledgmentRequired.
	if !bytes.Contains([]byte(stderr), []byte("indeterminate")) {
		t.Errorf("expected 'indeterminate' in stderr; got: %s", stderr)
	}
	if !bytes.Contains([]byte(stderr), []byte("acknowledge-indeterminate")) {
		t.Errorf("expected '--acknowledge-indeterminate' hint in stderr; got: %s", stderr)
	}
}

// TestCLI_AcknowledgeIndeterminate_PermittedWithFlag verifies that supplying
// --acknowledge-indeterminate allows the engine to proceed past the
// indeterminate guard. The run will fail for other reasons (no plan registered
// in the new store instance), but it must NOT fail with the indeterminate
// acknowledgment error — that would mean the flag was not read.
func TestCLI_AcknowledgeIndeterminate_PermittedWithFlag(t *testing.T) {
	dir := makeWorkDir(t)
	absDir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}

	runID := "test-indeterminate-run-ack"
	seedIndeterminateRun(t, absDir, runID)

	profilePath := filepath.Join(absDir, "unattended.profile.yaml")
	writeFile(t, profilePath, unattendedProfileYAML)

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(absDir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	stderr := captureStderr(t, func() int {
		return runRun([]string{
			"--resume", runID,
			"--run-dir", filepath.Join(absDir, ".runbook", "runs"),
			"--acknowledge-indeterminate",
			"--profile", profilePath,
			"--trace", "trace-ack.jsonl",
			"--output", "quiet",
		})
	})
	// Must NOT contain the acknowledgment-required error — that means the
	// flag bypassed the guard as intended. The run will fail for other
	// reasons (no plan in the new in-memory store instance), but that is
	// a different error path.
	if bytes.Contains([]byte(stderr), []byte("acknowledge-indeterminate")) &&
		bytes.Contains([]byte(stderr), []byte("indeterminate state")) {
		t.Errorf("--acknowledge-indeterminate did not bypass the guard; stderr: %s", stderr)
	}
}

// ─── helpers ────────────────────────────────────────────────────────────────

// seedIndeterminateRun writes a minimal run snapshot with RunStatusIndeterminate
// into the default run store location (.runbook/runs/<runID>) relative to the
// current directory. This simulates a run that halted mid-execution with an
// unconfirmed step.
func seedIndeterminateRun(t *testing.T, dir, runID string) {
	t.Helper()
	ctx := context.Background()
	runDir := filepath.Join(dir, ".runbook", "runs")
	store := internalrunstore.NewDirRunStore(runDir)
	state := engine.RunState{
		RunID:  runID,
		Status: engine.RunStatusIndeterminate,
	}
	if err := store.SaveState(ctx, state); err != nil {
		t.Fatalf("seedIndeterminateRun: SaveState: %v", err)
	}
}

// unattendedProfileYAML is a minimal profile suitable for use in tests that
// need a profile but do not care about specific approval or context settings.
const unattendedProfileYAML = `apiVersion: yawr.runtime-profile/v1
id: unattended-test-cli
context: cli-operator
attendance: unattended
approval:
  scope:
    allow_read: true
    allow_mutating: true
    allow_destructive: true
`
