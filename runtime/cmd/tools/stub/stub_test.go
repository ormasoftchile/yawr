package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/testutil"
)

func TestStubBinary(t *testing.T) {
	bin := buildBinary(t, "stub")
	req := map[string]any{
		"action": "send-message",
		"args":   map[string]any{"channel": "#ops"},
	}
	payload, _ := json.Marshal(req)

	cmd := exec.Command(bin, "--tool", "slack-notify")
	cmd.Stdin = bytes.NewReader(payload)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("stub failed: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed["tool"] != "slack-notify" {
		t.Fatalf("expected tool slack-notify, got %v", parsed["tool"])
	}
	if parsed["action"] != "send-message" {
		t.Fatalf("expected action send-message, got %v", parsed["action"])
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
