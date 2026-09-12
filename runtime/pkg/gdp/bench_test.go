package gdp

import (
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/pjvm"
)

func BenchmarkEngineGCPPathResolutionGDP(b *testing.B) {
	root, err := pjvm.FromAny(map[string]any{
		"service": map[string]any{
			"name": "api",
			"checks": []any{
				map[string]any{"name": "dns", "ok": true},
				map[string]any{"name": "ping", "ok": true},
			},
		},
	})
	if err != nil {
		b.Fatal(err)
	}
	idx := 1
	path := &Path{Dialect: DialectGCP, Segments: []Segment{{Name: "service"}, {Name: "checks"}, {Index: &idx}, {Name: "ok"}}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := Resolve(root, path); err != nil {
			b.Fatal(err)
		}
	}
}
