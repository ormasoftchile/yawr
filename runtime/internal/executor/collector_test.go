package executor

import (
	"context"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/input"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/testutil"
)

func TestCollectorExecutor_ValidatesCompleteResolvedForm(t *testing.T) {
	provider := &testutil.FakePromptProvider{FormResponses: map[string]*input.FormResponse{
		"findings": {Values: map[string]any{"health": "degraded", "undeclared": "value"}},
	}}
	exec := NewCollectorExecutor(provider, nil, nil)
	step := engine.ResolvedStep{ID: "findings", Kind: "collector", Spec: &schema.CollectorSpec{
		Fields: []schema.CollectorField{{
			Name: "health", Type: schema.FieldTypeSelect, Required: true,
			Options: []schema.ChoiceOption{{Label: "Healthy", Value: "healthy"}},
		}},
	}}

	result, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != engine.StepStatusFailed {
		t.Fatalf("status = %s, want failed", result.Status)
	}
	failures, ok := result.Output["validation_errors"].([]string)
	if !ok || len(failures) != 2 {
		t.Fatalf("validation_errors = %#v, want select and unknown-field failures", result.Output["validation_errors"])
	}
	if len(result.Vars) != 0 {
		t.Fatalf("invalid values reached run variables: %#v", result.Vars)
	}
}
