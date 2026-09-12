package pkgpath

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestValidateAuthoredSyntax_Backslash(t *testing.T) {
	if err := ValidateAuthoredSyntax(`tools\kubectl.tool.yaml`, true); err == nil {
		t.Fatal("expected PKG-007 for backslash separator")
	} else if c := errCode(err); c != "PKG-007" {
		t.Fatalf("expected PKG-007, got %s", c)
	}
}

func TestValidateAuthoredSyntax_AbsoluteRejectedForPackageInternal(t *testing.T) {
	if err := ValidateAuthoredSyntax("/etc/passwd", false); err == nil {
		t.Fatal("expected PKG-007 for absolute path in package-internal context")
	}
}

func TestValidateAuthoredSyntax_AbsoluteAllowedForWorkspaceLevel(t *testing.T) {
	if err := ValidateAuthoredSyntax("/vendor/acme", true); err != nil {
		t.Fatalf("unexpected error for workspace-level absolute path: %v", err)
	}
}

func TestValidateAuthoredSyntax_UNCRejected(t *testing.T) {
	if err := ValidateAuthoredSyntax(`\\host\share`, true); err == nil {
		t.Fatal("expected rejection of UNC-shaped path")
	}
}

func TestValidateAuthoredSyntax_ReservedDOSName(t *testing.T) {
	cases := []string{"CON", "con.tool.yaml", "COM1", "lpt9.txt"}
	for _, c := range cases {
		if err := ValidateAuthoredSyntax("tools/"+c, true); err == nil {
			t.Errorf("expected PKG-018 for reserved name %q", c)
		} else if code := errCode(err); code != "PKG-018" {
			t.Errorf("%q: expected PKG-018, got %s", c, code)
		}
	}
}

func TestValidateAuthoredSyntax_TrailingDotOrSpace(t *testing.T) {
	if err := ValidateAuthoredSyntax("tools/foo. ", true); err == nil {
		t.Fatal("expected PKG-018 for trailing space")
	}
	if err := ValidateAuthoredSyntax("tools/foo.", true); err == nil {
		t.Fatal("expected PKG-018 for trailing dot")
	}
}

func TestValidateAuthoredSyntax_TildeAndEnvExpansion(t *testing.T) {
	if err := ValidateAuthoredSyntax("~/tools/foo.tool.yaml", true); err == nil {
		t.Fatal("expected rejection of ~ expansion")
	}
	if err := ValidateAuthoredSyntax("$HOME/tools/foo.tool.yaml", true); err == nil {
		t.Fatal("expected rejection of env var expansion")
	}
}

func TestValidateAuthoredSyntax_ValidRelative(t *testing.T) {
	if err := ValidateAuthoredSyntax("tools/kubectl.tool.yaml", false); err != nil {
		t.Fatalf("unexpected error for valid relative path: %v", err)
	}
	if err := ValidateAuthoredSyntax("../runbooks/drain-node.yaml", false); err != nil {
		t.Fatalf("unexpected error for valid relative parent path: %v", err)
	}
}

func TestResolve_ContainmentOK(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "tools")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(sub, "kubectl.tool.yaml")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	resolved, external, err := Resolve(sub, "kubectl.tool.yaml", root, PackageInternal)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if external {
		t.Fatal("expected contained (not external)")
	}
	want, _ := filepath.EvalSymlinks(target)
	if resolved != want {
		t.Fatalf("resolved = %q, want %q", resolved, want)
	}
}

func TestResolve_EscapeRejectedForPackageInternal(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "tools")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir() // a different temp dir, definitely outside root
	_ = outside
	_, _, err := Resolve(sub, "../../etc/passwd", root, PackageInternal)
	if err == nil {
		t.Fatal("expected PKG-007 for escaping package-internal reference")
	}
	if c := errCode(err); c != "PKG-007" {
		t.Fatalf("expected PKG-007, got %s", c)
	}
}

func TestResolve_EscapeReportedExternalForWorkspaceLevel(t *testing.T) {
	root := t.TempDir()
	outsideDir := t.TempDir()
	target := filepath.Join(outsideDir, "vendor-tool")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		t.Fatal(err)
	}
	resolved, external, err := Resolve(root, filepath.ToSlash(rel), root, WorkspaceLevel)
	if err != nil {
		t.Fatalf("workspace-level escape must not be a hard error: %v", err)
	}
	if !external {
		t.Fatal("expected external=true for workspace-level path resolving outside root")
	}
	if resolved == "" {
		t.Fatal("expected a resolved path")
	}
}

func TestResolve_SymlinkEscapeRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevated privileges on Windows CI")
	}
	root := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "secret.tool.yaml")
	if err := os.WriteFile(outsideFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "linked.tool.yaml")
	if err := os.Symlink(outsideFile, link); err != nil {
		t.Skipf("symlink not supported in this environment: %v", err)
	}
	_, _, err := Resolve(root, "linked.tool.yaml", root, PackageInternal)
	if err == nil {
		t.Fatal("expected PKG-007 for symlink escaping package root")
	}
}

func TestNormalizeForComparison_CaseAndNFC(t *testing.T) {
	a := NormalizeForComparison("Tools/Foo.yaml", true)
	b := NormalizeForComparison("tools/foo.yaml", true)
	if a != b {
		t.Fatalf("expected case-insensitive normalization to match: %q vs %q", a, b)
	}
}

func errCode(err error) string {
	if c, ok := err.(interface{ Code() string }); ok {
		return c.Code()
	}
	return ""
}
