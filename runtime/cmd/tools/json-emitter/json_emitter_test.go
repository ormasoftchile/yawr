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

func TestJSONEmitterBinary(t *testing.T) {
	bin := buildBinary(t, "json-emitter")
	req := map[string]any{
		"action": "emit",
		"args":   map[string]any{"foo": "bar"},
	}
	payload, _ := json.Marshal(req)

	cmd := exec.Command(bin)
	cmd.Stdin = bytes.NewReader(payload)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("json-emitter failed: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed["result"] != "ok" {
		t.Fatalf("expected result ok, got %v", parsed["result"])
	}
	echo, ok := parsed["echo"].(map[string]any)
	if !ok || echo["foo"] != "bar" {
		t.Fatalf("expected echo foo=bar, got %v", parsed["echo"])
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
