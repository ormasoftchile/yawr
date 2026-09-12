package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestRun_Catalog_EmitsPackageAndCatalogTraceEvents is a live CLI
// integration test for item 2's remaining event kinds: package/resolved
// (one per tier-1 package resolved via requires:) and catalog/frozen
// (once, after Phase C completes), both emitted from
// internal/adapter.EmitPackageCatalogTraceEvents at the same runtime
// boundary (cmd/yawr/run.go, right after adapter.BuildPackageCatalog
// succeeds and before the plan/run even starts) using the run's own
// run_id, so the events genuinely correlate with the rest of the run's
// trace.jsonl rather than being computed and discarded.
func TestRun_Catalog_EmitsPackageAndCatalogTraceEvents(t *testing.T) {
	pkgsRoot := t.TempDir()
	pkgDir := filepath.Join(pkgsRoot, "trace-pkg")

	writeFile(t, filepath.Join(pkgDir, "yawr-package.yaml"), `apiVersion: yawr.tool-package/v1
meta:
  name: acme.trace-tools
  version: "1.0.0"
exports:
  tools:
    - id: pingtool
      path: tools/ping.tool.yaml
`)

	writeFile(t, filepath.Join(pkgDir, "tools", "ping.tool.yaml"), `apiVersion: yawr.tool/v1
meta:
  name: pingtool
  version: "1.0.0"
transport:
  mode: native
  command: does-not-run
actions:
  - name: ping
    description: no-op action used only to force catalog resolution
    args: {}
`)

	dir := makeWorkDir(t)
	absDir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}
	relPkg := relForwardSlash(t, absDir, pkgDir)

	writeFile(t, filepath.Join(dir, ".yawr", "config.yaml"), "apiVersion: yawr.config/v1\n"+
		"requires:\n"+
		"  - package: acme.trace-tools\n"+
		"    version: \"^1.0.0\"\n"+
		"    path: "+relPkg+"\n")

	writeFile(t, filepath.Join(dir, "runbook.yaml"), `$schema: "https://schemas.yawr.dev/yawr.runbook/v1.json"
apiVersion: yawr.runbook/v1
id: r-catalog-trace-live
name: catalog trace live CLI test
toolRefs:
  - name: pingtool
    package: acme.trace-tools
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

	_ = runCaptureStdout(t, []string{"runbook.yaml", "--trace", "trace.jsonl", "--output", "json"})

	traceBytes, err := os.ReadFile("trace.jsonl")
	if err != nil {
		t.Fatalf("read trace.jsonl: %v", err)
	}
	lines := splitLines(traceBytes)

	var resolvedRunID, frozenRunID string
	var sawPackageResolved, sawCatalogFrozen bool
	var pkgPayload struct {
		Name     string `json:"name"`
		Version  string `json:"version"`
		External bool   `json:"external"`
		Digest   string `json:"digest"`
	}
	var catalogPayload struct {
		CatalogDigest string         `json:"catalogDigest"`
		ToolCount     int            `json:"toolCount"`
		Tiers         map[string]int `json:"tiers"`
	}
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		var evt struct {
			Kind    string          `json:"kind"`
			RunID   string          `json:"run_id"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(line, &evt); err != nil {
			continue
		}
		switch evt.Kind {
		case "package/resolved":
			sawPackageResolved = true
			resolvedRunID = evt.RunID
			if err := json.Unmarshal(evt.Payload, &pkgPayload); err != nil {
				t.Fatalf("unmarshal package/resolved payload: %v", err)
			}
		case "catalog/frozen":
			sawCatalogFrozen = true
			frozenRunID = evt.RunID
			if err := json.Unmarshal(evt.Payload, &catalogPayload); err != nil {
				t.Fatalf("unmarshal catalog/frozen payload: %v", err)
			}
		}
	}

	if !sawPackageResolved {
		t.Fatalf("trace.jsonl does not contain a package/resolved event; raw: %s", traceBytes)
	}
	if pkgPayload.Name != "acme.trace-tools" {
		t.Fatalf("package/resolved name = %q, want %q", pkgPayload.Name, "acme.trace-tools")
	}
	// The package lives under a separate t.TempDir() tree from the
	// workspace, so it resolves outside the workspace root (
	// TV-PKG-PATH-002 ruling: PKG-W003/external, not a hard failure) --
	// confirming external is threaded through from pkgpath.ResolveKind
	// into the package/resolved trace event, not silently dropped.
	if !pkgPayload.External {
		t.Fatalf("package/resolved external = false, want true for a workspace-escaping package path")
	}
	if pkgPayload.Digest == "" {
		t.Fatalf("package/resolved digest is empty")
	}

	if !sawCatalogFrozen {
		t.Fatalf("trace.jsonl does not contain a catalog/frozen event; raw: %s", traceBytes)
	}
	if catalogPayload.CatalogDigest == "" {
		t.Fatalf("catalog/frozen catalog_digest is empty")
	}
	if catalogPayload.ToolCount < 1 {
		t.Fatalf("catalog/frozen tool_count = %d, want >= 1", catalogPayload.ToolCount)
	}

	if resolvedRunID == "" || resolvedRunID != frozenRunID {
		t.Fatalf("package/resolved run_id (%q) and catalog/frozen run_id (%q) must be non-empty and match", resolvedRunID, frozenRunID)
	}
}
