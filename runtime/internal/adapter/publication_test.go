package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	internalengine "github.com/ormasoftchile/yawr/runtime/internal/engine"
	"github.com/ormasoftchile/yawr/runtime/internal/executor"
	internalplanner "github.com/ormasoftchile/yawr/runtime/internal/planner"
	internalrunstore "github.com/ormasoftchile/yawr/runtime/internal/runstore"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

type publicationExecutorFunc func(context.Context, engine.ResolvedStep, map[string]any) (*engine.StepResult, error)

func (f publicationExecutorFunc) Execute(ctx context.Context, step engine.ResolvedStep, vars map[string]any) (*engine.StepResult, error) {
	return f(ctx, step, vars)
}

type publicationStore struct {
	*internalrunstore.DirRunStore
	fail bool
}

func (s *publicationStore) WriteTrace(ctx context.Context, runID string, event engine.Event) error {
	if s.fail && event.Kind == "step/started" && event.Payload["step_id"] == "child-1" {
		return errors.New("injected parent store failure")
	}
	return s.DirRunStore.WriteTrace(ctx, runID, event)
}

type publicationTrace struct{ fail bool }

func (*publicationTrace) Close() error { return nil }
func (w *publicationTrace) Append(event trace.TraceEvent) error {
	if w.fail && event.Kind == "step/started" && strings.Contains(string(event.Payload), `"step_id":"child-1"`) {
		return errors.New("injected parent trace failure")
	}
	return nil
}

func TestPublicationActualNestedForwarderBarrierAndFailure(t *testing.T) {
	for _, mode := range []string{"parallel-observer", "trace-failure", "store-failure", "cancel-parent"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			store := &publicationStore{DirRunStore: internalrunstore.NewDirRunStore(t.TempDir()), fail: mode == "store-failure"}
			defer store.Close()
			plat := platform.NewFakePlatform()
			registry := &testRegistry{executors: make(map[string]engine.StepExecutor)}
			runner := func(ctx context.Context, parent executor.SubStepParent, steps []schema.FlowNode, vars map[string]any) ([]*engine.StepResult, error) {
				return runSubStepsViaEngine(ctx, registry, parent, steps, vars, engine.RunModeReal,
					&noopTraceWriter{}, &noopDispatcher{}, plat, nil, nil, nil, nil, nil, nil, nil)
			}
			registry.Register("include", executor.NewIncludeExecutor(nil, runner, nil))
			entered, gate, releaseWork := make(chan struct{}), make(chan struct{}), make(chan struct{})
			work := make(chan string, 4)
			var once sync.Once
			var calls atomic.Int32
			var mu sync.Mutex
			observed := map[string]engine.Event{}
			registry.Register("cli", publicationExecutorFunc(func(ctx context.Context, step engine.ResolvedStep, _ map[string]any) (*engine.StepResult, error) {
				calls.Add(1)
				nodeID := engine.DebugNodeID(engine.DebugCallPathFromContext(ctx), step.ID)
				mu.Lock()
				event, visible := observed[nodeID]
				mu.Unlock()
				if !visible {
					t.Error("child executed without its parent observer having returned")
				}
				traceBytes, err := os.ReadFile(store.TracePath(engine.RunIDFromContext(ctx)))
				if err != nil {
					t.Error(err)
				}
				found := false
				var prior int64
				for _, line := range strings.Split(strings.TrimSpace(string(traceBytes)), "\n") {
					var saved struct {
						engine.Event
						Seq int64 `json:"seq"`
					}
					if err := json.Unmarshal([]byte(line), &saved); err != nil {
						t.Error(err)
						continue
					}
					if saved.Sequence == 0 {
						saved.Sequence = saved.Seq
					}
					if saved.Sequence <= prior {
						t.Errorf("parent trace sequence not strictly ordered: %s", line)
					}
					prior = saved.Sequence
					if saved.EventID == event.EventID && saved.Sequence == event.Sequence &&
						saved.Payload["qualified_node_id"] == nodeID {
						found = true
					}
				}
				if !found {
					t.Error("exact parent start identity not durable at child entry")
				}
				work <- nodeID
				select {
				case <-releaseWork:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return &engine.StepResult{StepID: step.ID, Status: engine.StepStatusCompleted, Outcome: engine.StepOutcomeSuccess}, nil
			}))
			plan := nestedDispatchIncludePlan(t, "publication-"+mode)
			if mode == "parallel-observer" {
				// Two independent adapter sub-engines share the parent's callback
				// owner; both must enter external work before either finishes.
				parallel := &schema.ParallelNode{ID: "parallel"}
				for _, label := range []string{"left", "right"} {
					parallel.Branches = append(parallel.Branches, schema.ParallelBranch{Label: label, Steps: []schema.FlowNode{
						{Step: &schema.Step{ID: label, Type: schema.StepTypeInclude, IncludeSpec: &schema.IncludeSpec{
							Include:       schema.IncludeConfig{Runbook: "child.runbook.yaml"},
							ResolvedSteps: []schema.FlowNode{{Step: &schema.Step{ID: "child-1", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "synthetic"}}}},
						}}},
					}})
				}
				plan.Steps = []engine.ResolvedStep{{ID: "parallel", Kind: "parallel", Spec: parallel}}
				for _, branch := range parallel.Branches {
					plan.Steps = append(plan.Steps, engine.ResolvedStep{
						ID: branch.Label, Kind: "include", Spec: branch.Steps[0].Step.IncludeSpec,
						Depth: 1, ParentID: "parallel", ParentKind: "parallel", BranchLabel: branch.Label,
					})
				}
				if err := internalplanner.ValidateExecutionPlan(plan); err != nil {
					t.Fatal(err)
				}
			}
			var handle engine.RunHandle
			onEvent := func(event engine.Event) {
				_ = handle.State()
				if event.Kind != "step/started" || event.Payload["step_id"] != "child-1" {
					return
				}
				if mode == "cancel-parent" {
					if err := handle.Cancel(ctx, "nested start"); err != nil {
						t.Error(err)
					}
				}
				once.Do(func() {
					close(entered)
					select {
					case <-gate:
					case <-ctx.Done():
					}
				})
				mu.Lock()
				observed[event.Payload["qualified_node_id"].(string)] = event
				mu.Unlock()
			}
			runtime := internalengine.New(engine.EngineConfig{
				Executors: registry, Dispatcher: &noopDispatcher{}, TraceWriter: &publicationTrace{fail: mode == "trace-failure"},
				Platform: plat, Store: store, OnEvent: onEvent,
			})
			var err error
			handle, err = runtime.Start(ctx, plan, engine.RunOptions{Mode: engine.RunModeReal, Store: store})
			if err != nil {
				t.Fatal(err)
			}
			finished := make(chan error, 1)
			go func() { _, err := handle.Next(ctx); finished <- err }()
			if strings.HasSuffix(mode, "failure") {
				select {
				case err := <-finished:
					if !errors.Is(err, engine.ErrTraceCommit) {
						t.Fatalf("parent publication failure was lost: %v", err)
					}
				case <-ctx.Done():
					t.Fatal("parent publication failure stranded child")
				}
				if calls.Load() != 0 {
					t.Fatal("failed publication dispatched external work")
				}
				return
			}
			select {
			case <-entered:
			case err := <-finished:
				t.Fatalf("nested run ended before callback: %v", err)
			case <-ctx.Done():
				t.Fatal("parent callback never entered")
			}
			select {
			case <-work:
				t.Fatal("child escaped held parent observer")
			case <-time.After(100 * time.Millisecond):
			}
			close(gate)
			if mode == "parallel-observer" {
				for range 2 {
					select {
					case <-work:
					case <-ctx.Done():
						t.Fatal("nested branch work serialized or publication stranded")
					}
				}
			}
			close(releaseWork)
			select {
			case err := <-finished:
				if err != nil && !(mode == "cancel-parent" && errors.Is(err, context.Canceled)) {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("nested Next did not settle")
			}
			if mode == "cancel-parent" && calls.Load() != 0 {
				t.Fatal("cancelled parent allowed child dispatch")
			}
		})
	}
}
