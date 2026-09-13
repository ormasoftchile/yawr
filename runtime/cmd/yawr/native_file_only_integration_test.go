//go:build windows && amd64

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/testutil"
)

func TestNativeFileOnlyActualRunStdio(t *testing.T) {
	dir := t.TempDir()
	fixture := filepath.Join(dir, "fixture.exe")
	if _, err := testutil.BuildGoBinary(findRepoRoot(t), fixture, "./internal/tool/testdata/fileonlyfixture"); err != nil {
		t.Fatal(err)
	}
	executable, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(executable)
	if err := os.WriteFile(filepath.Join(dir, "input.txt"), []byte("stdio-staged-value"), 0o600); err != nil {
		t.Fatal(err)
	}
	toolYAML := `apiVersion: yawr.tool/v1
meta: {name: fileonly, version: "1.0.0", description: Synthetic file-only fixture}
transport:
  mode: native-file-only
  command: fixture.exe
  sha256: ` + hex.EncodeToString(sum[:]) + `
  inputs: [input.txt]
actions:
  - name: read
    classification: read-only
    argv: [input.txt]
    result:
      format: yawr.query-result/v1
      source: stdout-json
      row_count: count
      columns: cols
      rows: items
`
	runbookYAML := `apiVersion: yawr.runbook/v1
id: file-only-stdio
name: File-only stdio production path
toolRefs:
  - {name: fileonly, path: fileonly.tool.yaml}
outputs:
  row_count: {type: integer, value_expr: row_count}
  rows: {type: array, value_expr: rows}
flow:
  - step:
      id: read
      type: tool
      tool: {name: fileonly, action: read}
      capture:
        row_count: outputs.row_count
        rows: outputs.rows
  - step: {id: publish, type: results, title: Results}
`
	toolPath := filepath.Join(dir, "fileonly.tool.yaml")
	runbookPath := filepath.Join(dir, "root.runbook.yaml")
	if err := os.WriteFile(toolPath, []byte(toolYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runbookPath, []byte(runbookYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := y1Command(t, "run", runbookPath, "--stdio", "--require-capabilities", "yawr.file-only-subprocess/v1", "--run-dir", filepath.Join(dir, "runs"))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("yawr run --stdio: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
	if !strings.Contains(stdout.String(), `"row_count"`) || !strings.Contains(stdout.String(), `stdio-staged-value`) {
		t.Fatalf("typed Results missing from stdio production path: %s", stdout.String())
	}
}
