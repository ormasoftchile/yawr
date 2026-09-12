package adapter

import (
	"context"
	"errors"
	"testing"
	"time"

	internalengine "github.com/ormasoftchile/yawr/runtime/internal/engine"
	"github.com/ormasoftchile/yawr/runtime/internal/executor"
	internalplanner "github.com/ormasoftchile/yawr/runtime/internal/planner"
	internalrunstore "github.com/ormasoftchile/yawr/runtime/internal/runstore"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

type callerCancelledNestedDispatchExecutor struct {
	prepared chan struct{}
}

func (executorImpl *callerCancelledNestedDispatchExecutor) Execute(
	ctx context.Context,
	step engine.ResolvedStep,
	_ map[string]any,
) (*engine.StepResult, error) {
	if _, err := engine.PrepareExternalDispatch(ctx, engine.DispatchRequest{
		Classification: "mutating", EndpointIdentity: "nested-cancel-provider",
		RenderedRequest: map[string]any{"step": step.ID},
	}); err != nil {
		return nil, err
	}
	close(executorImpl.prepared)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestNestedCallerCancellationCommitsMutatingDispatchIndeterminate(t *testing.T) {
	store := internalrunstore.NewDirRunStore(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	platformFake := platform.NewFakePlatform()
	dispatchExecutor := &callerCancelledNestedDispatchExecutor{prepared: make(chan struct{})}
	registry := &testRegistry{executors: make(map[string]engine.StepExecutor)}
	var subRunner executor.SubStepRunner
	subRunner = func(ctx context.Context, parent executor.SubStepParent, steps []schema.FlowNode, vars map[string]any) ([]*engine.StepResult, error) {
		return runSubStepsViaEngine(
			ctx, registry, parent, steps, vars, engine.RunModeReal,
			&noopTraceWriter{}, &noopDispatcher{}, platformFake,
			nil, nil, nil, nil, nil, nil, nil,
		)
	}
	registry.Register("cli", dispatchExecutor)
	registry.Register("include", executor.NewIncludeExecutor(nil, subRunner, nil))
	plan := &engine.ExecutionPlan{
		RunID: "run-nested-caller-cancel", RunbookPath: "nested-cancel.runbook.yaml",
		Steps: []engine.ResolvedStep{{
			ID: "include", Kind: "include", Spec: &schema.IncludeSpec{
				Include: schema.IncludeConfig{Runbook: "child.runbook.yaml"},
				ResolvedSteps: []schema.FlowNode{{Step: &schema.Step{
					ID: "child", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "mutate"},
				}}},
			},
		}},
		Metadata: engine.PlanMetadata{RunbookID: "nested-caller-cancel"},
	}
	if err := internalplanner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	runtime := internalengine.New(engine.EngineConfig{
		Executors: registry, Dispatcher: &noopDispatcher{}, TraceWriter: &noopTraceWriter{},
		Platform: platformFake, Store: store,
	})
	handle, err := runtime.Start(context.Background(), plan, engine.RunOptions{Mode: engine.RunModeReal, Store: store})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	nextCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, nextErr := handle.Next(nextCtx)
		done <- nextErr
	}()
	select {
	case <-dispatchExecutor.prepared:
	case <-time.After(time.Second):
		t.Fatal("nested dispatch was not prepared")
	}
	cancel()
	select {
	case nextErr := <-done:
		if !errors.Is(nextErr, engine.ErrIndeterminate) {
			t.Fatalf("Next error = %v, want ErrIndeterminate", nextErr)
		}
	case <-time.After(time.Second):
		t.Fatal("nested cancellation did not return")
	}
	state := handle.State()
	if state.Status != engine.RunStatusIndeterminate {
		t.Fatalf("run status = %s, want indeterminate", state.Status)
	}
	for _, dispatch := range state.Dispatches {
		if dispatch.EndpointIdentity == "nested-cancel-provider" && dispatch.Status != engine.DispatchStatusIndeterminate {
			t.Fatalf("nested dispatch status = %s, want indeterminate", dispatch.Status)
		}
	}
}
