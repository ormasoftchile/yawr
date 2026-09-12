package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRun_Substitution_Enum009_OutputViolation covers ENUM-009 (S4,
// substitution production path) end to end through the real `yawr run`
// CLI: a substituted action's outputs.<name> is enum-constrained, but the
// substitute runbook's own outputs.<name>.value materializes a value that
// is not a declared member. The step must fail with an ENUM-009 error
// surfaced through the existing --output=json error envelope, and the
// enum member list must never be echoed as the produced (rejected) value.
func TestRun_Substitution_Enum009_OutputViolation(t *testing.T) {
	pkgsRoot := t.TempDir()
	pkgDir := filepath.Join(pkgsRoot, "diag-pkg")

	writeFile(t, filepath.Join(pkgDir, "yawr-package.yaml"), `apiVersion: yawr.tool-package/v1
meta:
  name: acme.diag-tools
  version: "1.0.0"
exports:
  tools:
    - id: diagnostics
      path: tools/diagnostics.tool.yaml
`)

	writeFile(t, filepath.Join(pkgDir, "tools", "diagnostics.tool.yaml"), `apiVersion: yawr.tool/v1
meta:
  name: diagnostics
  version: "1.0.0"
transport:
  mode: native
  command: does-not-run
actions:
  - name: diagnose
    description: Diagnose a named resource via a substituted runbook
    args:
      name:
        type: string
        required: true
    outputs:
      severity:
        type: string
        enum: ["low", "high"]
    execute:
      kind: runbook
      path: ../runbooks/diagnose.runbook.yaml
`)

	writeFile(t, filepath.Join(pkgDir, "runbooks", "diagnose.runbook.yaml"), `$schema: "https://schemas.yawr.dev/yawr.runbook/v1.json"
apiVersion: yawr.runbook/v1
id: acme.diag-tools/diagnose
name: diagnose substitute
inputs:
  name:
    type: string
    required: true
    from: context
outputs:
  severity:
    type: string
    enum: ["low", "high"]
    value: "unknown"
flow:
  - step:
      id: mark
      type: noop
  - step:
      id: end
      type: end
      outcome: {category: success, code: ok}
`)

	dir := makeWorkDir(t)
	absDir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}
	relPkg := relForwardSlash(t, absDir, pkgDir)

	writeFile(t, filepath.Join(dir, ".yawr", "config.yaml"), "apiVersion: yawr.config/v1\n"+
		"requires:\n"+
		"  - package: acme.diag-tools\n"+
		"    version: \"^1.0.0\"\n"+
		"    path: "+relPkg+"\n")

	writeFile(t, filepath.Join(dir, "runbook.yaml"), `$schema: "https://schemas.yawr.dev/yawr.runbook/v1.json"
apiVersion: yawr.runbook/v1
id: r-substitution-enum-violation
name: substitution enum violation CLI test
toolRefs:
  - name: diagnostics
    package: acme.diag-tools
flow:
  - step:
      id: diagnose
      type: tool
      tool:
        name: diagnostics
        action: diagnose
        args:
          name: node-7
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

	out := runCaptureStdoutAnyExit(t, []string{"runbook.yaml", "--output", "json"})

	var summary jsonSummary
	if err := json.Unmarshal([]byte(out), &summary); err != nil {
		t.Fatalf("unmarshal JSON summary: %v (raw: %s)", err, out)
	}
	var found bool
	for _, s := range summary.Steps {
		if s.StepID != "diagnose" {
			continue
		}
		found = true
		if !strings.Contains(out, "ENUM-009") {
			t.Fatalf("expected ENUM-009 to surface via the JSON error envelope; raw: %s", out)
		}
		if strings.Contains(out, "\"low\"") || strings.Contains(out, "\"high\"") {
			t.Fatalf("enum member list must not be echoed in the error output; raw: %s", out)
		}
	}
	if !found {
		t.Fatalf("step %q not found in summary: %+v", "diagnose", summary)
	}
}

// runCaptureStdoutAnyExit is like runCaptureStdout but does not require a
// zero exit code -- used by tests that intentionally exercise a step
// failure (e.g. ENUM-009) and only care about the JSON summary content.
func runCaptureStdoutAnyExit(t *testing.T, args []string) string {
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

	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, rerr := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if rerr != nil {
			break
		}
	}
	return string(buf)
}
