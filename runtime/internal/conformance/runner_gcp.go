package conformance

import (
	"context"
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/pkg/capture"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	gcpparser "github.com/ormasoftchile/yawr/runtime/pkg/gcp/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/gdp"
	"github.com/ormasoftchile/yawr/runtime/pkg/pjvm"
)

// GCPRunner executes runnable GCP parser/path-resolution vectors.
type GCPRunner struct{}

// Run parses and resolves GCP vectors through the capture service.
func (GCPRunner) Run(_ context.Context, v Vector) Result {
	path, err := gcpparser.Parse(v.Input)
	if err != nil {
		return expectedErrorResult(v, err, "parse")
	}
	if path.Source.Kind == gdp.SourceStep && missingVectorStep(path, v.Variables) {
		return expectedErrorResult(v, errkit.New("GCP-PARSE-006", "step identifier not found"), "parse")
	}
	if v.Expected.WantsError() && v.Expected.ErrorClass == "GCP-PARSE" {
		return Result{Verdict: VerdictFail, Err: fmt.Errorf("expected %s/%s, got parse_ok", v.Expected.ErrorClass, v.Expected.ErrorCode)}
	}
	got, err := resolveGCP(path, v.Input, v.Variables)
	if err != nil {
		return expectedErrorResult(v, err, "resolve")
	}
	if v.Expected.WantsError() {
		return Result{Verdict: VerdictFail, Err: fmt.Errorf("expected %s/%s, got value %s", v.Expected.ErrorClass, v.Expected.ErrorCode, got.String())}
	}
	if v.Expected.PJVMValue == nil {
		return Result{Verdict: VerdictFail, Err: fmt.Errorf("expected value missing from vector")}
	}
	if !got.Equal(*v.Expected.PJVMValue) {
		return Result{Verdict: VerdictFail, Err: fmt.Errorf("expected %s, got %s", v.Expected.PJVMValue.String(), got.String())}
	}
	return Result{Verdict: VerdictPass}
}

func missingVectorStep(path *gdp.Path, vars map[string]any) bool {
	steps, _ := vars["steps"].(map[string]any)
	_, ok := steps[path.Source.StepID]
	return !ok
}

func resolveGCP(path *gdp.Path, input string, vars map[string]any) (pjvm.Value, error) {
	step := engine.ResolvedStep{ID: "vector", Capture: map[string]string{"out": input}}
	if def, ok := vars["capture_default"]; ok {
		step.CaptureDefaults = map[string]any{"out": def}
	}
	result := &engine.StepResult{StepID: "vector", Output: vectorOutput(vars)}
	prior := vectorStepResults(vars)
	captures, err := capture.New(vars, prior, nil).CaptureStep(nil, step, result)
	if err != nil {
		return pjvm.Null(), err
	}
	return captures["out"], nil
}

func vectorOutput(vars map[string]any) map[string]any {
	out := map[string]any{}
	for _, key := range []string{"stdout", "stderr", "exit_code", "http", "event"} {
		if v, ok := vars[key]; ok {
			out[key] = v
		}
	}
	return out
}

func vectorStepResults(vars map[string]any) map[string]*engine.StepResult {
	out := map[string]*engine.StepResult{}
	steps, _ := vars["steps"].(map[string]any)
	for id, raw := range steps {
		if env, ok := raw.(map[string]any); ok {
			out[id] = &engine.StepResult{StepID: id, Output: env}
		}
	}
	return out
}
