package tool

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/pkgpath"
	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

// TestScanStampsPackageProvenance is the regression guard for the yawr serve
// PKG-007 defect: the scan-built runtime registry used to leave SourcePath /
// PackageRoot empty, so every substituted (execute.kind: runbook) tool
// reached the executor with an empty containment root and any legal
// package-internal "../" execute.path failed pkgpath containment as
//
//	PKG-007: path "../runbooks/..." resolves outside its containment root ""
//
// The scan must now stamp each tool with the absolute .tool.yaml path and the
// owning package root (the directory holding yawr-package.yaml), matching the
// provenance pkg/pkgcatalog derives from requires: resolution.
func TestScanStampsPackageProvenance(t *testing.T) {
	root := t.TempDir()
	pkgDir := filepath.Join(root, "packages", "incident-routing-mock")
	toolPath := filepath.Join(pkgDir, "tools", "tsg-recommendation.tool.yaml")
	runbookPath := filepath.Join(pkgDir, "runbooks", "recommend.runbook.yaml")

	writeFile(t, filepath.Join(pkgDir, "yawr-package.yaml"),
		"apiVersion: yawr.tool-package/v1\n"+
			"meta:\n  name: sql-livesite.incident-routing\n  version: 1.0.0\n"+
			"exports:\n  tools:\n    - id: tsg-recommendation\n      path: tools/tsg-recommendation.tool.yaml\n")
	writeFile(t, runbookPath,
		"apiVersion: yawr.runbook/v1\nmeta:\n  id: recommend\n  title: Recommend\nsteps: []\n")
	// A substituted tool whose execute.path steps up out of tools/ into a
	// sibling package-internal directory -- legal, and the exact shape the
	// user's recommend_tsg tool uses.
	writeFile(t, toolPath,
		"apiVersion: yawr.tool/v1\n"+
			"meta:\n  name: tsg-recommendation\n  version: 1.0.0\n"+
			"transport:\n  mode: native\n  command: does-not-run\n"+
			"actions:\n  - name: recommend\n"+
			"    execute:\n      kind: runbook\n      path: ../runbooks/recommend.runbook.yaml\n")

	defs, err := ScanDirWithoutTests(root)
	if err != nil {
		t.Fatalf("ScanDirWithoutTests: %v", err)
	}
	var def *toolpkg.ToolDef
	for i := range defs {
		if defs[i].Name == "tsg-recommendation" {
			d := defs[i]
			def = &d
			break
		}
	}
	if def == nil {
		t.Fatalf("tsg-recommendation not found in scanned defs: %+v", defs)
	}

	wantSource, _ := filepath.Abs(toolPath)
	if def.SourcePath != wantSource {
		t.Errorf("SourcePath = %q, want %q", def.SourcePath, wantSource)
	}
	wantRoot, _ := filepath.Abs(pkgDir)
	if def.PackageRoot != wantRoot {
		t.Errorf("PackageRoot = %q, want %q", def.PackageRoot, wantRoot)
	}
	if def.PackageName != "sql-livesite.incident-routing" {
		t.Errorf("PackageName = %q, want %q", def.PackageName, "sql-livesite.incident-routing")
	}

	// End-to-end: the stamped provenance must make the package-internal "../"
	// execute.path resolve through the exact call the executor makes
	// (pkgsubst.Plan -> pkgpath.ResolveKind(KindExecutePath, SourcePath, ...,
	// PackageRoot)).
	action := def.Actions["recommend"]
	if action == nil || action.Execute == nil {
		t.Fatalf("recommend action missing execute spec: %+v", def.Actions)
	}
	resolved, _, rerr := pkgpath.ResolveKind(
		pkgpath.KindExecutePath, def.SourcePath, action.Execute.Path, "", def.PackageRoot)
	if rerr != nil {
		t.Fatalf("package-internal execute.path must resolve, got: %v", rerr)
	}
	wantResolved, _ := filepath.EvalSymlinks(runbookPath)
	if gotReal, _ := filepath.EvalSymlinks(resolved); gotReal != wantResolved {
		t.Errorf("resolved = %q, want %q", resolved, wantResolved)
	}
}

// TestScanProvenanceStillRejectsEscape proves the fix does not weaken
// containment: an execute.path that genuinely escapes the owning package root
// must still be rejected with PKG-007. The bug was an empty root, not an
// over-strict check.
func TestScanProvenanceStillRejectsEscape(t *testing.T) {
	root := t.TempDir()
	pkgDir := filepath.Join(root, "packages", "escaper")
	toolPath := filepath.Join(pkgDir, "tools", "escaper.tool.yaml")

	writeFile(t, filepath.Join(pkgDir, "yawr-package.yaml"),
		"apiVersion: yawr.tool-package/v1\n"+
			"meta:\n  name: escaper\n  version: 1.0.0\n"+
			"exports:\n  tools:\n    - id: escaper\n      path: tools/escaper.tool.yaml\n")
	// A path that climbs above the package root entirely.
	writeFile(t, toolPath,
		"apiVersion: yawr.tool/v1\n"+
			"meta:\n  name: escaper\n  version: 1.0.0\n"+
			"transport:\n  mode: native\n  command: does-not-run\n"+
			"actions:\n  - name: run\n"+
			"    execute:\n      kind: runbook\n      path: ../../../outside.runbook.yaml\n")
	// The escape target actually exists on disk, so the ONLY reason to reject
	// it is containment, not a missing file.
	writeFile(t, filepath.Join(root, "outside.runbook.yaml"),
		"apiVersion: yawr.runbook/v1\nmeta:\n  id: outside\n  title: Outside\nsteps: []\n")

	defs, err := ScanDirWithoutTests(root)
	if err != nil {
		t.Fatalf("ScanDirWithoutTests: %v", err)
	}
	var def *toolpkg.ToolDef
	for i := range defs {
		if defs[i].Name == "escaper" {
			d := defs[i]
			def = &d
			break
		}
	}
	if def == nil {
		t.Fatalf("escaper not found in scanned defs")
	}

	action := def.Actions["run"]
	if action == nil || action.Execute == nil {
		t.Fatalf("run action missing execute spec")
	}
	_, _, rerr := pkgpath.ResolveKind(
		pkgpath.KindExecutePath, def.SourcePath, action.Execute.Path, "", def.PackageRoot)
	if rerr == nil {
		t.Fatal("escaping execute.path must be rejected, got nil error")
	}
	coder, ok := rerr.(interface{ Code() string })
	if !ok || coder.Code() != "PKG-007" {
		t.Fatalf("escaping path must fail with PKG-007, got: %v", rerr)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// findDef returns the scanned runtime def with the given tool name, or nil.
func findDef(defs []toolpkg.ToolDef, name string) *toolpkg.ToolDef {
	for i := range defs {
		if defs[i].Name == name {
			d := defs[i]
			return &d
		}
	}
	return nil
}

// TestScanProvenanceNestedManifestPicksDeepest locks in the behaviour a
// reviewer verified by hand: when a yawr-package.yaml is nested inside another
// package's subtree, owningPackage() binds a tool to the NEAREST (deepest)
// manifest above it, not the outer one. The scan-derived root is therefore
// always the deepest owning package -- a subset of, or equal to, whatever root
// a requires: catalog binding would name -- so containment can only be equal
// or stricter, never wider. A package-internal "../" path that stays inside
// the deepest package must still resolve against that deeper root.
func TestScanProvenanceNestedManifestPicksDeepest(t *testing.T) {
	root := t.TempDir()
	outerDir := filepath.Join(root, "outer")
	innerDir := filepath.Join(outerDir, "inner")
	toolPath := filepath.Join(innerDir, "tools", "nested.tool.yaml")
	runbookPath := filepath.Join(innerDir, "runbooks", "nested.runbook.yaml")

	// Outer package manifest -- an ancestor of the tool, deliberately NOT the
	// one that should own it.
	writeFile(t, filepath.Join(outerDir, "yawr-package.yaml"),
		"apiVersion: yawr.tool-package/v1\n"+
			"meta:\n  name: outer.pkg\n  version: 1.0.0\n"+
			"exports:\n  tools: []\n")
	// Inner (nested) package manifest -- the nearest manifest above the tool.
	writeFile(t, filepath.Join(innerDir, "yawr-package.yaml"),
		"apiVersion: yawr.tool-package/v1\n"+
			"meta:\n  name: inner.pkg\n  version: 1.0.0\n"+
			"exports:\n  tools:\n    - id: nested\n      path: tools/nested.tool.yaml\n")
	writeFile(t, runbookPath,
		"apiVersion: yawr.runbook/v1\nmeta:\n  id: nested\n  title: Nested\nsteps: []\n")
	writeFile(t, toolPath,
		"apiVersion: yawr.tool/v1\n"+
			"meta:\n  name: nested\n  version: 1.0.0\n"+
			"transport:\n  mode: native\n  command: does-not-run\n"+
			"actions:\n  - name: run\n"+
			"    execute:\n      kind: runbook\n      path: ../runbooks/nested.runbook.yaml\n")

	defs, err := ScanDirWithoutTests(root)
	if err != nil {
		t.Fatalf("ScanDirWithoutTests: %v", err)
	}
	def := findDef(defs, "nested")
	if def == nil {
		t.Fatalf("nested tool not found in scanned defs")
	}

	wantRoot, _ := filepath.Abs(innerDir)
	if def.PackageRoot != wantRoot {
		t.Errorf("PackageRoot = %q, want the DEEPEST manifest dir %q", def.PackageRoot, wantRoot)
	}
	if outerAbs, _ := filepath.Abs(outerDir); def.PackageRoot == outerAbs {
		t.Errorf("PackageRoot resolved to the OUTER package %q; must bind to the nearest/deepest manifest", outerAbs)
	}
	if def.PackageName != "inner.pkg" {
		t.Errorf("PackageName = %q, want %q (deepest manifest's meta.name)", def.PackageName, "inner.pkg")
	}

	// The package-internal "../" path must resolve against the deeper root.
	action := def.Actions["run"]
	if action == nil || action.Execute == nil {
		t.Fatalf("run action missing execute spec")
	}
	if _, _, rerr := pkgpath.ResolveKind(
		pkgpath.KindExecutePath, def.SourcePath, action.Execute.Path, "", def.PackageRoot); rerr != nil {
		t.Fatalf("package-internal path must resolve against the deepest root, got: %v", rerr)
	}
}

// TestScanProvenanceAdHocFallback covers a tool that sits under NO package
// manifest: owningPackage() finds nothing, so PackageRoot falls back to the
// tool file's own directory (the documented tier-2/4 ad-hoc containment root)
// and PackageName is empty. Containment is then relative to that directory, so
// a path escaping it is still rejected.
func TestScanProvenanceAdHocFallback(t *testing.T) {
	root := t.TempDir()
	// No yawr-package.yaml anywhere in this subtree.
	toolDir := filepath.Join(root, "loose", "tools")
	toolPath := filepath.Join(toolDir, "adhoc.tool.yaml")
	writeFile(t, toolPath,
		"apiVersion: yawr.tool/v1\n"+
			"meta:\n  name: adhoc\n  version: 1.0.0\n"+
			"transport:\n  mode: native\n  command: does-not-run\n"+
			"actions:\n  - name: run\n"+
			"    execute:\n      kind: runbook\n      path: ../runbooks/adhoc.runbook.yaml\n")

	defs, err := ScanDirWithoutTests(root)
	if err != nil {
		t.Fatalf("ScanDirWithoutTests: %v", err)
	}
	def := findDef(defs, "adhoc")
	if def == nil {
		t.Fatalf("adhoc tool not found in scanned defs")
	}

	wantSource, _ := filepath.Abs(toolPath)
	if def.SourcePath != wantSource {
		t.Errorf("SourcePath = %q, want %q", def.SourcePath, wantSource)
	}
	wantRoot, _ := filepath.Abs(toolDir)
	if def.PackageRoot != wantRoot {
		t.Errorf("PackageRoot = %q, want the tool's own dir %q (ad-hoc fallback)", def.PackageRoot, wantRoot)
	}
	if def.PackageName != "" {
		t.Errorf("PackageName = %q, want empty for an ad-hoc tool", def.PackageName)
	}

	// The "../runbooks/..." path escapes the tool's own directory, so with the
	// ad-hoc root it correctly fails containment -- an ad-hoc tool has no
	// package to step around inside.
	action := def.Actions["run"]
	if action == nil || action.Execute == nil {
		t.Fatalf("run action missing execute spec")
	}
	_, _, rerr := pkgpath.ResolveKind(
		pkgpath.KindExecutePath, def.SourcePath, action.Execute.Path, "", def.PackageRoot)
	if rerr == nil {
		t.Fatal("ad-hoc tool escaping its own dir must be rejected, got nil")
	}
	if coder, ok := rerr.(interface{ Code() string }); !ok || coder.Code() != "PKG-007" {
		t.Fatalf("ad-hoc escape must fail with PKG-007, got: %v", rerr)
	}
}

// TestScanProvenanceManifestWithoutName covers a present-but-nameless manifest:
// owningPackage() still establishes the package root (the manifest's
// directory), but PackageName is empty because meta.name is absent. The root
// -- the field containment depends on -- must still be correct even when the
// name metadata is missing, so a package-internal path resolves.
func TestScanProvenanceManifestWithoutName(t *testing.T) {
	root := t.TempDir()
	pkgDir := filepath.Join(root, "nameless")
	toolPath := filepath.Join(pkgDir, "tools", "noname.tool.yaml")
	runbookPath := filepath.Join(pkgDir, "runbooks", "noname.runbook.yaml")

	// Manifest present, valid YAML, but with no meta.name.
	writeFile(t, filepath.Join(pkgDir, "yawr-package.yaml"),
		"apiVersion: yawr.tool-package/v1\n"+
			"meta:\n  version: 1.0.0\n"+
			"exports:\n  tools:\n    - id: noname\n      path: tools/noname.tool.yaml\n")
	writeFile(t, runbookPath,
		"apiVersion: yawr.runbook/v1\nmeta:\n  id: noname\n  title: NoName\nsteps: []\n")
	writeFile(t, toolPath,
		"apiVersion: yawr.tool/v1\n"+
			"meta:\n  name: noname\n  version: 1.0.0\n"+
			"transport:\n  mode: native\n  command: does-not-run\n"+
			"actions:\n  - name: run\n"+
			"    execute:\n      kind: runbook\n      path: ../runbooks/noname.runbook.yaml\n")

	defs, err := ScanDirWithoutTests(root)
	if err != nil {
		t.Fatalf("ScanDirWithoutTests: %v", err)
	}
	def := findDef(defs, "noname")
	if def == nil {
		t.Fatalf("noname tool not found in scanned defs")
	}

	wantRoot, _ := filepath.Abs(pkgDir)
	if def.PackageRoot != wantRoot {
		t.Errorf("PackageRoot = %q, want %q even without meta.name", def.PackageRoot, wantRoot)
	}
	if def.PackageName != "" {
		t.Errorf("PackageName = %q, want empty when the manifest declares no meta.name", def.PackageName)
	}

	// Root is intact, so the package-internal path still resolves.
	action := def.Actions["run"]
	if action == nil || action.Execute == nil {
		t.Fatalf("run action missing execute spec")
	}
	if _, _, rerr := pkgpath.ResolveKind(
		pkgpath.KindExecutePath, def.SourcePath, action.Execute.Path, "", def.PackageRoot); rerr != nil {
		t.Fatalf("package-internal path must resolve even with a nameless manifest, got: %v", rerr)
	}
}
