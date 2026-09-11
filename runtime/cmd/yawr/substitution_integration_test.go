package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestRun_Substitution_LiveSubstitutedAction is a live CLI integration test
// for action substitution (design/yawr/sections/06-tool-runtime.tex
// §Action Substitution): a package-exported tool declares an action whose
// execute.kind is "runbook"; the substitute runbook lives inside the
// package, is planned via pkg/pkgsubst.Plan (input contract, governance
// composition), executed in-process via the tool executor's substitution
// path (internal/executor/tool.go executeSubstitution), and its declared
// outputs: are propagated back onto the calling step's Output. This
// exercises the actual `yawr run` runtime path end to end, not just
// pkgsubst's own unit tests.
func TestRun_Substitution_LiveSubstitutedAction(t *testing.T) {
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

	// The declaring tool file's execute.path resolves relative to its own
	// directory (tools/), never the workspace or the calling runbook
	// (design/yawr/sections/06-tool-runtime.tex Table tab:tool-path-bases).
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
      summary:
        type: string
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
  summary:
    type: string
    value: "diagnosed-${name}"
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
id: r-substitution-live
name: substitution live CLI test
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

	out := runCaptureStdout(t, []string{"runbook.yaml", "--trace", "trace.jsonl", "--output", "json"})

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
		got, _ := s.Output["summary"].(string)
		if got != "diagnosed-node-7" {
			t.Fatalf("step %q output[summary] = %q, want %q (raw: %s)", "diagnose", got, "diagnosed-node-7", out)
		}
	}
	if !found {
		t.Fatalf("step %q not found in summary: %+v", "diagnose", summary)
	}

	// The tool/substituted trace event must be emitted at the real
	// runtime boundary (internal/executor/tool.go executeSubstitution),
	// not merely computed by pkgsubst and discarded.
	traceBytes, err := os.ReadFile("trace.jsonl")
	if err != nil {
		t.Fatalf("read trace.jsonl: %v", err)
	}
	if !jsonlContainsKind(t, traceBytes, "tool/substituted") {
		t.Fatalf("trace.jsonl does not contain a tool/substituted event; raw: %s", traceBytes)
	}
}

func jsonlContainsKind(t *testing.T, data []byte, kind string) bool {
	t.Helper()
	for _, line := range splitLines(data) {
		if len(line) == 0 {
			continue
		}
		var evt struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(line, &evt); err != nil {
			continue
		}
		if evt.Kind == kind {
			return true
		}
	}
	return false
}

func splitLines(data []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, b := range data {
		if b == '\n' {
			out = append(out, data[start:i])
			start = i + 1
		}
	}
	if start < len(data) {
		out = append(out, data[start:])
	}
	return out
}
