package executor

import (
	"context"
	"errors"
	"testing"

	internalexpr "github.com/ormasoftchile/yawr/runtime/internal/expr"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

// A failed dispatch still populates every declared capture so an
// on_error: continue successor can reference the name. What it must not do is
// report a declared outputs: value as the empty string: the value was never
// produced, and "" claims a type the contract did not declare. Process
// channels are different -- stdout/stderr are untyped text and exit_code is a
// numeric status, and all three exist even for a failed dispatch.
func TestToolExecutor_FailedDispatchDoesNotRetypeDeclaredOutputs(t *testing.T) {
	exec := NewToolExecutor(errorResultRuntime{
		result: &tool.ToolResult{Stdout: "out", Stderr: "diagnostic", ExitCode: 1},
		err:    errors.New("native tool queryer action query exited with code 1"),
	}, nil)
	step := engine.ResolvedStep{
		ID:   "failing_query",
		Kind: "tool",
		Spec: &schema.ToolCallSpec{Tool: schema.ToolInvocation{Name: "queryer", Action: "query"}},
		Capture: map[string]string{
			"ok":     "outputs.success",
			"rows":   "outputs.row_count",
			"nested": "outputs.detail.reason",
			"out":    "stdout",
			"errtxt": "stderr",
			"code":   "exit_code",
		},
	}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != engine.StepStatusFailed {
		t.Fatalf("status: got %s want failed", res.Status)
	}
	for _, name := range []string{"ok", "rows", "nested"} {
		value, present := res.Vars[name]
		if !present {
			t.Fatalf("capture %q absent: an on_error: continue successor cannot reference it", name)
		}
		if value != nil {
			t.Fatalf("capture %q = %#v, want nil: an unproduced declared output must not be retyped", name, value)
		}
	}
	if res.Vars["out"] != "" || res.Vars["errtxt"] != "" {
		t.Fatalf("process text channels changed: %#v", res.Vars)
	}
	if res.Vars["code"] != -1 {
		t.Fatalf("exit_code capture = %#v, want -1", res.Vars["code"])
	}
}

// The reason the value must be null rather than "" is that the failure guard
// has to be writable. Comparing the captured value against the type the action
// declares is the only branch reachable after on_error: continue; with "" it
// raises GXL-TYPE-001 and the guard becomes dead code.
func TestToolExecutor_FailedDeclaredOutputIsComparableToItsDeclaredType(t *testing.T) {
	exec := NewToolExecutor(errorResultRuntime{
		result: &tool.ToolResult{ExitCode: 1},
		err:    errors.New("native tool queryer action query exited with code 1"),
	}, nil)
	step := engine.ResolvedStep{
		ID:      "failing_query",
		Kind:    "tool",
		Spec:    &schema.ToolCallSpec{Tool: schema.ToolInvocation{Name: "queryer", Action: "query"}},
		Capture: map[string]string{"ok": "outputs.success", "n": "outputs.row_count"},
	}
	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	conditions := &internalexpr.SimpleConditionEvaluator{}
	for _, test := range []struct {
		condition string
		want      bool
	}{
		{condition: "ok != true", want: true},
		{condition: "ok == true", want: false},
		{condition: "ok == null", want: true},
		{condition: "n != 0", want: true},
	} {
		got, err := conditions.EvalBool(test.condition, res.Vars)
		if err != nil {
			t.Fatalf("eval %q: %v (the failure guard is unwritable)", test.condition, err)
		}
		if got != test.want {
			t.Fatalf("eval %q = %v, want %v", test.condition, got, test.want)
		}
	}
}
