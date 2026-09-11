package executor

import (
	"context"
	"reflect"
	"strings"
	"testing"

	internalexpr "github.com/ormasoftchile/yawr/runtime/internal/expr"
	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestTypedBindingsAtomicNativeValues(t *testing.T) {
	bindings := []schema.Binding{
		{Name: "flag", Type: "bool", Mutable: true, Value: false, ValuePresent: true},
		{Name: "number", Type: "integer", Mutable: true, Value: 0, ValuePresent: true},
		{Name: "nullable", Type: "any", Value: nil, ValuePresent: true},
		{Name: "rows", Type: "array", Mutable: true, Value: []any{}, ValuePresent: true},
	}
	seed := map[string]any{"input": "kept"}
	vars, err := InitializeBindings(bindings, seed)
	if err != nil {
		t.Fatal(err)
	}
	if len(seed) != 1 || vars["flag"] != false || vars["number"] != 0 || vars["nullable"] != nil {
		t.Fatalf("native values changed: %#v", vars)
	}
	writes, err := AssignBindings(bindings, []schema.Assignment{
		{Name: "number", Value: 2, ValuePresent: true},
		{Name: "rows", Value: []any{"${number}", false, nil, ""}, ValuePresent: true},
	}, vars)
	if err != nil {
		t.Fatal(err)
	}
	if vars["number"] != 0 || !reflect.DeepEqual(writes["rows"], []any{2, false, nil, ""}) {
		t.Fatalf("atomic ordered writes lost: %#v", writes)
	}
	writes, err = AssignBindings(bindings, []schema.Assignment{
		{Name: "number", Value: 3, ValuePresent: true},
		{Name: "flag", Value: "true", ValuePresent: true},
	}, vars)
	if err == nil || writes != nil || vars["number"] != 0 {
		t.Fatalf("failed assign escaped scratch: %#v, %v", writes, err)
	}
	for _, source := range []string{"${now()}", "${false and now() == now()}", "${list.order(rows, comparator)}"} {
		if _, err := ResolvePureTree(source, vars); err == nil {
			t.Fatalf("impure source admitted: %s", source)
		}
	}
}

func TestTypedNamedOutputsMissingIsNotNullOrFailure(t *testing.T) {
	vars := map[string]any{"value": nil, "flag": false, "zero": 0}
	outputs := map[string]*schema.Output{
		"null":    {Type: "any", ValueExpr: "value"},
		"missing": {Type: "any", ValueExpr: "absent", Optional: true},
		"report": {Type: "object", ValueTreePresent: true, ValueTree: map[string]any{
			"false": "${flag}", "zero": "${zero}", "null": "${value}", "rows": []any{},
		}},
	}
	result, err := EvaluateNamedOutputs(outputs, vars)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := result["missing"]; exists {
		t.Fatal("missing optional fabricated")
	}
	if value, exists := result["null"]; !exists || value.Value != nil {
		t.Fatal("present null omitted")
	}
	for _, expression := range []string{"1 / 0", "flag.unknown", "value.unknown", "zero + true"} {
		outputs["bad"] = &schema.Output{Type: "any", ValueExpr: expression, Optional: true}
		result, err := EvaluateNamedOutputs(outputs, vars)
		if err == nil || result != nil {
			t.Fatalf("optional suppressed evaluation failure %q", expression)
		}
	}
}

func TestFrozenSubstitutionValueExpressionContracts(t *testing.T) {
	closure, err := plansnapshot.EncodeFlowClosure([]schema.FlowNode{{Step: &schema.Step{
		ID: "child", Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name       string
		output     *schema.Output
		payload    any
		wantStatus engine.StepStatus
		wantError  string
	}{
		{"array", &schema.Output{Type: "array", ValueExpr: "payload"}, []any{map[string]any{"count": 2}}, engine.StepStatusCompleted, ""},
		{"wrong-type", &schema.Output{Type: "array", ValueExpr: "payload"}, "not-an-array", engine.StepStatusFailed, "array"},
		{"required-missing", &schema.Output{Type: "array", ValueExpr: "missing"}, nil, engine.StepStatusFailed, "missing"},
		{"optional-missing", &schema.Output{Type: "array", ValueExpr: "missing", Optional: true}, nil, engine.StepStatusCompleted, ""},
		{"enum", &schema.Output{Type: "string", ValueExpr: "payload", Enum: schema.EnumConstraint{"allowed"}}, "other", engine.StepStatusFailed, "ENUM-009"},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			runner := func(context.Context, SubStepParent, []schema.FlowNode, map[string]any) ([]*engine.StepResult, error) {
				calls++
				return []*engine.StepResult{{StepID: "child", Status: engine.StepStatusCompleted, Vars: map[string]any{"payload": test.payload}}}, nil
			}
			definition := &schema.ToolDef{Name: "typed", Actions: map[string]*schema.ToolAction{
				"run": {
					Execute: &schema.ExecuteSpec{Kind: "runbook", Path: "child.runbook.yaml"},
					Outputs: map[string]*schema.ArgDef{"result": {Type: test.output.Type, Enum: test.output.Enum}},
					FrozenSubstitution: &schema.FrozenToolSubstitution{
						RunbookPath: "child.runbook.yaml", RunbookID: "child", RunbookContentHash: strings.Repeat("a", 64),
						ExecutableClosure: closure, Outputs: map[string]*schema.Output{"result": test.output},
					},
				},
			}}
			executor := NewToolExecutorWithSubstitution(nil, &internalexpr.TemplateEvaluator{}, runner, nil, nil)
			result, err := executor.ExecuteFrozenSubstitution(context.Background(), engine.ResolvedStep{
				ID: "call", Kind: "tool", Spec: &schema.ToolCallSpec{Tool: schema.ToolInvocation{Name: "typed", Action: "run"}},
			}, nil, definition)
			if err != nil || result.Status != test.wantStatus || calls != 1 {
				t.Fatalf("result=%#v err=%v body calls=%d", result, err, calls)
			}
			if test.wantError != "" && (result.Error == nil || !strings.Contains(result.Error.Error(), test.wantError)) {
				t.Fatalf("error=%v, want %q", result.Error, test.wantError)
			}
			if test.output.Optional {
				if _, present := result.Output["result"]; present {
					t.Fatal("missing optional output was manufactured")
				}
			}
		})
	}
}
