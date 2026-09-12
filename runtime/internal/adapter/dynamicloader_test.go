package adapter

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	internalparser "github.com/ormasoftchile/yawr/runtime/internal/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/pkgcatalog"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// buildTestCatalog creates a minimal workspace with one package that exports
// one runbook and returns the frozen catalog.
func buildTestCatalog(t *testing.T, runbookFileExists bool) *pkgcatalog.Catalog {
	t.Helper()
	ws := t.TempDir()
	pkgRoot := filepath.Join(ws, "test-pkg")
	if err := os.MkdirAll(filepath.Join(pkgRoot, "runbooks"), 0o755); err != nil {
		t.Fatal(err)
	}

	const manifest = `apiVersion: yawr.tool-package/v1
meta:
  name: test-pkg
  version: "1.0.0"
exports:
  runbooks:
    - id: tsg-disk
      path: runbooks/tsg-disk.runbook.yaml
`
	const runbookYAML = `apiVersion: yawr.runbook/v1
id: tsg-disk
name: TSG Disk Pressure
flow: []
`
	if err := os.WriteFile(filepath.Join(pkgRoot, "yawr-package.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	// Always create the runbook file for catalog building (catalog Build validates the path).
	rbPath := filepath.Join(pkgRoot, "runbooks", "tsg-disk.runbook.yaml")
	if err := os.WriteFile(rbPath, []byte(runbookYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	catalog, errs := pkgcatalog.Build(pkgcatalog.BuildOptions{
		WorkspaceRoot: ws,
		ProjectRequires: []*schema.PackageRequirement{
			{Package: "test-pkg", Version: "1.0.0", Path: "test-pkg"},
		},
	})
	if len(errs) > 0 {
		t.Fatalf("catalog build errors: %v", errs)
	}

	if !runbookFileExists {
		// Remove the file AFTER catalog construction so the resolver sees a missing file.
		os.Remove(rbPath)
	}
	return catalog
}

func TestDynamicLoader_EmptyRef_DINC002(t *testing.T) {
	catalog := &pkgcatalog.Catalog{}
	resolver := NewCatalogIncludeResolver(catalog, nil)
	_, err := resolver.Resolve(context.Background(), "")
	if err == nil {
		t.Fatal("expected DINC-002 for empty ref")
	}
	assertDyncLoaderCode(t, err, "DINC-002")
}

func TestDynamicLoader_InvalidRef_DINC001(t *testing.T) {
	catalog := &pkgcatalog.Catalog{}
	resolver := NewCatalogIncludeResolver(catalog, nil)
	// Path-like refs are invalid.
	_, err := resolver.Resolve(context.Background(), "../../../etc/passwd")
	if err == nil {
		t.Fatal("expected DINC-001 for path-like ref")
	}
	assertDyncLoaderCode(t, err, "DINC-001")
}

func TestDynamicLoader_QualifiedNotFound_DINC002(t *testing.T) {
	// Empty catalog: no runbooks at all.
	catalog, errs := pkgcatalog.Build(pkgcatalog.BuildOptions{
		WorkspaceRoot: t.TempDir(),
	})
	if len(errs) > 0 {
		t.Fatalf("catalog build errors: %v", errs)
	}
	resolver := NewCatalogIncludeResolver(catalog, nil)
	_, err := resolver.Resolve(context.Background(), "missing-pkg/tsg-disk")
	if err == nil {
		t.Fatal("expected DINC-002 for qualified ref not in catalog")
	}
	assertDyncLoaderCode(t, err, "DINC-002")
}

func TestDynamicLoader_BareNotFound_DINC002(t *testing.T) {
	catalog, errs := pkgcatalog.Build(pkgcatalog.BuildOptions{
		WorkspaceRoot: t.TempDir(),
	})
	if len(errs) > 0 {
		t.Fatalf("catalog build errors: %v", errs)
	}
	resolver := NewCatalogIncludeResolver(catalog, nil)
	_, err := resolver.Resolve(context.Background(), "tsg-disk")
	if err == nil {
		t.Fatal("expected DINC-002 for bare ref not in catalog")
	}
	assertDyncLoaderCode(t, err, "DINC-002")
}

func TestDynamicLoader_FileMissing_DINC013(t *testing.T) {
	catalog := buildTestCatalog(t, false) // runbook file deleted after catalog build
	resolver := NewCatalogIncludeResolver(catalog, nil)
	_, err := resolver.Resolve(context.Background(), "test-pkg/tsg-disk")
	if err == nil {
		t.Fatal("expected DINC-013 for missing file after catalog lookup")
	}
	assertDyncLoaderCode(t, err, "DINC-013")
}

func TestDynamicLoader_RejectsBytesChangedAfterCatalogFreeze(t *testing.T) {
	catalog := buildTestCatalog(t, true)
	entry, ok := catalog.RunbookByQualified("test-pkg/tsg-disk")
	if !ok {
		t.Fatal("frozen catalog entry is missing")
	}
	changed := []byte(`apiVersion: yawr.runbook/v1
id: tsg-disk
name: Changed
flow:
  - step:
      id: changed-child
      type: noop
`)
	if err := os.WriteFile(entry.Path, changed, 0o644); err != nil {
		t.Fatalf("rewrite child: %v", err)
	}
	parserImpl, err := internalparser.New(platform.Real())
	if err != nil {
		t.Fatalf("parser: %v", err)
	}
	resolver := NewCatalogIncludeResolver(catalog, parserImpl)
	if _, err := resolver.Resolve(context.Background(), "test-pkg/tsg-disk"); err == nil {
		t.Fatal("resolver accepted bytes changed after catalog freeze")
	} else {
		assertDyncLoaderCode(t, err, "DINC-013")
	}
}

func assertDyncLoaderCode(t *testing.T, err error, expected string) {
	t.Helper()
	c, ok := err.(errkit.Coder)
	if !ok {
		t.Fatalf("expected errkit.Coder, got %T: %v", err, err)
	}
	if c.Code() != expected {
		t.Fatalf("expected code %s, got %s: %v", expected, c.Code(), err)
	}
}
