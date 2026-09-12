package conformance

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCompositionValuesConformance(t *testing.T) {
	dir := testdataDir(t)
	schema, err := compileSchema(filepath.Join(filepath.Dir(dir), "vector.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "tv-composition-values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateYAML(schema, data); err != nil {
		t.Fatal(err)
	}
	file, err := decodeVectorFile("tv-composition-values.yaml", data)
	if err != nil || len(file.Vectors) != 9 {
		t.Fatalf("composition corpus: %d vectors, error %v", len(file.Vectors), err)
	}
	for _, vector := range file.Vectors {
		t.Run(vector.ID, func(t *testing.T) {
			var runner Runner = EvalRunner{}
			if vector.Category == "GXL-PARSE" {
				runner = ParseRunner{}
			}
			result := runner.Run(context.Background(), vector)
			if result.Verdict != VerdictPass {
				t.Fatalf("composition conformance: %#v", result)
			}
		})
	}
}

// An optional authoring check uses the existing schema validator for the
// complete design corpus, including multi-document files.
func TestDesignCorpusSchema(t *testing.T) {
	dir := os.Getenv("YAWR_DESIGN_CORPUS")
	if dir == "" {
		t.Skip("YAWR_DESIGN_CORPUS is not configured")
	}
	schema, err := compileSchema(filepath.Join(dir, "vector.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(dir, "tv-*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("design corpus files: %v", err)
	}
	count := 0
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		for {
			var doc struct {
				Vectors []any `yaml:"vectors"`
			}
			err := decoder.Decode(&doc)
			if err == io.EOF {
				break
			}
			if err != nil || len(doc.Vectors) == 0 {
				t.Fatalf("%s: missing/invalid vectors: %v", path, err)
			}
			for _, vector := range doc.Vectors {
				encoded, err := yaml.Marshal(map[string]any{"vectors": []any{vector}})
				if err != nil {
					t.Fatal(err)
				}
				if err := validateYAML(schema, encoded); err != nil {
					t.Fatalf("%s: %v", path, err)
				}
				count++
			}
		}
	}
	t.Logf("OK %d vectors validate across %d design corpus files", count, len(files))
}
