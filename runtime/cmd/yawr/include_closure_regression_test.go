package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestRun_IncludeClosure_InvalidChildRequiresFailsPreflight(t *testing.T) {
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
		return runRun([]string{"runbook.yaml", "--output", "quiet", "--trace", "trace.jsonl"})
	})
	if runLast != exitValidation {
		t.Fatalf("expected validation failure for missing child package, got %d; output: %s", runLast, out)
	}
	if !strings.Contains(out, "PKG-001") || !strings.Contains(out, "acme.child-tools") {
		t.Fatalf("expected missing child package diagnostic, got: %s", out)
	}
	assertNoStructuredDispatch(t)
}

func TestRun_IncludeClosure_InvalidChildToolRefsFailsPreflight(t *testing.T) {
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
		return runRun([]string{"runbook.yaml", "--output", "quiet", "--trace", "trace.jsonl"})
	})
	if runLast != exitValidation {
		t.Fatalf("expected validation failure for unbound child tool, got %d; output: %s", runLast, out)
	}
	if !strings.Contains(out, "PKG-011") || !strings.Contains(out, "sometool") {
		t.Fatalf("expected child-local tool binding diagnostic, got: %s", out)
	}
	assertNoStructuredDispatch(t)
}
