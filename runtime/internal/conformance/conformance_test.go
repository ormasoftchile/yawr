package conformance

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"
)

func testdataDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve caller path")
	}
	return filepath.Join(filepath.Dir(file), "testdata")
}

func TestLoadSuite(t *testing.T) {
	suite, err := LoadSuite(context.Background(), testdataDir(t))
	if err != nil {
		t.Fatalf("LoadSuite() error = %v", err)
	}
	if got, want := suite.Count(), 280; got != want {
		t.Fatalf("Suite.Count() = %d, want %d", got, want)
	}
	foundNullExpected := false
	for _, file := range suite.Files {
		for _, vector := range file.Vectors {
			if vector.Expected.HasValue && vector.Expected.PJVMValue != nil && vector.Expected.PJVMValue.Kind() == "null" {
				foundNullExpected = true
			}
		}
	}
	if !foundNullExpected {
		t.Fatal("expected at least one explicit value:null vector")
	}
}

func TestConformanceScaffold(t *testing.T) {
	suite, err := LoadSuite(context.Background(), testdataDir(t))
	if err != nil {
		t.Fatalf("LoadSuite() error = %v", err)
	}
	skipped := 0
	for _, file := range suite.Files {
		file := file
		t.Run(file.Name, func(t *testing.T) {
			for _, vector := range file.Vectors {
				vector := vector
				t.Run(vector.ID, func(t *testing.T) {
					result := runnerFor(vector).Run(context.Background(), vector)
					switch result.Verdict {
					case VerdictSkip:
						if result.Reason == "" {
							t.Fatalf("%s skipped without an explicit reason", vector.ID)
						}
						skipped++
						t.Skipf("%s: %s", vector.ID, result.Reason)
					case VerdictPass:
						return
					case VerdictFail:
						t.Fatalf("%s failed: %v", vector.ID, result.Err)
					default:
						t.Fatalf("%s returned unknown verdict %q", vector.ID, result.Verdict)
					}
				})
			}
		})
	}
	t.Cleanup(func() {
		if skipped != 0 {
			t.Errorf("skipped %d vectors, want 0", skipped)
		}
		t.Logf("%d skipped (all P7 capture-service vectors execute)", skipped)
	})
}

func runnerFor(v Vector) Runner {
	switch v.SourceFile {
	case "tv-gxl-eval.yaml":
		return EvalRunner{}
	case "tv-gxl-parse.yaml":
		return ParseRunner{}
	case "tv-gxl-path.yaml":
		return PathRunner{}
	case "tv-gis-path.yaml":
		return GISRunner{}
	case "tv-gcp-path.yaml":
		return GCPRunner{}
	default:
		panic(fmt.Sprintf("no runner for %s", v.SourceFile))
	}
}
