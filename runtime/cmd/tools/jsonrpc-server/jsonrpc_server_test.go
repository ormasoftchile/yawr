package main

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/testutil"
)

func TestJSONRPCServerBinary(t *testing.T) {
	bin := buildBinary(t, "jsonrpc-server")

	cmd := exec.Command(bin)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	reader := bufio.NewReader(stdout)
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "echo",
			"arguments": map[string]any{"message": "hi"},
		},
	}
	payload, _ := json.Marshal(req)
	if _, err := stdin.Write(append(payload, '\n')); err != nil {
		t.Fatalf("write: %v", err)
	}

	line, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var resp map[string]any
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	result, ok := resp["result"].(map[string]any)
	if !ok || result["message"] != "hi" {
		t.Fatalf("expected message hi, got %v", resp["result"])
	}

	shutdown := map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "shutdown",
		"params":  map[string]any{},
	}
	shutdownPayload, _ := json.Marshal(shutdown)
	if _, err := stdin.Write(append(shutdownPayload, '\n')); err != nil {
		t.Fatalf("write shutdown: %v", err)
	}
	_ = stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
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
