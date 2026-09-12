package executor

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestIncludeForwardingGateTransaction(t *testing.T) {
	value := map[string]any{"false": false, "zero": 0, "ordered": []any{nil, false, 0}, "object": map[string]any{}}
	for _, test := range []struct {
		name        string
		gate        []string
		category    string
		published   bool
		wantStop    bool
		wantMissing bool
	}{
		{"success", []string{"blocked"}, "", true, false, false},
		{"blocked", []string{"blocked"}, "blocked", false, true, false},
		{"other-declared-category", []string{"cancelled"}, "cancelled", false, true, false},
		{"no-gate", nil, "blocked", false, false, true},
		{"unmatched-gate", []string{"resolved"}, "blocked", false, false, true},
		{"published-matching-gate", []string{"blocked"}, "blocked", true, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			child := &engine.StepResult{Status: engine.StepStatusCompleted, Vars: map[string]any{"local": "child"}}
			if test.category != "" {
				child.Vars["__run_outcome_category"] = test.category
				child.Vars["__run_outcome_code"] = "declared-code"
			}
			if test.published {
				child.Results = &engine.RunResults{Outputs: map[string]engine.NamedResultValue{"result": {Type: "object", Value: value}}}
			}
			spec := &schema.IncludeSpec{Include: schema.IncludeConfig{Runbook: "child.yaml", Gate: &schema.GateSpec{StopIf: test.gate}}}
			step := engine.ResolvedStep{ID: "invoke", Kind: "include", Spec: spec, Capture: map[string]string{"forwarded": "outputs.result", "alias": "local", "fallback_alias": "parent"}}
			parent := map[string]any{"parent": "parent-value", "forwarded": "stale-value"}
			result, err := NewIncludeExecutor(nil, stubRunner([]*engine.StepResult{child}), nil).Execute(context.Background(), step, parent)
			if test.wantMissing {
				if err == nil || !strings.Contains(err.Error(), `public output "result" was not published`) || result != nil {
					t.Fatalf("missing public output did not fail atomically: %#v, %v", result, err)
				}
				return
			}
			if err != nil || result.Status != engine.StepStatusCompleted {
				t.Fatalf("include: %#v, %v", result, err)
			}
			if result.Results != nil {
				t.Fatal("child publication promoted to parent")
			}
			if test.wantStop {
				if result.Output["terminal"] != true || result.Output["outcome_category"] != test.category || result.Output["outcome_code"] != "declared-code" {
					t.Fatalf("lost declared gate outcome: %#v", result.Output)
				}
				for dest := range step.Capture {
					if _, exists := result.Vars[dest]; exists {
						t.Fatalf("gate committed capture %s", dest)
					}
				}
			} else if !reflect.DeepEqual(result.Vars["forwarded"], value) || result.Vars["alias"] != "child" || result.Vars["fallback_alias"] != "parent-value" {
				t.Fatalf("capture changed native value: %#v", result.Vars)
			}
			if parent["forwarded"] != "stale-value" || len(parent) != 2 {
				t.Fatal("aggregation wrote parent before settlement")
			}
		})
	}
}

func TestIncludeForwardingGateDoesNotMaskRequiredFailure(t *testing.T) {
	for _, status := range []engine.StepStatus{engine.StepStatusFailed, engine.StepStatusDenied, engine.StepStatusIndeterminate, engine.StepStatusWaiting, engine.StepStatusRunning, engine.StepStatusPending, engine.StepStatusCompleted} {
		t.Run(string(status), func(t *testing.T) {
			cause := errors.New("original required producer failure")
			child := &engine.StepResult{Status: status, Error: cause, RequiredFailure: status == engine.StepStatusCompleted}
			end := &engine.StepResult{Status: engine.StepStatusCompleted, Vars: map[string]any{"__run_outcome_category": "blocked"}}
			spec := &schema.IncludeSpec{Include: schema.IncludeConfig{Runbook: "child.yaml", Gate: &schema.GateSpec{StopIf: []string{"blocked"}}}}
			step := engine.ResolvedStep{ID: "invoke", Kind: "include", Spec: spec, Capture: map[string]string{"forwarded": "outputs.result"}}
			result, err := NewIncludeExecutor(nil, stubRunner([]*engine.StepResult{child, end}), nil).Execute(context.Background(), step, nil)
			if err != nil || result == nil || result.Status != engine.StepStatusFailed || !errors.Is(result.Error, cause) {
				t.Fatalf("original failure masked by capture/gate: %#v, %v", result, err)
			}
			if result.Output["terminal"] != nil || result.Results != nil || result.PublicOutputs != nil {
				t.Fatal("gate converted failure into terminal domain outcome")
			}
		})
	}
}
