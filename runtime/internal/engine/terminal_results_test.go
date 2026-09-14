package engine

import (
	"context"
	"io"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/executor"
	"github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/internal/runstore"
	enginepkg "github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestTerminalResultsNoPublicationOnUnsuccessfulWork(t *testing.T) {
	for _, status := range []enginepkg.StepStatus{
		enginepkg.StepStatusFailed, enginepkg.StepStatusDenied, enginepkg.StepStatusIndeterminate,
		enginepkg.StepStatusWaiting, enginepkg.StepStatusPending, enginepkg.StepStatusRunning,
		enginepkg.StepStatusCompleted,
	} {
		t.Run(string(status), func(t *testing.T) {
			cfg := makeTestConfig()
			registry := newFakeExecutorRegistry()
			registry.Register("noop", stepExecutorFunc(func(_ context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
				return &enginepkg.StepResult{StepID: step.ID, Status: status, RequiredFailure: status == enginepkg.StepStatusCompleted}, nil
			}))
			registry.Register("end", executor.NewEndExecutor())
			cfg.Executors = registry
			store := runstore.NewDirRunStore(t.TempDir())
			defer store.Close()
			cfg.Store = store
			plan := &enginepkg.ExecutionPlan{RunID: "terminal-failed", RunbookPath: "terminal.yaml",
				Metadata: enginepkg.PlanMetadata{RunbookID: "terminal"},
				Outputs:  map[string]*schema.Output{"result": {Type: "string", ValueTree: "must-not-publish", ValueTreePresent: true}},
				Steps: []enginepkg.ResolvedStep{
					{ID: "required", Kind: "noop", Spec: &schema.NoopSpec{}, OnError: "continue"},
					{ID: "end", Kind: "end", Spec: &schema.EndSpec{PublishResults: true}},
					{ID: "tail", Kind: "noop", Spec: &schema.NoopSpec{}},
				},
			}
			if err := planner.ValidateExecutionPlan(plan); err != nil {
				t.Fatal(err)
			}
			handle, err := New(cfg).Start(context.Background(), plan, enginepkg.RunOptions{})
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 4; i++ {
				_, err := handle.Next(context.Background())
				if err != nil || handle.State().Status == enginepkg.RunStatusWaiting {
					break
				}
			}
			state := handle.State()
			if state.Results != nil {
				t.Fatal("published unsuccessful required work")
			}
			if state.StepResults["tail"] != nil {
				t.Fatal("dispatched after terminal failure")
			}
			loaded, err := store.LoadState(context.Background(), plan.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if loaded.Results != nil {
				t.Fatal("unsuccessful publication persisted")
			}
		})
	}
}

func TestTerminalResultsCancelledAndInvalidOutput(t *testing.T) {
	for _, cancel := range []bool{true, false} {
		t.Run(map[bool]string{true: "cancelled", false: "invalid-output"}[cancel], func(t *testing.T) {
			cfg := makeTestConfig()
			registry := newFakeExecutorRegistry()
			registry.Register("end", executor.NewEndExecutor())
			cfg.Executors = registry
			store := runstore.NewDirRunStore(t.TempDir())
			defer store.Close()
			cfg.Store = store
			plan := &enginepkg.ExecutionPlan{RunID: "terminal-cancel", RunbookPath: "terminal.yaml",
				Metadata: enginepkg.PlanMetadata{RunbookID: "terminal"},
				Outputs:  map[string]*schema.Output{"result": {Type: "string", ValueTree: 42, ValueTreePresent: true}},
				Steps:    []enginepkg.ResolvedStep{{ID: "end", Kind: "end", Spec: &schema.EndSpec{PublishResults: true}}},
			}
			if err := planner.ValidateExecutionPlan(plan); err != nil {
				t.Fatal(err)
			}
			handle, err := New(cfg).Start(context.Background(), plan, enginepkg.RunOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if cancel {
				if err := handle.Cancel(context.Background(), "operator cancelled"); err != nil {
					t.Fatal(err)
				}
			}
			result, err := handle.Next(context.Background())
			if !cancel && (err != nil && err != io.EOF || result == nil || result.Error == nil) {
				t.Fatalf("invalid output not diagnosed: result=%#v err=%v", result, err)
			}
			if handle.State().Results != nil {
				t.Fatal("published cancelled/invalid run")
			}
			durable, err := store.LoadState(context.Background(), plan.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if durable.Results != nil {
				t.Fatal("persisted cancelled/invalid publication")
			}
		})
	}
}
