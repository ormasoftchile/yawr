package adapter

import (
	"context"
	"errors"
	"io"
	"reflect"
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

type completedReturnBarrier struct {
	inner   engine.StepExecutor
	ready   chan struct{}
	release chan struct{}
}

func (barrier *completedReturnBarrier) Execute(ctx context.Context, step engine.ResolvedStep, vars map[string]any) (*engine.StepResult, error) {
	result, err := barrier.inner.Execute(ctx, step, vars)
	if err != nil {
		return result, err
	}
	close(barrier.ready)
	<-barrier.release
	return result, nil
}

func TestRunSubStepsViaEngine_ResumeDoesNotExecuteTerminalTail(t *testing.T) {
	base := t.TempDir()
	firstStore := internalrunstore.NewDirRunStore(base)
	t.Cleanup(func() { _ = firstStore.Close() })
	plat := platform.NewFakePlatform()
	makeConfig := func(store *internalrunstore.DirRunStore, recorder *recordingPassExecutor, barrier *completedReturnBarrier) engine.EngineConfig {
		registry := &testRegistry{executors: make(map[string]engine.StepExecutor)}
		runner := func(ctx context.Context, parent executor.SubStepParent, nodes []schema.FlowNode, vars map[string]any) ([]*engine.StepResult, error) {
			return runSubStepsViaEngine(ctx, registry, parent, nodes, vars, engine.RunModeReal,
				&noopTraceWriter{}, &noopDispatcher{}, plat, nil, nil, nil, nil, nil, nil, nil)
		}
		registry.Register("cli", recorder)
		registry.Register("end", executor.NewEndExecutor())
		include := executor.NewIncludeExecutor(nil, runner, nil)
		if barrier != nil {
			barrier.inner = include
			registry.Register("include", barrier)
		} else {
			registry.Register("include", include)
		}
		return engine.EngineConfig{
			Executors: registry, Dispatcher: &noopDispatcher{}, TraceWriter: &noopTraceWriter{},
			Platform: plat, Store: store,
		}
	}
	plan := &engine.ExecutionPlan{
		RunID: "resume-terminal-tail", RunbookPath: "root.runbook.yaml",
		Steps: []engine.ResolvedStep{
			{ID: "call", Kind: "include", Spec: &schema.IncludeSpec{
				Include: schema.IncludeConfig{Runbook: "child.runbook.yaml"},
				ResolvedSteps: []schema.FlowNode{
					{Step: &schema.Step{ID: "before", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "before"}}},
					{Step: &schema.Step{ID: "return", Type: schema.StepTypeEnd, EndSpec: &schema.EndSpec{}}},
					{Step: &schema.Step{ID: "must-not-run", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "must-not-run"}}},
				},
			}},
			{ID: "after", Kind: "cli", Spec: &schema.CLISpec{Command: "after"}},
		},
		Metadata: engine.PlanMetadata{RunbookID: "root"},
	}
	if err := internalplanner.ValidateExecutionPlan(plan); err != nil {
		t.Fatal(err)
	}
	firstRecorder := &recordingPassExecutor{}
	barrier := &completedReturnBarrier{ready: make(chan struct{}), release: make(chan struct{})}
	first, err := internalengine.New(makeConfig(firstStore, firstRecorder, barrier)).Start(context.Background(), plan, engine.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := first.Next(context.Background())
		done <- err
	}()
	t.Cleanup(func() {
		close(barrier.release)
		select {
		case staleErr := <-done:
			if !errors.Is(staleErr, engine.ErrCheckpointCommit) && !errors.Is(staleErr, engine.ErrRunLeaseStale) {
				t.Errorf("stale writer error = %v", staleErr)
			}
		case <-time.After(5 * time.Second):
			t.Error("stale writer did not stop")
		}
	})
	select {
	case <-barrier.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("did not reach committed return")
	}
	checkpoint, err := firstStore.LoadState(context.Background(), plan.RunID)
	if err != nil {
		t.Fatal(err)
	}
	for _, frame := range checkpoint.ExecutionFrames {
		if frame.Status != engine.ExecutionFrameStatusActive || frame.NextStepIndex != 3 ||
			frame.Results["2"].Status != engine.StepStatusSkipped {
			t.Fatalf("return was not checkpointed atomically: %#v", frame)
		}
	}
	if err := firstStore.Close(); err != nil {
		t.Fatal(err)
	}
	secondStore := internalrunstore.NewDirRunStore(base)
	t.Cleanup(func() { _ = secondStore.Close() })
	secondRecorder := &recordingPassExecutor{}
	second, err := internalengine.New(makeConfig(secondStore, secondRecorder, nil)).Resume(context.Background(), plan.RunID, engine.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for {
		_, err := second.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(firstRecorder.steps, []string{"before"}) ||
		!reflect.DeepEqual(secondRecorder.steps, []string{"after"}) {
		t.Fatalf("executions before/after resume: %v / %v", firstRecorder.steps, secondRecorder.steps)
	}
}
