package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	internaltrace "github.com/ormasoftchile/yawr/runtime/internal/trace"
	tracepkg "github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

// TestRun_DynamicInclude_SucceedsEndToEnd is a CLI-level regression test for
// DEF-001: a runbook containing a dynamic include (runbook_ref + resolve_from:
// catalog) must NOT crash the preflight lexical-scoping or substitution-plan
// walks. Before the fix, closureLexicalVisitor.BeforeInclude and
// substitutionPlanVisitor.BeforeInclude both returned load=true for every
// include, causing flowwalk.Walker to call cliLoader.Load("") → the working
// directory → parse failure with exit 2 before a single step ran.
func TestRun_DynamicInclude_SucceedsEndToEnd(t *testing.T) {
	pkgDir := t.TempDir()

	writeFile(t, filepath.Join(pkgDir, "yawr-package.yaml"), `apiVersion: yawr.tool-package/v1
meta:
  name: acme-dyntest
  version: "1.0.0"
exports:
  runbooks:
    - id: tsg-simple
      path: runbooks/tsg-simple.runbook.yaml
`)
	writeFile(t, filepath.Join(pkgDir, "runbooks", "tsg-simple.runbook.yaml"), `apiVersion: yawr.runbook/v1
id: tsg-simple
name: TSG Simple
flow:
  - step:
      id: done
      type: end
      outcome: {category: success, code: ok}
`)

	dir := makeWorkDir(t)
	absDir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}
	relPkg := relForwardSlash(t, absDir, pkgDir)

	writeFile(t, filepath.Join(dir, ".yawr", "config.yaml"),
		"apiVersion: yawr.config/v1\nrequires:\n"+
			"  - package: acme-dyntest\n"+
			"    version: \"^1.0.0\"\n"+
			"    path: "+relPkg+"\n")

	writeFile(t, filepath.Join(dir, "runbook.yaml"), `$schema: "https://schemas.yawr.dev/yawr.runbook/v1.json"
apiVersion: yawr.runbook/v1
id: r-dyninclude-cli
name: dynamic include CLI test
flow:
  - step:
      id: dyn
      type: include
      include:
        runbook_ref: "acme-dyntest/tsg-simple"
        resolve_from: catalog
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

	code := runRun([]string{"runbook.yaml", "--output", "quiet", "--trace", filepath.Join(dir, "trace.jsonl")})
	if code != exitSuccess {
		t.Fatalf("expected exitSuccess (%d) for dynamic include, got %d", exitSuccess, code)
	}
}

func TestRun_DynamicInclude_TraceRedactsChildSecret(t *testing.T) {
	const secret = "SENTINEL-dynamic-trace-secret-4f8c"
	dir := makeWorkDir(t)
	runbookPath := writeDynamicSecretIncludeFixture(t, dir, secret)
	tracePath := filepath.Join(filepath.Dir(runbookPath), "trace.jsonl")
	t.Chdir(dir)

	code := runRun([]string{
		runbookPath, "--output", "quiet", "--var", "child_ref=acme-dynsecret/child-secret", "--trace", tracePath,
	})
	if code != exitSuccess {
		t.Fatalf("dynamic include exit code = %d, want %d", code, exitSuccess)
	}
	traceBytes, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	if strings.Contains(string(traceBytes), secret) {
		t.Fatalf("dynamic child secret %q leaked into trace", secret)
	}
	if !strings.Contains(string(traceBytes), `"step_id":"copy-secret"`) || !strings.Contains(string(traceBytes), `"kind":"step/completed"`) {
		t.Fatal("trace redaction removed step_id or event kind")
	}
}

// TestRun_DynamicInclude_OnNotFound_Continue verifies that on_not_found:
// continue through the real CLI wiring produces exitSuccess but renders a
// visible skipped include, not the same checkmark used for executed steps.
func TestRun_DynamicInclude_OnNotFound_Continue(t *testing.T) {
	dir := makeWorkDir(t)

	writeFile(t, filepath.Join(dir, ".yawr", "config.yaml"), "apiVersion: yawr.config/v1\n")
	writeFile(t, filepath.Join(dir, "runbook.yaml"), `$schema: "https://schemas.yawr.dev/yawr.runbook/v1.json"
apiVersion: yawr.runbook/v1
id: r-dyninclude-notfound-continue
name: dynamic include not-found continue test
flow:
  - step:
      id: dyn
      type: include
      include:
        runbook_ref: "no-such-pkg/no-such-id"
        resolve_from: catalog
        on_not_found: continue
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

	out, code := captureStdoutForPreview(t, func() int {
		return runRun([]string{"runbook.yaml", "--output", "text", "--trace", filepath.Join(dir, "trace.jsonl")})
	})
	if code != exitSuccess {
		t.Fatalf("expected exitSuccess for on_not_found: continue, got %d; stdout:\n%s", code, out)
	}
	if !strings.Contains(out, "⊘ skipped dyn (include)") {
		t.Fatalf("expected skipped include marker in text output, got:\n%s", out)
	}
	if strings.Contains(out, "✓ dyn (include)") {
		t.Fatalf("skipped include must not render as executed checkmark, got:\n%s", out)
	}
	if !strings.Contains(out, "on_not_found: continue skipped the child runbook") {
		t.Fatalf("expected skip warning in text output, got:\n%s", out)
	}
	events, err := internaltrace.NewJSONLReader(filepath.Join(dir, "trace.jsonl")).ReadAll(context.Background())
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	notFoundIndex, skippedIndex := -1, -1
	for index, event := range events {
		if event.Kind != tracepkg.EventKindIncludeNotFound && event.Kind != tracepkg.EventKindStepSkipped {
			continue
		}
		var payload struct {
			StepID          string `json:"step_id"`
			QualifiedNodeID string `json:"qualified_node_id"`
			Invocation      int    `json:"invocation"`
			Reason          string `json:"reason"`
			Continued       bool   `json:"continued"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("decode %s: %v", event.Kind, err)
		}
		if payload.StepID != "dyn" {
			continue
		}
		if payload.QualifiedNodeID != "dyn" || payload.Invocation != 1 {
			t.Fatalf("%s occurrence = %#v", event.Kind, payload)
		}
		if event.Kind == tracepkg.EventKindIncludeNotFound {
			if !payload.Continued {
				t.Fatalf("not-found event was not continued: %#v", payload)
			}
			notFoundIndex = index
		} else if payload.Reason == "include_not_found" {
			skippedIndex = index
		}
	}
	if notFoundIndex < 0 || skippedIndex <= notFoundIndex {
		t.Fatalf("not-found/committed-skip order = %d/%d", notFoundIndex, skippedIndex)
	}
}

// TestRun_StaticInclude_EmptyRunbook_StillFails verifies that the DEF-001 fix
// does not regress into silently allowing a genuinely malformed static include
// (include: {runbook: ""}) to pass. An empty static runbook path must still
// produce a non-success exit code.
func TestRun_StaticInclude_EmptyRunbook_StillFails(t *testing.T) {
	dir := makeWorkDir(t)

	writeFile(t, filepath.Join(dir, ".yawr", "config.yaml"), "apiVersion: yawr.config/v1\n")
	// runbook_ref is absent; this is a static include with an empty runbook path.
	writeFile(t, filepath.Join(dir, "runbook.yaml"), `$schema: "https://schemas.yawr.dev/yawr.runbook/v1.json"
apiVersion: yawr.runbook/v1
id: r-bad-static-include
name: bad static include test
flow:
  - step:
      id: inc
      type: include
      include:
        runbook: ""
`)

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	code := runRun([]string{"runbook.yaml", "--output", "quiet", "--trace", filepath.Join(dir, "trace.jsonl")})
	if code == exitSuccess {
		t.Fatalf("expected non-success for static include with empty runbook path, got %d", code)
	}
}

func writeDynamicSecretIncludeFixture(t *testing.T, dir, secret string) string {
	t.Helper()
	pkgDir := t.TempDir()
	writeFile(t, filepath.Join(pkgDir, "yawr-package.yaml"), strings.ReplaceAll(`apiVersion: yawr.tool-package/v1
meta:
	name: acme-dynsecret
	version: "1.0.0"
exports:
	runbooks:
		- id: child-secret
			path: runbooks/child-secret.runbook.yaml
`, "\t", "  "))
	writeFile(t, filepath.Join(pkgDir, "runbooks", "child-secret.runbook.yaml"), strings.ReplaceAll(`apiVersion: yawr.runbook/v1
id: child-secret
name: Child secret
inputs:
	api_secret:
		type: secret
		required: true
flow:
	- step:
			id: copy-secret
			type: noop
			capture:
				child_copy: "child-${api_secret}"
`, "\t", "  "))
	absDir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}
	relPkg := relForwardSlash(t, absDir, pkgDir)
	writeFile(t, filepath.Join(dir, ".yawr", "config.yaml"),
		"apiVersion: yawr.config/v1\nrequires:\n"+
			"  - package: acme-dynsecret\n"+
			"    version: \"^1.0.0\"\n"+
			"    path: "+relPkg+"\n")
	runbookPath := filepath.Join(absDir, "runbook.yaml")
	writeFile(t, runbookPath, strings.ReplaceAll(`apiVersion: yawr.runbook/v1
id: dynamic-secret-root
name: Dynamic secret root
inputs:
	child_ref:
		type: string
		required: true
flow:
	- step:
			id: dynamic-child
			type: include
			include:
				runbook_ref: "${child_ref}"
				resolve_from: catalog
				with:
					api_secret: "`+secret+`"
`, "\t", "  "))
	return runbookPath
}
