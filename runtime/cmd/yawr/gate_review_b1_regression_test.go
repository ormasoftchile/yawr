package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// chdirForTest chdirs into dir for the duration of the test, restoring the
// previous working directory in t.Cleanup.
func chdirForTest(t *testing.T, dir string) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	return cwd
}

// TestRun_Substitution_PlanTimeContractViolation_DryRun is a B1 regression
// test (Barbara's gate review): a substitution whose input contract is
// broken (the substitute runbook's inputs: don't match the action's args:
// contract, PKG-013) must fail before the run ever starts -- including via
// the `yawr dry-run` subcommand, which never itself reaches
// internal/executor/tool.go's runtime pkgsubst.Plan call.
func TestRun_Substitution_PlanTimeContractViolation_DryRun(t *testing.T) {
	pkgsRoot := t.TempDir()
	pkgDir := filepath.Join(pkgsRoot, "bad-pkg")

	writeFile(t, filepath.Join(pkgDir, "yawr-package.yaml"), `apiVersion: yawr.tool-package/v1
meta:
  name: acme.bad-tools
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

	// The substitute declares no "name" input at all -- a direct PKG-013
	// input-signature violation against the action's args: contract.
	writeFile(t, filepath.Join(pkgDir, "runbooks", "diagnose.runbook.yaml"), `$schema: "https://schemas.yawr.dev/yawr.runbook/v1.json"
apiVersion: yawr.runbook/v1
id: acme.bad-tools/diagnose
name: diagnose substitute (broken contract)
outputs:
  summary:
    type: string
    value: "diagnosed"
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
		"  - package: acme.bad-tools\n"+
		"    version: \"^1.0.0\"\n"+
		"    path: "+relPkg+"\n")

	writeFile(t, filepath.Join(dir, "runbook.yaml"), `$schema: "https://schemas.yawr.dev/yawr.runbook/v1.json"
apiVersion: yawr.runbook/v1
id: r-substitution-badcontract
name: substitution bad contract dry-run test
toolRefs:
  - name: diagnostics
    package: acme.bad-tools
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

	out := captureStderr(t, func() int {
		return runDryRun([]string{"runbook.yaml", "--output", "quiet"})
	})
	if runLast == exitSuccess {
		t.Fatalf("expected non-zero exit for a plan-time substitution contract violation in dry-run mode, got 0; output: %s", out)
	}
	if !strings.Contains(out, "PKG-013") {
		t.Fatalf("expected PKG-013 in dry-run output, got: %s", out)
	}
}

// TestRun_Substitution_PlanTimeContractViolation_UnreachableStep is a B1
// regression test: an action-substitution contract violation reached only
// through a branch arm whose when: condition is false at plan time (and
// therefore never executes) must still be caught statically -- "validity
// is not conditional on execution path" per the gate review.
func TestRun_Substitution_PlanTimeContractViolation_UnreachableStep(t *testing.T) {
	pkgsRoot := t.TempDir()
	pkgDir := filepath.Join(pkgsRoot, "bad-pkg2")

	writeFile(t, filepath.Join(pkgDir, "yawr-package.yaml"), `apiVersion: yawr.tool-package/v1
meta:
  name: acme.bad-tools2
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
id: acme.bad-tools2/diagnose
name: diagnose substitute (broken contract)
outputs:
  summary:
    type: string
    value: "diagnosed"
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
		"  - package: acme.bad-tools2\n"+
		"    version: \"^1.0.0\"\n"+
		"    path: "+relPkg+"\n")

	// The substituted tool step lives inside a branch arm that will never
	// be true ("false" is a constant GXL condition) -- unreachable at
	// runtime, but the plan-time walk must still visit it.
	writeFile(t, filepath.Join(dir, "runbook.yaml"), `$schema: "https://schemas.yawr.dev/yawr.runbook/v1.json"
apiVersion: yawr.runbook/v1
id: r-substitution-unreachable
name: substitution unreachable branch test
toolRefs:
  - name: diagnostics
    package: acme.bad-tools2
flow:
  - step:
      id: maybe
      type: branch
      branches:
        - condition: "false"
          label: never
          steps:
            - step:
                id: diagnose
                type: tool
                tool:
                  name: diagnostics
                  action: diagnose
                  args:
                    name: node-7
        - else: true
          label: default
          steps:
            - step:
                id: noop
                type: noop
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
		t.Fatalf("expected non-zero exit for an unreachable-step substitution contract violation, got 0; output: %s", out)
	}
	if !strings.Contains(out, "PKG-013") {
		t.Fatalf("expected PKG-013 in output for the unreachable substituted step, got: %s", out)
	}
}
