package pjvm_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/perf"
	"github.com/ormasoftchile/yawr/runtime/pkg/pjvm"
)

func BenchmarkEnginePJVMConversion(b *testing.B) {
	raw := map[string]any{
		"service": map[string]any{
			"name":    "api",
			"ok":      true,
			"latency": 87.5,
			"checks":  []any{"dns", "ping", "tls"},
		},
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := pjvm.FromAny(raw); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEngineFullFixtureRun(b *testing.B) {
	repoRoot, err := perf.RepoRoot()
	if err != nil {
		b.Fatal(err)
	}
	fixtures := perf.FixturePaths(repoRoot)
	ctx := context.Background()
	b.ReportAllocs()
	for _, fixture := range fixtures {
		name := filepath.Base(fixture)
		b.Run(name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if _, err := perf.PlanFixture(ctx, repoRoot, fixture); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
