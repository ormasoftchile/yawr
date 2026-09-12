package conformance

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/pkgcatalog"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"gopkg.in/yaml.v3"
)

type packageVector struct {
	ID        string         `yaml:"id"`
	Input     string         `yaml:"input"`
	Variables map[string]any `yaml:"variables"`
}

func loadPackageVectors(t *testing.T) (string, []byte, []packageVector) {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test path")
	}
	dir := filepath.Join(filepath.Dir(file), "pkgdata")
	data, err := os.ReadFile(filepath.Join(dir, "tv-pkg-resolve.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		Vectors []packageVector `yaml:"vectors"`
	}
	if err := yaml.Unmarshal(data, &corpus); err != nil {
		t.Fatal(err)
	}
	return file, data, corpus.Vectors
}

func TestPackageResolutionVectorHarnessLoadsCorpus(t *testing.T) {
	file, data, vectors := loadPackageVectors(t)
	schema, err := compileSchema(filepath.Join(filepath.Dir(file), "vector.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateYAML(schema, data); err != nil {
		t.Fatalf("package vector schema validation: %v", err)
	}
	if got, want := len(vectors), 82; got != want {
		t.Fatalf("package vector count = %d, want %d", got, want)
	}
}

func TestPackageResolutionVectorExecutableEvidence(t *testing.T) {
	_, _, vectors := loadPackageVectors(t)
	var selected *packageVector
	for i := range vectors {
		if vectors[i].ID == "TV-PKG-VERSION-001" {
			selected = &vectors[i]
			break
		}
	}
	if selected == nil {
		t.Fatal("TV-PKG-VERSION-001 not found")
	}
	root := t.TempDir()
	for name, value := range selected.Variables {
		content, ok := value.(string)
		if !ok {
			continue
		}
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runbookPath := filepath.Join(root, filepath.FromSlash(selected.Input))
	runbookData, err := os.ReadFile(runbookPath)
	if err != nil {
		t.Fatal(err)
	}
	var runbook struct {
		Requires []*schema.PackageRequirement `yaml:"requires"`
	}
	if err := yaml.Unmarshal(runbookData, &runbook); err != nil {
		t.Fatal(err)
	}
	catalog, errs := pkgcatalog.Build(pkgcatalog.BuildOptions{
		WorkspaceRoot:   root,
		RunbookPath:     runbookPath,
		RunbookRequires: runbook.Requires,
	})
	if len(errs) != 0 {
		t.Fatalf("%s failed: %v", selected.ID, errs)
	}
	entry, ok := catalog.ByQualified("acme.incident-tools/kubectl")
	if !ok || entry.Tier != pkgcatalog.TierPackage || entry.Version != "1.0.0" {
		t.Fatalf("%s projection mismatch: %#v", selected.ID, entry)
	}
}
