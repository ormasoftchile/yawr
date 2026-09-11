package executor

import (
	"context"
	"reflect"
	"strconv"
	"strings"
	"testing"

	internalexpr "github.com/ormasoftchile/yawr/runtime/internal/expr"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestIterateTypedCollections(t *testing.T) {
	for _, concurrency := range []int{1, 3} {
		t.Run(strconv.Itoa(concurrency), func(t *testing.T) {
			runner := func(_ context.Context, _ SubStepParent, _ []schema.FlowNode, vars map[string]any) ([]*engine.StepResult, error) {
				return []*engine.StepResult{{
					StepID: "child", Status: engine.StepStatusCompleted,
					Vars: map[string]any{"rows": []any{map[string]any{"value": vars["item"]}}},
				}}, nil
			}
			executor := NewIterateExecutor(&internalexpr.TemplateEvaluator{}, nil, runner)
			spec := &schema.IterateNode{
				Over: "items", Concurrency: concurrency, Collect: map[string]string{"legacy": "${item}"},
				CollectValues: map[string]any{
					"records": map[string]any{
						"item": "${item}", "rows": "${rows}", "active": true,
						"metadata": []any{2.5, nil, "literal-${item}"},
					},
				},
			}
			result, err := executor.Execute(context.Background(), engine.ResolvedStep{ID: "loop", Kind: "iterate", Spec: spec},
				map[string]any{"items": []any{3, 1, 2}})
			if err != nil {
				t.Fatal(err)
			}
			want := []any{}
			for _, item := range []int{3, 1, 2} {
				want = append(want, map[string]any{
					"item": item, "rows": []any{map[string]any{"value": item}}, "active": true,
					"metadata": []any{2.5, nil, "literal-" + strconv.Itoa(item)},
				})
			}
			if !reflect.DeepEqual(result.Vars["records"], want) {
				t.Fatalf("typed associations = %#v, want %#v", result.Vars["records"], want)
			}
			for _, value := range result.Vars["legacy"].([]any) {
				if _, ok := value.(string); !ok {
					t.Fatalf("legacy collect changed type: %T", value)
				}
			}
		})
	}
}

func TestIterateTypedCollectionEmptyAndInvalid(t *testing.T) {
	calls := 0
	runner := func(context.Context, SubStepParent, []schema.FlowNode, map[string]any) ([]*engine.StepResult, error) {
		calls++
		return nil, nil
	}
	executor := NewIterateExecutor(&internalexpr.TemplateEvaluator{}, nil, runner)
	spec := &schema.IterateNode{Over: "items", CollectValues: map[string]any{"records": "${missing}"}}
	step := engine.ResolvedStep{ID: "loop", Kind: "iterate", Spec: spec}
	result, err := executor.Execute(context.Background(), step, map[string]any{"items": []any{}})
	if err != nil || calls != 0 || !reflect.DeepEqual(result.Vars["records"], []any{}) {
		t.Fatalf("empty collection = %#v, %v; calls=%d", result, err, calls)
	}
	for _, concurrency := range []int{1, 2} {
		spec.Concurrency = concurrency
		if _, err := executor.Execute(context.Background(), step, map[string]any{"items": []any{1}}); err == nil {
			t.Fatal("unresolved collect_values expression did not fail")
		}
	}
	spec.Collect = map[string]string{"records": "literal"}
	if _, err := executor.Execute(context.Background(), step, nil); err == nil || !strings.Contains(err.Error(), "both") {
		t.Fatalf("overlapping collection destinations: %v", err)
	}
}
