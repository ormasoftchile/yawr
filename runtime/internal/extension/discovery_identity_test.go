package extension

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverYawrDirectoryAndDuplicateRules(t *testing.T) {
	manifest := func(name, entrypoint string) string {
		return "name: " + name + "\nversion: 0.2.0\nentrypoint: " + entrypoint + "\ncapabilities: []\n"
	}
	t.Run("discovers yawr extensions", func(t *testing.T) {
		root := t.TempDir()
		writeExtensionManifest(t, filepath.Join(root, ".yawr", "extensions", "one"), manifestFilename, manifest("one", "one.exe"))
		writeExtensionManifest(t, filepath.Join(root, ".yawr", "extensions", "two"), manifestFilename, manifest("two", "two.exe"))
		decls, err := discover(context.Background(), nil, root)
		if err != nil {
			t.Fatal(err)
		}
		if len(decls) != 2 || decls[0].Name != "one" || decls[1].Name != "two" {
			t.Fatalf("declarations = %#v", decls)
		}
	})
	t.Run("identical duplicate dedupes", func(t *testing.T) {
		root := t.TempDir()
		content := manifest("same", "same.exe")
		writeExtensionManifest(t, filepath.Join(root, ".yawr", "extensions", "one"), manifestFilename, content)
		writeExtensionManifest(t, filepath.Join(root, ".yawr", "extensions", "two"), manifestFilename, content)
		decls, err := discover(context.Background(), nil, root)
		if err != nil || len(decls) != 1 {
			t.Fatalf("discover() = %#v, %v", decls, err)
		}
	})
	t.Run("different duplicate fails", func(t *testing.T) {
		root := t.TempDir()
		writeExtensionManifest(t, filepath.Join(root, ".yawr", "extensions", "one"), manifestFilename, manifest("same", "one.exe"))
		writeExtensionManifest(t, filepath.Join(root, ".yawr", "extensions", "two"), manifestFilename, manifest("same", "two.exe"))
		if _, err := discover(context.Background(), nil, root); err == nil {
			t.Fatal("expected duplicate conflict")
		}
	})
}

func writeExtensionManifest(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
