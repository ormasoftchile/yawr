package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	internalexpr "github.com/ormasoftchile/yawr/runtime/internal/expr"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

type debugLazyLoader map[string]*LoadedRunbook

func (loader debugLazyLoader) Load(_ context.Context, path string) (*LoadedRunbook, error) {
	runbook := loader[path]
	if runbook == nil {
		return nil, fmt.Errorf("missing debug runbook %s", path)
	}
	return runbook, nil
}

type snapshotClosureLoader struct {
	childPath string
}

func (snapshotClosureLoader) Load(context.Context, string) (*LoadedRunbook, error) {
	return nil, errors.New("materialized closure reopened a path")
}

func (loader snapshotClosureLoader) LoadSnapshot(_ context.Context, _ string, source []byte) (*LoadedRunbook, error) {
	switch string(source) {
	case "parent":
		return &LoadedRunbook{Flow: []schema.FlowNode{{Step: &schema.Step{
			ID: "nested", Type: schema.StepTypeInclude, IncludeSpec: &schema.IncludeSpec{
				Include: schema.IncludeConfig{Runbook: "child.runbook.yaml"}, LazyRunbookPath: loader.childPath,
			},
		}}}}, nil
	case "child":
		return &LoadedRunbook{Flow: []schema.FlowNode{{Step: &schema.Step{
			ID: "done", Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{},
		}}}}, nil
	default:
		return nil, fmt.Errorf("unexpected snapshot %q", source)
	}
}

func TestIncludeExecutorMaterializesTransitiveLazyClosure(t *testing.T) {
	directory := t.TempDir()
	parentPath := filepath.Join(directory, "parent.runbook.yaml")
	childPath := filepath.Join(directory, "child.runbook.yaml")
	if err := os.WriteFile(parentPath, []byte("parent"), 0o600); err != nil {
		t.Fatalf("write parent: %v", err)
	}
	if err := os.WriteFile(childPath, []byte("child"), 0o600); err != nil {
		t.Fatalf("write child: %v", err)
	}
	includeSpec := &schema.IncludeSpec{
		Include: schema.IncludeConfig{Runbook: "parent.runbook.yaml"}, LazyRunbookPath: parentPath,
	}
	plan := &engine.ExecutionPlan{Steps: []engine.ResolvedStep{{ID: "root", Kind: "include", Spec: includeSpec}}}
	executor := NewIncludeExecutor(nil, stubRunner(nil), snapshotClosureLoader{childPath: childPath})
	if err := executor.MaterializeLazyIncludes(context.Background(), plan); err != nil {
		t.Fatalf("MaterializeLazyIncludes: %v", err)
	}
	if err := os.Remove(parentPath); err != nil {
		t.Fatalf("remove parent: %v", err)
	}
	if err := os.Remove(childPath); err != nil {
		t.Fatalf("remove child: %v", err)
	}
	if includeSpec.LazyRunbookPath != "" || len(includeSpec.ResolvedSteps) != 1 {
		t.Fatalf("parent closure = %#v", includeSpec)
	}
	nested := includeSpec.ResolvedSteps[0].Step.IncludeSpec
	if nested == nil || nested.LazyRunbookPath != "" || len(nested.ResolvedSteps) != 1 || nested.ResolvedSteps[0].Step.ID != "done" {
		t.Fatalf("nested closure = %#v", nested)
	}
	if _, err := executor.Execute(context.Background(), plan.Steps[0], map[string]any{}); err != nil {
		t.Fatalf("Execute captured closure: %v", err)
	}
}

func TestIncludeResolveDebugProtection_TraversesLazyDescendants(t *testing.T) {
	loader := debugLazyLoader{
		"child": {
			Flow: []schema.FlowNode{{Step: &schema.Step{
				ID: "grandchild_call", Type: schema.StepTypeInclude,
				IncludeSpec: &schema.IncludeSpec{
					Include: schema.IncludeConfig{
						Runbook: "grandchild", With: map[string]string{"deep_secret": "Bearer ${deep_token}"},
					},
					LazyRunbookPath: "grandchild",
				},
			}}},
		},
		"grandchild": {
			Inputs: map[string]*schema.Input{"deep_secret": {Type: "secret"}},
			Governance: &schema.GovernanceConfig{Redact: []schema.RedactRule{{
				Pattern: "deep-[a-z]+", Replace: "<redacted>",
			}}},
			Flow: []schema.FlowNode{{Step: &schema.Step{ID: "done", Type: schema.StepTypeNoop}}},
		},
	}
	executor := NewIncludeExecutor(&internalexpr.TemplateEvaluator{}, stubRunner(nil), loader)
	step := engine.ResolvedStep{
		ID: "child_call", Kind: "include",
		Spec: &schema.IncludeSpec{
			Include: schema.IncludeConfig{Runbook: "child"}, LazyRunbookPath: "child",
		},
	}
	protection, complete := executor.ResolveDebugProtection(
		context.Background(), step, map[string]any{"deep_token": "deep-value"},
	)
	if !complete {
		t.Fatal("static lazy closure was marked incomplete")
	}
	if len(protection.ProtectedVars) != 2 || protection.ProtectedVars[0] != "deep_secret" || protection.ProtectedVars[1] != "deep_token" {
		t.Fatalf("protected vars = %#v", protection.ProtectedVars)
	}
	if len(protection.SecretValues) != 2 || len(protection.RedactionPatterns) != 1 {
		t.Fatalf("transitive protection = %#v", protection)
	}
}

func TestIncludeResolveDebugProtection_RetainsFutureDependencyNames(t *testing.T) {
	executor := NewIncludeExecutor(&internalexpr.TemplateEvaluator{}, stubRunner(nil), nil)
	step := engine.ResolvedStep{
		ID: "child_call", Kind: "include",
		Spec: &schema.IncludeSpec{
			Include:        schema.IncludeConfig{With: map[string]string{"api_secret": "Bearer ${future_token}"}},
			ResolvedInputs: map[string]*schema.Input{"api_secret": {Type: "secret"}},
		},
	}
	protection, complete := executor.ResolveDebugProtection(context.Background(), step, nil)
	if !complete {
		t.Fatal("named future dependency was marked incomplete")
	}
	if len(protection.ProtectedVars) != 2 || protection.ProtectedVars[0] != "api_secret" || protection.ProtectedVars[1] != "future_token" {
		t.Fatalf("protected vars = %#v", protection.ProtectedVars)
	}
}

func TestIncludeResolveDebugProtection_WholeVarsDependencyIsIncomplete(t *testing.T) {
	executor := NewIncludeExecutor(&internalexpr.TemplateEvaluator{}, stubRunner(nil), nil)
	step := engine.ResolvedStep{
		ID: "child_call", Kind: "include",
		Spec: &schema.IncludeSpec{
			Include:        schema.IncludeConfig{With: map[string]string{"api_secret": "${vars}"}},
			ResolvedInputs: map[string]*schema.Input{"api_secret": {Type: "secret"}},
		},
	}
	_, complete := executor.ResolveDebugProtection(context.Background(), step, map[string]any{"known": "value"})
	if complete {
		t.Fatal("whole-vars dependency was marked complete")
	}
}

// stubRunner is a SubStepRunner stub: it ignores the passed nodes and
// simply returns the canned results provided at construction time.
func stubRunner(results []*engine.StepResult) SubStepRunner {
	return func(_ context.Context, _ SubStepParent, _ []schema.FlowNode, _ map[string]any) ([]*engine.StepResult, error) {
		return results, nil
	}
}

func TestIncludeExecutor_WithoutGate(t *testing.T) {
	exec := NewIncludeExecutor(nil, stubRunner(nil), nil)

	step := engine.ResolvedStep{
		ID:   "include-1",
		Kind: "include",
		Spec: &schema.IncludeSpec{
			Include: schema.IncludeConfig{Runbook: "child.yaml"},
		},
	}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if res.Status != engine.StepStatusCompleted {
		t.Fatalf("expected status completed, got %v", res.Status)
	}
	if res.Output["terminal"] != nil {
		t.Fatalf("expected no terminal flag without gate, got %v", res.Output["terminal"])
	}
}

func TestIncludeExecutor_GateMatches(t *testing.T) {
	// The gate reads __run_outcome_category from merged child vars, so put
	// it into a child StepResult.
	childResult := &engine.StepResult{
		Status: engine.StepStatusCompleted,
		Vars: map[string]any{
			"__run_outcome_category": "cancelled",
			"__run_outcome_code":     "user_cancelled",
		},
	}
	exec := NewIncludeExecutor(nil, stubRunner([]*engine.StepResult{childResult}), nil)

	step := engine.ResolvedStep{
		ID:   "include-1",
		Kind: "include",
		Spec: &schema.IncludeSpec{
			Include: schema.IncludeConfig{
				Runbook: "child.yaml",
				Gate:    &schema.GateSpec{StopIf: []string{"cancelled", "skipped"}},
			},
		},
	}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if res.Status != engine.StepStatusCompleted {
		t.Fatalf("expected status completed, got %v", res.Status)
	}
	if terminal, ok := res.Output["terminal"].(bool); !ok || !terminal {
		t.Fatalf("expected terminal=true when gate matches, got %v", res.Output["terminal"])
	}
	if res.Output["outcome_category"] != "cancelled" {
		t.Fatalf("expected outcome_category=cancelled, got %v", res.Output["outcome_category"])
	}
	if res.Output["outcome_code"] != "user_cancelled" {
		t.Fatalf("expected outcome_code=user_cancelled, got %v", res.Output["outcome_code"])
	}
}

func TestIncludeExecutor_GateDoesNotMatch(t *testing.T) {
	childResult := &engine.StepResult{
		Status: engine.StepStatusCompleted,
		Vars:   map[string]any{"__run_outcome_category": "success"},
	}
	exec := NewIncludeExecutor(nil, stubRunner([]*engine.StepResult{childResult}), nil)

	step := engine.ResolvedStep{
		ID:   "include-1",
		Kind: "include",
		Spec: &schema.IncludeSpec{
			Include: schema.IncludeConfig{
				Runbook: "child.yaml",
				Gate:    &schema.GateSpec{StopIf: []string{"cancelled", "skipped"}},
			},
		},
	}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if res.Output["terminal"] != nil {
		t.Fatalf("expected no terminal flag when gate doesn't match, got %v", res.Output["terminal"])
	}
}

func TestIncludeExecutor_GateWithNoOutcome(t *testing.T) {
	exec := NewIncludeExecutor(nil, stubRunner(nil), nil)

	step := engine.ResolvedStep{
		ID:   "include-1",
		Kind: "include",
		Spec: &schema.IncludeSpec{
			Include: schema.IncludeConfig{
				Runbook: "child.yaml",
				Gate:    &schema.GateSpec{StopIf: []string{"cancelled"}},
			},
		},
	}

	res, err := exec.Execute(context.Background(), step, map[string]any{"other_var": "value"})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if res.Output["terminal"] != nil {
		t.Fatalf("expected no terminal flag when no outcome is set, got %v", res.Output["terminal"])
	}
}

func TestIncludeExecutor_PropagatesChildVarsAndCapture(t *testing.T) {
	childResult := &engine.StepResult{
		Status: engine.StepStatusCompleted,
		Vars: map[string]any{
			"child_a": "alpha",
			"child_b": "beta",
		},
	}
	exec := NewIncludeExecutor(nil, stubRunner([]*engine.StepResult{childResult}), nil)

	step := engine.ResolvedStep{
		ID:   "include-1",
		Kind: "include",
		Spec: &schema.IncludeSpec{
			Include: schema.IncludeConfig{Runbook: "child.yaml"},
		},
		Capture: map[string]string{"renamed_a": "child_a"},
	}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if res.Vars["child_a"] != "alpha" {
		t.Fatalf("expected implicit child_a=alpha, got %v", res.Vars["child_a"])
	}
	if res.Vars["child_b"] != "beta" {
		t.Fatalf("expected implicit child_b=beta, got %v", res.Vars["child_b"])
	}
	if res.Vars["renamed_a"] != "alpha" {
		t.Fatalf("expected capture renamed_a=alpha, got %v", res.Vars["renamed_a"])
	}
}

func TestIncludeExecutor_FailedChildPropagates(t *testing.T) {
	// When the sub-runner returns ErrSubRunFailed (child sub-run terminated
	// as failed), the include executor returns (failed result, nil) so the
	// parent engine can apply resolveOnError on the container step.
	exec := NewIncludeExecutor(nil, func(_ context.Context, _ SubStepParent, _ []schema.FlowNode, _ map[string]any) ([]*engine.StepResult, error) {
		return []*engine.StepResult{
			{Status: engine.StepStatusCompleted},
			{Status: engine.StepStatusFailed},
		}, fmt.Errorf("%w: step child-2", ErrSubRunFailed)
	}, nil)

	step := engine.ResolvedStep{
		ID:   "include-1",
		Kind: "include",
		Spec: &schema.IncludeSpec{Include: schema.IncludeConfig{Runbook: "child.yaml"}},
	}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("expected nil error (failed result), got: %v", err)
	}
	if res.Status != engine.StepStatusFailed {
		t.Fatalf("expected StepStatusFailed, got %v", res.Status)
	}
}

func TestIncludeExecutor_InfraError_Propagates(t *testing.T) {
	// A genuine infrastructure error (not ErrSubRunFailed) must propagate
	// as (nil, err) so failRun handles it.
	exec := NewIncludeExecutor(nil, func(_ context.Context, _ SubStepParent, _ []schema.FlowNode, _ map[string]any) ([]*engine.StepResult, error) {
		return nil, fmt.Errorf("network timeout")
	}, nil)

	step := engine.ResolvedStep{
		ID:   "include-1",
		Kind: "include",
		Spec: &schema.IncludeSpec{Include: schema.IncludeConfig{Runbook: "child.yaml"}},
	}

	_, err := exec.Execute(context.Background(), step, map[string]any{})
	if err == nil {
		t.Fatal("expected infrastructure error to propagate")
	}
}

func TestIncludeExecutor_FailedChildNilError_ReportsCompleted(t *testing.T) {
	// When the sub-runner returns a failed child result alongside nil error,
	// that means the failure was tolerated (on_error: continue). The include
	// must report Completed, not Failed.
	exec := NewIncludeExecutor(nil, stubRunner([]*engine.StepResult{
		{Status: engine.StepStatusCompleted},
		{Status: engine.StepStatusFailed},
	}), nil)

	step := engine.ResolvedStep{
		ID:   "include-1",
		Kind: "include",
		Spec: &schema.IncludeSpec{Include: schema.IncludeConfig{Runbook: "child.yaml"}},
	}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != engine.StepStatusCompleted {
		t.Fatalf("expected status completed (failure was tolerated), got %v", res.Status)
	}
}
