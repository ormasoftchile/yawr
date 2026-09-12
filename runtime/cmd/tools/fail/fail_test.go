package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/testutil"
)

func TestFailBinary(t *testing.T) {
	bin := buildBinary(t, "fail")
	req := map[string]any{
		"action": "fail",
		"args":   map[string]any{"exit_code": 42},
	}
	payload, _ := json.Marshal(req)

	cmd := exec.Command(bin)
	cmd.Stdin = bytes.NewReader(payload)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected failure")
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected ExitError, got %T", err)
	}
	if exitErr.ExitCode() != 42 {
		t.Fatalf("expected exit code 42, got %d", exitErr.ExitCode())
	}
	if !strings.Contains(string(out), "error: forced failure") {
		t.Fatalf("expected stderr message, got %q", string(out))
	}
}

func buildBinary(t *testing.T, name string) string {
	t.Helper()
	root := repoRoot()
	dir := filepath.Join(root, ".testtools", "cmd-tools", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	output, err := testutil.BuildGoBinary(filepath.Dir(currentFile()), filepath.Join(dir, name), ".")
	if err != nil {
		t.Fatalf("build failed: %v", err)
	}
	return output
}

func repoRoot() string {
	return filepath.Clean(filepath.Join(filepath.Dir(currentFile()), "..", "..", ".."))
}

func currentFile() string {
	_, file, _, _ := runtime.Caller(0)
	return file
}
