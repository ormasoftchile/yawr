package main

// serve_package_map_test.go — proves that yawr serve --package-map
// deterministically selects the intended tool definition and scan order
// does NOT decide.
//
// Design: two directories each contain a .tool.yaml with the same logical
// name but different descriptions. A base scan dir contains the "v1" tool
// (which a plain scan would find). The package-map's tool-paths: points to
// the "v2" directory. newServingToolRegistry must bind v2, regardless of
// which directory alphabetically sorts first.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestServe_PackageMap_DeterminesToolBinding proves that two contract-identical
// tool definitions (same name, different descriptions) are disambiguated by
// --package-map's tool-paths: and NOT by filesystem scan order.
//
// Control: without package-map extra paths, the base-scan tool is bound.
// Experiment: with extra path pointing to the v2 dir, v2 wins regardless of
//
//	alphabetical walk order (v1 dir name "aaa-v1" < v2 dir name "zzz-v2",
//	so without package-map the scan would visit v1 last and bind v1's def
//	only if alphabetical; we force v2 to win via explicit package-map path).
func TestServe_PackageMap_DeterminesToolBinding(t *testing.T) {
	root := t.TempDir()

	// "zzz-v2" sorts after "aaa-v1" alphabetically — so a naive scan of root
	// visits aaa-v1 first, then zzz-v2 last, and zzz-v2 wins (last write
	// wins in the registry). This means without a package-map, zzz-v2 binds
	// due to scan order. With the package-map pointing to aaa-v1, aaa-v1
	// must win despite having the alphabetically-earlier walk position.
	v1Dir := filepath.Join(root, "aaa-v1")
	v2Dir := filepath.Join(root, "zzz-v2")
	if err := os.MkdirAll(v1Dir, 0o755); err != nil {
		t.Fatalf("mkdir v1: %v", err)
	}
	if err := os.MkdirAll(v2Dir, 0o755); err != nil {
		t.Fatalf("mkdir v2: %v", err)
	}

	const toolName = "dual-def-tool"
	const toolYAML = `apiVersion: yawr.tool/v1
meta:
  name: ` + toolName + `
  version: "1.0.0"
transport:
  mode: vscode-mcp
actions:
  - name: call
    description: "%s"
    classification: read-only
`

	writeFile(t, filepath.Join(v1Dir, toolName+".tool.yaml"),
		"apiVersion: yawr.tool/v1\nmeta:\n  name: "+toolName+"\n  version: \"1.0.0\"\ntransport:\n  mode: vscode-mcp\nactions:\n  - name: call\n    description: \"v1-definition\"\n    classification: read-only\n")
	writeFile(t, filepath.Join(v2Dir, toolName+".tool.yaml"),
		"apiVersion: yawr.tool/v1\nmeta:\n  name: "+toolName+"\n  version: \"1.0.0\"\ntransport:\n  mode: vscode-mcp\nactions:\n  - name: call\n    description: \"v2-definition\"\n    classification: read-only\n")

	_ = toolYAML // keep import clean

	// Control: no extra paths — scan root; zzz-v2 sorts last so v2 binds.
	regControl, err := newServingToolRegistry(root)
	if err != nil {
		t.Fatalf("newServingToolRegistry (control): %v", err)
	}
	key := toolName + "/call"
	controlDef, ok := regControl.tools[key]
	if !ok {
		t.Fatalf("control: tool %q not found in registry", key)
	}
	controlDesc := controlDef.Actions["call"].Description
	// Regardless of which one the scan happened to bind, record it.
	t.Logf("control (no package-map): bound description = %q", controlDesc)

	// Experiment: explicitly ask for v1 via extra paths.
	// v1 must win even though "zzz-v2" sorts after "aaa-v1" and would
	// win in a pure walk-order tie-break.
	regExperiment, err := newServingToolRegistry(root, v1Dir)
	if err != nil {
		t.Fatalf("newServingToolRegistry (experiment): %v", err)
	}
	expDef, ok := regExperiment.tools[key]
	if !ok {
		t.Fatalf("experiment: tool %q not found in registry", key)
	}
	expDesc := expDef.Actions["call"].Description
	if expDesc != "v1-definition" {
		t.Errorf("--package-map FAILED: expected description %q (from v1Dir), got %q; "+
			"scan order is deciding the binding instead of the explicit package-map path",
			"v1-definition", expDesc)
	}
	t.Logf("experiment (package-map→v1Dir): bound description = %q (want v1-definition)", expDesc)

	// Flip: explicitly ask for v2 — must also win deterministically.
	regV2, err := newServingToolRegistry(root, v2Dir)
	if err != nil {
		t.Fatalf("newServingToolRegistry (v2 experiment): %v", err)
	}
	v2Def, ok := regV2.tools[key]
	if !ok {
		t.Fatalf("v2 experiment: tool %q not found in registry", key)
	}
	v2Desc := v2Def.Actions["call"].Description
	if v2Desc != "v2-definition" {
		t.Errorf("--package-map(v2) FAILED: expected description %q, got %q",
			"v2-definition", v2Desc)
	}
	t.Logf("v2 experiment (package-map→v2Dir): bound description = %q (want v2-definition)", v2Desc)

	// Prove scan order does not decide: if we hadn't used extra paths,
	// the control and the v1 experiment could differ, demonstrating that
	// the scan result is non-deterministic across machines whereas the
	// explicit package-map path is always deterministic.
	if expDesc == controlDesc {
		t.Logf("note: scan happened to pick the same as package-map on this run " +
			"(filesystem walk order matched). This is acceptable — the invariant is " +
			"that the explicit path ALWAYS wins, not that it always differs.")
	}
}

// TestServe_PackageMap_MissingFile verifies that --package-map with a
// non-existent file causes an exitValidation-level error from loadPackageMap,
// not a crash or silent ignore. We test loadPackageMap directly since runServe
// would block waiting for the network.
func TestServe_PackageMap_MissingFile(t *testing.T) {
	cfg, err := loadPackageMap("/absolutely/does/not/exist/package-map.yaml")
	if err == nil {
		t.Fatalf("expected error for missing package-map file, got nil (cfg=%v)", cfg)
	}
	t.Logf("loadPackageMap missing file error (expected): %v", err)
}

// TestServe_PackageMap_MalformedFile verifies that a malformed config file
// (wrong apiVersion) causes an error from loadPackageMap.
func TestServe_PackageMap_MalformedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	writeFile(t, path, "apiVersion: wrong/v99\nrequires: []\n")
	cfg, err := loadPackageMap(path)
	if err == nil {
		t.Fatalf("expected error for wrong apiVersion, got nil (cfg=%v)", cfg)
	}
	t.Logf("loadPackageMap malformed file error (expected): %v", err)
}

// TestServe_PackageMap_RequiresAccepted verifies that a valid package-map
// containing requires: is now accepted as catalog input. Serve no longer
// rejects it at startup because per-run catalog resolution consumes it once
// the posted runbook is known.
func TestServe_PackageMap_RequiresAccepted(t *testing.T) {
	dir := t.TempDir()
	pmPath := filepath.Join(dir, "requires-map.yaml")
	writeFile(t, pmPath, "apiVersion: yawr.config/v1\nrequires:\n  - package: acme.incident-tools\n    version: \"^1.0.0\"\n    path: ./vendor/pkg\n")

	pmCfg, err := loadPackageMap(pmPath)
	if err != nil {
		t.Fatalf("loadPackageMap: %v", err)
	}
	merged, _ := mergePackageBindings(nil, pmCfg.Requires)
	if len(merged) != 1 || merged[0].Package != "acme.incident-tools" || merged[0].Path != "./vendor/pkg" {
		t.Fatalf("requires: package-map binding was not retained for serve catalog input: %+v", merged)
	}
}

// TestServe_PackageMap_ToolPathsOnlyAccepted verifies that a package-map with
// only tool-paths: (no requires:) is accepted and the tool-paths: binding
// takes effect — proving the requires: check does not break the valid path.
func TestServe_PackageMap_ToolPathsOnlyAccepted(t *testing.T) {
	root := t.TempDir()
	pmDir := filepath.Join(root, "pm-dir")
	if err := os.MkdirAll(pmDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	const toolName = "tp-only-tool"
	writeFile(t, filepath.Join(pmDir, toolName+".tool.yaml"),
		"apiVersion: yawr.tool/v1\nmeta:\n  name: "+toolName+"\n  version: \"1.0.0\"\ntransport:\n  mode: vscode-mcp\nactions:\n  - name: call\n    description: \"from-pm-dir\"\n    classification: read-only\n")

	dir := t.TempDir()
	pmPath := filepath.Join(dir, "tool-paths-only.yaml")
	writeFile(t, pmPath, "apiVersion: yawr.config/v1\ntool-paths:\n  - "+filepath.ToSlash(pmDir)+"\n")

	pmCfg, err := loadPackageMap(pmPath)
	if err != nil {
		t.Fatalf("loadPackageMap: %v", err)
	}
	if len(pmCfg.Requires) != 0 {
		t.Fatalf("expected no requires:, got %d entries", len(pmCfg.Requires))
	}
	// Verify the tool-paths: binding takes effect in newServingToolRegistry.
	reg, err := newServingToolRegistry(root, pmCfg.ToolPaths...)
	if err != nil {
		t.Fatalf("newServingToolRegistry: %v", err)
	}
	key := toolName + "/call"
	def, ok := reg.tools[key]
	if !ok {
		t.Fatalf("tool-paths: binding did not register tool %q", key)
	}
	if def.Actions["call"].Description != "from-pm-dir" {
		t.Errorf("expected description %q, got %q", "from-pm-dir", def.Actions["call"].Description)
	}
	t.Logf("tool-paths:-only package-map accepted and applied correctly")
}

func containsSubstr(s, sub string) bool {
	return strings.Contains(s, sub)
}
