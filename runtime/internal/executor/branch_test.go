package executor

import (
	"context"
	"testing"

	internalExpr "github.com/ormasoftchile/yawr/runtime/internal/expr"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

type fakeConditionEvaluator struct {
	results map[string]bool
}

func (f fakeConditionEvaluator) EvalBool(condition string, vars map[string]any) (bool, error) {
	return f.results[condition], nil
}

func TestCopyVarsIsolatesNestedRuntimeValues(t *testing.T) {
	source := map[string]any{
		"object": map[string]any{"value": "original"},
		"items":  []any{map[string]any{"value": "original"}},
	}
	cloned := copyVars(source)
	cloned["object"].(map[string]any)["value"] = "changed"
	cloned["items"].([]any)[0].(map[string]any)["value"] = "changed"
	if source["object"].(map[string]any)["value"] != "original" ||
		source["items"].([]any)[0].(map[string]any)["value"] != "original" {
		t.Fatalf("nested runtime values were aliased: %#v", source)
	}
}

func TestBranchExecutor_FirstMatch(t *testing.T) {
	var called bool
	runner := func(ctx context.Context, _ SubStepParent, steps []schema.FlowNode, vars map[string]any) ([]*engine.StepResult, error) {
		called = true
		return []*engine.StepResult{{StepID: "child", Status: engine.StepStatusCompleted, Vars: map[string]any{"x": "y"}}}, nil
	}
	cond := fakeConditionEvaluator{results: map[string]bool{"a": true, "b": false}}
	exec := NewBranchExecutor(cond, runner)

	step := engine.ResolvedStep{ID: "branch", Kind: "branch", Spec: &schema.BranchSpec{Branches: []schema.BranchArm{
		{Condition: "a", Label: "first", Steps: []schema.FlowNode{{Step: &schema.Step{ID: "s1", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "echo"}}}}},
		{Condition: "b", Label: "second", Steps: []schema.FlowNode{{Step: &schema.Step{ID: "s2", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "echo"}}}}},
	}}}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !called {
		t.Fatal("expected substeps to run")
	}
	if res.Output["matched_arm"] != "first" {
		t.Fatalf("expected matched_arm first, got %v", res.Output["matched_arm"])
	}
	if res.Output["matched_arm_index"] != 0 {
		t.Fatalf("expected matched_arm_index 0, got %v", res.Output["matched_arm_index"])
	}
}

func TestBranchExecutor_ElseArm(t *testing.T) {
	var matched string
	runner := func(ctx context.Context, _ SubStepParent, steps []schema.FlowNode, vars map[string]any) ([]*engine.StepResult, error) {
		matched = steps[0].Step.ID
		return []*engine.StepResult{{StepID: "child", Status: engine.StepStatusCompleted}}, nil
	}
	cond := fakeConditionEvaluator{results: map[string]bool{"a": false}}
	exec := NewBranchExecutor(cond, runner)

	step := engine.ResolvedStep{ID: "branch", Kind: "branch", Spec: &schema.BranchSpec{Branches: []schema.BranchArm{
		{Condition: "a", Steps: []schema.FlowNode{{Step: &schema.Step{ID: "s1", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "echo"}}}}},
		{Else: true, Label: "fallback", Steps: []schema.FlowNode{{Step: &schema.Step{ID: "s2", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "echo"}}}}},
	}}}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if matched != "s2" {
		t.Fatalf("expected else arm step s2, got %s", matched)
	}
	if res.Output["matched_arm"] != "fallback" {
		t.Fatalf("expected matched_arm fallback, got %v", res.Output["matched_arm"])
	}
	if res.Output["matched_arm_index"] != 1 {
		t.Fatalf("expected matched_arm_index 1, got %v", res.Output["matched_arm_index"])
	}
}

func TestBranchExecutor_NoMatch(t *testing.T) {
	runner := func(ctx context.Context, _ SubStepParent, steps []schema.FlowNode, vars map[string]any) ([]*engine.StepResult, error) {
		return nil, nil
	}
	cond := fakeConditionEvaluator{results: map[string]bool{"a": false}}
	exec := NewBranchExecutor(cond, runner)

	step := engine.ResolvedStep{ID: "branch", Kind: "branch", Spec: &schema.BranchSpec{Branches: []schema.BranchArm{
		{Condition: "a", Steps: []schema.FlowNode{{Step: &schema.Step{ID: "s1", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "echo"}}}}},
	}}}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != engine.StepStatusSkipped {
		t.Fatalf("expected skipped, got %s", res.Status)
	}
}

func TestBranchExecutor_MergesVars(t *testing.T) {
	runner := func(ctx context.Context, _ SubStepParent, steps []schema.FlowNode, vars map[string]any) ([]*engine.StepResult, error) {
		return []*engine.StepResult{
			{StepID: "a", Status: engine.StepStatusCompleted, Vars: map[string]any{"first": "1"}},
			{StepID: "b", Status: engine.StepStatusCompleted, Vars: map[string]any{"second": "2"}},
		}, nil
	}
	exec := NewBranchExecutor(fakeConditionEvaluator{results: map[string]bool{"a": true}}, runner)
	step := engine.ResolvedStep{ID: "branch", Kind: "branch", Spec: &schema.BranchSpec{Branches: []schema.BranchArm{
		{Condition: "a", Steps: []schema.FlowNode{{Step: &schema.Step{ID: "s1", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "echo"}}}}},
	}}}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Vars["first"] != "1" || res.Vars["second"] != "2" {
		t.Fatalf("expected merged vars, got %v", res.Vars)
	}
}

// Regression tests for the vars.all_passed bug
// These tests use the actual SimpleConditionEvaluator to verify
// that branch conditions work correctly with both bare variables
// and vars. prefix syntax

func TestBranchExecutor_ConditionMatchesCorrectArm_DnsFail(t *testing.T) {
	var calledArm string
	runner := func(ctx context.Context, _ SubStepParent, steps []schema.FlowNode, vars map[string]any) ([]*engine.StepResult, error) {
		if len(steps) > 0 && steps[0].Step != nil {
			calledArm = steps[0].Step.ID
		}
		return []*engine.StepResult{{StepID: steps[0].Step.ID, Status: engine.StepStatusCompleted}}, nil
	}

	cond := internalExpr.NewSimpleConditionEvaluator(nil)
	exec := NewBranchExecutor(cond, runner)

	step := engine.ResolvedStep{ID: "branch", Kind: "branch", Spec: &schema.BranchSpec{Branches: []schema.BranchArm{
		{Condition: `all_passed == "dns_fail"`, Label: "dns_fail_arm", Steps: []schema.FlowNode{{Step: &schema.Step{ID: "dns_fail_step", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "echo dns_fail"}}}}},
		{Condition: `all_passed == "routing_fail"`, Label: "routing_fail_arm", Steps: []schema.FlowNode{{Step: &schema.Step{ID: "routing_fail_step", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "echo routing_fail"}}}}},
		{Condition: `all_passed == "all_pass"`, Label: "all_pass_arm", Steps: []schema.FlowNode{{Step: &schema.Step{ID: "all_pass_step", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "echo all_pass"}}}}},
	}}}

	vars := map[string]any{"all_passed": "dns_fail"}
	res, err := exec.Execute(context.Background(), step, vars)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if calledArm != "dns_fail_step" {
		t.Fatalf("expected dns_fail_step to run, got %s", calledArm)
	}
	if res.Output["matched_arm"] != "dns_fail_arm" {
		t.Fatalf("expected matched_arm dns_fail_arm, got %v", res.Output["matched_arm"])
	}
}

func TestBranchExecutor_ConditionMatchesCorrectArm_AllPass(t *testing.T) {
	var calledArm string
	runner := func(ctx context.Context, _ SubStepParent, steps []schema.FlowNode, vars map[string]any) ([]*engine.StepResult, error) {
		if len(steps) > 0 && steps[0].Step != nil {
			calledArm = steps[0].Step.ID
		}
		return []*engine.StepResult{{StepID: steps[0].Step.ID, Status: engine.StepStatusCompleted}}, nil
	}

	cond := internalExpr.NewSimpleConditionEvaluator(nil)
	exec := NewBranchExecutor(cond, runner)

	step := engine.ResolvedStep{ID: "branch", Kind: "branch", Spec: &schema.BranchSpec{Branches: []schema.BranchArm{
		{Condition: `all_passed == "dns_fail"`, Label: "dns_fail_arm", Steps: []schema.FlowNode{{Step: &schema.Step{ID: "dns_fail_step", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "echo dns_fail"}}}}},
		{Condition: `all_passed == "all_pass"`, Label: "all_pass_arm", Steps: []schema.FlowNode{{Step: &schema.Step{ID: "all_pass_step", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "echo all_pass"}}}}},
	}}}

	vars := map[string]any{"all_passed": "all_pass"}
	res, err := exec.Execute(context.Background(), step, vars)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if calledArm != "all_pass_step" {
		t.Fatalf("expected all_pass_step to run, got %s", calledArm)
	}
	if res.Output["matched_arm"] != "all_pass_arm" {
		t.Fatalf("expected matched_arm all_pass_arm, got %v", res.Output["matched_arm"])
	}
}

func TestBranchExecutor_NoMatchReturnsSkipped(t *testing.T) {
	var calledArm string
	runner := func(ctx context.Context, _ SubStepParent, steps []schema.FlowNode, vars map[string]any) ([]*engine.StepResult, error) {
		if len(steps) > 0 && steps[0].Step != nil {
			calledArm = steps[0].Step.ID
		}
		return []*engine.StepResult{}, nil
	}

	cond := internalExpr.NewSimpleConditionEvaluator(nil)
	exec := NewBranchExecutor(cond, runner)

	step := engine.ResolvedStep{ID: "branch", Kind: "branch", Spec: &schema.BranchSpec{Branches: []schema.BranchArm{
		{Condition: `all_passed == "dns_fail"`, Steps: []schema.FlowNode{{Step: &schema.Step{ID: "dns_fail_step", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "echo dns_fail"}}}}},
		{Condition: `all_passed == "routing_fail"`, Steps: []schema.FlowNode{{Step: &schema.Step{ID: "routing_fail_step", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "echo routing_fail"}}}}},
	}}}

	vars := map[string]any{"all_passed": "unknown_value"}
	res, err := exec.Execute(context.Background(), step, vars)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != engine.StepStatusSkipped {
		t.Fatalf("expected skipped status, got %s", res.Status)
	}
	if calledArm != "" {
		t.Fatalf("expected no arm to run, but %s ran", calledArm)
	}
}

func TestBranchExecutor_VarsPrefixCondition(t *testing.T) {
	var calledArm string
	runner := func(ctx context.Context, _ SubStepParent, steps []schema.FlowNode, vars map[string]any) ([]*engine.StepResult, error) {
		if len(steps) > 0 && steps[0].Step != nil {
			calledArm = steps[0].Step.ID
		}
		return []*engine.StepResult{{StepID: steps[0].Step.ID, Status: engine.StepStatusCompleted}}, nil
	}

	cond := internalExpr.NewSimpleConditionEvaluator(nil)
	exec := NewBranchExecutor(cond, runner)

	step := engine.ResolvedStep{ID: "branch", Kind: "branch", Spec: &schema.BranchSpec{Branches: []schema.BranchArm{
		{Condition: `vars.all_passed == "dns_fail"`, Label: "dns_fail_arm", Steps: []schema.FlowNode{{Step: &schema.Step{ID: "dns_fail_step", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "echo dns_fail"}}}}},
		{Condition: `vars.all_passed == "all_pass"`, Label: "all_pass_arm", Steps: []schema.FlowNode{{Step: &schema.Step{ID: "all_pass_step", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "echo all_pass"}}}}},
	}}}

	vars := map[string]any{"all_passed": "dns_fail"}
	res, err := exec.Execute(context.Background(), step, vars)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if calledArm != "dns_fail_step" {
		t.Fatalf("expected dns_fail_step to run with vars. prefix, got %s", calledArm)
	}
	if res.Output["matched_arm"] != "dns_fail_arm" {
		t.Fatalf("expected matched_arm dns_fail_arm, got %v", res.Output["matched_arm"])
	}
}

func TestBranchExecutor_WrongArmDoesNotRun(t *testing.T) {
	callCount := 0
	var calledSteps []string
	runner := func(ctx context.Context, _ SubStepParent, steps []schema.FlowNode, vars map[string]any) ([]*engine.StepResult, error) {
		callCount++
		for _, step := range steps {
			if step.Step != nil {
				calledSteps = append(calledSteps, step.Step.ID)
			}
		}
		return []*engine.StepResult{{StepID: steps[0].Step.ID, Status: engine.StepStatusCompleted}}, nil
	}

	cond := internalExpr.NewSimpleConditionEvaluator(nil)
	exec := NewBranchExecutor(cond, runner)

	step := engine.ResolvedStep{ID: "branch", Kind: "branch", Spec: &schema.BranchSpec{Branches: []schema.BranchArm{
		{Condition: `all_passed == "dns_fail"`, Label: "dns_fail_arm", Steps: []schema.FlowNode{{Step: &schema.Step{ID: "dns_fail_step", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "echo dns_fail"}}}}},
		{Condition: `all_passed == "routing_fail"`, Label: "routing_fail_arm", Steps: []schema.FlowNode{{Step: &schema.Step{ID: "routing_fail_step", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "echo routing_fail"}}}}},
		{Condition: `all_passed == "all_pass"`, Label: "all_pass_arm", Steps: []schema.FlowNode{{Step: &schema.Step{ID: "all_pass_step", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "echo all_pass"}}}}},
	}}}

	vars := map[string]any{"all_passed": "routing_fail"}
	res, err := exec.Execute(context.Background(), step, vars)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if callCount != 1 {
		t.Fatalf("expected runner to be called exactly once, got %d calls", callCount)
	}
	if len(calledSteps) != 1 || calledSteps[0] != "routing_fail_step" {
		t.Fatalf("expected only routing_fail_step to run, got %v", calledSteps)
	}
	if res.Output["matched_arm"] != "routing_fail_arm" {
		t.Fatalf("expected matched_arm routing_fail_arm, got %v", res.Output["matched_arm"])
	}
}
