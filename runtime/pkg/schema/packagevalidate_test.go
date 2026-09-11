package schema

import "testing"

func TestScanForbiddenPackageKeys_ToolPackages(t *testing.T) {
	doc := []byte(`
apiVersion: yawr.runbook/v1
id: r1
name: r1
toolPackages:
  - package: acme.incident-tools
flow: []
`)
	errs := ScanForbiddenPackageKeys(doc)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %v", len(errs), errs)
	}
	if c, ok := errs[0].(interface{ Code() string }); !ok || c.Code() != "PKG-020" {
		t.Fatalf("expected PKG-020, got %v", errs[0])
	}
}

func TestScanForbiddenPackageKeys_ToolRefsAlias(t *testing.T) {
	doc := []byte(`
apiVersion: yawr.runbook/v1
id: r1
name: r1
toolRefs:
  - name: kubectl
    alias: k
  - name: curl
flow: []
`)
	errs := ScanForbiddenPackageKeys(doc)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %v", len(errs), errs)
	}
	if c, ok := errs[0].(interface{ Code() string }); !ok || c.Code() != "PKG-021" {
		t.Fatalf("expected PKG-021, got %v", errs[0])
	}
}

func TestScanForbiddenPackageKeys_CleanDocument(t *testing.T) {
	doc := []byte(`
apiVersion: yawr.runbook/v1
id: r1
name: r1
requires:
  - package: acme.incident-tools
    version: "^1.4.0"
toolRefs:
  - name: kubectl
    package: acme.incident-tools
flow: []
`)
	errs := ScanForbiddenPackageKeys(doc)
	if len(errs) != 0 {
		t.Fatalf("expected no errors, got %v", errs)
	}
}
