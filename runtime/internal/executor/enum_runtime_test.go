package executor

import (
	"context"
	"strings"
	"testing"

	internalexpr "github.com/ormasoftchile/yawr/runtime/internal/expr"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/testutil"
	"github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

// TestToolExecutor_Enum008_RejectsNonMemberArg covers ENUM-008 (S1): a
// materialized (post-GIS-interpolation) tool arg value bound to an
// enum-constrained action arg must be a declared member, checked before
// dispatch.
func TestToolExecutor_Enum008_RejectsNonMemberArg(t *testing.T) {
	runtime := testutil.NewFakeToolRuntime()
	runtime.RegisterDef("kubectl", &tool.ToolDef{
		Name: "kubectl",
		Actions: map[string]*tool.ToolAction{
			"drain-node": {Args: map[string]*tool.ArgDef{
				"mode": {Type: "string", Enum: schema.EnumConstraint{"graceful", "force"}},
			}},
		},
	})
	exec := NewToolExecutor(runtime, &internalexpr.TemplateEvaluator{})
	step := engine.ResolvedStep{
		ID:   "step-1",
		Kind: "tool",
		Spec: &schema.ToolCallSpec{Tool: schema.ToolInvocation{Name: "kubectl", Action: "drain-node", Args: map[string]any{"mode": "${mode}"}}},
	}

	res, err := exec.Execute(context.Background(), step, map[string]any{"mode": "immediate"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != engine.StepStatusFailed {
		t.Fatalf("expected failed status, got %s", res.Status)
	}
	if res.Error == nil || !strings.Contains(res.Error.Error(), "ENUM-008") {
		t.Fatalf("expected ENUM-008 error, got %v", res.Error)
	}
	if len(runtime.Calls) != 0 {
		t.Fatalf("expected invocation to be blocked, got %d calls", len(runtime.Calls))
	}
}

// TestToolExecutor_Enum008_AllowsMemberArg is the corresponding happy path.
func TestToolExecutor_Enum008_AllowsMemberArg(t *testing.T) {
	runtime := testutil.NewFakeToolRuntime()
	runtime.RegisterDef("kubectl", &tool.ToolDef{
		Name: "kubectl",
		Actions: map[string]*tool.ToolAction{
			"drain-node": {Args: map[string]*tool.ArgDef{
				"mode": {Type: "string", Enum: schema.EnumConstraint{"graceful", "force"}},
			}},
		},
	})
	exec := NewToolExecutor(runtime, &internalexpr.TemplateEvaluator{})
	step := engine.ResolvedStep{
		ID:   "step-1",
		Kind: "tool",
		Spec: &schema.ToolCallSpec{Tool: schema.ToolInvocation{Name: "kubectl", Action: "drain-node", Args: map[string]any{"mode": "${mode}"}}},
	}

	res, err := exec.Execute(context.Background(), step, map[string]any{"mode": "graceful"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != engine.StepStatusCompleted {
		t.Fatalf("expected completed, got %s: %v", res.Status, res.Error)
	}
	if len(runtime.Calls) != 1 {
		t.Fatalf("expected invocation to proceed, got %d calls", len(runtime.Calls))
	}
}

// TestToolExecutor_Enum008_UnconstrainedArgUnaffected confirms an arg with
// Without an enum declaration there is no enum constraint.
func TestToolExecutor_Enum008_UnconstrainedArgUnaffected(t *testing.T) {
	runtime := testutil.NewFakeToolRuntime()
	runtime.RegisterDef("kubectl", &tool.ToolDef{
		Name: "kubectl",
		Actions: map[string]*tool.ToolAction{
			"drain-node": {Args: map[string]*tool.ArgDef{
				"mode": {Type: "string"},
			}},
		},
	})
	exec := NewToolExecutor(runtime, &internalexpr.TemplateEvaluator{})
	step := engine.ResolvedStep{
		ID:   "step-1",
		Kind: "tool",
		Spec: &schema.ToolCallSpec{Tool: schema.ToolInvocation{Name: "kubectl", Action: "drain-node", Args: map[string]any{"mode": "anything-goes"}}},
	}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != engine.StepStatusCompleted {
		t.Fatalf("expected completed, got %s: %v", res.Status, res.Error)
	}
}
