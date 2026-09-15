package pkgcatalog

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProjectBindingsUseSuppliedSourceAndOverrideOnlyProject(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		filepath.Join(root, ".yawr", "config.yaml"): "apiVersion: yawr.config/v1\nrequires:\n  - {package: query, version: '^1.0.0', path: real}\ntool-paths: [shared]\n",
		filepath.Join(root, "map.yaml"):             "apiVersion: yawr.config/v1\nrequires:\n  - {package: query, version: '^2.0.0', path: mock}\ntool-paths: [mock-tools]\n",
	}
	source := &Source{ReadFile: func(path string) ([]byte, error) {
		value, ok := files[path]
		if !ok {
			return nil, os.ErrNotExist
		}
		return []byte(value), nil
	}}
	config, origins, err := ReadProjectBindings(source, root, "map.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Requires) != 1 || config.Requires[0].Path != "mock" ||
		config.Requires[0].Version != "^2.0.0" || len(origins) != 1 ||
		len(config.ToolPaths) != 2 || config.ToolPaths[0] != "shared" || config.ToolPaths[1] != "mock-tools" {
		t.Fatalf("bindings = %+v origins = %+v", config, origins)
	}
	delete(files, filepath.Join(root, ".yawr", "config.yaml"))
	if _, _, err := ReadProjectBindings(source, root, ""); err != nil {
		t.Fatalf("optional project config: %v", err)
	}
	if _, _, err := ReadProjectBindings(source, root, "missing.yaml"); err == nil {
		t.Fatal("explicit missing package map was ignored")
	}
	files[filepath.Join(root, ".yawr", "config.yaml")] = "apiVersion: unsupported"
	if _, _, err := ReadProjectBindings(source, root, ""); err == nil {
		t.Fatal("malformed project configuration was ignored")
	}
}
