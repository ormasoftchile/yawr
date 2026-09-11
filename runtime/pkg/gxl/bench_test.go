package gxl_test

import (
	"testing"

	gxleval "github.com/ormasoftchile/yawr/runtime/pkg/gxl/eval"
	gxlparser "github.com/ormasoftchile/yawr/runtime/pkg/gxl/parser"
)

var benchGXLEnv = map[string]any{
	"service": map[string]any{
		"name":    "api",
		"healthy": true,
		"latency": 87,
		"tags":    []any{"prod", "critical", "http"},
	},
	"vars": map[string]any{"region": "us-east-1"},
}

func BenchmarkEngineGXLParseEval(b *testing.B) {
	cases := []string{
		`service.healthy == true and service.latency < 100`,
		`str.contains(service.name, "api") and region == "us-east-1"`,
		`list.contains(service.tags, "critical") or service.latency < 50`,
	}
	scope, err := gxleval.FromAny(benchGXLEnv)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for _, expr := range cases {
		b.Run(expr, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				ast, err := gxlparser.Parse(expr)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := gxleval.Eval(ast, scope); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
