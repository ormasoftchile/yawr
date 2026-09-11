package conformance

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"
)

func enumDataDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve caller path")
	}
	return filepath.Join(filepath.Dir(file), "enumdata")
}

// TestLoadEnumSuite pins the frozen corpus size
// (58 declaration vectors plus 13 governance vectors
// tool-governance vectors + 1 GOV-014 classification-validation vector
// = 72 total)
// and that the schema-validated decode succeeds.
func TestLoadEnumSuite(t *testing.T) {
	vectors, err := LoadEnumSuite(context.Background(), enumDataDir(t))
	if err != nil {
		t.Fatalf("LoadEnumSuite() error = %v", err)
	}
	if got, want := len(vectors), 72; got != want {
		t.Fatalf("len(vectors) = %d, want %d", got, want)
	}
}

// TestEnumConformance is the authoritative R2 vector execution report: it
// runs every tv-enum.yaml vector through the real yawr CLI (or, for the
// three ENUM-SUBST catalog vectors, the identical pkgcatalog.Build path
// the CLI uses) and requires an explicit, named reason for every skip
// (barbara-enum-mvp-implementation-gate.md R2: "explicitly map any
// design-level/non-executable vectors rather than ignoring them").
func TestEnumConformance(t *testing.T) {
	vectors, err := LoadEnumSuite(context.Background(), enumDataDir(t))
	if err != nil {
		t.Fatalf("LoadEnumSuite() error = %v", err)
	}
	harness := NewEnumHarness(t)

	passed, skipped, failed := 0, 0, 0
	for _, v := range vectors {
		v := v
		t.Run(v.ID, func(t *testing.T) {
			result := harness.Run(context.Background(), t, v)
			switch result.Verdict {
			case VerdictSkip:
				if result.Reason == "" {
					t.Fatalf("%s skipped without an explicit reason", v.ID)
				}
				skipped++
				t.Skipf("%s: %s", v.ID, result.Reason)
			case VerdictPass:
				passed++
			case VerdictFail:
				failed++
				t.Fatalf("%s failed: %v", v.ID, result.Err)
			default:
				failed++
				t.Fatalf("%s returned unknown verdict %q", v.ID, result.Verdict)
			}
		})
	}
	t.Cleanup(func() {
		t.Logf("TV-ENUM conformance report: %d vectors total, %d passed, %d skipped (named reasons), %d failed",
			len(vectors), passed, skipped, failed)
	})
}
