package executor

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	internalexpr "github.com/ormasoftchile/yawr/runtime/internal/expr"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestIterateExecutor_OverCollection(t *testing.T) {
	var seen []any
	runner := func(ctx context.Context, _ SubStepParent, steps []schema.FlowNode, vars map[string]any) ([]*engine.StepResult, error) {
		seen = append(seen, vars["svc"])
		return []*engine.StepResult{{StepID: "child", Status: engine.StepStatusCompleted}}, nil
	}
	exec := NewIterateExecutor(nil, nil, runner)

	step := engine.ResolvedStep{ID: "iterate", Kind: "iterate", Spec: &schema.IterateNode{
		Over:  "$.services",
		As:    "svc",
		Steps: []schema.FlowNode{{Step: &schema.Step{ID: "s1", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "echo"}}}},
	}}

	res, err := exec.Execute(context.Background(), step, map[string]any{"services": []string{"a", "b"}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Output["iterations"] != 2 {
		t.Fatalf("expected 2 iterations, got %v", res.Output["iterations"])
	}
	if len(seen) != 2 || seen[0] != "a" || seen[1] != "b" {
		t.Fatalf("unexpected iteration vars: %v", seen)
	}
}

func TestIterateExecutor_ProvidesOneBasedExecutionFrameIteration(t *testing.T) {
	var iterationIndices []int
	runner := func(_ context.Context, parent SubStepParent, _ []schema.FlowNode, _ map[string]any) ([]*engine.StepResult, error) {
		iterationIndices = append(iterationIndices, parent.IterationIndex)
		return []*engine.StepResult{{StepID: "child", Status: engine.StepStatusCompleted}}, nil
	}
	exec := NewIterateExecutor(nil, nil, runner)
	step := engine.ResolvedStep{ID: "iterate", Kind: "iterate", Spec: &schema.IterateNode{
		Over: "$.items", Steps: []schema.FlowNode{{Step: &schema.Step{ID: "child", Type: schema.StepTypeNoop}}},
	}}
	if _, err := exec.Execute(context.Background(), step, map[string]any{"items": []string{"a", "b"}}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(iterationIndices) != 2 || iterationIndices[0] != 1 || iterationIndices[1] != 2 {
		t.Fatalf("iteration frame indices = %#v, want [1 2]", iterationIndices)
	}
}

func TestIterateExecutor_MaxLimit(t *testing.T) {
	calls := 0
	runner := func(ctx context.Context, _ SubStepParent, steps []schema.FlowNode, vars map[string]any) ([]*engine.StepResult, error) {
		calls++
		return []*engine.StepResult{{StepID: "child", Status: engine.StepStatusCompleted}}, nil
	}
	exec := NewIterateExecutor(nil, nil, runner)

	step := engine.ResolvedStep{ID: "iterate", Kind: "iterate", Spec: &schema.IterateNode{
		Over:  "5",
		As:    "n",
		Max:   2,
		Steps: []schema.FlowNode{{Step: &schema.Step{ID: "s1", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "echo"}}}},
	}}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Output["iterations"] != 2 {
		t.Fatalf("expected 2 iterations, got %v", res.Output["iterations"])
	}
	if calls != 2 {
		t.Fatalf("expected 2 calls, got %d", calls)
	}
}

func TestIterateExecutor_UntilCondition(t *testing.T) {
	calls := 0
	runner := func(ctx context.Context, _ SubStepParent, steps []schema.FlowNode, vars map[string]any) ([]*engine.StepResult, error) {
		calls++
		return []*engine.StepResult{{StepID: "child", Status: engine.StepStatusCompleted}}, nil
	}
	cond := internalexpr.NewSimpleConditionEvaluator(&internalexpr.TemplateEvaluator{})
	exec := NewIterateExecutor(nil, cond, runner)

	step := engine.ResolvedStep{ID: "iterate", Kind: "iterate", Spec: &schema.IterateNode{
		Over:  "1,2,3",
		As:    "n",
		Until: `n == "2"`,
		Steps: []schema.FlowNode{{Step: &schema.Step{ID: "s1", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "echo"}}}},
	}}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Output["iterations"] != 2 {
		t.Fatalf("expected 2 iterations, got %v", res.Output["iterations"])
	}
	if calls != 2 {
		t.Fatalf("expected 2 calls, got %d", calls)
	}
}

func TestIterateExecutor_ConcurrentExecution(t *testing.T) {
	var mu sync.Mutex
	seen := make(map[any]bool)

	runner := func(ctx context.Context, _ SubStepParent, steps []schema.FlowNode, vars map[string]any) ([]*engine.StepResult, error) {
		item := vars["item"]
		mu.Lock()
		seen[item] = true
		mu.Unlock()
		return []*engine.StepResult{{
			StepID: "child",
			Status: engine.StepStatusCompleted,
			Vars:   map[string]any{"result_" + fmt.Sprint(item): item},
		}}, nil
	}
	exec := NewIterateExecutor(nil, nil, runner)

	step := engine.ResolvedStep{ID: "iterate", Kind: "iterate", Spec: &schema.IterateNode{
		Over:        "1,2,3,4,5,6,7,8,9",
		As:          "item",
		Concurrency: 3,
		Steps:       []schema.FlowNode{{Step: &schema.Step{ID: "s1", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "echo"}}}},
	}}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(seen) != 9 {
		t.Fatalf("expected 9 items processed, got %d", len(seen))
	}
	if res.Status != engine.StepStatusCompleted {
		t.Fatalf("expected status completed, got %v", res.Status)
	}
}

func TestIterateExecutor_ConcurrentWithCollect(t *testing.T) {
	var mu sync.Mutex
	counter := 0

	runner := func(ctx context.Context, _ SubStepParent, steps []schema.FlowNode, vars map[string]any) ([]*engine.StepResult, error) {
		if vars["item"] == "a" {
			time.Sleep(20 * time.Millisecond)
		}
		mu.Lock()
		counter++
		mu.Unlock()
		return []*engine.StepResult{{StepID: "child", Status: engine.StepStatusCompleted}}, nil
	}
	exec := NewIterateExecutor(&internalexpr.TemplateEvaluator{}, nil, runner)

	step := engine.ResolvedStep{ID: "iterate", Kind: "iterate", Spec: &schema.IterateNode{
		Over:        "a,b,c",
		As:          "item",
		Concurrency: 2,
		Collect:     map[string]string{"items": "${item}"},
		Steps:       []schema.FlowNode{{Step: &schema.Step{ID: "s1", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "echo"}}}},
	}}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if counter != 3 {
		t.Fatalf("expected 3 iterations, got %d", counter)
	}
	if got, want := res.Vars["items"], []any{"a", "b", "c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("collect order = %#v, want %#v", got, want)
	}
}

func TestIterateExecutor_ConcurrentRejectsUntil(t *testing.T) {
	exec := NewIterateExecutor(nil, nil, func(context.Context, SubStepParent, []schema.FlowNode, map[string]any) ([]*engine.StepResult, error) {
		return nil, nil
	})
	step := engine.ResolvedStep{ID: "iterate", Kind: "iterate", Spec: &schema.IterateNode{
		Over: "a,b", Concurrency: 2, Until: "done",
	}}
	if _, err := exec.Execute(context.Background(), step, map[string]any{}); err == nil {
		t.Fatal("concurrent until accepted with nondeterministic scheduling")
	}
}

func TestIterateExecutor_ConcurrentFailFast(t *testing.T) {
	var mu sync.Mutex
	calls := 0

	runner := func(ctx context.Context, _ SubStepParent, steps []schema.FlowNode, vars map[string]any) ([]*engine.StepResult, error) {
		mu.Lock()
		calls++
		item := vars["item"]
		mu.Unlock()

		// Simulate a fatal engine-level error for item "5".
		// In the real engine, a fatal failure without on_error: continue bubbles
		// up as a runner error, not just a StepStatusFailed result. Step-level failures with
		// continuing failures return results with StepStatusFailed but no runner error.
		if item == "5" {
			return nil, fmt.Errorf("fatal iteration failure on item 5")
		}
		return []*engine.StepResult{{StepID: "child", Status: engine.StepStatusCompleted}}, nil
	}
	exec := NewIterateExecutor(nil, nil, runner)

	step := engine.ResolvedStep{ID: "iterate", Kind: "iterate", Spec: &schema.IterateNode{
		Over:        "1,2,3,4,5,6,7,8,9,10",
		As:          "item",
		Concurrency: 3,
		Steps:       []schema.FlowNode{{Step: &schema.Step{ID: "s1", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "echo"}}}},
	}}

	_, err := exec.Execute(context.Background(), step, map[string]any{})
	if err == nil {
		t.Fatalf("expected error when iteration fails with runner error, got nil")
	}
}

func TestIterateExecutor_Concurrency0Sequential(t *testing.T) {
	calls := 0
	runner := func(ctx context.Context, _ SubStepParent, steps []schema.FlowNode, vars map[string]any) ([]*engine.StepResult, error) {
		calls++
		return []*engine.StepResult{{StepID: "child", Status: engine.StepStatusCompleted}}, nil
	}
	exec := NewIterateExecutor(nil, nil, runner)

	step := engine.ResolvedStep{ID: "iterate", Kind: "iterate", Spec: &schema.IterateNode{
		Over:        "3",
		As:          "n",
		Concurrency: 0, // Should fall through to sequential
		Steps:       []schema.FlowNode{{Step: &schema.Step{ID: "s1", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "echo"}}}},
	}}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Output["iterations"] != 3 {
		t.Fatalf("expected 3 iterations, got %v", res.Output["iterations"])
	}
	if calls != 3 {
		t.Fatalf("expected 3 calls, got %d", calls)
	}
}

func TestIterateExecutor_Concurrency1Sequential(t *testing.T) {
	calls := 0
	runner := func(ctx context.Context, _ SubStepParent, steps []schema.FlowNode, vars map[string]any) ([]*engine.StepResult, error) {
		calls++
		return []*engine.StepResult{{StepID: "child", Status: engine.StepStatusCompleted}}, nil
	}
	exec := NewIterateExecutor(nil, nil, runner)

	step := engine.ResolvedStep{ID: "iterate", Kind: "iterate", Spec: &schema.IterateNode{
		Over:        "2",
		As:          "n",
		Concurrency: 1, // Should fall through to sequential
		Steps:       []schema.FlowNode{{Step: &schema.Step{ID: "s1", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "echo"}}}},
	}}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Output["iterations"] != 2 {
		t.Fatalf("expected 2 iterations, got %v", res.Output["iterations"])
	}
	if calls != 2 {
		t.Fatalf("expected 2 calls, got %d", calls)
	}
}
