package adapter

import (
	"context"
	"io"
	"reflect"
	"strconv"
	"testing"

	internalengine "github.com/ormasoftchile/yawr/runtime/internal/engine"
	"github.com/ormasoftchile/yawr/runtime/internal/executor"
	internalexpr "github.com/ormasoftchile/yawr/runtime/internal/expr"
	internalplanner "github.com/ormasoftchile/yawr/runtime/internal/planner"
	internalrunstore "github.com/ormasoftchile/yawr/runtime/internal/runstore"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestRunSubStepsViaEngine_NestedEarlyReturnSettlesFrames(t *testing.T) {
	for _, kind := range []string{"include", "branch", "iterate"} {
		t.Run(kind, func(t *testing.T) {
			store := internalrunstore.NewDirRunStore(t.TempDir())
			t.Cleanup(func() { _ = store.Close() })
			plat := platform.NewFakePlatform()
			recorder := &recordingPassExecutor{}
			registry := &testRegistry{executors: make(map[string]engine.StepExecutor)}
			evaluator := &internalexpr.TemplateEvaluator{}
			condition := &internalexpr.SimpleConditionEvaluator{}
			runner := func(ctx context.Context, parent executor.SubStepParent, nodes []schema.FlowNode, vars map[string]any) ([]*engine.StepResult, error) {
				return runSubStepsViaEngine(ctx, registry, parent, nodes, vars, engine.RunModeReal,
					&noopTraceWriter{}, &noopDispatcher{}, plat, nil, nil, nil, nil, evaluator, condition, nil)
			}
			registry.Register("cli", recorder)
			registry.Register("end", executor.NewEndExecutor())
			registry.Register("include", executor.NewIncludeExecutor(evaluator, runner, nil))
			registry.Register("branch", executor.NewBranchExecutor(condition, runner))
			registry.Register("iterate", executor.NewIterateExecutor(evaluator, condition, runner))
			leaf := func(id string) schema.FlowNode {
				return schema.FlowNode{Step: &schema.Step{
					ID: id, Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: id},
				}}
			}
			returning := []schema.FlowNode{
				leaf("before-return"),
				{Step: &schema.Step{ID: "return", Type: schema.StepTypeEnd, EndSpec: &schema.EndSpec{}}},
				leaf("must-not-run"),
			}
			for index := 0; index < 24; index++ {
				returning = append(returning, leaf("unreachable-"+strconv.Itoa(index)))
			}
			var nested schema.FlowNode
			switch kind {
			case "include":
				nested.Step = &schema.Step{ID: "nested", Type: schema.StepTypeInclude, IncludeSpec: &schema.IncludeSpec{
					Include: schema.IncludeConfig{Runbook: "nested.runbook.yaml"}, ResolvedSteps: returning,
				}}
			case "branch":
				nested.Step = &schema.Step{ID: "nested", Type: schema.StepTypeBranch, BranchSpec: &schema.BranchSpec{
					Branches: []schema.BranchArm{{Condition: "true", Steps: returning}},
				}}
			case "iterate":
				nested.Iterate = &schema.IterateNode{ID: "nested", Over: "items", As: "item", Steps: returning}
			}
			plan := &engine.ExecutionPlan{
				RunID: "early-return-" + kind, RunbookPath: "root.runbook.yaml",
				Steps: []engine.ResolvedStep{
					{ID: "outer", Kind: "include", Spec: &schema.IncludeSpec{
						Include:       schema.IncludeConfig{Runbook: "outer.runbook.yaml"},
						ResolvedSteps: []schema.FlowNode{nested, leaf("outer-after")},
					}},
					{ID: "root-after", Kind: "cli", Spec: &schema.CLISpec{Command: "root-after"}},
				},
				Metadata: engine.PlanMetadata{RunbookID: "early-return"},
			}
			if err := internalplanner.ValidateExecutionPlan(plan); err != nil {
				t.Fatal(err)
			}
			runtime := internalengine.New(engine.EngineConfig{
				Executors: registry, Dispatcher: &noopDispatcher{}, TraceWriter: &noopTraceWriter{},
				Platform: plat, Store: store,
			})
			handle, err := runtime.Start(context.Background(), plan, engine.RunOptions{
				Store: store, RuntimeVars: map[string]any{"items": []any{"first", "second"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = handle.Cancel(context.Background(), "test cleanup") })
			for {
				_, err := handle.Next(context.Background())
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatalf("Next: %v", err)
				}
			}
			want := []string{"before-return", "root-after"}
			if kind == "include" {
				want = []string{"before-return", "outer-after", "root-after"}
			}
			if !reflect.DeepEqual(recorder.steps, want) {
				t.Fatalf("executed = %v, want %v", recorder.steps, want)
			}
			state, err := store.LoadState(context.Background(), plan.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if state.Status != engine.RunStatusCompleted || len(state.ExecutionFrames) != 2 {
				t.Fatalf("status=%s frames=%d", state.Status, len(state.ExecutionFrames))
			}
			for _, frame := range state.ExecutionFrames {
				if frame.Status != engine.ExecutionFrameStatusCompleted || frame.NextStepIndex != frame.StepCount {
					t.Fatalf("unsettled frame: %#v", frame)
				}
				if frame.ParentStepID == "nested" {
					for index := 2; index < frame.StepCount; index++ {
						skipped := frame.Results[strconv.Itoa(index)]
						if skipped == nil || skipped.Status != engine.StepStatusSkipped || skipped.Output["skip_reason"] != "terminal" {
							t.Fatalf("unexecuted tail was not checkpointed as skipped: %#v", skipped)
						}
					}
				}
			}
		})
	}
}
