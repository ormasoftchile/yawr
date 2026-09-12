package gcp_test

import (
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/capture"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	gcpparser "github.com/ormasoftchile/yawr/runtime/pkg/gcp/parser"
)

func BenchmarkEngineGCPPathResolution(b *testing.B) {
	step := engine.ResolvedStep{
		ID: "collect",
		Capture: map[string]string{
			"service": "json.service.name",
			"status":  "json.service.status",
			"first":   "json.service.checks[0].name",
		},
	}
	vp := &engine.ValidatedPlan{
		Source:   &engine.ExecutionPlan{Steps: []engine.ResolvedStep{step}},
		GCPPaths: map[engine.StepRef]*gcpparser.Path{},
	}
	for name, src := range step.Capture {
		path, err := gcpparser.Parse(src)
		if err != nil {
			b.Fatal(err)
		}
		vp.GCPPaths[engine.StepRef{StepID: step.ID, FieldPath: "capture." + name}] = path
	}
	result := &engine.StepResult{Output: map[string]any{
		"stdout":    `{"service":{"name":"api","status":"ok","checks":[{"name":"dns"},{"name":"ping"}]}}`,
		"stderr":    "",
		"exit_code": 0,
	}}
	svc := capture.New(nil, nil, nil)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := svc.CaptureStep(vp, step, result); err != nil {
			b.Fatal(err)
		}
	}
}
