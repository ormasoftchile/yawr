package adapter

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	internalparser "github.com/ormasoftchile/yawr/runtime/internal/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/pkgcatalog"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"golang.org/x/sys/windows"
)

func TestScopedLoaderCapturedShortPathOutsideEntrypointDirectory(t *testing.T) {
	for _, outsideWorkspace := range []bool{false, true} {
		t.Run(map[bool]string{false: "workspace", true: "outside-workspace"}[outsideWorkspace], func(t *testing.T) {
			testScopedLoaderCapturedShortPath(t, outsideWorkspace)
		})
	}
}

func testScopedLoaderCapturedShortPath(t *testing.T, outsideWorkspace bool) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(root, "entry", "root.runbook.yaml")
	child := filepath.Join(root, "shared", "child.runbook.yaml")
	scopedWrite(t, entry, scopedRunbook("root", "flow:\n  - step: {id: child, type: include, include: {runbook: ../shared/child.runbook.yaml}}\n"))
	scopedWrite(t, child, scopedRunbook("child", "flow:\n  - step: {id: captured, type: noop}\n"))
	ptr, err := windows.UTF16PtrFromString(root)
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]uint16, 32768)
	length, err := windows.GetShortPathName(ptr, &buffer[0], uint32(len(buffer)))
	if err != nil || length >= uint32(len(buffer)) {
		t.Fatalf("GetShortPathName: length=%d error=%v", length, err)
	}
	shortRoot := windows.UTF16ToString(buffer[:length])
	if scopedPathKey(shortRoot) == scopedPathKey(root) {
		t.Skip("the test filesystem does not expose a distinct Windows short path")
	}
	p, err := internalparser.New(platform.Real())
	if err != nil {
		t.Fatal(err)
	}
	workspace := shortRoot
	if outsideWorkspace {
		workspace = t.TempDir()
	}
	prepared, err := PrepareScopedRun(context.Background(), ScopedRunOptions{
		Catalog: pkgcatalog.BuildOptions{WorkspaceRoot: workspace}, Parser: p,
		Entrypoint: filepath.Join(shortRoot, "entry", "root.runbook.yaml"),
	})
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := prepared.Loader.Load(context.Background(), child)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(child); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(shortRoot, "shared", "child.runbook.yaml")
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "inferred-owner", true: "explicit-owner"}[explicit], func(t *testing.T) {
			loaded := canonical
			var err error
			if explicit {
				loaded, err = prepared.Loader.LoadScope(context.Background(), alias, canonical.Runbook.LexicalScopeID)
			} else {
				loaded, err = prepared.Loader.Load(context.Background(), alias)
			}
			if err != nil {
				t.Fatalf("captured short-path alias %q: %v", alias, err)
			}
			if loaded.Runbook.LexicalScopeID != canonical.Runbook.LexicalScopeID || loaded.Source != alias {
				t.Fatalf("short-path ownership or display source changed: %+v", loaded)
			}
		})
	}
}
