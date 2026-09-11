package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// r21FixturePath resolves the r21 runbook fixture relative to this test file's package directory.
var r21FixturePath = filepath.Join("..", "..", "internal", "parser", "testdata", "runbooks", "r21-yawr-run", "schema.yaml")

func TestRun_SuccessExitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX shell utilities (echo, false, /bin/sh)")
	}
	dir := makeWorkDir(t)
	tracePath := filepath.Join(dir, "trace.jsonl")

	code := runRun([]string{
		r21FixturePath,
		"--trace", tracePath,
		"--output", "quiet",
		"--var", "environment=dev",
	})
	if code != exitSuccess {
		t.Fatalf("expected exit code %d, got %d", exitSuccess, code)
	}
	if _, err := os.Stat(tracePath); err != nil {
		t.Fatalf("expected trace file: %v", err)
	}
}

func TestRun_FailureExitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX shell utilities (echo, false, /bin/sh)")
	}
	dir := makeWorkDir(t)
	runbookPath := filepath.Join(dir, "fail.yaml")
	tracePath := filepath.Join(dir, "trace.jsonl")

	if err := os.WriteFile(runbookPath, []byte(failingRunbook()), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	code := runRun([]string{
		runbookPath,
		"--trace", tracePath,
		"--output", "quiet",
	})
	if code != exitFailure {
		t.Fatalf("expected exit code %d, got %d", exitFailure, code)
	}
}

func failingRunbook() string {
	return `$schema: "https://schemas.yawr.dev/yawr.runbook/v1.json"
apiVersion: yawr.runbook/v1
id: fail-run
name: fail run
flow:
  - step:
      id: fail
      type: cli
      command: "false"
`
}

func makeWorkDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(".", "yawr-run-test-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
