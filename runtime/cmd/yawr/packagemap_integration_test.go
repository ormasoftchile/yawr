package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/testutil"
)

// TestRun_PackageMap_RealVsMockBinding is a CLI integration test modeled on
// the r23-tool-package conformance fixture
// A runbook
// pins a package name (acme.incident-tools) and bare tool name (kubectl)
// via toolRefs. Run unchanged with no flag, the project's real
// .yawr/config.yaml requires: binding is used; run with --package-map
// pointing at a second (mock) package root using the identical yawr.config/v1
// shape, the exact same runbook resolves the mock binding instead — no
// runbook edit either way (task requirement: same unchanged runbook, real
// project binding normally, mock binding under --package-map).
func TestRun_PackageMap_RealVsMockBinding(t *testing.T) {
	repoRoot := findRepoRoot(t)
	echoBin, err := testutil.BuildGoBinary(repoRoot, filepath.Join(t.TempDir(), "pkgmap-echo"), "./cmd/tools/echo")
	if err != nil {
		t.Fatalf("build echo tool: %v", err)
	}

	// Package roots live OUTSIDE the workspace root used for the run (see
	// below) so the generic <workspace>/** tool-file directory scan
	// (adapter.BuildEngineConfig's ToolScanDir walk) never sees them --
	// only the requires:/toolRefs: catalog resolution does. This isolates
	// the assertion to the Tool Packages MVP binding path itself.
	pkgsRoot := t.TempDir()
	realPkgDir := filepath.Join(pkgsRoot, "real-pkg")
	mockPkgDir := filepath.Join(pkgsRoot, "mock-pkg")
	writeAcmeIncidentToolsPackage(t, realPkgDir, echoBin, "real-binding")
	writeAcmeIncidentToolsPackage(t, mockPkgDir, echoBin, "mock-binding")

	dir := makeWorkDir(t)
	absDir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}

	// requires[].path is workspace-level.
	// runtime.tex §Secure Path Resolution): relative "../" segments
	// resolving outside the workspace are permitted (only reported, never
	// rejected — PKG-W003), so a relative path up to the external pkgsRoot
	// works without needing an authored absolute path.
	relReal := relForwardSlash(t, absDir, realPkgDir)
	relMock := relForwardSlash(t, absDir, mockPkgDir)

	writeFile(t, filepath.Join(dir, ".yawr", "config.yaml"), "apiVersion: yawr.config/v1\n"+
		"requires:\n"+
		"  - package: acme.incident-tools\n"+
		"    version: \"^1.0.0\"\n"+
		"    path: "+relReal+"\n")

	packageMapPath := filepath.Join(dir, "package-map.yaml")
	writeFile(t, packageMapPath, "apiVersion: yawr.config/v1\n"+
		"requires:\n"+
		"  - package: acme.incident-tools\n"+
		"    version: \"^1.0.0\"\n"+
		"    path: "+relMock+"\n")

	runbookPath := filepath.Join(dir, "runbook.yaml")
	writeFile(t, runbookPath, `$schema: "https://schemas.yawr.dev/yawr.runbook/v1.json"
apiVersion: yawr.runbook/v1
id: r23-pkg-cli
name: r23 package CLI binding test
toolRefs:
  - name: kubectl
    package: acme.incident-tools
flow:
  - step:
      id: identify
      type: tool
      tool:
        name: kubectl
        action: identify
      capture:
        result: stdout
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

	// Real project binding: no --package-map.
	realOut := runCaptureStdout(t, []string{"runbook.yaml", "--trace", "real-trace.jsonl", "--output", "json"})
	if got := capturedResult(t, realOut, "identify"); got != "real-binding" {
		t.Fatalf("real project binding: expected stdout %q, got %q (raw: %s)", "real-binding", got, realOut)
	}

	// The exact same, unchanged runbook + --package-map: mock binding.
	mockOut := runCaptureStdout(t, []string{"runbook.yaml", "--trace", "mock-trace.jsonl", "--output", "json", "--package-map", "package-map.yaml"})
	if got := capturedResult(t, mockOut, "identify"); got != "mock-binding" {
		t.Fatalf("--package-map binding: expected stdout %q, got %q (raw: %s)", "mock-binding", got, mockOut)
	}
}

// TestRun_PackageMap_MissingPackagePath asserts a stable PKG-* code
// (not a generic error) when a package-map/config-shaped file references a
// package path that doesn't exist.
func TestRun_PackageMap_MissingPackagePath(t *testing.T) {
	dir := makeWorkDir(t)
	writeFile(t, filepath.Join(dir, ".yawr", "config.yaml"), `apiVersion: yawr.config/v1
requires:
  - package: acme.incident-tools
    version: "^1.0.0"
    path: ./vendor/does-not-exist
`)
	runbookPath := filepath.Join(dir, "runbook.yaml")
	writeFile(t, runbookPath, `$schema: "https://schemas.yawr.dev/yawr.runbook/v1.json"
apiVersion: yawr.runbook/v1
id: r-pkg-missing
name: missing package path
toolRefs:
  - name: kubectl
    package: acme.incident-tools
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
		t.Fatalf("expected exit code %d, got %d (stderr: %s)", exitValidation, runLast, stderr)
	}
	if !bytes.Contains([]byte(stderr), []byte("PKG-001")) {
		t.Fatalf("expected PKG-001 in stderr, got: %s", stderr)
	}
}

// --- helpers ---

var runLast int

func writeAcmeIncidentToolsPackage(t *testing.T, root, echoBin, marker string) {
	t.Helper()
	writeFile(t, filepath.Join(root, "yawr-package.yaml"), `apiVersion: yawr.tool-package/v1
meta:
  name: acme.incident-tools
  version: "1.0.0"
exports:
  tools:
    - id: kubectl
      path: tools/kubectl.tool.yaml
`)
	command := filepath.ToSlash(echoBin)
	writeFile(t, filepath.Join(root, "tools", "kubectl.tool.yaml"), `apiVersion: yawr.tool/v1
meta:
  name: kubectl
  version: "1.0.0"
transport:
  mode: native
  command: `+yamlQuote(command)+`
actions:
  - name: identify
    description: Identify which package root this binding resolved to
    argv: ["--message", "`+marker+`"]
    returns: text
`)
}

func yamlQuote(s string) string {
	return "\"" + s + "\""
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

func relForwardSlash(t *testing.T, base, target string) string {
	t.Helper()
	rel, err := filepath.Rel(base, target)
	if err != nil {
		t.Fatalf("Rel(%s, %s): %v", base, target, err)
	}
	return filepath.ToSlash(rel)
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find repo root (go.mod) above %s", dir)
		}
		dir = parent
	}
}

// runCaptureStdout runs runRun with args and returns everything written to
// os.Stdout during the call.
func runCaptureStdout(t *testing.T, args []string) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	os.Stdout = w
	code := runRun(args)
	_ = w.Close()
	os.Stdout = old
	runLast = code

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	if code != exitSuccess {
		t.Fatalf("runRun(%v) exit code = %d, want %d; stdout: %s", args, code, exitSuccess, buf.String())
	}
	return buf.String()
}

// captureStderr runs fn with os.Stderr redirected and returns everything
// written to it, recording fn's return code into runLast.
func captureStderr(t *testing.T, fn func() int) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	os.Stderr = w
	code := fn()
	_ = w.Close()
	os.Stderr = old
	runLast = code

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	return buf.String()
}

type jsonSummary struct {
	Status string `json:"status"`
	Steps  []struct {
		StepID string         `json:"step_id"`
		Output map[string]any `json:"output,omitempty"`
	} `json:"steps"`
}

func capturedResult(t *testing.T, jsonOut, stepID string) string {
	t.Helper()
	var summary jsonSummary
	if err := json.Unmarshal([]byte(jsonOut), &summary); err != nil {
		t.Fatalf("unmarshal JSON summary: %v (raw: %s)", err, jsonOut)
	}
	for _, s := range summary.Steps {
		if s.StepID == stepID {
			if v, ok := s.Output["stdout"].(string); ok {
				return v
			}
			return ""
		}
	}
	t.Fatalf("step %q not found in summary: %+v", stepID, summary)
	return ""
}
