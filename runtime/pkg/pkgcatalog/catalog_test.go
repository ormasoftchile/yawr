package pkgcatalog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/pkgpath"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

// writeFile creates a file (and its parent dirs) with the given content.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const kubectlToolYAML = `apiVersion: yawr.tool/v1
meta: {name: kubectl, version: "1.0.0"}
transport:
  mode: native
  command: kubectl
actions:
  - name: drain
    description: Drain a node
    argv: ["drain", "${node}"]
    args:
      node: { type: string, required: true }
    returns: text
`

func writeAcmePackage(t *testing.T, root string) {
	writeFile(t, filepath.Join(root, "yawr-package.yaml"), `apiVersion: yawr.tool-package/v1
meta:
  name: acme.incident-tools
  version: "1.4.2"
exports:
  tools:
    - id: kubectl
      path: tools/kubectl.tool.yaml
`)
	writeFile(t, filepath.Join(root, "tools", "kubectl.tool.yaml"), kubectlToolYAML)
}

func TestBuild_Tier1_PackageResolution(t *testing.T) {
	ws := t.TempDir()
	pkgRoot := filepath.Join(ws, "vendor", "acme-incident-tools")
	writeAcmePackage(t, pkgRoot)

	opts := BuildOptions{
		WorkspaceRoot: ws,
		ProjectRequires: []*schema.PackageRequirement{
			{Package: "acme.incident-tools", Version: "^1.4.0", Path: "vendor/acme-incident-tools"},
		},
	}
	cat, errs := Build(opts)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	entry, ok := cat.ByQualified("acme.incident-tools/kubectl")
	if !ok {
		t.Fatal("expected qualified entry acme.incident-tools/kubectl")
	}
	if entry.Tier != TierPackage {
		t.Fatalf("expected TierPackage, got %v", entry.Tier)
	}
	bare := cat.ByBare("kubectl")
	if len(bare) != 1 {
		t.Fatalf("expected 1 bare entry, got %d", len(bare))
	}
	if len(cat.Packages) != 1 || cat.Packages[0].Name != "acme.incident-tools" {
		t.Fatalf("expected locked package info, got %+v", cat.Packages)
	}
}

func TestBuild_YawrPackageManifest(t *testing.T) {
	manifest := `apiVersion: yawr.tool-package/v1
meta: {name: acme.incident-tools, version: "1.4.2"}
exports:
  tools: [{id: kubectl, path: tools/kubectl.tool.yaml}]
`
	ws := t.TempDir()
	root := filepath.Join(ws, "package")
	writeFile(t, filepath.Join(root, schema.PackageManifestFilename), manifest)
	writeFile(t, filepath.Join(root, "tools", "kubectl.tool.yaml"), kubectlToolYAML)
	_, errs := Build(BuildOptions{WorkspaceRoot: ws, ProjectRequires: []*schema.PackageRequirement{
		{Package: "acme.incident-tools", Version: "^1.4.0", Path: "package"},
	}})
	if len(errs) != 0 {
		t.Fatalf("Build() errors = %v", errs)
	}
}

func TestBuild_PKG002_VersionConstraintUnsatisfied(t *testing.T) {
	ws := t.TempDir()
	pkgRoot := filepath.Join(ws, "vendor", "acme-incident-tools")
	writeAcmePackage(t, pkgRoot) // version 1.4.2

	opts := BuildOptions{
		WorkspaceRoot: ws,
		ProjectRequires: []*schema.PackageRequirement{
			{Package: "acme.incident-tools", Version: "^2.0.0", Path: "vendor/acme-incident-tools"},
		},
	}
	_, errs := Build(opts)
	if !hasCode(errs, "PKG-002") {
		t.Fatalf("expected PKG-002, got %v", errs)
	}
}

// TestBuild_ExternalRequiresPath_PKGW003NotPKG007 exercises Barbara's
// binding ruling (TV-PKG-PATH-002): a project-scope requires[].path that
// resolves outside the workspace root is operator configuration -- it MUST
// succeed (external:sha256:... lock root) and be reported as PKG-W003, and
// MUST NOT be rejected as PKG-007.
func TestBuild_ExternalRequiresPath_PKGW003NotPKG007(t *testing.T) {
	container := t.TempDir()
	ws := filepath.Join(container, "workspace")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	pkgRoot := filepath.Join(container, "outside", "acme-incident-tools")
	writeAcmePackage(t, pkgRoot)

	opts := BuildOptions{
		WorkspaceRoot: ws,
		ProjectRequires: []*schema.PackageRequirement{
			{Package: "acme.incident-tools", Version: "^1.4.0", Path: "../outside/acme-incident-tools"},
		},
	}
	cat, errs := Build(opts)
	fatal, warnings := errkit.SplitWarnings(errs)
	if len(fatal) != 0 {
		t.Fatalf("expected no fatal errors, got %v", fatal)
	}
	if !hasCode(warnings, "PKG-W003") {
		t.Fatalf("expected PKG-W003 warning, got %v", warnings)
	}
	if hasCode(errs, "PKG-007") {
		t.Fatalf("PKG-007 MUST NOT be raised for a workspace-level escape: %v", errs)
	}
	if _, ok := cat.ByQualified("acme.incident-tools/kubectl"); !ok {
		t.Fatal("expected the external package's tool to still be catalogued")
	}
	if len(cat.Packages) != 1 || !strings.HasPrefix(cat.Packages[0].Root, "external:sha256:") {
		t.Fatalf("expected external:sha256:... lock root, got %+v", cat.Packages)
	}
}

func TestBuild_PKG005_DuplicatePackageInOneRequiresList(t *testing.T) {
	ws := t.TempDir()
	opts := BuildOptions{
		WorkspaceRoot: ws,
		ProjectRequires: []*schema.PackageRequirement{
			{Package: "acme.incident-tools", Version: "^1.0.0", Path: "vendor/a"},
			{Package: "acme.incident-tools", Version: "^1.0.0", Path: "vendor/a"},
		},
	}
	_, errs := Build(opts)
	if !hasCode(errs, "PKG-005") {
		t.Fatalf("expected PKG-005, got %v", errs)
	}
}

func TestBuild_EquivalentProjectAndRunbookPathsDoNotConflict(t *testing.T) {
	ws := t.TempDir()
	pkgRoot := filepath.Join(ws, "packages", "acme-incident-tools")
	writeAcmePackage(t, pkgRoot)
	runbookPath := filepath.Join(ws, "hands-on-tests", "local", "scenario.runbook.yaml")
	writeFile(t, runbookPath, "apiVersion: yawr.runbook/v1\nid: scenario\nflow: []\n")

	cat, errs := Build(BuildOptions{
		WorkspaceRoot: ws,
		ProjectRequires: []*schema.PackageRequirement{
			{Package: "acme.incident-tools", Version: "^1.0.0", Path: "./packages/acme-incident-tools"},
		},
		RunbookRequires: []*schema.PackageRequirement{
			{Package: "acme.incident-tools", Version: "^1.4.0", Path: "../../packages/acme-incident-tools"},
		},
		RunbookPath: runbookPath,
	})
	if hasCode(errs, "PKG-024") {
		t.Fatalf("equivalent resolved paths must not conflict: %v", errs)
	}
	if fatal, _ := errkit.SplitWarnings(errs); len(fatal) != 0 {
		t.Fatalf("unexpected fatal errors: %v", fatal)
	}
	if _, ok := cat.ByQualified("acme.incident-tools/kubectl"); !ok {
		t.Fatal("expected equivalent bindings to load the package")
	}
}

func TestBuild_EquivalentPathsStillIntersectVersionConstraints(t *testing.T) {
	ws := t.TempDir()
	pkgRoot := filepath.Join(ws, "packages", "acme-incident-tools")
	writeAcmePackage(t, pkgRoot)
	runbookPath := filepath.Join(ws, "hands-on-tests", "local", "scenario.runbook.yaml")
	writeFile(t, runbookPath, "apiVersion: yawr.runbook/v1\nid: scenario\nflow: []\n")

	_, errs := Build(BuildOptions{
		WorkspaceRoot: ws,
		ProjectRequires: []*schema.PackageRequirement{
			{Package: "acme.incident-tools", Version: "^1.0.0", Path: "./packages/acme-incident-tools"},
		},
		RunbookRequires: []*schema.PackageRequirement{
			{Package: "acme.incident-tools", Version: "^2.0.0", Path: "../../packages/acme-incident-tools"},
		},
		RunbookPath: runbookPath,
	})
	if !hasCode(errs, "PKG-002") {
		t.Fatalf("equivalent paths must still intersect incompatible constraints, got %v", errs)
	}
	if hasCode(errs, "PKG-024") {
		t.Fatalf("equivalent paths must not be misclassified as conflicting: %v", errs)
	}
}

func TestBuild_SymlinkAliasPathsDoNotConflict(t *testing.T) {
	ws := t.TempDir()
	pkgRoot := filepath.Join(ws, "packages", "acme-incident-tools")
	writeAcmePackage(t, pkgRoot)
	aliasRoot := filepath.Join(ws, "aliases")
	if err := os.MkdirAll(aliasRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(aliasRoot, "acme-incident-tools")
	if err := os.Symlink(pkgRoot, alias); err != nil {
		t.Skipf("symlink not supported in this environment: %v", err)
	}
	runbookPath := filepath.Join(ws, "hands-on-tests", "local", "scenario.runbook.yaml")
	writeFile(t, runbookPath, "apiVersion: yawr.runbook/v1\nid: scenario\nflow: []\n")

	_, errs := Build(BuildOptions{
		WorkspaceRoot: ws,
		ProjectRequires: []*schema.PackageRequirement{
			{Package: "acme.incident-tools", Version: "^1.0.0", Path: "packages/acme-incident-tools"},
		},
		RunbookRequires: []*schema.PackageRequirement{
			{Package: "acme.incident-tools", Version: "^1.0.0", Path: "../../aliases/acme-incident-tools"},
		},
		RunbookPath: runbookPath,
	})
	if fatal, _ := errkit.SplitWarnings(errs); len(fatal) != 0 {
		t.Fatalf("symlink aliases resolving to one package must not conflict: %v", fatal)
	}
}

func TestBuild_DifferentProjectAndRunbookPathsStillConflict(t *testing.T) {
	ws := t.TempDir()
	writeAcmePackage(t, filepath.Join(ws, "packages", "project-tools"))
	writeAcmePackage(t, filepath.Join(ws, "packages", "runbook-tools"))
	runbookPath := filepath.Join(ws, "hands-on-tests", "local", "scenario.runbook.yaml")
	writeFile(t, runbookPath, "apiVersion: yawr.runbook/v1\nid: scenario\nflow: []\n")

	_, errs := Build(BuildOptions{
		WorkspaceRoot: ws,
		ProjectRequires: []*schema.PackageRequirement{
			{Package: "acme.incident-tools", Version: "^1.0.0", Path: "./packages/project-tools"},
		},
		RunbookRequires: []*schema.PackageRequirement{
			{Package: "acme.incident-tools", Version: "^1.0.0", Path: "../../packages/runbook-tools"},
		},
		RunbookPath: runbookPath,
	})
	if !hasCode(errs, "PKG-024") {
		t.Fatalf("expected different resolved paths to conflict, got %v", errs)
	}
}

func TestBuild_MissingRunbookPathFailsClosedAsConflict(t *testing.T) {
	ws := t.TempDir()
	writeAcmePackage(t, filepath.Join(ws, "packages", "project-tools"))
	runbookPath := filepath.Join(ws, "hands-on-tests", "local", "scenario.runbook.yaml")
	writeFile(t, runbookPath, "apiVersion: yawr.runbook/v1\nid: scenario\nflow: []\n")

	_, errs := Build(BuildOptions{
		WorkspaceRoot: ws,
		ProjectRequires: []*schema.PackageRequirement{
			{Package: "acme.incident-tools", Version: "^1.0.0", Path: "packages/project-tools"},
		},
		RunbookRequires: []*schema.PackageRequirement{
			{Package: "acme.incident-tools", Version: "^1.0.0", Path: "../../packages/missing-tools"},
		},
		RunbookPath: runbookPath,
	})
	if countCode(errs, "PKG-024") != 1 {
		t.Fatalf("missing runbook path must produce exactly one PKG-024, got %v", errs)
	}
}

func TestCompareRequirementPaths_RunbookResolutionFailureDoesNotPinProject(t *testing.T) {
	ws := t.TempDir()
	projectRoot := filepath.Join(ws, "packages", "project-tools")
	writeAcmePackage(t, projectRoot)
	runbookPath := filepath.Join(ws, "hands-on-tests", "local", "scenario.runbook.yaml")
	writeFile(t, runbookPath, "apiVersion: yawr.runbook/v1\nid: scenario\nflow: []\n")
	previous := projectRoot
	for index := 0; index < 9; index++ {
		link := filepath.Join(filepath.Dir(runbookPath), fmt.Sprintf("blocked-link-%d", index))
		if err := os.Symlink(previous, link); err != nil {
			t.Skipf("symlink not supported in this environment: %v", err)
		}
		previous = link
	}

	comparison := compareRequirementPaths(
		"packages/project-tools",
		"blocked-link-8",
		ws,
		runbookPath,
	)
	if comparison.projectResolved != nil {
		t.Fatalf("failed runbook path resolution must not retain a project pin: %#v", comparison.projectResolved)
	}
}

func TestLoadPackage_RejectsResolvedDirectoryIdentityMismatch(t *testing.T) {
	ws := t.TempDir()
	originalRoot := filepath.Join(ws, "packages", "original-tools")
	replacementRoot := filepath.Join(ws, "packages", "replacement-tools")
	writeAcmePackage(t, originalRoot)
	writeAcmePackage(t, replacementRoot)
	originalInfo, err := os.Stat(originalRoot)
	if err != nil {
		t.Fatal(err)
	}

	catalog := &Catalog{WorkspaceRoot: ws}
	errs := catalog.loadPackage(
		&schema.PackageRequirement{Package: "acme.incident-tools", Version: "^1.0.0", Path: "packages/replacement-tools"},
		pkgpath.KindRequiresProject,
		BuildOptions{WorkspaceRoot: ws},
		nil,
		&resolvedRequirementPath{path: replacementRoot, identity: originalInfo},
	)
	if countCode(errs, "PKG-008") != 1 {
		t.Fatalf("expected one PKG-008 identity-change error, got %v", errs)
	}
}

func TestBuild_SameTextProjectAndRunbookPathsCanStillConflict(t *testing.T) {
	ws := t.TempDir()
	writeAcmePackage(t, filepath.Join(ws, "vendor", "acme-incident-tools"))
	runbookPath := filepath.Join(ws, "hands-on-tests", "local", "scenario.runbook.yaml")
	writeFile(t, runbookPath, "apiVersion: yawr.runbook/v1\nid: scenario\nflow: []\n")
	writeAcmePackage(t, filepath.Join(filepath.Dir(runbookPath), "vendor", "acme-incident-tools"))

	_, errs := Build(BuildOptions{
		WorkspaceRoot: ws,
		ProjectRequires: []*schema.PackageRequirement{
			{Package: "acme.incident-tools", Version: "^1.0.0", Path: "vendor/acme-incident-tools"},
		},
		RunbookRequires: []*schema.PackageRequirement{
			{Package: "acme.incident-tools", Version: "^1.0.0", Path: "vendor/acme-incident-tools"},
		},
		RunbookPath: runbookPath,
	})
	if !hasCode(errs, "PKG-024") {
		t.Fatalf("same authored text resolving to different directories must conflict, got %v", errs)
	}
}

func TestBuild_InvalidRunbookPathTakesPrecedenceOverConflict(t *testing.T) {
	ws := t.TempDir()
	writeAcmePackage(t, filepath.Join(ws, "packages", "acme-incident-tools"))
	runbookPath := filepath.Join(ws, "hands-on-tests", "local", "scenario.runbook.yaml")
	writeFile(t, runbookPath, "apiVersion: yawr.runbook/v1\nid: scenario\nflow: []\n")

	_, errs := Build(BuildOptions{
		WorkspaceRoot: ws,
		ProjectRequires: []*schema.PackageRequirement{
			{Package: "acme.incident-tools", Version: "^1.0.0", Path: "packages/acme-incident-tools"},
		},
		RunbookRequires: []*schema.PackageRequirement{
			{Package: "acme.incident-tools", Version: "^1.0.0", Path: "..\\..\\packages\\acme-incident-tools"},
		},
		RunbookPath: runbookPath,
	})
	if !hasCode(errs, "PKG-007") {
		t.Fatalf("expected invalid runbook path to report PKG-007, got %v", errs)
	}
	if countCode(errs, "PKG-007") != 1 {
		t.Fatalf("expected exactly one PKG-007 diagnostic, got %v", errs)
	}
	if hasCode(errs, "PKG-024") {
		t.Fatalf("invalid path syntax must not be misclassified as PKG-024: %v", errs)
	}
}

func TestBuild_PKG001_NoPath(t *testing.T) {
	ws := t.TempDir()
	opts := BuildOptions{
		WorkspaceRoot: ws,
		RunbookRequires: []*schema.PackageRequirement{
			{Package: "acme.incident-tools", Version: "^1.0.0"},
		},
	}
	_, errs := Build(opts)
	if !hasCode(errs, "PKG-001") {
		t.Fatalf("expected PKG-001, got %v", errs)
	}
}

func TestBuild_PKG023_ExportIDMismatch(t *testing.T) {
	ws := t.TempDir()
	pkgRoot := filepath.Join(ws, "vendor", "bad-tools")
	writeFile(t, filepath.Join(pkgRoot, "yawr-package.yaml"), `apiVersion: yawr.tool-package/v1
meta:
  name: acme.bad-tools
  version: "1.0.0"
exports:
  tools:
    - id: kubectl
      path: tools/kubectl.tool.yaml
`)
	writeFile(t, filepath.Join(pkgRoot, "tools", "kubectl.tool.yaml"), `apiVersion: yawr.tool/v1
meta: {name: not-kubectl, version: "1.0.0"}
transport:
  mode: native
  command: kubectl
actions:
  - name: drain
    argv: ["drain"]
`)
	opts := BuildOptions{
		WorkspaceRoot: ws,
		ProjectRequires: []*schema.PackageRequirement{
			{Package: "acme.bad-tools", Version: "^1.0.0", Path: "vendor/bad-tools"},
		},
	}
	_, errs := Build(opts)
	if !hasCode(errs, "PKG-023") {
		t.Fatalf("expected PKG-023, got %v", errs)
	}
}

func TestBuild_PKG016_NonEmptyDependencies(t *testing.T) {
	ws := t.TempDir()
	pkgRoot := filepath.Join(ws, "vendor", "deptool")
	writeFile(t, filepath.Join(pkgRoot, "yawr-package.yaml"), `apiVersion: yawr.tool-package/v1
meta:
  name: acme.deptool
  version: "1.0.0"
dependencies:
  - somepkg
exports:
  tools:
    - id: kubectl
      path: tools/kubectl.tool.yaml
`)
	writeFile(t, filepath.Join(pkgRoot, "tools", "kubectl.tool.yaml"), kubectlToolYAML)
	opts := BuildOptions{
		WorkspaceRoot: ws,
		ProjectRequires: []*schema.PackageRequirement{
			{Package: "acme.deptool", Version: "^1.0.0", Path: "vendor/deptool"},
		},
	}
	_, errs := Build(opts)
	if !hasCode(errs, "PKG-016") {
		t.Fatalf("expected PKG-016, got %v", errs)
	}
}

func TestBuild_PKG006_SameTierCollision(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "tools", "a", "kubectl.tool.yaml"), kubectlToolYAML)
	writeFile(t, filepath.Join(ws, "tools", "b", "kubectl.tool.yaml"), kubectlToolYAML)

	opts := BuildOptions{WorkspaceRoot: ws}
	_, errs := Build(opts)
	if !hasCode(errs, "PKG-006") {
		t.Fatalf("expected PKG-006 for tier-2 collision, got %v", errs)
	}
}

func TestBuild_DeterministicOrdering(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "tools", "zzz.tool.yaml"), `apiVersion: yawr.tool/v1
meta: {name: zzz}
transport: {mode: native, command: zzz}
actions: [{name: run, argv: ["run"]}]
`)
	writeFile(t, filepath.Join(ws, "tools", "aaa.tool.yaml"), `apiVersion: yawr.tool/v1
meta: {name: aaa}
transport: {mode: native, command: aaa}
actions: [{name: run, argv: ["run"]}]
`)
	cat1, errs1 := Build(BuildOptions{WorkspaceRoot: ws})
	cat2, errs2 := Build(BuildOptions{WorkspaceRoot: ws})
	if len(errs1) != 0 || len(errs2) != 0 {
		t.Fatalf("unexpected errors: %v / %v", errs1, errs2)
	}
	if cat1.CatalogDigest() != cat2.CatalogDigest() {
		t.Fatalf("expected byte-identical catalog digests across runs: %s vs %s", cat1.CatalogDigest(), cat2.CatalogDigest())
	}
	entries := cat1.Entries()
	if len(entries) < 2 {
		t.Fatalf("expected at least 2 entries, got %d", len(entries))
	}
	if entries[0].Qualified != "aaa" || entries[1].Qualified != "zzz" {
		t.Fatalf("expected sorted (aaa, zzz) order, got %s, %s", entries[0].Qualified, entries[1].Qualified)
	}
}

func TestBindFile_ByPackage_CollisionProof(t *testing.T) {
	ws := t.TempDir()
	pkgRoot := filepath.Join(ws, "vendor", "acme-incident-tools")
	writeAcmePackage(t, pkgRoot)
	// Also add a tier-2 project tool with the SAME bare name, which would
	// otherwise collide/shadow.
	writeFile(t, filepath.Join(ws, "tools", "kubectl.tool.yaml"), kubectlToolYAML)

	cat, errs := Build(BuildOptions{
		WorkspaceRoot: ws,
		ProjectRequires: []*schema.PackageRequirement{
			{Package: "acme.incident-tools", Version: "^1.4.0", Path: "vendor/acme-incident-tools"},
		},
	})
	if len(errs) != 0 {
		t.Fatalf("unexpected Build errors: %v", errs)
	}

	runbookPath := filepath.Join(ws, "runbook.yaml")
	refs := []*schema.ToolRef{
		// kubectlToolYAML declares version: "1.0.0"; ^1.0.0 is satisfied.
		{Name: "kubectl", Package: "acme.incident-tools", Version: "^1.0.0"},
	}
	bindings, berrs := BindFile(cat, runbookPath, refs, "")
	if len(berrs) != 0 {
		t.Fatalf("unexpected bind errors: %v", berrs)
	}
	if len(bindings) != 1 || bindings[0].Name != "kubectl" {
		t.Fatalf("expected 1 binding for kubectl, got %+v", bindings)
	}
}

func TestBindFile_ByPackage_VersionConstraintUnsatisfied_PKG002(t *testing.T) {
	ws := t.TempDir()
	pkgRoot := filepath.Join(ws, "vendor", "acme-incident-tools")
	writeAcmePackage(t, pkgRoot) // exports kubectl.tool.yaml with version: "1.0.0"

	cat, errs := Build(BuildOptions{
		WorkspaceRoot: ws,
		ProjectRequires: []*schema.PackageRequirement{
			{Package: "acme.incident-tools", Version: "^1.4.0", Path: "vendor/acme-incident-tools"},
		},
	})
	if len(errs) != 0 {
		t.Fatalf("unexpected Build errors: %v", errs)
	}

	runbookPath := filepath.Join(ws, "runbook.yaml")
	refs := []*schema.ToolRef{
		// Package pin resolves the tool unambiguously; the ref demands
		// ^2.0.0 but the resolved tool declares version 1.0.0.
		{Name: "kubectl", Package: "acme.incident-tools", Version: "^2.0.0"},
	}
	_, berrs := BindFile(cat, runbookPath, refs, "")
	if len(berrs) != 1 {
		t.Fatalf("expected exactly 1 bind error, got %v", berrs)
	}
	if c, ok := berrs[0].(interface{ Code() string }); !ok || c.Code() != "PKG-002" {
		t.Fatalf("expected PKG-002, got %v", berrs[0])
	}
}

func TestBindFile_ByPackage_VersionConstraintSatisfied(t *testing.T) {
	ws := t.TempDir()
	pkgRoot := filepath.Join(ws, "vendor", "acme-incident-tools")
	writeAcmePackage(t, pkgRoot) // exports kubectl.tool.yaml with version: "1.0.0"

	cat, errs := Build(BuildOptions{
		WorkspaceRoot: ws,
		ProjectRequires: []*schema.PackageRequirement{
			{Package: "acme.incident-tools", Version: "^1.4.0", Path: "vendor/acme-incident-tools"},
		},
	})
	if len(errs) != 0 {
		t.Fatalf("unexpected Build errors: %v", errs)
	}

	runbookPath := filepath.Join(ws, "runbook.yaml")
	refs := []*schema.ToolRef{
		{Name: "kubectl", Package: "acme.incident-tools", Version: "^1.0.0"},
	}
	bindings, berrs := BindFile(cat, runbookPath, refs, "")
	if len(berrs) != 0 {
		t.Fatalf("unexpected bind errors: %v", berrs)
	}
	if len(bindings) != 1 || bindings[0].Name != "kubectl" {
		t.Fatalf("expected 1 binding for kubectl, got %+v", bindings)
	}
}

func TestBindFile_UndeclaredCrossTierShadow_PKG022(t *testing.T) {
	ws := t.TempDir()
	pkgRoot := filepath.Join(ws, "vendor", "acme-incident-tools")
	writeAcmePackage(t, pkgRoot)
	writeFile(t, filepath.Join(ws, "tools", "kubectl.tool.yaml"), kubectlToolYAML)

	cat, errs := Build(BuildOptions{
		WorkspaceRoot: ws,
		ProjectRequires: []*schema.PackageRequirement{
			{Package: "acme.incident-tools", Version: "^1.4.0", Path: "vendor/acme-incident-tools"},
		},
	})
	if len(errs) != 0 {
		t.Fatalf("unexpected Build errors: %v", errs)
	}

	runbookPath := filepath.Join(ws, "runbook.yaml")
	// Bare name only, no package/path: ambiguous between tier1 and tier2.
	refs := []*schema.ToolRef{{Name: "kubectl"}}
	_, berrs := BindFile(cat, runbookPath, refs, "")
	if !hasCode(berrs, "PKG-022") {
		t.Fatalf("expected PKG-022, got %v", berrs)
	}
}

func TestBindFile_PKG029_PackageAndPathMutuallyExclusive(t *testing.T) {
	ws := t.TempDir()
	runbookPath := filepath.Join(ws, "runbook.yaml")
	refs := []*schema.ToolRef{
		{Name: "kubectl", Package: "acme.incident-tools", Path: "./tools/kubectl.tool.yaml"},
	}
	cat, _ := Build(BuildOptions{WorkspaceRoot: ws})
	_, berrs := BindFile(cat, runbookPath, refs, "")
	if !hasCode(berrs, "PKG-029") {
		t.Fatalf("expected PKG-029, got %v", berrs)
	}
}

func TestBindFile_PKG030_UnresolvedName(t *testing.T) {
	ws := t.TempDir()
	runbookPath := filepath.Join(ws, "runbook.yaml")
	refs := []*schema.ToolRef{{Name: "does-not-exist"}}
	cat, _ := Build(BuildOptions{WorkspaceRoot: ws})
	_, berrs := BindFile(cat, runbookPath, refs, "")
	if !hasCode(berrs, "PKG-030") {
		t.Fatalf("expected PKG-030, got %v", berrs)
	}
}

func TestBindFile_ByPath_Tier4(t *testing.T) {
	ws := t.TempDir()
	toolPath := filepath.Join(ws, "adhoc", "kubectl.tool.yaml")
	writeFile(t, toolPath, kubectlToolYAML)
	runbookPath := filepath.Join(ws, "runbook.yaml")
	refs := []*schema.ToolRef{{Name: "kubectl", Path: "./adhoc/kubectl.tool.yaml"}}
	cat, _ := Build(BuildOptions{WorkspaceRoot: ws})
	bindings, berrs := BindFile(cat, runbookPath, refs, "")
	if len(berrs) != 0 {
		t.Fatalf("unexpected errors: %v", berrs)
	}
	if len(bindings) != 1 || bindings[0].Def.Actions["drain"] == nil {
		t.Fatalf("expected kubectl binding with drain action, got %+v", bindings)
	}
}

// TestBindFile_ExternalToolRefsPath_PKGW003NotPKG007 exercises Barbara's
// binding ruling for the tier-4 toolRefs[].path case (TV-PKG-PATH-008's
// defect class): an ordinary top-level runbook's toolRefs[].path resolving
// outside the workspace root MUST still bind successfully and be reported
// as PKG-W003, never rejected as PKG-007.
func TestBindFile_ExternalToolRefsPath_PKGW003NotPKG007(t *testing.T) {
	container := t.TempDir()
	ws := filepath.Join(container, "workspace")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	toolPath := filepath.Join(container, "outside", "kubectl.tool.yaml")
	writeFile(t, toolPath, kubectlToolYAML)
	runbookPath := filepath.Join(ws, "runbook.yaml")
	refs := []*schema.ToolRef{{Name: "kubectl", Path: "../outside/kubectl.tool.yaml"}}

	cat, _ := Build(BuildOptions{WorkspaceRoot: ws})
	bindings, berrs := BindFile(cat, runbookPath, refs, "")
	fatal, warnings := errkit.SplitWarnings(berrs)
	if len(fatal) != 0 {
		t.Fatalf("expected no fatal errors, got %v", fatal)
	}
	if !hasCode(warnings, "PKG-W003") {
		t.Fatalf("expected PKG-W003 warning, got %v", warnings)
	}
	if hasCode(berrs, "PKG-007") {
		t.Fatalf("PKG-007 MUST NOT be raised for a workspace-level escape: %v", berrs)
	}
	if len(bindings) != 1 || bindings[0].Def.Actions["drain"] == nil {
		t.Fatalf("expected kubectl binding to still succeed, got %+v", bindings)
	}
}

func TestFileDigest_Deterministic(t *testing.T) {
	ws := t.TempDir()
	p := filepath.Join(ws, "a.txt")
	writeFile(t, p, "hello world")
	d1, err := FileDigest(p)
	if err != nil {
		t.Fatal(err)
	}
	d2, err := FileDigest(p)
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Fatalf("expected stable digest, got %s vs %s", d1, d2)
	}
	if len(d1) != len("sha256:")+64 {
		t.Fatalf("expected sha256:<64 hex>, got %s", d1)
	}
}

func hasCode(errs []error, code string) bool {
	for _, e := range errs {
		if c, ok := e.(interface{ Code() string }); ok && c.Code() == code {
			return true
		}
	}
	return false
}

func countCode(errs []error, code string) int {
	count := 0
	for _, e := range errs {
		if c, ok := e.(interface{ Code() string }); ok && c.Code() == code {
			count++
		}
	}
	return count
}

var _ = toolpkg.ToolDef{} // keep import used if test set shrinks
