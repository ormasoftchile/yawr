package pkgpath

import (
	"os"
	"path/filepath"
	"testing"
)

func mkdirp(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func touch(t *testing.T, path string) {
	t.Helper()
	mkdirp(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TV-PKG-PATH-001: requires[].path at project scope is workspace-root
// relative; a path staying inside the workspace root resolves cleanly.
func TestResolveKind_RequiresProject_InsideWorkspace(t *testing.T) {
	ws := t.TempDir()
	touch(t, filepath.Join(ws, "vendor", "acme-incident-tools", "yawr-package.yaml"))

	resolved, external, err := ResolveKind(KindRequiresProject, "", "vendor/acme-incident-tools", ws, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if external {
		t.Fatal("expected not external")
	}
	want, _ := filepath.EvalSymlinks(filepath.Join(ws, "vendor", "acme-incident-tools"))
	if resolved != want {
		t.Fatalf("resolved = %q, want %q", resolved, want)
	}
}

// requires[].path at project/runbook scope is workspace-level: escaping the
// workspace root is permitted (not PKG-007), only reported as external for
// the caller to decide whether to emit PKG-W003
// (design/yawr/sections/06-tool-runtime.tex §Resolution base per path kind:
// "workspace-level kinds ... MAY legitimately resolve outside it --- they
// are never rejected merely for escaping the workspace").
func TestResolveKind_RequiresProject_EscapeIsExternalNotError(t *testing.T) {
	ws := t.TempDir()
	parent := filepath.Dir(ws)
	touch(t, filepath.Join(parent, "outside-pkg", "yawr-package.yaml"))

	_, external, err := ResolveKind(KindRequiresProject, "", "../outside-pkg", ws, "")
	if err != nil {
		t.Fatalf("workspace-level escape must not be an error, got %v", err)
	}
	if !external {
		t.Fatal("expected external=true for a workspace-root escape")
	}
}

// TV-PKG-PATH-003: requires[].path at runbook scope resolves relative to
// the DECLARING runbook file, not the workspace root.
func TestResolveKind_RequiresRunbook_RelativeToDeclaringFile(t *testing.T) {
	ws := t.TempDir()
	declaringRunbook := filepath.Join(ws, "sub", "runbook.yaml")
	touch(t, declaringRunbook)
	touch(t, filepath.Join(ws, "sub", "vendor", "acme-incident-tools", "yawr-package.yaml"))

	resolved, external, err := ResolveKind(KindRequiresRunbook, declaringRunbook, "./vendor/acme-incident-tools", ws, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if external {
		t.Fatal("expected not external")
	}
	want, _ := filepath.EvalSymlinks(filepath.Join(ws, "sub", "vendor", "acme-incident-tools"))
	if resolved != want {
		t.Fatalf("resolved = %q, want %q", resolved, want)
	}
}

// TV-PKG-PATH-004: exports.tools[].path resolves relative to the package's
// own yawr-package.yaml (package root); an escape above the package root is
// PKG-007 even if the target is still inside the workspace.
func TestResolveKind_ExportsTools_EscapeIsPKG007(t *testing.T) {
	ws := t.TempDir()
	pkgRoot := filepath.Join(ws, "vendor", "acme-incident-tools")
	manifestPath := filepath.Join(pkgRoot, "yawr-package.yaml")
	touch(t, manifestPath)
	touch(t, filepath.Join(ws, "outside-tool.tool.yaml"))

	_, _, err := ResolveKind(KindExportsTools, manifestPath, "../../outside-tool.tool.yaml", ws, pkgRoot)
	if err == nil {
		t.Fatal("expected PKG-007 for a package-internal escape above the package root")
	}
	if c, ok := err.(interface{ Code() string }); !ok || c.Code() != "PKG-007" {
		t.Fatalf("expected PKG-007, got %v", err)
	}
}

// TV-PKG-PATH-005: execute.path's resolution base (the declaring
// .tool.yaml's own directory) is not necessarily the same as its
// containment root (the package root). tools/kubectl.tool.yaml using
// execute.path: ../runbooks/drain-node.yaml resolves against tools/
// (the file's own directory), landing at <packageRoot>/runbooks/
// drain-node.yaml — inside the package root, therefore legal.
func TestResolveKind_ExecutePath_ResolutionBaseVsContainmentRoot(t *testing.T) {
	ws := t.TempDir()
	pkgRoot := filepath.Join(ws, "vendor", "acme-incident-tools")
	toolFile := filepath.Join(pkgRoot, "tools", "kubectl.tool.yaml")
	touch(t, toolFile)
	touch(t, filepath.Join(pkgRoot, "runbooks", "drain-node.yaml"))

	resolved, _, err := ResolveKind(KindExecutePath, toolFile, "../runbooks/drain-node.yaml", ws, pkgRoot)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want, _ := filepath.EvalSymlinks(filepath.Join(pkgRoot, "runbooks", "drain-node.yaml"))
	if resolved != want {
		t.Fatalf("resolved = %q, want %q", resolved, want)
	}
}

// TV-PKG-PATH-006: execute.path with a '..' escape that leaves the package
// root entirely (tools/../../outside.yaml) is PKG-007, distinguishing the
// legal same-package '..' of PATH-005 from a genuine escape.
func TestResolveKind_ExecutePath_GenuineEscape_PKG007(t *testing.T) {
	ws := t.TempDir()
	pkgRoot := filepath.Join(ws, "vendor", "acme-incident-tools")
	toolFile := filepath.Join(pkgRoot, "tools", "kubectl.tool.yaml")
	touch(t, toolFile)
	touch(t, filepath.Join(ws, "outside.yaml"))

	_, _, err := ResolveKind(KindExecutePath, toolFile, "../../outside.yaml", ws, pkgRoot)
	if err == nil {
		t.Fatal("expected PKG-007 for a genuine package-root escape")
	}
	if c, ok := err.(interface{ Code() string }); !ok || c.Code() != "PKG-007" {
		t.Fatalf("expected PKG-007, got %v", err)
	}
}

// toolRefs[].path (tier 4) is workspace-level for an ordinary top-level
// runbook, but package-internal (containment = package root) when the
// declaring runbook file is itself only reachable via a package
// (design/yawr/sections/06-tool-runtime.tex §Resolution base per path kind,
// the toolRefs[].path package-rule override bullet).
func TestResolveKind_ToolRefsPath_PackageInternalOverride(t *testing.T) {
	ws := t.TempDir()
	pkgRoot := filepath.Join(ws, "vendor", "acme-incident-tools")
	substituteRunbook := filepath.Join(pkgRoot, "runbooks", "drain-node.yaml")
	touch(t, substituteRunbook)
	touch(t, filepath.Join(pkgRoot, "tools", "kubectl.tool.yaml"))
	touch(t, filepath.Join(ws, "tools", "kubectl.tool.yaml")) // a workspace-level decoy

	// Sibling export inside the same package: legal package-internal ref.
	resolved, _, err := ResolveKind(KindToolRefsPathPackageInternal, substituteRunbook, "../tools/kubectl.tool.yaml", ws, pkgRoot)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want, _ := filepath.EvalSymlinks(filepath.Join(pkgRoot, "tools", "kubectl.tool.yaml"))
	if resolved != want {
		t.Fatalf("resolved = %q, want %q", resolved, want)
	}

	// An attempt to reach outside the package root from that same
	// package-internal runbook is PKG-007 (never merely a workspace-level
	// external report), because the override makes it package-internal.
	_, _, err = ResolveKind(KindToolRefsPathPackageInternal, substituteRunbook, "../../../tools/kubectl.tool.yaml", ws, pkgRoot)
	if err == nil {
		t.Fatal("expected PKG-007 for a package-internal toolRefs escape")
	}
	if c, ok := err.(interface{ Code() string }); !ok || c.Code() != "PKG-007" {
		t.Fatalf("expected PKG-007, got %v", err)
	}
}

// An ordinary top-level runbook's toolRefs[].path stays workspace-level:
// pointing at any tier-4 .tool.yaml on disk (even outside a package root)
// is legal.
func TestResolveKind_ToolRefsPath_WorkspaceLevel_NotPackageInternal(t *testing.T) {
	ws := t.TempDir()
	runbookFile := filepath.Join(ws, "runbook.yaml")
	touch(t, runbookFile)
	touch(t, filepath.Join(ws, "tools", "kubectl.tool.yaml"))

	resolved, external, err := ResolveKind(KindToolRefsPath, runbookFile, "tools/kubectl.tool.yaml", ws, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if external {
		t.Fatal("expected not external")
	}
	want, _ := filepath.EvalSymlinks(filepath.Join(ws, "tools", "kubectl.tool.yaml"))
	if resolved != want {
		t.Fatalf("resolved = %q, want %q", resolved, want)
	}
}

// AllowsAbsolute: workspace-level kinds may use a leading '/'; package-
// internal kinds may not (design/yawr/sections/06-tool-runtime.tex §Syntax
// rules, rule 2).
func TestKind_AllowsAbsolute(t *testing.T) {
	cases := []struct {
		k    Kind
		want bool
	}{
		{KindRequiresProject, true},
		{KindRequiresRunbook, true},
		{KindToolPaths, true},
		{KindToolRefsPath, true},
		{KindExportsTools, false},
		{KindExecutePath, false},
		{KindToolRefsPathPackageInternal, false},
		{KindPackageInternalInclude, false},
	}
	for _, c := range cases {
		if got := c.k.AllowsAbsolute(); got != c.want {
			t.Errorf("Kind(%d).AllowsAbsolute() = %v, want %v", c.k, got, c.want)
		}
	}
}
