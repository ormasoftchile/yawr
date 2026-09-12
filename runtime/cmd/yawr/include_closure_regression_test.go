package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestRun_IncludeClosure_ChildRequires_FailClosed_PKG017 is a §5
// regression test: an included
// runbook that declares its own requires: is not lexically merged into
// the frozen global package set by this runtime revision. Rather than
// silently resolving it against the root's dynamically-scoped registry
// This covers the fail-closed include resolution mode.
// §Includes, Lexical Scoping, and Global Package Set rejects), the run
// MUST fail closed with a typed, diagnosable PKG-017.
func TestRun_IncludeClosure_ChildRequires_FailClosed_PKG017(t *testing.T) {
	dir := makeWorkDir(t)
	writeFile(t, filepath.Join(dir, "child.runbook.yaml"), `$schema: "https://schemas.yawr.dev/yawr.runbook/v1.json"
apiVersion: yawr.runbook/v1
id: r-include-child-requires
name: child declares its own requires
requires:
  - package: acme.child-tools
    version: "^1.0.0"
    path: ./does-not-matter
flow:
  - step:
      id: end
      type: end
      outcome: {category: success, code: ok}
`)
	writeFile(t, filepath.Join(dir, "runbook.yaml"), `$schema: "https://schemas.yawr.dev/yawr.runbook/v1.json"
apiVersion: yawr.runbook/v1
id: r-include-parent
name: parent includes a child with its own requires
flow:
  - step:
      id: go
      type: include
      include:
        runbook: ./child.runbook.yaml
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
		t.Fatalf("expected non-zero exit for an included runbook declaring requires:, got 0; output: %s", out)
	}
	if !strings.Contains(out, "PKG-017") {
		t.Fatalf("expected PKG-017 in output, got: %s", out)
	}
}

// TestRun_IncludeClosure_ChildToolRefs_FailClosed_PKG017 is a §5
// regression test: an included runbook that declares its own toolRefs: is
// the "dynamic scoping" case the ratified spec explicitly rejects (a
// child runbook's tool-name meaning must not depend on who included it);
// this runtime revision fails closed with PKG-017 rather than silently
// binding the child's toolRefs into the shared, process-global registry.
func TestRun_IncludeClosure_ChildToolRefs_FailClosed_PKG017(t *testing.T) {
	dir := makeWorkDir(t)
	writeFile(t, filepath.Join(dir, "child.runbook.yaml"), `$schema: "https://schemas.yawr.dev/yawr.runbook/v1.json"
apiVersion: yawr.runbook/v1
id: r-include-child-toolrefs
name: child declares its own toolRefs
toolRefs:
  - name: sometool
    package: acme.some-tools
flow:
  - step:
      id: end
      type: end
      outcome: {category: success, code: ok}
`)
	writeFile(t, filepath.Join(dir, "runbook.yaml"), `$schema: "https://schemas.yawr.dev/yawr.runbook/v1.json"
apiVersion: yawr.runbook/v1
id: r-include-parent2
name: parent includes a child with its own toolRefs
flow:
  - step:
      id: go
      type: include
      include:
        runbook: ./child.runbook.yaml
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
		t.Fatalf("expected non-zero exit for an included runbook declaring toolRefs:, got 0; output: %s", out)
	}
	if !strings.Contains(out, "PKG-017") {
		t.Fatalf("expected PKG-017 in output, got: %s", out)
	}
}
