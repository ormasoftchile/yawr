package adapter

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// writeAdapterFile is a small local helper (mirrors pkgcatalog's test
// helper) so this file has no cross-package test dependency.
func writeAdapterFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const adapterKubectlToolYAML = `apiVersion: yawr.tool/v1
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

func adapterWriteAcmePackage(t *testing.T, root string) {
	writeAdapterFile(t, filepath.Join(root, "yawr-package.yaml"), `apiVersion: yawr.tool-package/v1
meta:
  name: acme.incident-tools
  version: "1.4.2"
exports:
  tools:
    - id: kubectl
      path: tools/kubectl.tool.yaml
`)
	writeAdapterFile(t, filepath.Join(root, "tools", "kubectl.tool.yaml"), adapterKubectlToolYAML)
}

// TestBuildPackageCatalogAndResolveToolRefsViaCatalog exercises the new
// integration surface end-to-end: build a workspace catalog once, then
// bind a runbook file's toolRefs against it via the package-pin path.
func TestBuildPackageCatalogAndResolveToolRefsViaCatalog(t *testing.T) {
	ws := t.TempDir()
	pkgRoot := filepath.Join(ws, "vendor", "acme-incident-tools")
	adapterWriteAcmePackage(t, pkgRoot)

	opts := PackageCatalogOptions{
		WorkspaceRoot: ws,
		ProjectRequires: []*schema.PackageRequirement{
			{Package: "acme.incident-tools", Version: "^1.4.0", Path: "vendor/acme-incident-tools"},
		},
	}
	runbookPath := filepath.Join(ws, "runbook.yaml")
	cat, errs := BuildPackageCatalog(opts, runbookPath, nil)
	if len(errs) != 0 {
		t.Fatalf("unexpected BuildPackageCatalog errors: %v", errs)
	}

	refs := []*schema.ToolRef{
		{Name: "kubectl", Package: "acme.incident-tools", Version: "^1.0.0"},
	}
	defs, berrs := ResolveToolRefsViaCatalog(cat, runbookPath, refs)
	if len(berrs) != 0 {
		t.Fatalf("unexpected ResolveToolRefsViaCatalog errors: %v", berrs)
	}
	if len(defs) != 1 || defs[0].Name != "kubectl" {
		t.Fatalf("expected 1 def named kubectl, got %+v", defs)
	}
}

// TestResolveToolRefsViaCatalog_EmptyRefs confirms the nil-refs short
// circuit (matching ResolveToolRefs's existing behaviour).
func TestResolveToolRefsViaCatalog_EmptyRefs(t *testing.T) {
	defs, errs := ResolveToolRefsViaCatalog(nil, "irrelevant.yaml", nil)
	if defs != nil || errs != nil {
		t.Fatalf("expected nil, nil for empty refs; got %v, %v", defs, errs)
	}
}

func TestResolveToolRefsViaCatalog_WarningsDoNotDiscardBindings(t *testing.T) {
	ws := t.TempDir()
	container := filepath.Dir(ws)
	toolPath := filepath.Join(container, "kubectl.tool.yaml")
	writeAdapterFile(t, toolPath, adapterKubectlToolYAML)
	runbookPath := filepath.Join(ws, "runbook.yaml")
	refs := []*schema.ToolRef{{
		Name: "kubectl",
		Path: "../kubectl.tool.yaml",
	}}

	opts := PackageCatalogOptions{WorkspaceRoot: ws}
	cat, catErrs := BuildPackageCatalog(opts, runbookPath, nil)
	if len(catErrs) != 0 {
		t.Fatalf("unexpected BuildPackageCatalog errors: %v", catErrs)
	}

	defs, errs := ResolveToolRefsViaCatalog(cat, runbookPath, refs)
	if len(defs) != 1 || defs[0].Name != "kubectl" {
		t.Fatalf("expected the binding to succeed despite the external-root warning, got defs=%+v errs=%v", defs, errs)
	}
	if len(errs) == 0 {
		t.Fatal("expected the external-root warning to still be surfaced, got none")
	}
}
