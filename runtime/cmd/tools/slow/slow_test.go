package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/testutil"
)

func TestSlowBinary(t *testing.T) {
	bin := buildBinary(t, "slow")
	req := map[string]any{
		"action": "run",
		"args":   map[string]any{"delay_seconds": 1},
	}
	payload, _ := json.Marshal(req)

	start := time.Now()
	cmd := exec.Command(bin)
	cmd.Stdin = bytes.NewReader(payload)
	if out, err := cmd.Output(); err != nil {
		t.Fatalf("slow failed: %v (%s)", err, string(out))
	}
	if time.Since(start) < 900*time.Millisecond {
		t.Fatalf("expected delay, got %v", time.Since(start))
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
