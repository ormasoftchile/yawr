package executor

import (
	"context"
	"errors"
	"strings"
	"testing"

	internalexpr "github.com/ormasoftchile/yawr/runtime/internal/expr"
	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestFrozenSubstitutionFailureOutputContracts(t *testing.T) {
	closure, err := plansnapshot.EncodeFlowClosure([]schema.FlowNode{{Step: &schema.Step{
		ID: "child", Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, expression, kind, wantError string
		optional, produced                bool
		enum                              schema.EnumConstraint
	}{
		{name: "typed", expression: "rows", kind: "array", produced: true},
		{name: "missing", expression: "missing", kind: "array", wantError: "missing"},
		{name: "optional", expression: "missing", kind: "array", optional: true},
		{name: "wrong-type", expression: "status", kind: "array", wantError: "array"},
		{name: "enum", expression: "status", kind: "string", enum: schema.EnumConstraint{"observed"}, wantError: "ENUM-009"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cause := errors.New("query transport failed")
			runner := func(context.Context, SubStepParent, []schema.FlowNode, map[string]any) ([]*engine.StepResult, error) {
				return []*engine.StepResult{{
					StepID: "child", Status: engine.StepStatusFailed, Error: cause,
					Vars: map[string]any{"rows": []any{}, "status": "failed"},
				}}, errors.Join(ErrSubRunFailed, cause)
			}
			def := &schema.ToolDef{Name: "failure", Actions: map[string]*schema.ToolAction{"run": {
				Execute: &schema.ExecuteSpec{Kind: "runbook", Path: "child.runbook.yaml"},
				Outputs: map[string]*schema.ArgDef{"value": {Type: test.kind, Enum: test.enum}},
				FrozenSubstitution: &schema.FrozenToolSubstitution{
					RunbookPath: "child.runbook.yaml", RunbookID: "child", RunbookContentHash: strings.Repeat("a", 64),
					ExecutableClosure: closure, Outputs: map[string]*schema.Output{
						"value": {Type: test.kind, ValueExpr: test.expression, Optional: test.optional, Enum: test.enum},
					},
				},
			}}}
			exec := NewToolExecutorWithSubstitution(nil, &internalexpr.TemplateEvaluator{}, runner, nil, nil)
			result, err := exec.ExecuteFrozenSubstitution(context.Background(), engine.ResolvedStep{
				ID: "call", Kind: "tool", Spec: &schema.ToolCallSpec{Tool: schema.ToolInvocation{Name: "failure", Action: "run"}},
				Capture: map[string]string{"captured": "outputs.value"},
			}, nil, def)
			if err != nil || result.Status != engine.StepStatusFailed || !errors.Is(result.Error, cause) || !errors.Is(result.Error, ErrSubRunFailed) {
				t.Fatalf("failure replaced: result=%#v err=%v", result, err)
			}
			if test.wantError != "" && !strings.Contains(result.Error.Error(), test.wantError) {
				t.Fatalf("terminal output contract not checked: %v", result.Error)
			}
			_, output := result.Output["value"]
			_, captured := result.Vars["captured"]
			if output != test.produced || captured != test.produced {
				t.Fatalf("invalid/missing output manufactured, or valid failure output lost: %#v", result)
			}
		})
	}
}

func TestFrozenSubstitutionInterruptedRunnerDoesNotExport(t *testing.T) {
	runner := func(context.Context, SubStepParent, []schema.FlowNode, map[string]any) ([]*engine.StepResult, error) {
		return nil, context.Canceled
	}
	exec := NewToolExecutorWithSubstitution(nil, &internalexpr.TemplateEvaluator{}, runner, nil, nil)
	closure, _ := plansnapshot.EncodeFlowClosure([]schema.FlowNode{{Step: &schema.Step{ID: "pending", Type: schema.StepTypeNoop}}})
	def := &schema.ToolDef{Name: "pending", Actions: map[string]*schema.ToolAction{"run": {
		Execute: &schema.ExecuteSpec{Kind: "runbook", Path: "pending.runbook.yaml"},
		FrozenSubstitution: &schema.FrozenToolSubstitution{
			RunbookPath: "pending.runbook.yaml", RunbookID: "pending", RunbookContentHash: strings.Repeat("a", 64),
			ExecutableClosure: closure,
		},
	}}}
	result, err := exec.ExecuteFrozenSubstitution(context.Background(), engine.ResolvedStep{
		ID: "call", Kind: "tool", Spec: &schema.ToolCallSpec{Tool: schema.ToolInvocation{Name: "pending", Action: "run"}},
	}, nil, def)
	if result != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("interruption became a terminal output: %#v %v", result, err)
	}
}
