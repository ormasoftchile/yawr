package extension

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadManifest_Valid(t *testing.T) {
	path := filepath.Join("testdata", "manifest-valid.yaml")
	manifest, err := readManifest(path)
	if err != nil {
		t.Fatalf("expected manifest to parse: %v", err)
	}
	if manifest.Name != "hello-ext" {
		t.Fatalf("expected name hello-ext")
	}
}

func TestReadManifestDirYawrManifest(t *testing.T) {
	root := t.TempDir()
	data := []byte("name: hello-ext\nversion: 0.1.0\nentrypoint: hello-ext\ncapabilities: []\ncompatibility:\n  yawr_min_version: \"0.2.0\"\n")
	if err := os.WriteFile(filepath.Join(root, manifestFilename), data, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, path, err := readManifestDir(root)
	if err != nil || manifest.Name != "hello-ext" || filepath.Base(path) != manifestFilename {
		t.Fatalf("readManifestDir() = %#v, %q, %v", manifest, path, err)
	}
}

func TestReadManifest_MissingName(t *testing.T) {
	path := filepath.Join("testdata", "manifest-missing-name.yaml")
	if _, err := readManifest(path); err == nil {
		t.Fatalf("expected error")
	}
}

func TestReadManifest_UnknownCapability(t *testing.T) {
	path := filepath.Join("testdata", "manifest-unknown-capability.yaml")
	if _, err := readManifest(path); err == nil {
		t.Fatalf("expected error")
	}
}

func TestReadManifest_IncompatibleVersion(t *testing.T) {
	path := filepath.Join("testdata", "manifest-incompatible-version.yaml")
	if _, err := readManifest(path); err == nil {
		t.Fatalf("expected error")
	}
}
