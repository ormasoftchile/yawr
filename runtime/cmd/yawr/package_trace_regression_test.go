package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestRun_PackageMap_TraceRecordsConstraintSourcesAndOrigin is a regression
// test for package binding and trace behavior:
//
//   - item 1: package/resolved's constraintSources field used to be
//     unconditionally emitted as []string{} even though the project's own
//     requires: entry supplied a real version: constraint.
//   - item 9: a --package-map override's provenance (which binding
//     scope -- "project" vs. "package-map" -- actually won for a given
//     package name) used to be stderr-only and never reached the trace,
//     even though §7.4 makes package provenance part of the evidence
//     record.
//
// Both must now show up in the same package/resolved trace event.
func TestRun_PackageMap_TraceRecordsConstraintSourcesAndOrigin(t *testing.T) {
	pkgsRoot := t.TempDir()
	pkgDir := filepath.Join(pkgsRoot, "cs-pkg")

	writeFile(t, filepath.Join(pkgDir, "yawr-package.yaml"), `apiVersion: yawr.tool-package/v1
meta:
  name: acme.cs-tools
  version: "1.2.0"
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
		"  - package: acme.cs-tools\n"+
		"    version: \"^1.0.0\"\n"+
		"    path: "+relPkg+"\n")

	packageMapPath := filepath.Join(dir, "package-map.yaml")
	writeFile(t, packageMapPath, "apiVersion: yawr.config/v1\n"+
		"requires:\n"+
		"  - package: acme.cs-tools\n"+
		"    version: \"^1.0.0\"\n"+
		"    path: "+relPkg+"\n")

	writeFile(t, filepath.Join(dir, "runbook.yaml"), `$schema: "https://schemas.yawr.dev/yawr.runbook/v1.json"
apiVersion: yawr.runbook/v1
id: r-cs-origin-trace
name: constraint sources and origin trace test
toolRefs:
  - name: pingtool
    package: acme.cs-tools
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

	_ = runCaptureStdout(t, []string{"runbook.yaml", "--trace", "trace.jsonl", "--output", "json", "--package-map", "package-map.yaml"})

	traceBytes, err := os.ReadFile("trace.jsonl")
	if err != nil {
		t.Fatalf("read trace.jsonl: %v", err)
	}
	lines := splitLines(traceBytes)

	var pkgPayload struct {
		Name              string   `json:"name"`
		ConstraintSources []string `json:"constraintSources"`
		Origin            string   `json:"origin"`
	}
	var found bool
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		var evt struct {
			Kind    string          `json:"kind"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(line, &evt); err != nil {
			continue
		}
		if evt.Kind != "package/resolved" {
			continue
		}
		if err := json.Unmarshal(evt.Payload, &pkgPayload); err != nil {
			t.Fatalf("unmarshal package/resolved payload: %v", err)
		}
		found = true
	}
	if !found {
		t.Fatalf("trace.jsonl does not contain a package/resolved event; raw: %s", traceBytes)
	}
	if pkgPayload.Name != "acme.cs-tools" {
		t.Fatalf("package/resolved name = %q, want %q", pkgPayload.Name, "acme.cs-tools")
	}
	if len(pkgPayload.ConstraintSources) == 0 {
		t.Fatalf("package/resolved constraintSources is empty; want at least one declaration site")
	}
	if pkgPayload.Origin != "package-map" {
		t.Fatalf("package/resolved origin = %q, want %q", pkgPayload.Origin, "package-map")
	}
}
