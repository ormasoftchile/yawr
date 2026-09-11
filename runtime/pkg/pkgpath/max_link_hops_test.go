package pkgpath

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestResolveLinks_HopLimitExceeded_PKG008 is a regression test for §3.7 of
// maxLinkHops = 8 must be enforced;
// link-chain bounding relied entirely on the host OS's own ELOOP ceiling,
// which is unspecified and typically much larger than 8. Build a chain of
// maxLinkHops+2 symlinks, each pointing at the next, and confirm resolution
// fails PKG-008 rather than succeeding (or surfacing only the OS's own,
// much-later ELOOP).
func TestResolveLinks_HopLimitExceeded_PKG008(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevated privileges on Windows CI")
	}
	dir := t.TempDir()
	final := filepath.Join(dir, "final.txt")
	if err := os.WriteFile(final, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	chainLen := maxLinkHops + 2
	prev := final
	var head string
	for i := 0; i < chainLen; i++ {
		link := filepath.Join(dir, "link"+string(rune('a'+i))+".lnk")
		if err := os.Symlink(prev, link); err != nil {
			t.Skipf("symlink not supported in this environment: %v", err)
		}
		prev = link
		head = link
	}

	_, err := resolveLinks(head)
	if err == nil {
		t.Fatal("expected PKG-008 for a symlink chain exceeding maxLinkHops")
	}
	if c := errCode(err); c != "PKG-008" {
		t.Fatalf("expected PKG-008, got %s (%v)", c, err)
	}
}

// TestResolveLinks_HopLimitNotExceeded_OK confirms a chain at or under
// maxLinkHops still resolves successfully (no false positive).
func TestResolveLinks_HopLimitNotExceeded_OK(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevated privileges on Windows CI")
	}
	dir := t.TempDir()
	final := filepath.Join(dir, "final.txt")
	if err := os.WriteFile(final, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	prev := final
	var head string
	for i := 0; i < maxLinkHops; i++ {
		link := filepath.Join(dir, "ok"+string(rune('a'+i))+".lnk")
		if err := os.Symlink(prev, link); err != nil {
			t.Skipf("symlink not supported in this environment: %v", err)
		}
		prev = link
		head = link
	}

	resolved, err := resolveLinks(head)
	if err != nil {
		t.Fatalf("unexpected error for a chain at the hop limit: %v", err)
	}
	if resolved == "" {
		t.Fatal("expected a resolved path")
	}
}
