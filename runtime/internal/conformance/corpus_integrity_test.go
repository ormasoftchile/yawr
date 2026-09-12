package conformance

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestConsolidatedCorpusStableIDCollisions(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test path")
	}
	root := filepath.Dir(file)
	paths := []string{
		filepath.Join(root, "testdata", "tv-gxl-eval.yaml"),
		filepath.Join(root, "testdata", "tv-gxl-parse.yaml"),
		filepath.Join(root, "testdata", "tv-gxl-path.yaml"),
		filepath.Join(root, "testdata", "tv-gis-path.yaml"),
		filepath.Join(root, "testdata", "tv-gcp-path.yaml"),
		filepath.Join(root, "enumdata", "tv-enum.yaml"),
		filepath.Join(root, "pkgdata", "tv-pkg-resolve.yaml"),
	}
	seen := map[string]any{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var file struct {
			Vectors []map[string]any `yaml:"vectors"`
		}
		if err := yaml.Unmarshal(data, &file); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		for _, vector := range file.Vectors {
			id, _ := vector["id"].(string)
			if id == "" {
				t.Fatalf("%s contains a vector without an id", path)
			}
			if prior, exists := seen[id]; exists && !reflect.DeepEqual(prior, vector) {
				t.Fatalf("stable case ID %s has differing definitions", id)
			}
			seen[id] = vector
		}
	}
}
