package adapter

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	internalengine "github.com/ormasoftchile/yawr/runtime/internal/engine"
	"github.com/ormasoftchile/yawr/runtime/internal/executor"
	internalplanner "github.com/ormasoftchile/yawr/runtime/internal/planner"
	internalrunstore "github.com/ormasoftchile/yawr/runtime/internal/runstore"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/eventbus"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

// failAlwaysExecutor always returns a step-level failure.
type failAlwaysExecutor struct{}

func (f *failAlwaysExecutor) Execute(_ context.Context, step engine.ResolvedStep, _ map[string]any) (*engine.StepResult, error) {
	return &engine.StepResult{
		StepID:  step.ID,
		Status:  engine.StepStatusFailed,
		Outcome: engine.StepOutcomeFailed,
		Error:   errors.New("forced failure"),
	}, nil
}

// passExecutor always succeeds.
type passExecutor struct{}

func (p *passExecutor) Execute(_ context.Context, step engine.ResolvedStep, _ map[string]any) (*engine.StepResult, error) {
	return &engine.StepResult{
		StepID:  step.ID,
		Status:  engine.StepStatusCompleted,
		Outcome: engine.StepOutcomeSuccess,
	}, nil
}

type stdoutPassExecutor struct{}

func (*stdoutPassExecutor) Execute(_ context.Context, step engine.ResolvedStep, _ map[string]any) (*engine.StepResult, error) {
	return &engine.StepResult{
		StepID: step.ID, Status: engine.StepStatusCompleted, Outcome: engine.StepOutcomeSuccess,
		Output: map[string]any{"stdout": "must not be recollected"},
	}, nil
}

type recordingPassExecutor struct {
	steps []string
}

type structuralPathExecutor struct {
	path []schema.DynamicIncludeFrameIdentity
}

func (capture *structuralPathExecutor) Execute(
	ctx context.Context,
	step engine.ResolvedStep,
	_ map[string]any,
) (*engine.StepResult, error) {
	capture.path = engine.DynamicIncludeStructuralPathFromContext(ctx)
	return &engine.StepResult{
		StepID: step.ID, Status: engine.StepStatusCompleted, Outcome: engine.StepOutcomeSuccess,
	}, nil
}

type nestedDispatchExecutor struct {
	steps []string
}

func (executor *nestedDispatchExecutor) Execute(ctx context.Context, step engine.ResolvedStep, _ map[string]any) (*engine.StepResult, error) {
	if _, err := engine.PrepareExternalDispatch(ctx, engine.DispatchRequest{
		Classification: "unspecified", EndpointIdentity: "nested-provider",
		RenderedRequest: map[string]any{"step": step.ID},
	}); err != nil {
		return nil, err
	}
	executor.steps = append(executor.steps, step.ID)
	return &engine.StepResult{
		StepID: step.ID, Status: engine.StepStatusCompleted, Outcome: engine.StepOutcomeSuccess,
		Output: map[string]any{"step": step.ID}, Vars: map[string]any{"last": step.ID},
	}, nil
}

type gatedNestedDispatchExecutor struct {
	blockedStep string
	started     chan struct{}
	release     chan struct{}
	steps       []string
}

type secondCallGatedDispatchExecutor struct {
	mu      sync.Mutex
	calls   int
	started chan struct{}
	release chan struct{}
}

func (executor *secondCallGatedDispatchExecutor) Execute(ctx context.Context, step engine.ResolvedStep, _ map[string]any) (*engine.StepResult, error) {
	executor.mu.Lock()
	executor.calls++
	call := executor.calls
	executor.mu.Unlock()
	if call == 2 {
		close(executor.started)
		<-executor.release
	}
	if _, err := engine.PrepareExternalDispatch(ctx, engine.DispatchRequest{
		Classification: "unspecified", EndpointIdentity: "iterate-provider",
		RenderedRequest: map[string]any{"step": step.ID, "call": call},
	}); err != nil {
		return nil, err
	}
	return &engine.StepResult{
		StepID: step.ID, Status: engine.StepStatusCompleted, Outcome: engine.StepOutcomeSuccess,
		Vars: map[string]any{"child_call": call},
	}, nil
}

func (executor *secondCallGatedDispatchExecutor) callCount() int {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return executor.calls
}

func (executor *gatedNestedDispatchExecutor) Execute(ctx context.Context, step engine.ResolvedStep, _ map[string]any) (*engine.StepResult, error) {
	if step.ID == executor.blockedStep {
		close(executor.started)
		<-executor.release
	}
	if _, err := engine.PrepareExternalDispatch(ctx, engine.DispatchRequest{
		Classification: "unspecified", EndpointIdentity: "nested-provider",
		RenderedRequest: map[string]any{"step": step.ID},
	}); err != nil {
		return nil, err
	}
	executor.steps = append(executor.steps, step.ID)
	return &engine.StepResult{
		StepID: step.ID, Status: engine.StepStatusCompleted, Outcome: engine.StepOutcomeSuccess,
		Output: map[string]any{"step": step.ID}, Vars: map[string]any{"last": step.ID},
	}, nil
}

func (executor *recordingPassExecutor) Execute(_ context.Context, step engine.ResolvedStep, _ map[string]any) (*engine.StepResult, error) {
	executor.steps = append(executor.steps, step.ID)
	return &engine.StepResult{
		StepID: step.ID, Status: engine.StepStatusCompleted, Outcome: engine.StepOutcomeSuccess,
		Vars: map[string]any{"last": step.ID},
	}, nil
}

type resumedFrameCommitter struct {
	frame   engine.ExecutionFrameState
	commits []engine.ExecutionFrameStepCommit
}

func (committer *resumedFrameCommitter) BeginExecutionFrame(_ context.Context, _ engine.ExecutionFrameRequest) (engine.ExecutionFrameState, error) {
	return committer.frame, nil
}

func (committer *resumedFrameCommitter) CommitExecutionFrameStep(_ context.Context, commit engine.ExecutionFrameStepCommit) (engine.ExecutionFrameState, error) {
	committer.commits = append(committer.commits, commit)
	committer.frame.NextStepIndex = commit.StepIndex + 1
	committer.frame.WorkingVars = commit.WorkingVars
	committer.frame.Results["1"] = commit.Result
	return committer.frame, nil
}

// noopTraceWriter discards trace events.
type noopTraceWriter struct{}

func (n *noopTraceWriter) Append(_ trace.TraceEvent) error { return nil }
func (n *noopTraceWriter) Close() error                    { return nil }

// testRegistry implements engine.ExecutorRegistry for tests.
type testRegistry struct {
	executors map[string]engine.StepExecutor
}

func (r *testRegistry) Register(kind string, exec engine.StepExecutor) { r.executors[kind] = exec }
func (r *testRegistry) Lookup(kind string) engine.StepExecutor         { return r.executors[kind] }

// noopDispatcher satisfies eventbus.EventDispatcher.
type noopDispatcher struct{}

func (n *noopDispatcher) Dispatch(_ eventbus.InboundEvent) error { return nil }
func (n *noopDispatcher) Wait(_ context.Context, _ string, _ eventbus.EventFilter, _ time.Duration) (*eventbus.InboundEvent, error) {
	return nil, errors.New("no dispatcher")
}
func (n *noopDispatcher) Cancel(_ string, _ string) {}

// TestRunSubStepsViaEngine_FailedChild_ReturnsError exercises the real
// runSubStepsViaEngine path: a child step fails with default on_error (stop),
// the sub-engine marks itself as failed, and the function returns an error.
func TestRunSubStepsViaEngine_FailedChild_ReturnsError(t *testing.T) {
	reg := &testRegistry{executors: map[string]engine.StepExecutor{
		"tool": &failAlwaysExecutor{},
	}}

	nodes := []schema.FlowNode{
		{Step: &schema.Step{ID: "child-1", Type: schema.StepTypeTool}},
	}

	results, err := runSubStepsViaEngine(
		context.Background(),
		reg,
		executor.SubStepParent{ID: "parent", Kind: "include"},
		nodes,
		nil,
		engine.RunModeReal,
		&noopTraceWriter{},
		&noopDispatcher{},
		platform.NewFakePlatform(),
		nil, nil, nil, nil, nil, nil, nil,
	)
	if err == nil {
		t.Fatalf("expected error from failed sub-run, got nil (results=%d)", len(results))
	}
	if !errors.Is(err, executor.ErrSubRunFailed) {
		t.Fatalf("expected ErrSubRunFailed sentinel, got: %v", err)
	}
	if !strings.Contains(err.Error(), "forced failure") {
		t.Fatalf("nested failure cause was lost: %v", err)
	}
}

func TestRunSubStepsViaEngineStorelessReplayUsesFirstFrameInvocation(t *testing.T) {
	capture := &structuralPathExecutor{}
	registry := &testRegistry{executors: map[string]engine.StepExecutor{"noop": capture}}
	_, err := runSubStepsViaEngine(
		executor.WithExecutorRegistry(context.Background(), registry),
		registry,
		executor.SubStepParent{ID: "outer", Kind: "include", RunbookPath: "outer.runbook.yaml"},
		[]schema.FlowNode{{Step: &schema.Step{ID: "child", Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{}}}},
		nil,
		engine.RunModeReplay,
		&noopTraceWriter{},
		&noopDispatcher{},
		platform.NewFakePlatform(),
		nil, nil, nil, nil, nil, nil, nil,
	)
	if err != nil {
		t.Fatalf("runSubStepsViaEngine: %v", err)
	}
	if len(capture.path) != 1 || capture.path[0].QualifiedNodeID != "outer" ||
		capture.path[0].Kind != "include" || capture.path[0].Invocation != 1 {
		t.Fatalf("storeless replay structural path = %#v", capture.path)
	}
}

// TestRunSubStepsViaEngine_OnErrorContinue_ReturnsNil exercises the real path:
// a child step fails with on_error=continue, the sub-engine continues,
// and the function returns nil error.
func TestRunSubStepsViaEngine_OnErrorContinue_ReturnsNil(t *testing.T) {
	reg := &testRegistry{executors: map[string]engine.StepExecutor{
		"tool": &failAlwaysExecutor{},
		"end":  &passExecutor{},
	}}

	nodes := []schema.FlowNode{
		{Step: &schema.Step{ID: "child-1", Type: schema.StepTypeTool, OnError: "continue"}},
		{Step: &schema.Step{ID: "child-2", Type: "end"}},
	}

	results, err := runSubStepsViaEngine(
		context.Background(),
		reg,
		executor.SubStepParent{ID: "parent", Kind: "include"},
		nodes,
		nil,
		engine.RunModeReal,
		&noopTraceWriter{},
		&noopDispatcher{},
		platform.NewFakePlatform(),
		nil, nil, nil, nil, nil, nil, nil,
	)
	if err != nil {
		t.Fatalf("expected nil error (failure tolerated), got: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results (both steps ran), got %d", len(results))
	}
}

func TestRunSubStepsViaEngine_ResumesSequentialExecutionFrame(t *testing.T) {
	executorImpl := &recordingPassExecutor{}
	registry := &testRegistry{executors: map[string]engine.StepExecutor{"tool": executorImpl}}
	frameID := engine.InteractionPayloadDigest([]byte("resumed frame"))
	committer := &resumedFrameCommitter{frame: engine.ExecutionFrameState{
		SchemaVersion: engine.ExecutionFrameStateSchemaV1, FrameID: frameID,
		ParentStepID: "parent", Kind: "include", CallPath: []engine.DebugCallFrame{{StepID: "parent"}},
		Invocation: 1, StepCount: 2, StepIDs: []string{"child-1", "child-2"},
		NextStepIndex: 1, Status: engine.ExecutionFrameStatusActive,
		WorkingVars: map[string]any{"saved": true},
		Results: map[string]*engine.StepResult{
			"0": {StepID: "child-1", Status: engine.StepStatusCompleted, Vars: map[string]any{"saved": true}},
		},
	}}
	ctx := engine.WithExecutionFrameCommitter(context.Background(), committer)
	results, err := runSubStepsViaEngine(
		ctx,
		registry,
		executor.SubStepParent{ID: "parent", Kind: "include"},
		[]schema.FlowNode{
			{Step: &schema.Step{ID: "child-1", Type: schema.StepTypeTool}},
			{Step: &schema.Step{ID: "child-2", Type: schema.StepTypeTool}},
		},
		map[string]any{"initial": true},
		engine.RunModeReal,
		&noopTraceWriter{},
		&noopDispatcher{},
		platform.NewFakePlatform(),
		nil, nil, nil, nil, nil, nil, nil,
	)
	if err != nil {
		t.Fatalf("runSubStepsViaEngine: %v", err)
	}
	if len(results) != 2 || results[0].StepID != "child-1" || results[1].StepID != "child-2" {
		t.Fatalf("results = %#v", results)
	}
	if len(executorImpl.steps) != 1 || executorImpl.steps[0] != "child-2" {
		t.Fatalf("executed steps = %#v, want only child-2", executorImpl.steps)
	}
	if len(committer.commits) != 1 || committer.commits[0].StepIndex != 1 ||
		committer.commits[0].WorkingVars["saved"] != true || committer.commits[0].WorkingVars["last"] != "child-2" {
		t.Fatalf("frame commits = %#v", committer.commits)
	}
}

func TestRunSubStepsViaEngine_PropagatesExactHandoffFrameInvocation(t *testing.T) {
	frameID := engine.InteractionPayloadDigest([]byte("second handoff frame invocation"))
	committer := &resumedFrameCommitter{frame: engine.ExecutionFrameState{
		SchemaVersion: engine.ExecutionFrameStateSchemaV1, FrameID: frameID,
		ParentStepID: "parent", Kind: "include", CallPath: []engine.DebugCallFrame{{StepID: "parent"}},
		Invocation: 2, StepCount: 1, StepIDs: []string{"handoff"}, NextStepIndex: 0,
		Status: engine.ExecutionFrameStatusActive, Results: map[string]*engine.StepResult{},
	}}
	registry := &testRegistry{executors: map[string]engine.StepExecutor{
		"handoff": executor.NewHandoffExecutor(nil),
	}}
	ctx := engine.WithExecutionFrameCommitter(context.Background(), committer)
	tracker := engine.NewDebugInvocationTracker()
	tracker.Next([]engine.DebugCallFrame{{StepID: "parent"}}, "handoff")
	ctx = engine.WithDebugInvocationTracker(ctx, tracker)
	_, err := runSubStepsViaEngine(
		ctx, registry, executor.SubStepParent{
			ID: "parent", Kind: "include", RunbookPath: filepath.Join("sub", "child.runbook.yaml"),
		},
		[]schema.FlowNode{{Step: &schema.Step{
			ID: "handoff", Type: schema.StepTypeHandoff,
			HandoffSpec: &schema.HandoffSpec{Handoff: schema.HandoffConfig{
				Runbook: "target.runbook.yaml",
				Reason:  schema.HandoffReason{Code: "continue", Summary: "Continue investigation"},
			}},
		}}}, nil, engine.RunModeReal, &noopTraceWriter{}, &noopDispatcher{}, platform.NewFakePlatform(),
		nil, nil, nil, nil, nil, nil, nil,
	)
	request, ok := engine.HandoffRequestFromError(err)
	if !ok {
		t.Fatalf("runSubStepsViaEngine error = %v, want handoff request", err)
	}
	if request.QualifiedNodeID != "parent/handoff" || request.StepID != "handoff" ||
		request.FrameID != frameID || request.FrameStepIndex != 0 || request.Invocation != 2 ||
		request.RetryAttempt != 1 || request.OccurrenceSequence != 0 || len(request.CallPath) != 1 ||
		request.CallPath[0].StepID != "parent" ||
		request.CallPath[0].RunbookPath != filepath.Join("sub", "child.runbook.yaml") {
		t.Fatalf("nested handoff request = %#v", request)
	}
}

func TestRunSubStepsViaEngineHonorsExplicitNilReplayEvidenceHook(t *testing.T) {
	registry := &testRegistry{executors: map[string]engine.StepExecutor{"cli": &stdoutPassExecutor{}}}
	ctx := executor.WithRunEvidenceHook(context.Background(), nil)
	results, err := runSubStepsViaEngine(
		ctx, registry, executor.SubStepParent{ID: "parent", Kind: "tool-substitution"},
		[]schema.FlowNode{{Step: &schema.Step{
			ID: "saved-child", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "saved"},
		}}}, nil, engine.RunModeReplay, &noopTraceWriter{}, &noopDispatcher{}, platform.NewFakePlatform(),
		nil, nil, nil, nil, nil, nil, nil,
	)
	if err != nil {
		t.Fatalf("runSubStepsViaEngine: %v", err)
	}
	if len(results) != 1 || len(results[0].Evidence) != 0 {
		t.Fatalf("nested replay evidence = %#v", results)
	}
}

func TestRunSubStepsViaEngine_CommitsNestedDispatchesIndependently(t *testing.T) {
	var lifecycle []engine.Event
	store := internalrunstore.NewDirRunStore(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	platformFake := platform.NewFakePlatform()
	dispatchExecutor := &nestedDispatchExecutor{}
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
		RunID: "run-nested-dispatches", RunbookPath: "nested-dispatches.runbook.yaml",
		Steps: []engine.ResolvedStep{{
			ID: "include", Kind: "include", Spec: &schema.IncludeSpec{
				Include: schema.IncludeConfig{Runbook: "child.runbook.yaml"},
				ResolvedSteps: []schema.FlowNode{
					{Step: &schema.Step{ID: "child-1", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "one"}}},
					{Step: &schema.Step{ID: "child-2", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "two"}}},
				},
			},
		}},
		Metadata: engine.PlanMetadata{RunbookID: "nested-dispatches"},
	}
	if err := internalplanner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	runtime := internalengine.New(engine.EngineConfig{
		Executors: registry, Dispatcher: &noopDispatcher{}, TraceWriter: &noopTraceWriter{},
		Platform: platformFake, Store: store,
		OnEvent: func(event engine.Event) { lifecycle = append(lifecycle, event) },
	})
	handle, err := runtime.Start(context.Background(), plan, engine.RunOptions{Mode: engine.RunModeReal, Store: store})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = handle.Cancel(context.Background(), "test cleanup") })
	result, err := handle.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if result.Status != engine.StepStatusCompleted || len(dispatchExecutor.steps) != 2 {
		t.Fatalf("result=%#v executed=%#v", result, dispatchExecutor.steps)
	}
	state := handle.State()
	if len(state.Dispatches) != 2 || len(state.ExecutionFrames) != 1 {
		t.Fatalf("dispatches=%#v frames=%#v", state.Dispatches, state.ExecutionFrames)
	}
	for _, dispatch := range state.Dispatches {
		if dispatch.FrameID == "" || dispatch.Status != engine.DispatchStatusSettled {
			t.Fatalf("nested dispatch = %#v", dispatch)
		}
	}
	for _, frame := range state.ExecutionFrames {
		if frame.Status != engine.ExecutionFrameStatusCompleted || frame.NextStepIndex != 2 {
			t.Fatalf("completed frame = %#v", frame)
		}
	}
	starts, terminals := map[string]int{}, map[string]int{}
	for _, event := range lifecycle {
		id, _ := event.Payload["qualified_node_id"].(string)
		switch event.Kind {
		case "step/started":
			starts[id]++
			if terminals[id] != 0 {
				t.Errorf("child start published after durable terminal: %s", id)
			}
		case "step/completed":
			terminals[id]++
			if starts[id] != 1 || terminals[id] != 1 {
				t.Errorf("noncanonical child lifecycle %s: starts=%d terminals=%d", id, starts[id], terminals[id])
			}
		}
	}
	if len(terminals) != 3 {
		t.Fatalf("want parent and two child terminals, got %v", terminals)
	}
}

func TestRunSubStepsViaEngine_ResumesAfterCommittedNestedChild(t *testing.T) {
	base := t.TempDir()
	firstStore := internalrunstore.NewDirRunStore(base)
	platformFake := platform.NewFakePlatform()
	firstExecutor := &gatedNestedDispatchExecutor{
		blockedStep: "child-2", started: make(chan struct{}), release: make(chan struct{}),
	}
	firstRegistry := &testRegistry{executors: make(map[string]engine.StepExecutor)}
	firstRunner := func(ctx context.Context, parent executor.SubStepParent, steps []schema.FlowNode, vars map[string]any) ([]*engine.StepResult, error) {
		return runSubStepsViaEngine(
			ctx, firstRegistry, parent, steps, vars, engine.RunModeReal,
			&noopTraceWriter{}, &noopDispatcher{}, platformFake,
			nil, nil, nil, nil, nil, nil, nil,
		)
	}
	firstRegistry.Register("cli", firstExecutor)
	firstRegistry.Register("include", executor.NewIncludeExecutor(nil, firstRunner, nil))
	plan := nestedDispatchIncludePlan(t, "run-resume-nested-child")
	firstRuntime := internalengine.New(engine.EngineConfig{
		Executors: firstRegistry, Dispatcher: &noopDispatcher{}, TraceWriter: &noopTraceWriter{},
		Platform: platformFake, Store: firstStore,
	})
	firstHandle, err := firstRuntime.Start(context.Background(), plan, engine.RunOptions{Mode: engine.RunModeReal, Store: firstStore})
	if err != nil {
		t.Fatalf("Start first: %v", err)
	}
	firstDone := make(chan error, 1)
	go func() {
		_, nextErr := firstHandle.Next(context.Background())
		firstDone <- nextErr
	}()
	select {
	case <-firstExecutor.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first process did not reach child-2")
	}
	beforeCrash, err := firstStore.LoadState(context.Background(), plan.RunID)
	if err != nil {
		t.Fatalf("LoadState before crash: %v", err)
	}
	for _, frame := range beforeCrash.ExecutionFrames {
		if frame.NextStepIndex != 1 || frame.Results["0"] == nil {
			t.Fatalf("frame before crash = %#v", frame)
		}
	}
	if err := firstStore.Close(); err != nil {
		t.Fatalf("Close first process store: %v", err)
	}

	secondStore := internalrunstore.NewDirRunStore(base)
	secondExecutor := &nestedDispatchExecutor{}
	secondRegistry := &testRegistry{executors: make(map[string]engine.StepExecutor)}
	secondRunner := func(ctx context.Context, parent executor.SubStepParent, steps []schema.FlowNode, vars map[string]any) ([]*engine.StepResult, error) {
		return runSubStepsViaEngine(
			ctx, secondRegistry, parent, steps, vars, engine.RunModeReal,
			&noopTraceWriter{}, &noopDispatcher{}, platformFake,
			nil, nil, nil, nil, nil, nil, nil,
		)
	}
	secondRegistry.Register("cli", secondExecutor)
	secondRegistry.Register("include", executor.NewIncludeExecutor(nil, secondRunner, nil))
	secondRuntime := internalengine.New(engine.EngineConfig{
		Executors: secondRegistry, Dispatcher: &noopDispatcher{}, TraceWriter: &noopTraceWriter{},
		Platform: platformFake, Store: secondStore,
	})
	secondHandle, err := secondRuntime.Resume(context.Background(), plan.RunID, engine.RunOptions{Mode: engine.RunModeReal, Store: secondStore})
	if err != nil {
		t.Fatalf("Resume second: %v", err)
	}
	result, err := secondHandle.Next(context.Background())
	if err != nil {
		t.Fatalf("resumed Next: %v", err)
	}
	if result.StepID != "include" || len(secondExecutor.steps) != 1 || secondExecutor.steps[0] != "child-2" {
		t.Fatalf("resumed result=%#v executed=%#v", result, secondExecutor.steps)
	}
	resumedState := secondHandle.State()
	for _, frame := range resumedState.ExecutionFrames {
		if frame.Status != engine.ExecutionFrameStatusCompleted || frame.NextStepIndex != 2 {
			t.Fatalf("resumed frame = %#v", frame)
		}
	}

	close(firstExecutor.release)
	select {
	case staleErr := <-firstDone:
		if !errors.Is(staleErr, engine.ErrCheckpointCommit) && !errors.Is(staleErr, engine.ErrRunLeaseStale) {
			t.Fatalf("stale process error = %v", staleErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stale process did not stop")
	}
	if err := firstStore.Close(); err != nil {
		t.Fatalf("Close stale process store: %v", err)
	}
	if len(firstExecutor.steps) != 1 || firstExecutor.steps[0] != "child-1" {
		t.Fatalf("stale process dispatched after takeover: %#v", firstExecutor.steps)
	}
	_ = secondHandle.Cancel(context.Background(), "test cleanup")
	_ = secondStore.Close()
}

func TestRunSubStepsViaEngine_ResumesSequentialIterationFrame(t *testing.T) {
	base := t.TempDir()
	firstStore := internalrunstore.NewDirRunStore(base)
	platformFake := platform.NewFakePlatform()
	firstChild := &secondCallGatedDispatchExecutor{started: make(chan struct{}), release: make(chan struct{})}
	var releaseFirst sync.Once
	t.Cleanup(func() {
		releaseFirst.Do(func() { close(firstChild.release) })
		_ = firstStore.Close()
	})
	firstRegistry := &testRegistry{executors: make(map[string]engine.StepExecutor)}
	firstRunner := func(ctx context.Context, parent executor.SubStepParent, steps []schema.FlowNode, vars map[string]any) ([]*engine.StepResult, error) {
		return runSubStepsViaEngine(
			ctx, firstRegistry, parent, steps, vars, engine.RunModeReal,
			&noopTraceWriter{}, &noopDispatcher{}, platformFake,
			nil, nil, nil, nil, nil, nil, nil,
		)
	}
	firstRegistry.Register("cli", firstChild)
	firstRegistry.Register("iterate", executor.NewIterateExecutor(nil, nil, firstRunner))
	plan := persistentIteratePlan(t, "run-resume-iterate")
	firstRuntime := internalengine.New(engine.EngineConfig{
		Executors: firstRegistry, Dispatcher: &noopDispatcher{}, TraceWriter: &noopTraceWriter{},
		Platform: platformFake, Store: firstStore,
	})
	firstHandle, err := firstRuntime.Start(context.Background(), plan, engine.RunOptions{Store: firstStore})
	if err != nil {
		t.Fatalf("Start first: %v", err)
	}
	firstDone := make(chan error, 1)
	go func() {
		_, nextErr := firstHandle.Next(context.Background())
		firstDone <- nextErr
	}()
	select {
	case <-firstChild.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first process did not reach iteration 2")
	}
	state, err := firstStore.LoadState(context.Background(), plan.RunID)
	if err != nil {
		t.Fatalf("LoadState first: %v", err)
	}
	if len(state.ExecutionFrames) != 2 {
		t.Fatalf("iteration frame count = %d, want 2", len(state.ExecutionFrames))
	}
	if err := firstStore.Close(); err != nil {
		t.Fatalf("Close first process: %v", err)
	}

	secondStore := internalrunstore.NewDirRunStore(base)
	secondChild := &secondCallGatedDispatchExecutor{started: make(chan struct{}), release: make(chan struct{})}
	secondRegistry := &testRegistry{executors: make(map[string]engine.StepExecutor)}
	secondRunner := func(ctx context.Context, parent executor.SubStepParent, steps []schema.FlowNode, vars map[string]any) ([]*engine.StepResult, error) {
		return runSubStepsViaEngine(
			ctx, secondRegistry, parent, steps, vars, engine.RunModeReal,
			&noopTraceWriter{}, &noopDispatcher{}, platformFake,
			nil, nil, nil, nil, nil, nil, nil,
		)
	}
	secondRegistry.Register("cli", secondChild)
	secondRegistry.Register("iterate", executor.NewIterateExecutor(nil, nil, secondRunner))
	secondRuntime := internalengine.New(engine.EngineConfig{
		Executors: secondRegistry, Dispatcher: &noopDispatcher{}, TraceWriter: &noopTraceWriter{},
		Platform: platformFake, Store: secondStore,
	})
	secondHandle, err := secondRuntime.Resume(context.Background(), plan.RunID, engine.RunOptions{Store: secondStore})
	if err != nil {
		t.Fatalf("Resume second: %v", err)
	}
	result, err := secondHandle.Next(context.Background())
	if err != nil {
		t.Fatalf("resumed Next: %v", err)
	}
	if result.Status != engine.StepStatusCompleted || result.Output["iterations"] != 2 || secondChild.callCount() != 1 {
		t.Fatalf("resumed iterate result=%#v child calls=%d", result, secondChild.callCount())
	}

	releaseFirst.Do(func() { close(firstChild.release) })
	select {
	case staleErr := <-firstDone:
		if !errors.Is(staleErr, engine.ErrCheckpointCommit) && !errors.Is(staleErr, engine.ErrRunLeaseStale) {
			t.Fatalf("stale iterate error = %v", staleErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stale iterate process did not stop")
	}
	_ = firstStore.Close()
	_ = secondHandle.Cancel(context.Background(), "test cleanup")
	_ = secondStore.Close()
}

func persistentIteratePlan(t *testing.T, runID string) *engine.ExecutionPlan {
	t.Helper()
	iterate := &schema.IterateNode{
		ID: "iterate", Over: "2", As: "item",
		Steps: []schema.FlowNode{{Step: &schema.Step{ID: "child", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "child"}}}},
	}
	plan := &engine.ExecutionPlan{
		RunID: runID, RunbookPath: "iterate.runbook.yaml",
		Steps: []engine.ResolvedStep{
			{ID: "iterate", Kind: "iterate", Spec: iterate},
			{ID: "child", Kind: "cli", Spec: &schema.CLISpec{Command: "child"}, Depth: 1, ParentID: "iterate", ParentKind: "iterate"},
		},
		Metadata: engine.PlanMetadata{RunbookID: "iterate-resume"},
	}
	if err := internalplanner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	return plan
}

func nestedDispatchIncludePlan(t *testing.T, runID string) *engine.ExecutionPlan {
	t.Helper()
	plan := &engine.ExecutionPlan{
		RunID: runID, RunbookPath: "nested-dispatches.runbook.yaml",
		Steps: []engine.ResolvedStep{{
			ID: "include", Kind: "include", Spec: &schema.IncludeSpec{
				Include: schema.IncludeConfig{Runbook: "child.runbook.yaml"},
				ResolvedSteps: []schema.FlowNode{
					{Step: &schema.Step{ID: "child-1", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "one"}}},
					{Step: &schema.Step{ID: "child-2", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "two"}}},
				},
			},
		}},
		Metadata: engine.PlanMetadata{RunbookID: "nested-dispatches"},
	}
	if err := internalplanner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	return plan
}
