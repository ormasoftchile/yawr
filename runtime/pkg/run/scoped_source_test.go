package run

import (
	"io/fs"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func TestEmbeddedCatalogSourceNeverFallsBackToHost(t *testing.T) {
	root := t.TempDir()
	source := embeddedCatalogSource(root, fstest.MapFS{
		"nested/child.runbook.yaml": {Data: []byte("captured child")},
	})
	data, err := source.ReadFile(filepath.Join(root, "nested", "child.runbook.yaml"))
	if err != nil || string(data) != "captured child" {
		t.Fatalf("embedded read = %q, %v", data, err)
	}
	if _, err := source.Stat(filepath.Join(root, "nested")); err != nil {
		t.Fatal(err)
	}
	var visited []string
	if err := source.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			visited = append(visited, path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(visited) != 1 || visited[0] != filepath.Join(root, "nested", "child.runbook.yaml") {
		t.Fatalf("walk = %v", visited)
	}
	outside := filepath.Join(filepath.Dir(root), "outside.yaml")
	if _, err := source.ReadFile(outside); err == nil {
		t.Fatal("read escaped embedded root")
	}
	if _, err := source.Stat(outside); err == nil {
		t.Fatal("stat escaped embedded root")
	}
	if err := source.WalkDir(outside, func(string, fs.DirEntry, error) error { return nil }); err == nil {
		t.Fatal("walk escaped embedded root")
	}
}
