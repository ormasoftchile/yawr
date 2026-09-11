package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// TestRun_Substitution_OutputsCaptureRoot is a B5 regression test
// The `outputs.<name>` GCP capture root
// (gcp.ebnf §3.4a) resolves against the current step's own declared
// substitution outputs and can be captured into a variable via the step's
// own capture: block, distinct from the pre-existing runtime-materialized
// step.Output convenience the substitution executor already populates.
func TestRun_Substitution_OutputsCaptureRoot(t *testing.T) {
	pkgsRoot := t.TempDir()
	pkgDir := filepath.Join(pkgsRoot, "cap-pkg")

	writeFile(t, filepath.Join(pkgDir, "yawr-package.yaml"), `apiVersion: yawr.tool-package/v1
meta:
  name: acme.cap-tools
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
      summary:
        type: string
    execute:
      kind: runbook
      path: ../runbooks/diagnose.runbook.yaml
`)

	writeFile(t, filepath.Join(pkgDir, "runbooks", "diagnose.runbook.yaml"), `$schema: "https://schemas.yawr.dev/yawr.runbook/v1.json"
apiVersion: yawr.runbook/v1
id: acme.cap-tools/diagnose
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
		"  - package: acme.cap-tools\n"+
		"    version: \"^1.0.0\"\n"+
		"    path: "+relPkg+"\n")

	writeFile(t, filepath.Join(dir, "runbook.yaml"), `$schema: "https://schemas.yawr.dev/yawr.runbook/v1.json"
apiVersion: yawr.runbook/v1
id: r-outputs-capture
name: outputs.<name> capture root test
toolRefs:
  - name: diagnostics
    package: acme.cap-tools
flow:
  - step:
      id: diagnose
      type: tool
      tool:
        name: diagnostics
        action: diagnose
        args:
          name: node-7
      capture:
        captured_summary: outputs.summary
  - step:
      id: end
      type: end
      outcome: {category: success, code: ok}
`)

	chdirForTest(t, dir)

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
			t.Fatalf("step diagnose output[summary] = %q, want %q (raw: %s)", got, "diagnosed-node-7", out)
		}
	}
	if !found {
		t.Fatalf("step %q not found in summary: %+v", "diagnose", summary)
	}
}

// TestRun_Substitution_OutputsCaptureRoot_WrongStepKind_PlanTimeFailure is a
// B5 regression test: outputs.<name> is only valid in the capture: block of
// a tool step whose bound action is execute.kind: runbook (gcp.ebnf §3.4a);
// used against a plain (non-substituted) tool action, it MUST be a plan-
// time GCP-PARSE-001 failure, not a silent no-op or a runtime panic.
func TestRun_Substitution_OutputsCaptureRoot_WrongStepKind_PlanTimeFailure(t *testing.T) {
	dir := makeWorkDir(t)
	writeFile(t, filepath.Join(dir, "runbook.yaml"), `$schema: "https://schemas.yawr.dev/yawr.runbook/v1.json"
apiVersion: yawr.runbook/v1
id: r-outputs-capture-wrong-kind
name: outputs.<name> against a non-substituted step
flow:
  - step:
      id: hello
      type: cli
      command: echo hi
      capture:
        bogus: outputs.summary
  - step:
      id: end
      type: end
      outcome: {category: success, code: ok}
`)

	chdirForTest(t, dir)

	out := captureStderr(t, func() int {
		return runRun([]string{"runbook.yaml", "--output", "quiet"})
	})
	if runLast == exitSuccess {
		t.Fatalf("expected non-zero exit for outputs.<name> used against a non-substitution step, got 0; output: %s", out)
	}
	if !strings.Contains(out, "GCP-PARSE-001") {
		t.Fatalf("expected GCP-PARSE-001 in output, got: %s", out)
	}
}
