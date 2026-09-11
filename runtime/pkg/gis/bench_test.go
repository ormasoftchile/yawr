package gis

import (
	"testing"

	gxleval "github.com/ormasoftchile/yawr/runtime/pkg/gxl/eval"
)

var benchGISVars = map[string]any{
	"svc": map[string]any{
		"name":    "api",
		"host":    "api.internal",
		"healthy": true,
		"latency": 87,
	},
	"report": "ok",
}

func BenchmarkEngineGISInterpolation(b *testing.B) {
	cases := []string{
		`service=${svc.name} host=${svc.host}`,
		`status=${svc.healthy == true} latency=${svc.latency}`,
		`summary=${str.toUpper(report)} \${literal}`,
	}
	scope, err := gxleval.FromAny(benchGISVars)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for _, tmpl := range cases {
		b.Run(tmpl, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if _, err := Interpolate(tmpl, scope); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
