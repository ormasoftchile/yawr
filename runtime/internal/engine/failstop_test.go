package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/ormasoftchile/yawr/runtime/internal/executor"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

// alwaysTrueCondEval is a test ConditionEvaluator that returns true for any condition.
type alwaysTrueCondEval struct{}

func (a *alwaysTrueCondEval) EvalBool(_ string, _ map[string]any) (bool, error) {
	return true, nil
}

// TestFailStop_FirstToolFail_StopsRun verifies that a failed step with no
// recovery stops the run: subsequent steps never execute.
func TestFailStop_FirstToolFail_StopsRun(t *testing.T) {
	tw := &fakeTraceWriter{}

	reg := newFakeExecutorRegistry()
	reg.Register("tool", &failingExecutor{err: errors.New("tool error")})
	reg.Register("branch", &passThroughExecutor{})
	reg.Register("display", &passThroughExecutor{})
	reg.Register("end", &passThroughExecutor{})

	cfg := engine.EngineConfig{
		Executors:   reg,
		Dispatcher:  newFakeEventDispatcher(),
		TraceWriter: tw,
		Platform:    platform.NewFakePlatform(),
	}
	eng := New(cfg)

	plan := makeTestPlan(
		engine.ResolvedStep{ID: "get_icm_incident", Kind: "tool", Spec: &cliStepSpec{}},
		engine.ResolvedStep{ID: "recommend_tsg", Kind: "tool", Spec: &cliStepSpec{}},
		engine.ResolvedStep{ID: "show_result", Kind: "branch", Spec: &cliStepSpec{}},
		engine.ResolvedStep{ID: "display", Kind: "display", Spec: &cliStepSpec{}},
		engine.ResolvedStep{ID: "done", Kind: "end", Spec: &cliStepSpec{}},
	)

	handle, err := eng.Start(context.Background(), engine.ValidatedForTest(plan), engine.RunOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	go func() {
		for range handle.Events() {
		}
	}()

	// First call: the failed step is returned.
	result, err := handle.Next(context.Background())
	if err != nil {
		t.Fatalf("Next returned error: %v", err)
	}
	if result == nil || result.Status != engine.StepStatusFailed {
		t.Fatalf("expected StepStatusFailed, got %+v", result)
	}
	if result.StepID != "get_icm_incident" {
		t.Fatalf("expected step get_icm_incident, got %s", result.StepID)
	}

	// Second call: EOF (run is done).
	_, err = handle.Next(context.Background())
	if err != io.EOF {
		t.Fatalf("expected io.EOF, got %v", err)
	}

	// Verify run state is failed.
	state := handle.State()
	if state.Status != engine.RunStatusFailed {
		t.Fatalf("expected RunStatusFailed, got %s", state.Status)
	}

	// Verify trace events.
	time.Sleep(10 * time.Millisecond)
	traceEvents := tw.collect()

	var foundRunFailed bool
	stepStartedCount := 0
	for _, ev := range traceEvents {
		if ev.Kind == trace.EventKindRunFailed {
			foundRunFailed = true
		}
		if ev.Kind == trace.EventKindStepStarted {
			stepStartedCount++
		}
	}

	if !foundRunFailed {
		t.Error("expected run/failed in trace")
	}
	if stepStartedCount != 1 {
		t.Errorf("expected exactly 1 step/started event (only the failed step), got %d", stepStartedCount)
	}

	// Verify no step/started occurs after run/failed.
	var runFailedIdx int
	for i, ev := range traceEvents {
		if ev.Kind == trace.EventKindRunFailed {
			runFailedIdx = i
			break
		}
	}
	for i := runFailedIdx + 1; i < len(traceEvents); i++ {
		if traceEvents[i].Kind == trace.EventKindStepStarted {
			t.Errorf("step/started event at index %d after run/failed at index %d", i, runFailedIdx)
		}
	}
}

// TestFailStop_OnErrorContinue_Continues verifies that explicit
// on_error: continue allows the run to continue past a failure.
func TestFailStop_OnErrorContinue_Continues(t *testing.T) {
	reg := newFakeExecutorRegistry()
	reg.Register("tool", &failingExecutor{err: errors.New("expected failure")})
	reg.Register("end", &passThroughExecutor{})

	cfg := engine.EngineConfig{
		Executors:   reg,
		Dispatcher:  newFakeEventDispatcher(),
		TraceWriter: &fakeTraceWriter{},
		Platform:    platform.NewFakePlatform(),
	}
	eng := New(cfg)

	plan := makeTestPlan(
		engine.ResolvedStep{ID: "failing-step", Kind: "tool", Spec: &cliStepSpec{}, OnError: "continue"},
		engine.ResolvedStep{ID: "next-step", Kind: "end", Spec: &cliStepSpec{}},
	)

	handle, err := eng.Start(context.Background(), engine.ValidatedForTest(plan), engine.RunOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	go func() {
		for range handle.Events() {
		}
	}()

	// First step fails but continues.
	result, err := handle.Next(context.Background())
	if err != nil {
		t.Fatalf("Next[0] error: %v", err)
	}
	if result.StepID != "failing-step" || result.Status != engine.StepStatusFailed {
		t.Fatalf("expected failing-step/Failed, got %s/%s", result.StepID, result.Status)
	}

	// Second step executes.
	result, err = handle.Next(context.Background())
	if err != nil {
		t.Fatalf("Next[1] error: %v", err)
	}
	if result.StepID != "next-step" || result.Status != engine.StepStatusCompleted {
		t.Fatalf("expected next-step/Completed, got %s/%s", result.StepID, result.Status)
	}
}

// TestFailStop_ParallelBranchFail_StopsLaterSiblings verifies that a failed
// parallel branch (container) stops subsequent top-level steps.
func TestFailStop_ParallelBranchFail_StopsLaterSiblings(t *testing.T) {
	reg := newFakeExecutorRegistry()
	reg.Register("tool", &failingExecutor{err: errors.New("branch step failed")})
	reg.Register("display", &passThroughExecutor{})

	tw := &fakeTraceWriter{}
	cfg := engine.EngineConfig{
		Executors:   reg,
		Dispatcher:  newFakeEventDispatcher(),
		TraceWriter: tw,
		Platform:    platform.NewFakePlatform(),
	}
	eng := New(cfg)

	plan := makeTestPlan(
		engine.ResolvedStep{
			ID:   "parallel-container",
			Kind: "parallel",
			Spec: &testParallelSpec{
				branches: []engine.BranchSpec{
					{
						Label: "branch-a",
						Steps: []engine.ResolvedStep{
							{ID: "branch-step", Kind: "tool", Spec: &cliStepSpec{}},
						},
					},
				},
			},
		},
		engine.ResolvedStep{ID: "after-parallel", Kind: "display", Spec: &cliStepSpec{}},
	)

	handle, err := eng.Start(context.Background(), engine.ValidatedForTest(plan), engine.RunOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	go func() {
		for range handle.Events() {
		}
	}()

	// First Next: the parallel container returns as failed.
	result, err := handle.Next(context.Background())
	if err != nil {
		t.Fatalf("Next[0] error: %v", err)
	}
	if result == nil || result.Status != engine.StepStatusFailed {
		t.Fatalf("expected parallel container StepStatusFailed, got %+v", result)
	}

	// Second Next: EOF (run terminated, "after-parallel" never ran).
	_, err = handle.Next(context.Background())
	if err != io.EOF {
		t.Fatalf("expected io.EOF (run stopped), got %v", err)
	}

	state := handle.State()
	if state.Status != engine.RunStatusFailed {
		t.Errorf("expected RunStatusFailed, got %s", state.Status)
	}

	// Verify "after-parallel" never started.
	for _, ev := range tw.collect() {
		if ev.Kind == trace.EventKindStepStarted {
			var payload map[string]any
			_ = json.Unmarshal(ev.Payload, &payload)
			if payload != nil && payload["step_id"] == "after-parallel" {
				t.Error("after-parallel step/started should NOT have been emitted")
			}
		}
	}
}

// TestFailStop_ParallelBranch_OnErrorContinue verifies that a step inside
// a parallel branch with on_error: continue doesn't kill the branch.
func TestFailStop_ParallelBranch_OnErrorContinue(t *testing.T) {
	reg := newFakeExecutorRegistry()
	reg.Register("tool", stepExecutorFunc(func(_ context.Context, step engine.ResolvedStep, _ map[string]any) (*engine.StepResult, error) {
		if step.ID == "failing-branch-step" {
			return &engine.StepResult{
				StepID:  step.ID,
				Status:  engine.StepStatusFailed,
				Outcome: engine.StepOutcomeFailed,
				Error:   errors.New("expected"),
			}, nil
		}
		return &engine.StepResult{
			StepID:  step.ID,
			Status:  engine.StepStatusCompleted,
			Outcome: engine.StepOutcomeSuccess,
		}, nil
	}))

	cfg := engine.EngineConfig{
		Executors:   reg,
		Dispatcher:  newFakeEventDispatcher(),
		TraceWriter: &fakeTraceWriter{},
		Platform:    platform.NewFakePlatform(),
	}
	eng := New(cfg)

	plan := makeTestPlan(
		engine.ResolvedStep{
			ID:   "parallel-container",
			Kind: "parallel",
			Spec: &testParallelSpec{
				branches: []engine.BranchSpec{
					{
						Label: "branch-a",
						Steps: []engine.ResolvedStep{
							{ID: "failing-branch-step", Kind: "tool", Spec: &cliStepSpec{}, OnError: "continue"},
							{ID: "next-branch-step", Kind: "tool", Spec: &cliStepSpec{}},
						},
					},
				},
			},
		},
	)

	handle, err := eng.Start(context.Background(), engine.ValidatedForTest(plan), engine.RunOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	go func() {
		for range handle.Events() {
		}
	}()

	// The parallel container should complete (branch failure was tolerated).
	result, err := handle.Next(context.Background())
	if err != nil {
		t.Fatalf("Next error: %v", err)
	}
	if result.Status == engine.StepStatusFailed {
		t.Fatal("parallel container should NOT be failed when branch step has on_error: continue")
	}
}

// makeSubStepRunner creates a SubStepRunner that mirrors runSubStepsViaEngine:
// it creates a sub-engine, runs the steps, and returns an error if the
// sub-engine terminates in a failed state.
func makeSubStepRunner(cfg engine.EngineConfig) executor.SubStepRunner {
	return func(ctx context.Context, parent executor.SubStepParent, nodes []schema.FlowNode, vars map[string]any) ([]*engine.StepResult, error) {
		steps := make([]engine.ResolvedStep, 0, len(nodes))
		for _, node := range nodes {
			if node.Step != nil {
				s := node.Step
				steps = append(steps, engine.ResolvedStep{
					ID:      s.ID,
					Kind:    string(s.Type),
					Spec:    &cliStepSpec{},
					OnError: s.OnError,
				})
			}
		}
		plan := &engine.ExecutionPlan{
			RunbookPath: "substeps",
			Steps:       steps,
			Metadata:    engine.PlanMetadata{RunbookID: "substeps", PlannedAt: time.Now()},
		}
		eng := New(cfg)
		handle, err := eng.Start(ctx, engine.ValidatedForTest(plan), engine.RunOptions{})
		if err != nil {
			return nil, err
		}
		go func() {
			for range handle.Events() {
			}
		}()
		var results []*engine.StepResult
		for {
			result, err := handle.Next(ctx)
			if err == io.EOF {
				break
			}
			if err != nil {
				return results, err
			}
			results = append(results, result)
		}
		if st := handle.State(); st.Status == engine.RunStatusFailed {
			return results, fmt.Errorf("%w: step %s", executor.ErrSubRunFailed, st.CurrentStep)
		}
		return results, nil
	}
}

// TestFailStop_IncludeChildFail_StopsRun verifies that an include step whose
// child fails (no recovery) terminates the parent run as failed, and
// subsequent siblings never start. Exercises the real engine+adapter path.
func TestFailStop_IncludeChildFail_StopsRun(t *testing.T) {
	tw := &fakeTraceWriter{}

	reg := newFakeExecutorRegistry()
	reg.Register("tool", &failingExecutor{err: errors.New("child tool error")})
	reg.Register("display", &passThroughExecutor{})

	cfg := engine.EngineConfig{
		Executors:   reg,
		Dispatcher:  newFakeEventDispatcher(),
		TraceWriter: tw,
		Platform:    platform.NewFakePlatform(),
	}

	// Wire the IncludeExecutor with a real sub-engine SubStepRunner.
	includeExec := executor.NewIncludeExecutor(nil, makeSubStepRunner(cfg), nil)
	reg.Register("include", includeExec)

	plan := makeTestPlan(
		engine.ResolvedStep{
			ID:   "include-step",
			Kind: "include",
			Spec: &schema.IncludeSpec{
				Include: schema.IncludeConfig{Runbook: "child.yaml"},
				ResolvedSteps: []schema.FlowNode{
					{Step: &schema.Step{ID: "child-tool", Type: schema.StepTypeTool}},
				},
			},
		},
		engine.ResolvedStep{ID: "after-include", Kind: "display", Spec: &cliStepSpec{}},
	)

	eng := New(cfg)
	handle, err := eng.Start(context.Background(), engine.ValidatedForTest(plan), engine.RunOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	go func() {
		for range handle.Events() {
		}
	}()

	// Include step reports failure as a StepResult (not infrastructure error).
	// The parent engine applies resolveOnError (default=stop) → run terminates.
	result, err := handle.Next(context.Background())
	if err != nil {
		t.Fatalf("Next[0] unexpected error: %v", err)
	}
	if result == nil || result.Status != engine.StepStatusFailed {
		t.Fatalf("expected include step StepStatusFailed, got %+v", result)
	}

	// Run should be terminal.
	_, err = handle.Next(context.Background())
	if err != io.EOF {
		t.Fatalf("expected io.EOF (run stopped), got %v", err)
	}

	state := handle.State()
	if state.Status != engine.RunStatusFailed {
		t.Fatalf("expected RunStatusFailed, got %s", state.Status)
	}

	// Verify "after-include" never started.
	time.Sleep(10 * time.Millisecond)
	for _, ev := range tw.collect() {
		if ev.Kind == trace.EventKindStepStarted {
			var payload map[string]any
			_ = json.Unmarshal(ev.Payload, &payload)
			if payload != nil && payload["step_id"] == "after-include" {
				t.Error("after-include step/started should NOT have been emitted")
			}
		}
	}
}

// TestFailStop_IncludeOnErrorContinue_Continues verifies that an include step
// whose child has on_error: continue completes successfully.
func TestFailStop_IncludeOnErrorContinue_Continues(t *testing.T) {
	reg := newFakeExecutorRegistry()
	reg.Register("tool", &failingExecutor{err: errors.New("child tool error")})
	reg.Register("end", &passThroughExecutor{})

	cfg := engine.EngineConfig{
		Executors:   reg,
		Dispatcher:  newFakeEventDispatcher(),
		TraceWriter: &fakeTraceWriter{},
		Platform:    platform.NewFakePlatform(),
	}

	includeExec := executor.NewIncludeExecutor(nil, makeSubStepRunner(cfg), nil)
	reg.Register("include", includeExec)

	plan := makeTestPlan(
		engine.ResolvedStep{
			ID:   "include-step",
			Kind: "include",
			Spec: &schema.IncludeSpec{
				Include: schema.IncludeConfig{Runbook: "child.yaml"},
				ResolvedSteps: []schema.FlowNode{
					{Step: &schema.Step{ID: "child-tool", Type: schema.StepTypeTool, OnError: "continue"}},
				},
			},
		},
		engine.ResolvedStep{ID: "after-include", Kind: "end", Spec: &cliStepSpec{}},
	)

	eng := New(cfg)
	handle, err := eng.Start(context.Background(), engine.ValidatedForTest(plan), engine.RunOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	go func() {
		for range handle.Events() {
		}
	}()

	// Include step should complete (child failure was tolerated).
	result, err := handle.Next(context.Background())
	if err != nil {
		t.Fatalf("Next[0] error: %v", err)
	}
	if result.Status != engine.StepStatusCompleted {
		t.Fatalf("expected include step Completed (child had on_error: continue), got %s", result.Status)
	}

	// Sibling should run.
	result, err = handle.Next(context.Background())
	if err != nil {
		t.Fatalf("Next[1] error: %v", err)
	}
	if result.StepID != "after-include" || result.Status != engine.StepStatusCompleted {
		t.Fatalf("expected after-include/Completed, got %s/%s", result.StepID, result.Status)
	}
}

// TestFailStop_BranchChildFail_StopsRun verifies that a branch executor step
// whose matched arm fails (no recovery) terminates the parent run.
func TestFailStop_BranchChildFail_StopsRun(t *testing.T) {
	tw := &fakeTraceWriter{}

	reg := newFakeExecutorRegistry()
	reg.Register("tool", &failingExecutor{err: errors.New("branch child error")})
	reg.Register("display", &passThroughExecutor{})

	cfg := engine.EngineConfig{
		Executors:   reg,
		Dispatcher:  newFakeEventDispatcher(),
		TraceWriter: tw,
		Platform:    platform.NewFakePlatform(),
	}

	branchExec := executor.NewBranchExecutor(nil, makeSubStepRunner(cfg))
	reg.Register("branch", branchExec)

	plan := makeTestPlan(
		engine.ResolvedStep{
			ID:   "branch-step",
			Kind: "branch",
			Spec: &schema.BranchSpec{
				Branches: []schema.BranchArm{
					{
						Label:     "always",
						Condition: "true",
						Steps: []schema.FlowNode{
							{Step: &schema.Step{ID: "branch-child", Type: schema.StepTypeTool}},
						},
					},
				},
			},
		},
		engine.ResolvedStep{ID: "after-branch", Kind: "display", Spec: &cliStepSpec{}},
	)

	eng := New(cfg)
	handle, err := eng.Start(context.Background(), engine.ValidatedForTest(plan), engine.RunOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	go func() {
		for range handle.Events() {
		}
	}()

	// Branch step reports failure as a StepResult (not infrastructure error).
	// The parent engine applies resolveOnError (default=stop) → run terminates.
	result, err := handle.Next(context.Background())
	if err != nil {
		t.Fatalf("Next[0] unexpected error: %v", err)
	}
	if result == nil || result.Status != engine.StepStatusFailed {
		t.Fatalf("expected branch step StepStatusFailed, got %+v", result)
	}

	// Run should be terminal.
	_, err = handle.Next(context.Background())
	if err != io.EOF {
		t.Fatalf("expected io.EOF (run stopped), got %v", err)
	}

	state := handle.State()
	if state.Status != engine.RunStatusFailed {
		t.Fatalf("expected RunStatusFailed, got %s", state.Status)
	}

	// "after-branch" never started.
	time.Sleep(10 * time.Millisecond)
	for _, ev := range tw.collect() {
		if ev.Kind == trace.EventKindStepStarted {
			var payload map[string]any
			_ = json.Unmarshal(ev.Payload, &payload)
			if payload != nil && payload["step_id"] == "after-branch" {
				t.Error("after-branch step/started should NOT have been emitted")
			}
		}
	}
}

// TestFailStop_IncludeOnErrorContinue verifies that on_error: continue on an
// include step allows the run to continue even when a child fails.
func TestFailStop_IncludeOnErrorContinue(t *testing.T) {
	reg := newFakeExecutorRegistry()
	reg.Register("tool", &failingExecutor{err: errors.New("child error")})
	reg.Register("end", &passThroughExecutor{})

	cfg := engine.EngineConfig{
		Executors:   reg,
		Dispatcher:  newFakeEventDispatcher(),
		TraceWriter: &fakeTraceWriter{},
		Platform:    platform.NewFakePlatform(),
	}
	includeExec := executor.NewIncludeExecutor(nil, makeSubStepRunner(cfg), nil)
	reg.Register("include", includeExec)

	plan := makeTestPlan(
		engine.ResolvedStep{
			ID:      "include-step",
			Kind:    "include",
			OnError: "continue",
			Spec: &schema.IncludeSpec{
				Include: schema.IncludeConfig{Runbook: "child.yaml"},
				ResolvedSteps: []schema.FlowNode{
					{Step: &schema.Step{ID: "child-tool", Type: schema.StepTypeTool}},
				},
			},
		},
		engine.ResolvedStep{ID: "after-include", Kind: "end", Spec: &cliStepSpec{}},
	)

	eng := New(cfg)
	handle, err := eng.Start(context.Background(), engine.ValidatedForTest(plan), engine.RunOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	go func() {
		for range handle.Events() {
		}
	}()

	// Include step fails but on_error=continue → run continues.
	result, err := handle.Next(context.Background())
	if err != nil {
		t.Fatalf("Next[0] error: %v", err)
	}
	if result.Status != engine.StepStatusFailed {
		t.Fatalf("expected include StepStatusFailed, got %s", result.Status)
	}

	// Sibling runs.
	result, err = handle.Next(context.Background())
	if err != nil {
		t.Fatalf("Next[1] error: %v", err)
	}
	if result.StepID != "after-include" || result.Status != engine.StepStatusCompleted {
		t.Fatalf("expected after-include/Completed, got %s/%s", result.StepID, result.Status)
	}
}

// TestFailStop_BranchOnErrorContinue verifies that on_error: continue on a
// branch step allows the run to continue even when the matched arm fails.
func TestFailStop_BranchOnErrorContinue(t *testing.T) {
	reg := newFakeExecutorRegistry()
	reg.Register("tool", &failingExecutor{err: errors.New("branch child error")})
	reg.Register("end", &passThroughExecutor{})

	cfg := engine.EngineConfig{
		Executors:   reg,
		Dispatcher:  newFakeEventDispatcher(),
		TraceWriter: &fakeTraceWriter{},
		Platform:    platform.NewFakePlatform(),
	}
	branchExec := executor.NewBranchExecutor(&alwaysTrueCondEval{}, makeSubStepRunner(cfg))
	reg.Register("branch", branchExec)

	plan := makeTestPlan(
		engine.ResolvedStep{
			ID:      "branch-step",
			Kind:    "branch",
			OnError: "continue",
			Spec: &schema.BranchSpec{
				Branches: []schema.BranchArm{
					{
						Label:     "always",
						Condition: "true",
						Steps: []schema.FlowNode{
							{Step: &schema.Step{ID: "branch-child", Type: schema.StepTypeTool}},
						},
					},
				},
			},
		},
		engine.ResolvedStep{ID: "after-branch", Kind: "end", Spec: &cliStepSpec{}},
	)

	eng := New(cfg)
	handle, err := eng.Start(context.Background(), engine.ValidatedForTest(plan), engine.RunOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	go func() {
		for range handle.Events() {
		}
	}()

	// Branch fails but on_error=continue → run continues.
	result, err := handle.Next(context.Background())
	if err != nil {
		t.Fatalf("Next[0] error: %v", err)
	}
	if result.Status != engine.StepStatusFailed {
		t.Fatalf("expected branch StepStatusFailed, got %s", result.Status)
	}

	// Sibling runs.
	result, err = handle.Next(context.Background())
	if err != nil {
		t.Fatalf("Next[1] error: %v", err)
	}
	if result.StepID != "after-branch" || result.Status != engine.StepStatusCompleted {
		t.Fatalf("expected after-branch/Completed, got %s/%s", result.StepID, result.Status)
	}
}

// TestFailStop_IncludeOnErrorGoto verifies that on_error: goto:<step> on an
// include step jumps to the target step when the child fails.
func TestFailStop_IncludeOnErrorGoto(t *testing.T) {
	reg := newFakeExecutorRegistry()
	reg.Register("tool", &failingExecutor{err: errors.New("child error")})
	reg.Register("end", &passThroughExecutor{})

	cfg := engine.EngineConfig{
		Executors:   reg,
		Dispatcher:  newFakeEventDispatcher(),
		TraceWriter: &fakeTraceWriter{},
		Platform:    platform.NewFakePlatform(),
	}
	includeExec := executor.NewIncludeExecutor(nil, makeSubStepRunner(cfg), nil)
	reg.Register("include", includeExec)

	plan := makeTestPlan(
		engine.ResolvedStep{
			ID:      "include-step",
			Kind:    "include",
			OnError: "goto:error-handler",
			Spec: &schema.IncludeSpec{
				Include: schema.IncludeConfig{Runbook: "child.yaml"},
				ResolvedSteps: []schema.FlowNode{
					{Step: &schema.Step{ID: "child-tool", Type: schema.StepTypeTool}},
				},
			},
		},
		engine.ResolvedStep{ID: "skipped-step", Kind: "end", Spec: &cliStepSpec{}},
		engine.ResolvedStep{ID: "error-handler", Kind: "end", Spec: &cliStepSpec{}},
	)

	eng := New(cfg)
	handle, err := eng.Start(context.Background(), engine.ValidatedForTest(plan), engine.RunOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	go func() {
		for range handle.Events() {
		}
	}()

	// Include step fails → goto:error-handler routes the error while preserving
	// the failed result in durable history.
	result, err := handle.Next(context.Background())
	if err != nil {
		t.Fatalf("Next[0] error: %v", err)
	}
	if result.StepID != "include-step" {
		t.Fatalf("expected include-step, got %s", result.StepID)
	}

	// Next step should be the error handler, not the skipped step.
	result, err = handle.Next(context.Background())
	if err != nil {
		t.Fatalf("Next[1] error: %v", err)
	}
	if result.StepID != "error-handler" {
		t.Fatalf("expected goto to jump to error-handler, got %s", result.StepID)
	}
}

// TestFailStop_InfraError_HardFails verifies that a genuine infrastructure
// error from the sub-runner (not ErrSubRunFailed) still hard-fails via
// failRun and is NOT swallowed as a failed result — even when on_error=continue.
func TestFailStop_InfraError_HardFails(t *testing.T) {
	reg := newFakeExecutorRegistry()
	reg.Register("end", &passThroughExecutor{})

	// Wire include executor with a runner that returns an infra error.
	infraRunner := func(_ context.Context, _ executor.SubStepParent, _ []schema.FlowNode, _ map[string]any) ([]*engine.StepResult, error) {
		return nil, errors.New("disk I/O error") // NOT ErrSubRunFailed
	}
	includeExec := executor.NewIncludeExecutor(nil, infraRunner, nil)
	reg.Register("include", includeExec)

	cfg := engine.EngineConfig{
		Executors:   reg,
		Dispatcher:  newFakeEventDispatcher(),
		TraceWriter: &fakeTraceWriter{},
		Platform:    platform.NewFakePlatform(),
	}
	eng := New(cfg)

	plan := makeTestPlan(
		engine.ResolvedStep{
			ID:      "include-step",
			Kind:    "include",
			OnError: "continue", // Even with continue, infra errors are fatal.
			Spec: &schema.IncludeSpec{
				Include: schema.IncludeConfig{Runbook: "child.yaml"},
				ResolvedSteps: []schema.FlowNode{
					{Step: &schema.Step{ID: "child-tool", Type: schema.StepTypeTool}},
				},
			},
		},
		engine.ResolvedStep{ID: "after-include", Kind: "end", Spec: &cliStepSpec{}},
	)

	handle, err := eng.Start(context.Background(), engine.ValidatedForTest(plan), engine.RunOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	go func() {
		for range handle.Events() {
		}
	}()

	// Infrastructure error → failRun → (nil, err).
	_, err = handle.Next(context.Background())
	if err == nil {
		t.Fatal("expected infrastructure error to propagate as fatal")
	}
	if err == io.EOF {
		t.Fatal("expected infra error, not io.EOF")
	}

	state := handle.State()
	if state.Status != engine.RunStatusFailed {
		t.Fatalf("expected RunStatusFailed, got %s", state.Status)
	}
}
