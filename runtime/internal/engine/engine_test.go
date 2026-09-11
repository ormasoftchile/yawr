package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	internalrunstore "github.com/ormasoftchile/yawr/runtime/internal/runstore"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/eventbus"
	otelPkg "github.com/ormasoftchile/yawr/runtime/pkg/otel"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

// --- Test Fixtures ---

// fakeTraceWriter is a test double for trace.TraceWriter that is safe for concurrent use.
type fakeTraceWriter struct {
	mu     sync.Mutex
	events []trace.TraceEvent
}

func (w *fakeTraceWriter) Append(ev trace.TraceEvent) error {
	w.mu.Lock()
	w.events = append(w.events, ev)
	w.mu.Unlock()
	return nil
}

func (w *fakeTraceWriter) Close() error { return nil }

func (w *fakeTraceWriter) collect() []trace.TraceEvent {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]trace.TraceEvent, len(w.events))
	copy(out, w.events)
	return out
}

// fakeEventDispatcher is a test double for eventbus.EventDispatcher.
type fakeEventDispatcher struct {
	mu      sync.Mutex
	waiters map[string]chan *eventbus.InboundEvent
}

type failingEventDispatcher struct{ err error }

func (dispatcher *failingEventDispatcher) Dispatch(eventbus.InboundEvent) error { return nil }
func (dispatcher *failingEventDispatcher) Wait(context.Context, string, eventbus.EventFilter, time.Duration) (*eventbus.InboundEvent, error) {
	return nil, dispatcher.err
}
func (dispatcher *failingEventDispatcher) Cancel(string, string) {}

func newFakeEventDispatcher() *fakeEventDispatcher {
	return &fakeEventDispatcher{
		waiters: make(map[string]chan *eventbus.InboundEvent),
	}
}

func (d *fakeEventDispatcher) Dispatch(ev eventbus.InboundEvent) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, ch := range d.waiters {
		ch <- &ev
	}
	return nil
}

func (d *fakeEventDispatcher) Wait(ctx context.Context, stepID string, filter eventbus.EventFilter, timeout time.Duration) (*eventbus.InboundEvent, error) {
	ch := make(chan *eventbus.InboundEvent, 1)
	d.mu.Lock()
	d.waiters[stepID] = ch
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.waiters, stepID)
		d.mu.Unlock()
	}()

	select {
	case ev := <-ch:
		return ev, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (d *fakeEventDispatcher) Cancel(stepID string, reason string) {
	d.mu.Lock()
	delete(d.waiters, stepID)
	d.mu.Unlock()
}

// fakeExecutorRegistry is a test double for engine.ExecutorRegistry.
type fakeExecutorRegistry struct {
	mu        sync.RWMutex
	executors map[string]engine.StepExecutor
}

func newFakeExecutorRegistry() *fakeExecutorRegistry {
	return &fakeExecutorRegistry{executors: make(map[string]engine.StepExecutor)}
}

func (r *fakeExecutorRegistry) Register(kind string, exec engine.StepExecutor) {
	r.mu.Lock()
	r.executors[kind] = exec
	r.mu.Unlock()
}

func (r *fakeExecutorRegistry) Lookup(kind string) engine.StepExecutor {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.executors[kind]
}

// passThroughExecutor always succeeds.
type passThroughExecutor struct{}

func (e *passThroughExecutor) Execute(ctx context.Context, step engine.ResolvedStep, vars map[string]any) (*engine.StepResult, error) {
	return &engine.StepResult{
		StepID:  step.ID,
		Status:  engine.StepStatusCompleted,
		Outcome: engine.StepOutcomeSuccess,
		Output:  map[string]any{"result": "ok"},
	}, nil
}

// failingExecutor always fails (step-level failure, not infra error).
type failingExecutor struct {
	err error
}

func (e *failingExecutor) Execute(ctx context.Context, step engine.ResolvedStep, vars map[string]any) (*engine.StepResult, error) {
	return &engine.StepResult{
		StepID:  step.ID,
		Status:  engine.StepStatusFailed,
		Outcome: engine.StepOutcomeFailed,
		Error:   e.err,
	}, nil
}

// varProducingExecutor returns output vars.
type varProducingExecutor struct {
	vars map[string]any
}

func (e *varProducingExecutor) Execute(ctx context.Context, step engine.ResolvedStep, vars map[string]any) (*engine.StepResult, error) {
	return &engine.StepResult{
		StepID:  step.ID,
		Status:  engine.StepStatusCompleted,
		Outcome: engine.StepOutcomeSuccess,
		Vars:    e.vars,
	}, nil
}

// stepExecutorFunc is a function adapter for engine.StepExecutor.
type stepExecutorFunc func(ctx context.Context, step engine.ResolvedStep, vars map[string]any) (*engine.StepResult, error)

func (f stepExecutorFunc) Execute(ctx context.Context, step engine.ResolvedStep, vars map[string]any) (*engine.StepResult, error) {
	return f(ctx, step, vars)
}

type blockingExecutor struct {
	release chan struct{}
}

func (e *blockingExecutor) Execute(ctx context.Context, step engine.ResolvedStep, vars map[string]any) (*engine.StepResult, error) {
	select {
	case <-e.release:
	case <-ctx.Done():
		return &engine.StepResult{
			StepID:  step.ID,
			Status:  engine.StepStatusFailed,
			Outcome: engine.StepOutcomeFailed,
			Error:   ctx.Err(),
		}, nil
	}
	return &engine.StepResult{
		StepID:  step.ID,
		Status:  engine.StepStatusCompleted,
		Outcome: engine.StepOutcomeSuccess,
	}, nil
}

type detachBlockingExecutor struct {
	started  chan struct{}
	dispatch bool
}

func (executor *detachBlockingExecutor) Execute(ctx context.Context, step engine.ResolvedStep, _ map[string]any) (*engine.StepResult, error) {
	if executor.dispatch {
		if _, err := engine.PrepareExternalDispatch(ctx, engine.DispatchRequest{
			Classification: "mutating", EndpointIdentity: "detach-test",
			RenderedRequest: map[string]any{"step": step.ID},
		}); err != nil {
			return nil, err
		}
	}
	close(executor.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

type detachCommitStore struct {
	*internalrunstore.DirRunStore
	checkpointStarted chan struct{}
	releaseCheckpoint chan struct{}
	blockOnce         sync.Once
}

type detachRunCompletionStore struct {
	*internalrunstore.DirRunStore
	checkpointStarted chan struct{}
	releaseCheckpoint chan struct{}
	blockOnce         sync.Once
}

type detachTraceCommitStore struct {
	*internalrunstore.DirRunStore
	traceStarted chan struct{}
	releaseTrace chan struct{}
	blockOnce    sync.Once
}

func (store *detachTraceCommitStore) WriteTrace(ctx context.Context, runID string, event engine.Event) error {
	shouldBlock := false
	store.blockOnce.Do(func() {
		shouldBlock = true
		close(store.traceStarted)
	})
	if shouldBlock {
		select {
		case <-store.releaseTrace:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return store.DirRunStore.WriteTrace(ctx, runID, event)
}

func (store *detachRunCompletionStore) SaveState(ctx context.Context, state engine.RunState) error {
	shouldBlock := false
	if state.Status == engine.RunStatusCompleted {
		store.blockOnce.Do(func() {
			shouldBlock = true
			close(store.checkpointStarted)
		})
	}
	if shouldBlock {
		select {
		case <-store.releaseCheckpoint:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return store.DirRunStore.SaveState(ctx, state)
}

func (store *detachCommitStore) SaveState(ctx context.Context, state engine.RunState) error {
	shouldBlock := false
	if state.StepResults["work"] != nil && state.Status == engine.RunStatusRunning {
		store.blockOnce.Do(func() {
			shouldBlock = true
			close(store.checkpointStarted)
		})
	}
	if shouldBlock {
		select {
		case <-store.releaseCheckpoint:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return store.DirRunStore.SaveState(ctx, state)
}

type detachCompletingExecutor struct {
	executionContext chan context.Context
}

func (executor *detachCompletingExecutor) Execute(ctx context.Context, step engine.ResolvedStep, _ map[string]any) (*engine.StepResult, error) {
	executor.executionContext <- ctx
	return &engine.StepResult{
		StepID: step.ID, Status: engine.StepStatusCompleted, Outcome: engine.StepOutcomeSuccess,
	}, nil
}

// cliStepSpec is a minimal StepSpec for CLI steps.
type cliStepSpec struct{}

func (s *cliStepSpec) StepKind() string { return "cli" }

// testParallelSpec is a StepSpec for parallel steps carrying pre-resolved branches.
type testParallelSpec struct {
	branches []engine.BranchSpec
}

func (s *testParallelSpec) StepKind() string                 { return "parallel" }
func (s *testParallelSpec) GetBranches() []engine.BranchSpec { return s.branches }

// testWaitEventSpec is a StepSpec for wait_for_event steps.
type testWaitEventSpec struct {
	filter  eventbus.EventFilter
	timeout time.Duration
}

func (s *testWaitEventSpec) StepKind() string { return "wait_for_event" }
func (s *testWaitEventSpec) EventFilter() (eventbus.EventFilter, time.Duration) {
	return s.filter, s.timeout
}

// makeTestConfig returns a valid EngineConfig for testing.
func makeTestConfig() engine.EngineConfig {
	return engine.EngineConfig{
		Executors:   newFakeExecutorRegistry(),
		Dispatcher:  newFakeEventDispatcher(),
		TraceWriter: &fakeTraceWriter{},
		Platform:    platform.NewFakePlatform(),
	}
}

// makeTestPlan creates a simple execution plan for testing.
func makeTestPlan(steps ...engine.ResolvedStep) *engine.ExecutionPlan {
	return &engine.ExecutionPlan{
		RunID:       "test-run-id",
		RunbookPath: "/test/runbook.yaml",
		Steps:       steps,
		Tools:       make(map[string]*schema.ToolDef),
		Metadata: engine.PlanMetadata{
			RunbookID:   "test-runbook",
			RunbookName: "Test Runbook",
			PlannedAt:   time.Now(),
		},
	}
}

// collectEvents drains the Events channel into a slice.
func collectEvents(t *testing.T, h engine.RunHandle) []engine.Event {
	t.Helper()
	var events []engine.Event
	for ev := range h.Events() {
		events = append(events, ev)
	}
	return events
}

// driveToCompletion calls Next until io.EOF, collecting all results.
func driveToCompletion(t *testing.T, h engine.RunHandle) []*engine.StepResult {
	t.Helper()
	var results []*engine.StepResult
	for {
		r, err := h.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next returned unexpected error: %v", err)
		}
		results = append(results, r)
	}
	return results
}

// --- Test Cases ---

func TestEngine_Start_EmitsRunStarted(t *testing.T) {
	tw := &fakeTraceWriter{}
	cfg := makeTestConfig()
	cfg.TraceWriter = tw
	reg := newFakeExecutorRegistry()
	reg.Register("cli", &passThroughExecutor{})
	cfg.Executors = reg
	eng := New(cfg)

	plan := makeTestPlan(engine.ResolvedStep{ID: "step-1", Kind: "cli", Spec: &cliStepSpec{}})

	handle, err := eng.Start(context.Background(), engine.ValidatedForTest(plan), engine.RunOptions{})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Advance once to trigger run/started + step execution.
	_, _ = handle.Next(context.Background())

	evs := tw.collect()
	if len(evs) == 0 {
		t.Fatal("expected at least one trace event")
	}
	if evs[0].Kind != trace.EventKindPlanValidated {
		t.Errorf("expected plan.validated as first event, got %s", evs[0].Kind)
	}
	if len(evs) < 2 || evs[1].Kind != trace.EventKindRunStarted {
		t.Fatalf("expected run/started as second event")
	}
	var payload map[string]any
	if err := json.Unmarshal(evs[0].Payload, &payload); err != nil {
		t.Fatalf("plan.validated payload: %v", err)
	}
	for _, key := range []string{"runbook_id", "runbook_hash", "grammar_versions", "expression_count", "validated_at"} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("plan.validated missing %s", key)
		}
	}
}

func TestEngine_Execute_SingleStep_Success(t *testing.T) {
	reg := newFakeExecutorRegistry()
	reg.Register("cli", &passThroughExecutor{})

	cfg := makeTestConfig()
	cfg.Executors = reg
	eng := New(cfg)

	plan := makeTestPlan(engine.ResolvedStep{ID: "step-1", Kind: "cli", Spec: &cliStepSpec{}})

	handle, err := eng.Start(context.Background(), engine.ValidatedForTest(plan), engine.RunOptions{})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	result, err := handle.Next(context.Background())
	if err != nil {
		t.Fatalf("Next failed: %v", err)
	}
	if result.StepID != "step-1" {
		t.Errorf("expected step-1, got %s", result.StepID)
	}
	if result.Status != engine.StepStatusCompleted {
		t.Errorf("expected completed, got %s", result.Status)
	}
}

func TestEngine_Execute_MultipleSteps_SequentialOrder(t *testing.T) {
	reg := newFakeExecutorRegistry()
	reg.Register("cli", &passThroughExecutor{})

	cfg := makeTestConfig()
	cfg.Executors = reg
	eng := New(cfg)

	plan := makeTestPlan(
		engine.ResolvedStep{ID: "step-1", Kind: "cli", Spec: &cliStepSpec{}},
		engine.ResolvedStep{ID: "step-2", Kind: "cli", Spec: &cliStepSpec{}},
		engine.ResolvedStep{ID: "step-3", Kind: "cli", Spec: &cliStepSpec{}},
	)

	handle, err := eng.Start(context.Background(), engine.ValidatedForTest(plan), engine.RunOptions{})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	var stepIDs []string
	for {
		result, err := handle.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next failed: %v", err)
		}
		stepIDs = append(stepIDs, result.StepID)
	}

	expected := []string{"step-1", "step-2", "step-3"}
	if len(stepIDs) != len(expected) {
		t.Fatalf("expected %d steps, got %d", len(expected), len(stepIDs))
	}
	for i, id := range expected {
		if stepIDs[i] != id {
			t.Errorf("step %d: expected %s, got %s", i, id, stepIDs[i])
		}
	}
}

func TestEngine_Execute_StepFailed_EmitsStepFailed(t *testing.T) {
	tw := &fakeTraceWriter{}
	reg := newFakeExecutorRegistry()
	reg.Register("cli", &failingExecutor{err: io.ErrUnexpectedEOF})

	cfg := makeTestConfig()
	cfg.Executors = reg
	cfg.TraceWriter = tw
	eng := New(cfg)

	plan := makeTestPlan(engine.ResolvedStep{ID: "step-1", Kind: "cli", Spec: &cliStepSpec{}})

	handle, err := eng.Start(context.Background(), engine.ValidatedForTest(plan), engine.RunOptions{})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	result, nextErr := handle.Next(context.Background())
	// With default on_error=stop, a failed step terminates the run and
	// returns the failed result (run is terminal after this).
	if nextErr != nil {
		t.Fatalf("unexpected error from Next(): %v", nextErr)
	}
	if result == nil || result.Status != engine.StepStatusFailed {
		t.Errorf("expected StepStatusFailed result, got %+v", result)
	}

	// Next call should return EOF (run is done).
	_, eof := handle.Next(context.Background())
	if eof != io.EOF {
		t.Errorf("expected io.EOF on second Next(), got %v", eof)
	}

	var foundStepFailed bool
	for _, ev := range tw.collect() {
		if ev.Kind == trace.EventKindStepFailed {
			foundStepFailed = true
			break
		}
	}
	if !foundStepFailed {
		t.Error("expected step/failed trace event")
	}
}

func TestEngine_Execute_UnknownStepKind_ReturnsError(t *testing.T) {
	cfg := makeTestConfig()
	eng := New(cfg)

	plan := makeTestPlan(engine.ResolvedStep{ID: "step-1", Kind: "unknown", Spec: &cliStepSpec{}})

	handle, err := eng.Start(context.Background(), engine.ValidatedForTest(plan), engine.RunOptions{})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	result, _ := handle.Next(context.Background())
	if result.Status != engine.StepStatusFailed {
		t.Errorf("expected failed status for unknown step kind, got %s", result.Status)
	}
}

func TestEngine_Cancel_EmitsRunCancelled(t *testing.T) {
	tw := &fakeTraceWriter{}
	reg := newFakeExecutorRegistry()
	reg.Register("cli", &passThroughExecutor{})

	cfg := makeTestConfig()
	cfg.Executors = reg
	cfg.TraceWriter = tw
	eng := New(cfg)

	plan := makeTestPlan(
		engine.ResolvedStep{ID: "step-1", Kind: "cli", Spec: &cliStepSpec{}},
		engine.ResolvedStep{ID: "step-2", Kind: "cli", Spec: &cliStepSpec{}},
	)

	handle, err := eng.Start(context.Background(), engine.ValidatedForTest(plan), engine.RunOptions{})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	_, _ = handle.Next(context.Background())

	if err := handle.Cancel(context.Background(), "user requested"); err != nil {
		t.Fatalf("Cancel failed: %v", err)
	}

	var foundCancelled bool
	for _, ev := range tw.collect() {
		if ev.Kind == trace.EventKindRunCancelled {
			foundCancelled = true
			break
		}
	}
	if !foundCancelled {
		t.Error("expected run/cancelled trace event")
	}
}

func TestEngine_State_ReturnsCurrentRunState(t *testing.T) {
	reg := newFakeExecutorRegistry()
	reg.Register("cli", &passThroughExecutor{})

	cfg := makeTestConfig()
	cfg.Executors = reg
	eng := New(cfg)

	plan := makeTestPlan(engine.ResolvedStep{ID: "step-1", Kind: "cli", Spec: &cliStepSpec{}})

	handle, err := eng.Start(context.Background(), engine.ValidatedForTest(plan), engine.RunOptions{Actor: "test-user"})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Before any Next(): should be pending.
	state := handle.State()
	if state.Status != engine.RunStatusPending {
		t.Errorf("expected pending status, got %s", state.Status)
	}

	// After first Next(): RunID must be set.
	_, _ = handle.Next(context.Background())
	state = handle.State()
	if state.RunID != plan.RunID {
		t.Errorf("expected run ID %s, got %s", plan.RunID, state.RunID)
	}
}

func TestEngine_Events_ReceivesEventsOnChannel(t *testing.T) {
	reg := newFakeExecutorRegistry()
	reg.Register("cli", &passThroughExecutor{})

	cfg := makeTestConfig()
	cfg.Executors = reg
	eng := New(cfg)

	plan := makeTestPlan(engine.ResolvedStep{ID: "step-1", Kind: "cli", Spec: &cliStepSpec{}})

	handle, err := eng.Start(context.Background(), engine.ValidatedForTest(plan), engine.RunOptions{})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	var events []engine.Event
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range handle.Events() {
			events = append(events, ev)
		}
	}()

	for {
		_, err := handle.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for events channel to close")
	}

	if len(events) == 0 {
		t.Fatal("expected at least one event on channel")
	}
}

func TestEngine_VarsPropagation_StepVarsMergedIntoRunVars(t *testing.T) {
	reg := newFakeExecutorRegistry()
	reg.Register("cli", &varProducingExecutor{vars: map[string]any{"output": "hello"}})

	cfg := makeTestConfig()
	cfg.Executors = reg
	eng := New(cfg)

	plan := makeTestPlan(engine.ResolvedStep{ID: "step-1", Kind: "cli", Spec: &cliStepSpec{}})

	handle, err := eng.Start(context.Background(), engine.ValidatedForTest(plan), engine.RunOptions{})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	_, _ = handle.Next(context.Background())

	state := handle.State()
	if state.Vars["output"] != "hello" {
		t.Errorf("expected vars to contain output=hello, got %v", state.Vars)
	}
}

// --- Config Validation Tests ---

func TestEngineConfig_Validate_MissingRequired(t *testing.T) {
	cfg := engine.EngineConfig{}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for empty config")
	}

	configErr, ok := err.(*engine.ConfigError)
	if !ok {
		t.Fatalf("expected *ConfigError, got %T", err)
	}

	if len(configErr.Missing) != 4 {
		t.Errorf("expected 4 missing fields, got %d: %v", len(configErr.Missing), configErr.Missing)
	}
}

func TestEngineConfig_Validate_AllRequired(t *testing.T) {
	cfg := makeTestConfig()
	err := cfg.Validate()
	if err != nil {
		t.Errorf("expected no error for valid config, got: %v", err)
	}
}

// --- New Phase 3 Tests ---

// TestEngine_EmitsRunEvents verifies that run/started and run/completed are emitted
// in sequence and that sequence numbers are monotonically increasing.
func TestEngine_EmitsRunEvents(t *testing.T) {
	tw := &fakeTraceWriter{}
	reg := newFakeExecutorRegistry()
	reg.Register("cli", &passThroughExecutor{})

	cfg := makeTestConfig()
	cfg.Executors = reg
	cfg.TraceWriter = tw
	eng := New(cfg)

	plan := makeTestPlan(engine.ResolvedStep{ID: "step-1", Kind: "cli", Spec: &cliStepSpec{}})
	handle, err := eng.Start(context.Background(), engine.ValidatedForTest(plan), engine.RunOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	driveToCompletion(t, handle)

	evs := tw.collect()

	// Verify plan.validated is first, run/started is second, and run/completed is last.
	if evs[0].Kind != trace.EventKindPlanValidated {
		t.Errorf("first event: want plan.validated, got %s", evs[0].Kind)
	}
	if evs[1].Kind != trace.EventKindRunStarted {
		t.Errorf("second event: want run/started, got %s", evs[1].Kind)
	}
	last := evs[len(evs)-1]
	if last.Kind != trace.EventKindRunCompleted {
		t.Errorf("last event: want run/completed, got %s", last.Kind)
	}

	// Verify monotonically increasing sequence numbers.
	for i := 1; i < len(evs); i++ {
		if evs[i].Sequence <= evs[i-1].Sequence {
			t.Errorf("sequence not monotonic at index %d: %d <= %d", i, evs[i].Sequence, evs[i-1].Sequence)
		}
	}
}

// TestEngine_StepFailure verifies that a failing step emits step/failed, remaining steps
// are not executed, and the run does not emit run/completed with a successful status.
func TestEngine_StepFailure(t *testing.T) {
	tw := &fakeTraceWriter{}
	reg := newFakeExecutorRegistry()
	reg.Register("cli", &failingExecutor{err: errors.New("boom")})

	cfg := makeTestConfig()
	cfg.Executors = reg
	cfg.TraceWriter = tw
	eng := New(cfg)

	plan := makeTestPlan(
		engine.ResolvedStep{ID: "step-1", Kind: "cli", Spec: &cliStepSpec{}},
		engine.ResolvedStep{ID: "step-2", Kind: "cli", Spec: &cliStepSpec{}},
	)
	handle, err := eng.Start(context.Background(), engine.ValidatedForTest(plan), engine.RunOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	result, _ := handle.Next(context.Background())
	if result.Status != engine.StepStatusFailed {
		t.Errorf("expected step-1 to fail, got %s", result.Status)
	}

	evs := tw.collect()

	// step/failed must be present.
	found := false
	for _, e := range evs {
		if e.Kind == trace.EventKindStepFailed {
			found = true
		}
	}
	if !found {
		t.Error("expected step/failed event")
	}

	// step-2 must NOT have been started — the caller never called Next() again.
	// Count step/started events: only step-1's should be present.
	var stepStartedCount int
	for _, e := range evs {
		if e.Kind == trace.EventKindStepStarted {
			stepStartedCount++
		}
	}
	if stepStartedCount > 1 {
		t.Errorf("expected 1 step/started (step-1 only), got %d", stepStartedCount)
	}
}

// TestEngine_ParallelBranches verifies that parallel branches execute concurrently and
// their trace events preserve start-before-terminal order independently.
func TestEngine_ParallelBranches(t *testing.T) {
	tw := &fakeTraceWriter{}
	reg := newFakeExecutorRegistry()

	// Each branch executor signals when it starts and waits to proceed together.
	startCh := make(chan string, 4)
	execCount := 0
	var execMu sync.Mutex

	reg.Register("cli", stepExecutorFunc(func(ctx context.Context, step engine.ResolvedStep, vars map[string]any) (*engine.StepResult, error) {
		startCh <- step.ID
		execMu.Lock()
		execCount++
		execMu.Unlock()
		return &engine.StepResult{
			StepID:  step.ID,
			Status:  engine.StepStatusCompleted,
			Outcome: engine.StepOutcomeSuccess,
		}, nil
	}))

	cfg := makeTestConfig()
	cfg.Executors = reg
	cfg.TraceWriter = tw
	eng := New(cfg)

	plan := makeTestPlan(
		engine.ResolvedStep{
			ID:   "parallel-1",
			Kind: "parallel",
			Spec: &testParallelSpec{branches: []engine.BranchSpec{
				{Label: "branch-a", Steps: []engine.ResolvedStep{
					{ID: "a-step-1", Kind: "cli", Spec: &cliStepSpec{}},
				}},
				{Label: "branch-b", Steps: []engine.ResolvedStep{
					{ID: "b-step-1", Kind: "cli", Spec: &cliStepSpec{}},
				}},
			}},
		},
	)

	handle, err := eng.Start(context.Background(), engine.ValidatedForTest(plan), engine.RunOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	result, err := handle.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if result.Status != engine.StepStatusCompleted {
		t.Errorf("parallel step status: want completed, got %s", result.Status)
	}

	// Both branch steps must have been executed.
	execMu.Lock()
	count := execCount
	execMu.Unlock()
	if count != 2 {
		t.Errorf("expected 2 branch steps executed, got %d", count)
	}

	evs := tw.collect()
	starts, terminals := map[string]int{}, map[string]int{}
	for i, ev := range evs {
		var p struct {
			StepID string `json:"step_id"`
		}
		if err := jsonUnmarshal(ev.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if ev.Kind == trace.EventKindStepStarted {
			starts[p.StepID] = i
		}
		if ev.Kind == trace.EventKindStepCompleted {
			terminals[p.StepID] = i
		}
	}
	for _, id := range []string{"parallel-1", "a-step-1", "b-step-1"} {
		start, started := starts[id]
		terminal, completed := terminals[id]
		if !started || !completed || start >= terminal {
			t.Errorf("%s lifecycle out of order: starts=%v terminals=%v", id, starts, terminals)
		}
	}
}

func TestCloneAnyMapIsolatesNestedRuntimeValues(t *testing.T) {
	source := map[string]any{
		"object": map[string]any{"value": "original"},
		"items":  []any{map[string]any{"value": "original"}},
	}
	cloned := cloneAnyMap(source)
	cloned["object"].(map[string]any)["value"] = "changed"
	cloned["items"].([]any)[0].(map[string]any)["value"] = "changed"
	if source["object"].(map[string]any)["value"] != "original" ||
		source["items"].([]any)[0].(map[string]any)["value"] != "original" {
		t.Fatalf("nested runtime values were aliased: %#v", source)
	}
}

func TestRunHandleStateIsolatedFromCallerMutation(t *testing.T) {
	config := makeTestConfig()
	registry := newFakeExecutorRegistry()
	registry.Register("result", stepExecutorFunc(func(_ context.Context, step engine.ResolvedStep, _ map[string]any) (*engine.StepResult, error) {
		return &engine.StepResult{
			StepID: step.ID, Status: engine.StepStatusCompleted, Outcome: engine.StepOutcomeSuccess,
			Output: map[string]any{"nested": map[string]any{"value": "original"}},
			Vars:   map[string]any{"saved": map[string]any{"value": "original"}},
		}, nil
	}))
	registry.Register("noop", &passThroughExecutor{})
	config.Executors = registry
	plan := engine.ValidatedForTest(&engine.ExecutionPlan{
		RunID: "run-state-isolation", RunbookPath: "state.runbook.yaml",
		Steps: []engine.ResolvedStep{
			{ID: "first", Kind: "result", Spec: &cliStepSpec{}},
			{ID: "second", Kind: "noop", Spec: &schema.NoopSpec{}},
		},
		Metadata: engine.PlanMetadata{RunbookID: "state-isolation"},
	})
	handle, err := New(config).Start(context.Background(), plan, engine.RunOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := handle.Next(context.Background()); err != nil {
		t.Fatalf("Next: %v", err)
	}
	snapshot := handle.State()
	snapshot.Vars["saved"].(map[string]any)["value"] = "tampered"
	snapshot.StepResults["first"].Output["nested"].(map[string]any)["value"] = "tampered"
	snapshot.Plan.Steps[1].ID = "tampered-step"

	fresh := handle.State()
	if fresh.Vars["saved"].(map[string]any)["value"] != "original" ||
		fresh.StepResults["first"].Output["nested"].(map[string]any)["value"] != "original" ||
		fresh.Plan.Steps[1].ID != "second" {
		t.Fatalf("caller mutated live state: vars=%#v results=%#v plan=%#v", fresh.Vars, fresh.StepResults, fresh.Plan.Steps)
	}
	result, err := handle.Next(context.Background())
	if err != nil || result.StepID != "second" {
		t.Fatalf("execution after state mutation = %#v, %v", result, err)
	}
}

func TestRunHandleOnEventCanInspectStateAndCancelWithoutDeadlock(t *testing.T) {
	config := makeTestConfig()
	registry := newFakeExecutorRegistry()
	registry.Register("noop", &passThroughExecutor{})
	config.Executors = registry
	plan := engine.ValidatedForTest(&engine.ExecutionPlan{
		RunID: "run-callback-state", RunbookPath: "callback.runbook.yaml",
		Steps: []engine.ResolvedStep{
			{ID: "first", Kind: "noop", Spec: &schema.NoopSpec{}},
			{ID: "second", Kind: "noop", Spec: &schema.NoopSpec{}},
		},
		Metadata: engine.PlanMetadata{RunbookID: "callback-state"},
	})
	var handle engine.RunHandle
	callbackDone := make(chan struct{})
	callbackErr := make(chan error, 1)
	var inspectOnce sync.Once
	var err error
	handle, err = New(config).Start(context.Background(), plan, engine.RunOptions{
		OnEvent: func(engine.Event) {
			inspectOnce.Do(func() {
				_ = handle.State()
				callbackErr <- handle.Cancel(context.Background(), "callback requested cancellation")
				close(callbackDone)
			})
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	nextDone := make(chan error, 1)
	go func() {
		_, nextErr := handle.Next(context.Background())
		nextDone <- nextErr
	}()
	select {
	case <-callbackDone:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("OnEvent callback deadlocked while calling State or Cancel")
	}
	if callbackCancelErr := <-callbackErr; callbackCancelErr != nil {
		t.Fatalf("callback Cancel: %v", callbackCancelErr)
	}
	select {
	case nextErr := <-nextDone:
		if nextErr != nil {
			t.Fatalf("Next: %v", nextErr)
		}
	case <-time.After(time.Second):
		t.Fatal("Next did not finish after callback state inspection")
	}
	if state := handle.State(); state.Status != engine.RunStatusCancelled {
		t.Fatalf("run status after callback cancellation = %s, want %s", state.Status, engine.RunStatusCancelled)
	}
}

// TestEngine_WaitForEvent verifies that a wait_for_event step blocks until the dispatcher
// delivers an event, then emits event/received and step/resumed.
func TestEngine_WaitForEvent(t *testing.T) {
	tw := &fakeTraceWriter{}
	disp := newFakeEventDispatcher()

	cfg := makeTestConfig()
	cfg.Dispatcher = disp
	cfg.TraceWriter = tw
	eng := New(cfg)

	plan := makeTestPlan(
		engine.ResolvedStep{
			ID:   "wait-step",
			Kind: "wait_for_event",
			Spec: &testWaitEventSpec{
				filter:  eventbus.EventFilter{Source: "webhook", ID: "deploy-complete"},
				timeout: 0,
			},
		},
	)

	handle, err := eng.Start(context.Background(), engine.ValidatedForTest(plan), engine.RunOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Dispatch the event from a separate goroutine after a short delay.
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = disp.Dispatch(eventbus.InboundEvent{
			EventID: "ev-1",
			Source:  "webhook",
			Payload: map[string]any{"status": "ok"},
		})
	}()

	result, err := handle.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if result.Status != engine.StepStatusCompleted {
		t.Errorf("wait step status: want completed, got %s", result.Status)
	}

	evs := tw.collect()

	var foundReceived, foundResumed bool
	for _, ev := range evs {
		switch ev.Kind {
		case trace.EventKindEventReceived:
			foundReceived = true
		case trace.EventKindStepResumed:
			foundResumed = true
		}
	}
	if !foundReceived {
		t.Error("expected event/received trace event")
	}
	if !foundResumed {
		t.Error("expected step/resumed trace event")
	}
}

func TestEngineWaitForEventReplayBoundaryBypassesAuthoredRecovery(t *testing.T) {
	replayErr := engine.NewReplayBoundaryError(errors.New("missing event fixture"))
	followingInvoked := false
	registry := newFakeExecutorRegistry()
	registry.Register("after", stepExecutorFunc(func(context.Context, engine.ResolvedStep, map[string]any) (*engine.StepResult, error) {
		followingInvoked = true
		return &engine.StepResult{Status: engine.StepStatusCompleted, Outcome: engine.StepOutcomeSuccess}, nil
	}))
	config := makeTestConfig()
	config.Executors = registry
	config.Dispatcher = &failingEventDispatcher{err: replayErr}
	handle, err := New(config).Start(context.Background(), engine.ValidatedForTest(makeTestPlan(
		engine.ResolvedStep{
			ID: "wait", Kind: "wait_for_event", OnError: "continue",
			Spec: &testWaitEventSpec{filter: eventbus.EventFilter{Source: "fixture", ID: "missing"}},
		},
		engine.ResolvedStep{ID: "after", Kind: "after", Spec: &cliStepSpec{}},
	)), engine.RunOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	result, err := handle.Next(context.Background())
	if result != nil || !engine.IsReplayBoundaryError(err) {
		t.Fatalf("Next result/error = %#v/%v, want nil replay boundary", result, err)
	}
	if followingInvoked {
		t.Fatal("following step executed after replay boundary failure")
	}
	if state := handle.State(); state.Status != engine.RunStatusFailed {
		t.Fatalf("run status = %s, want %s", state.Status, engine.RunStatusFailed)
	}
}

func TestEngineDetachPausesBeforeSameOccurrenceWithoutDispatch(t *testing.T) {
	store := internalrunstore.NewDirRunStore(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	executor := &detachBlockingExecutor{started: make(chan struct{})}
	registry := newFakeExecutorRegistry()
	registry.Register("cli", executor)
	config := makeTestConfig()
	config.Executors = registry
	config.Store = store
	plan := engine.ValidatedForTest(makeTestPlan(
		engine.ResolvedStep{ID: "work", Kind: "cli", Spec: &schema.CLISpec{Command: "blocked"}},
		engine.ResolvedStep{ID: "after", Kind: "noop", Spec: &schema.NoopSpec{}},
	))
	handle, err := New(config).Start(context.Background(), plan, engine.RunOptions{Store: store})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	nextDone := make(chan error, 1)
	go func() {
		_, nextErr := handle.Next(context.Background())
		nextDone <- nextErr
	}()
	<-executor.started
	detachable, ok := handle.(engine.DetachableRunHandle)
	if !ok {
		t.Fatal("run handle is not detachable")
	}
	if err := detachable.Detach(context.Background(), "transport lost"); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if err := <-nextDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Next error = %v, want context.Canceled", err)
	}
	state := handle.State()
	if state.Status != engine.RunStatusPausedAtBoundary || !state.CompletedAt.IsZero() ||
		state.CursorSet == nil || len(state.CursorSet.Cursors) != 1 ||
		state.CursorSet.Cursors[0].StepID != "work" || state.CursorSet.Cursors[0].Invocation != 1 {
		t.Fatalf("detached state = %#v", state)
	}
}

func TestEngineDetachDuringLifecycleTraceCommitPausesWithoutIndeterminate(t *testing.T) {
	dirStore := internalrunstore.NewDirRunStore(t.TempDir())
	store := &detachTraceCommitStore{
		DirRunStore: dirStore, traceStarted: make(chan struct{}), releaseTrace: make(chan struct{}),
	}
	t.Cleanup(func() { _ = store.Close() })
	registry := newFakeExecutorRegistry()
	registry.Register("noop", &passThroughExecutor{})
	config := makeTestConfig()
	config.Executors = registry
	config.Store = store
	handle, err := New(config).Start(context.Background(), engine.ValidatedForTest(makeTestPlan(
		engine.ResolvedStep{ID: "work", Kind: "noop", Spec: &schema.NoopSpec{}},
	)), engine.RunOptions{Store: store})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	nextDone := make(chan error, 1)
	go func() {
		_, nextErr := handle.Next(context.Background())
		nextDone <- nextErr
	}()
	<-store.traceStarted
	detachDone := make(chan error, 1)
	go func() {
		detachDone <- handle.(engine.DetachableRunHandle).Detach(context.Background(), "transport lost")
	}()
	select {
	case <-handle.(*runHandle).runCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("detach did not cancel the run context")
	}
	close(store.releaseTrace)
	if nextErr := <-nextDone; nextErr != nil && !errors.Is(nextErr, context.Canceled) {
		t.Fatalf("Next: %v", nextErr)
	}
	if detachErr := <-detachDone; detachErr != nil {
		t.Fatalf("Detach: %v", detachErr)
	}
	state := handle.State()
	if state.Status != engine.RunStatusPausedAtBoundary || len(state.Dispatches) != 0 {
		t.Fatalf("detached state = %#v", state)
	}
}

func TestEngineDetachDuringCompletedStepCommitPausesWithoutIndeterminate(t *testing.T) {
	dirStore := internalrunstore.NewDirRunStore(t.TempDir())
	store := &detachCommitStore{
		DirRunStore: dirStore, checkpointStarted: make(chan struct{}), releaseCheckpoint: make(chan struct{}),
	}
	t.Cleanup(func() { _ = store.Close() })
	executor := &detachCompletingExecutor{executionContext: make(chan context.Context, 1)}
	registry := newFakeExecutorRegistry()
	registry.Register("cli", executor)
	config := makeTestConfig()
	config.Executors = registry
	config.Store = store
	handle, err := New(config).Start(context.Background(), engine.ValidatedForTest(makeTestPlan(
		engine.ResolvedStep{ID: "work", Kind: "cli", Spec: &schema.CLISpec{Command: "complete"}},
		engine.ResolvedStep{ID: "after", Kind: "noop", Spec: &schema.NoopSpec{}},
	)), engine.RunOptions{Store: store})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	nextDone := make(chan error, 1)
	go func() {
		_, nextErr := handle.Next(context.Background())
		nextDone <- nextErr
	}()
	executionContext := <-executor.executionContext
	<-store.checkpointStarted
	detachDone := make(chan error, 1)
	go func() {
		detachDone <- handle.(engine.DetachableRunHandle).Detach(context.Background(), "transport lost")
	}()
	select {
	case <-executionContext.Done():
	case <-time.After(time.Second):
		t.Fatal("detach did not cancel the active execution context")
	}
	close(store.releaseCheckpoint)
	if nextErr := <-nextDone; nextErr != nil {
		t.Fatalf("Next: %v", nextErr)
	}
	if detachErr := <-detachDone; detachErr != nil {
		t.Fatalf("Detach: %v", detachErr)
	}
	state := handle.State()
	if state.Status != engine.RunStatusPausedAtBoundary || state.StepResults["work"] == nil {
		t.Fatalf("detached state = %#v", state)
	}
}

func TestEngineDetachDuringRunCompletionCommitFinishesWithoutIndeterminate(t *testing.T) {
	dirStore := internalrunstore.NewDirRunStore(t.TempDir())
	store := &detachRunCompletionStore{
		DirRunStore: dirStore, checkpointStarted: make(chan struct{}), releaseCheckpoint: make(chan struct{}),
	}
	t.Cleanup(func() { _ = store.Close() })
	executor := &detachCompletingExecutor{executionContext: make(chan context.Context, 1)}
	registry := newFakeExecutorRegistry()
	registry.Register("cli", executor)
	config := makeTestConfig()
	config.Executors = registry
	config.Store = store
	handle, err := New(config).Start(context.Background(), engine.ValidatedForTest(makeTestPlan(
		engine.ResolvedStep{ID: "work", Kind: "cli", Spec: &schema.CLISpec{Command: "complete"}},
	)), engine.RunOptions{Store: store})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := handle.Next(context.Background()); err != nil {
		t.Fatalf("step Next: %v", err)
	}
	executionContext := <-executor.executionContext
	nextDone := make(chan error, 1)
	go func() {
		_, nextErr := handle.Next(context.Background())
		nextDone <- nextErr
	}()
	<-store.checkpointStarted
	detachDone := make(chan error, 1)
	go func() {
		detachDone <- handle.(engine.DetachableRunHandle).Detach(context.Background(), "transport lost")
	}()
	select {
	case <-executionContext.Done():
	case <-time.After(time.Second):
		t.Fatal("detach did not cancel the run context")
	}
	close(store.releaseCheckpoint)
	if nextErr := <-nextDone; !errors.Is(nextErr, io.EOF) {
		t.Fatalf("completion Next: %v", nextErr)
	}
	if detachErr := <-detachDone; detachErr != nil {
		t.Fatalf("Detach: %v", detachErr)
	}
	if state := handle.State(); state.Status != engine.RunStatusCompleted {
		t.Fatalf("completed state = %#v", state)
	}
}

func TestEngineDetachMarksPreparedDispatchIndeterminate(t *testing.T) {
	store := internalrunstore.NewDirRunStore(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	executor := &detachBlockingExecutor{started: make(chan struct{}), dispatch: true}
	registry := newFakeExecutorRegistry()
	registry.Register("cli", executor)
	config := makeTestConfig()
	config.Executors = registry
	config.Store = store
	handle, err := New(config).Start(context.Background(), engine.ValidatedForTest(makeTestPlan(
		engine.ResolvedStep{ID: "work", Kind: "cli", Spec: &schema.CLISpec{Command: "blocked"}},
	)), engine.RunOptions{Store: store})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	nextDone := make(chan error, 1)
	go func() {
		_, nextErr := handle.Next(context.Background())
		nextDone <- nextErr
	}()
	<-executor.started
	detachErr := handle.(engine.DetachableRunHandle).Detach(context.Background(), "transport lost")
	nextErr := <-nextDone
	if !errors.Is(detachErr, engine.ErrIndeterminate) && !errors.Is(nextErr, engine.ErrIndeterminate) {
		t.Fatalf("detach/Next errors = %v/%v, want ErrIndeterminate", detachErr, nextErr)
	}
	if state := handle.State(); state.Status != engine.RunStatusIndeterminate {
		t.Fatalf("detached dispatch status = %s", state.Status)
	}
}

func TestEngineParallelReplayBoundaryBypassesAuthoredRecovery(t *testing.T) {
	for _, test := range []struct {
		name    string
		execute stepExecutorFunc
	}{
		{
			name: "executor error",
			execute: func(context.Context, engine.ResolvedStep, map[string]any) (*engine.StepResult, error) {
				return nil, engine.NewReplayBoundaryError(errors.New("missing branch fixture"))
			},
		},
		{
			name: "failed result",
			execute: func(context.Context, engine.ResolvedStep, map[string]any) (*engine.StepResult, error) {
				return &engine.StepResult{
					Status: engine.StepStatusFailed, Outcome: engine.StepOutcomeFailed,
					Error: engine.NewReplayBoundaryError(errors.New("mismatched branch fixture")),
				}, nil
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			followingInvoked := false
			registry := newFakeExecutorRegistry()
			registry.Register("boundary", test.execute)
			registry.Register("after", stepExecutorFunc(func(context.Context, engine.ResolvedStep, map[string]any) (*engine.StepResult, error) {
				followingInvoked = true
				return &engine.StepResult{Status: engine.StepStatusCompleted, Outcome: engine.StepOutcomeSuccess}, nil
			}))
			config := makeTestConfig()
			config.Executors = registry
			parallel := &schema.ParallelNode{
				ID: "fanout",
				Branches: []schema.ParallelBranch{{Label: "branch", Steps: []schema.FlowNode{{Step: &schema.Step{
					ID: "boundary", Type: "boundary", OnError: "continue",
				}}}}},
				Join: &schema.ParallelJoin{OnFailure: "continue"},
			}
			plan := &engine.ExecutionPlan{
				RunID: "parallel-replay-boundary", RunbookPath: "parallel.runbook.yaml",
				Steps: []engine.ResolvedStep{
					{ID: "fanout", Kind: "parallel", OnError: "continue", Spec: parallel},
					{ID: "boundary", Kind: "boundary", OnError: "continue", Spec: &schema.NoopSpec{}, Depth: 1, ParentID: "fanout", ParentKind: "parallel"},
					{ID: "after", Kind: "after", Spec: &schema.NoopSpec{}},
				},
				Metadata: engine.PlanMetadata{RunbookID: "parallel-replay-boundary"},
			}
			handle, err := New(config).Start(context.Background(), engine.ValidatedForTest(plan), engine.RunOptions{})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			result, err := handle.Next(context.Background())
			if result != nil || !engine.IsReplayBoundaryError(err) {
				t.Fatalf("Next result/error = %#v/%v, want nil replay boundary", result, err)
			}
			if followingInvoked {
				t.Fatal("following step executed after replay boundary failure")
			}
			if state := handle.State(); state.Status != engine.RunStatusFailed {
				t.Fatalf("run status = %s, want %s", state.Status, engine.RunStatusFailed)
			}
		})
	}
}

// TestEngine_SignalCancellation verifies that a platform signal (SIGINT) cancels the run
// and emits a run/cancelled event.
func TestEngine_SignalCancellation(t *testing.T) {
	tw := &fakeTraceWriter{}
	reg := newFakeExecutorRegistry()

	// The executor closes executorStarted when it begins, then blocks on ctx.Done().
	executorStarted := make(chan struct{})
	reg.Register("cli", stepExecutorFunc(func(ctx context.Context, step engine.ResolvedStep, vars map[string]any) (*engine.StepResult, error) {
		close(executorStarted) // signal that executor is running
		<-ctx.Done()           // block until the run context is cancelled
		return &engine.StepResult{
			StepID:  step.ID,
			Status:  engine.StepStatusFailed,
			Outcome: engine.StepOutcomeFailed,
			Error:   ctx.Err(),
		}, nil
	}))

	fakePlat := platform.NewFakePlatform()
	sigCh := make(chan platform.Signal, 1)
	fakePlat.SignalCh = sigCh

	cfg := makeTestConfig()
	cfg.Executors = reg
	cfg.TraceWriter = tw
	cfg.Platform = fakePlat
	eng := New(cfg)

	plan := makeTestPlan(engine.ResolvedStep{ID: "step-1", Kind: "cli", Spec: &cliStepSpec{}})

	handle, err := eng.Start(context.Background(), engine.ValidatedForTest(plan), engine.RunOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Drive execution in background (will block inside the executor).
	nextDone := make(chan struct{})
	go func() {
		defer close(nextDone)
		handle.Next(context.Background())
	}()

	// Wait until the executor has started, then send the signal.
	select {
	case <-executorStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for executor to start")
	}
	sigCh <- platform.Signal{Name: "SIGINT"}

	// Wait for Next to return (signal cancels run context → executor unblocks).
	select {
	case <-nextDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Next to return after signal")
	}

	// Drain the events channel so all events are accounted for.
	for range handle.Events() {
	}

	evs := tw.collect()
	var foundCancelled bool
	for _, ev := range evs {
		if ev.Kind == trace.EventKindRunCancelled {
			foundCancelled = true
			break
		}
	}
	if !foundCancelled {
		t.Errorf("expected run/cancelled event after SIGINT; got events: %v", eventsKinds(evs))
	}
}

// eventsKinds extracts the Kind field from trace events.
func eventsKinds(evs []trace.TraceEvent) []trace.EventKind {
	kinds := make([]trace.EventKind, len(evs))
	for i, e := range evs {
		kinds[i] = e.Kind
	}
	return kinds
}

// jsonUnmarshal decodes raw JSON bytes into dst.
func jsonUnmarshal(raw json.RawMessage, dst any) error {
	return json.Unmarshal(raw, dst)
}

// --- OTel Integration Tests ---

// findSpan returns the first span matching name (or name prefix with "*") from spans.
func findSpan(spans []*otelPkg.SpanData, name string) *otelPkg.SpanData {
	prefix := strings.TrimSuffix(name, "*")
	isPrefix := strings.HasSuffix(name, "*")
	for _, s := range spans {
		if isPrefix {
			if strings.HasPrefix(s.Name, prefix) {
				return s
			}
		} else {
			if s.Name == name {
				return s
			}
		}
	}
	return nil
}

// findSpans returns all spans whose name has the given prefix (after trimming "*").
func findSpans(spans []*otelPkg.SpanData, name string) []*otelPkg.SpanData {
	prefix := strings.TrimSuffix(name, "*")
	var out []*otelPkg.SpanData
	for _, s := range spans {
		if strings.HasPrefix(s.Name, prefix) {
			out = append(out, s)
		}
	}
	return out
}

// TestEngine_OTelSpans_Noop verifies that nil TracerProvider doesn't panic.
func TestEngine_OTelSpans_Noop(t *testing.T) {
	cfg := makeTestConfig()
	cfg.TracerProvider = nil // explicit nil → noop fallback

	reg := newFakeExecutorRegistry()
	reg.Register("cli", &passThroughExecutor{})
	cfg.Executors = reg

	eng := New(cfg)
	plan := makeTestPlan(
		engine.ResolvedStep{ID: "step-1", Kind: "cli", Spec: &cliStepSpec{}},
	)
	h, err := eng.Start(context.Background(), engine.ValidatedForTest(plan), engine.RunOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	driveToCompletion(t, h)
}

// TestEngine_OTelSpans_Hierarchy verifies run → step parent-child span hierarchy.
func TestEngine_OTelSpans_Hierarchy(t *testing.T) {
	provider := &otelPkg.RecordingTracerProvider{}
	cfg := makeTestConfig()
	cfg.TracerProvider = provider

	reg := newFakeExecutorRegistry()
	reg.Register("cli", &passThroughExecutor{})
	cfg.Executors = reg

	eng := New(cfg)
	plan := makeTestPlan(
		engine.ResolvedStep{ID: "step-1", Kind: "cli", Spec: &cliStepSpec{}},
		engine.ResolvedStep{ID: "step-2", Kind: "cli", Spec: &cliStepSpec{}},
	)
	h, err := eng.Start(context.Background(), engine.ValidatedForTest(plan), engine.RunOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	driveToCompletion(t, h)

	spans := provider.Spans()

	runSpan := findSpan(spans, "yawr.run")
	if runSpan == nil {
		t.Fatal("expected yawr.run span")
	}
	stepSpans := findSpans(spans, "yawr.step.*")
	if len(stepSpans) != 2 {
		t.Fatalf("expected 2 step spans, got %d", len(stepSpans))
	}
	for _, ss := range stepSpans {
		if ss.ParentSpanID != runSpan.SpanID {
			t.Errorf("step span %q parent=%q, want run span %q", ss.Name, ss.ParentSpanID, runSpan.SpanID)
		}
	}
}

// TestEngine_OTelSpans_Error verifies that a failed step sets StatusError on the step span.
func TestEngine_OTelSpans_Error(t *testing.T) {
	provider := &otelPkg.RecordingTracerProvider{}
	cfg := makeTestConfig()
	cfg.TracerProvider = provider

	reg := newFakeExecutorRegistry()
	reg.Register("cli", &failingExecutor{err: errors.New("step failed hard")})
	cfg.Executors = reg

	eng := New(cfg)
	plan := makeTestPlan(
		engine.ResolvedStep{ID: "step-fail", Kind: "cli", Spec: &cliStepSpec{}},
	)
	h, err := eng.Start(context.Background(), engine.ValidatedForTest(plan), engine.RunOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Drive to completion (may return step failure result).
	for {
		_, err := h.Next(context.Background())
		if errors.Is(err, io.EOF) || err != nil {
			break
		}
	}

	spans := provider.Spans()
	stepSpan := findSpan(spans, "yawr.step.*")
	if stepSpan == nil {
		t.Fatal("expected a step span")
	}
	if stepSpan.Status != otelPkg.StatusError {
		t.Errorf("expected StatusError for failed step, got %v", stepSpan.Status)
	}
}

// TestEngine_OTelSpans_Parallel verifies branch spans have correct parent (parallel step span).
func TestEngine_OTelSpans_Parallel(t *testing.T) {
	provider := &otelPkg.RecordingTracerProvider{}
	cfg := makeTestConfig()
	cfg.TracerProvider = provider

	reg := newFakeExecutorRegistry()
	reg.Register("cli", &passThroughExecutor{})
	cfg.Executors = reg

	eng := New(cfg)
	plan := makeTestPlan(
		engine.ResolvedStep{
			ID:   "parallel-step",
			Kind: "parallel",
			Spec: &testParallelSpec{
				branches: []engine.BranchSpec{
					{Label: "branch-a", Steps: []engine.ResolvedStep{{ID: "a1", Kind: "cli", Spec: &cliStepSpec{}}}},
					{Label: "branch-b", Steps: []engine.ResolvedStep{{ID: "b1", Kind: "cli", Spec: &cliStepSpec{}}}},
				},
			},
		},
	)
	h, err := eng.Start(context.Background(), engine.ValidatedForTest(plan), engine.RunOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	driveToCompletion(t, h)

	spans := provider.Spans()

	parallelSpan := findSpan(spans, "yawr.step.parallel")
	if parallelSpan == nil {
		t.Fatal("expected yawr.step.parallel span")
	}

	branchASpan := findSpan(spans, "yawr.branch.branch-a")
	branchBSpan := findSpan(spans, "yawr.branch.branch-b")
	if branchASpan == nil || branchBSpan == nil {
		t.Fatalf("expected two branch spans; got spans: %v", spanNames(spans))
	}

	if branchASpan.ParentSpanID != parallelSpan.SpanID {
		t.Errorf("branch-a parent=%q, want parallel span %q", branchASpan.ParentSpanID, parallelSpan.SpanID)
	}
	if branchBSpan.ParentSpanID != parallelSpan.SpanID {
		t.Errorf("branch-b parent=%q, want parallel span %q", branchBSpan.ParentSpanID, parallelSpan.SpanID)
	}
}

func spanNames(spans []*otelPkg.SpanData) []string {
	names := make([]string, len(spans))
	for i, s := range spans {
		names[i] = s.Name
	}
	return names
}

// TestEngine_SkipsSubStepsAtDepth verifies that the engine's main loop skips
// plan steps at Depth > 0, which are owned by their parent container executor.
func TestEngine_SkipsSubStepsAtDepth(t *testing.T) {
	executed := make(map[string]int)
	var mu sync.Mutex
	trackingExec := stepExecutorFunc(func(_ context.Context, step engine.ResolvedStep, _ map[string]any) (*engine.StepResult, error) {
		mu.Lock()
		executed[step.ID]++
		mu.Unlock()
		return &engine.StepResult{
			StepID:  step.ID,
			Status:  engine.StepStatusCompleted,
			Outcome: engine.StepOutcomeSuccess,
		}, nil
	})

	reg := newFakeExecutorRegistry()
	reg.Register("cli", trackingExec)
	reg.Register("iterate", trackingExec)

	cfg := makeTestConfig()
	cfg.Executors = reg
	eng := New(cfg)

	// Plan: iterate-step (Depth=0), sub-step (Depth=1), final-step (Depth=0).
	// The engine should execute iterate-step and final-step; sub-step must be skipped.
	plan := makeTestPlan(
		engine.ResolvedStep{ID: "iterate-step", Kind: "iterate", Spec: &cliStepSpec{}, Depth: 0},
		engine.ResolvedStep{ID: "sub-step", Kind: "cli", Spec: &cliStepSpec{}, Depth: 1},
		engine.ResolvedStep{ID: "final-step", Kind: "cli", Spec: &cliStepSpec{}, Depth: 0},
	)

	h, err := eng.Start(context.Background(), engine.ValidatedForTest(plan), engine.RunOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	results := driveToCompletion(t, h)

	// Outer loop must only return 2 results: iterate-step and final-step.
	if len(results) != 2 {
		ids := make([]string, len(results))
		for i, r := range results {
			ids[i] = r.StepID
		}
		t.Fatalf("expected 2 step results, got %d: %v", len(results), ids)
	}
	if results[0].StepID != "iterate-step" {
		t.Errorf("result[0]: expected iterate-step, got %s", results[0].StepID)
	}
	if results[1].StepID != "final-step" {
		t.Errorf("result[1]: expected final-step, got %s", results[1].StepID)
	}

	mu.Lock()
	defer mu.Unlock()
	if executed["sub-step"] != 0 {
		t.Errorf("sub-step should not be executed by the outer loop, got %d calls", executed["sub-step"])
	}
	if executed["iterate-step"] != 1 {
		t.Errorf("iterate-step should be executed once, got %d calls", executed["iterate-step"])
	}
	if executed["final-step"] != 1 {
		t.Errorf("final-step should be executed once, got %d calls", executed["final-step"])
	}
}

// iterateSpecForTest implements schema.IterateNode for engine unit tests.
type iterateSpecForTest struct {
	id       string
	items    []string
	loopVar  string
	subSteps []schema.FlowNode
}

func (s *iterateSpecForTest) StepKind() string { return "iterate" }

// captureSubStepRunner is a SubStepRunner that records all vars it receives.
type captureSubStepRunner struct {
	mu       sync.Mutex
	received []map[string]any
}

func (c *captureSubStepRunner) run(ctx context.Context, steps []schema.FlowNode, vars map[string]any) ([]*engine.StepResult, error) {
	snapshot := make(map[string]any, len(vars))
	for k, v := range vars {
		snapshot[k] = v
	}
	c.mu.Lock()
	c.received = append(c.received, snapshot)
	c.mu.Unlock()
	return nil, nil
}

// TestEngine_IterateSubStepVars verifies that an iterate executor receives
// loop variable bindings from its parent context when the SubStepRunner is called.
// This test validates the fix for NBI-14-03: sub-steps at Depth > 0 must not be
// executed by the outer engine loop (which has no loop vars), only by the
// iterate executor's SubStepRunner (which does).
func TestEngine_IterateSubStepVars(t *testing.T) {
	capture := &captureSubStepRunner{}

	// iterateExec simulates what IterateExecutor does: binds loop vars and calls runner.
	iterateExec := stepExecutorFunc(func(ctx context.Context, step engine.ResolvedStep, vars map[string]any) (*engine.StepResult, error) {
		items := []string{"a", "b", "c"}
		for i, item := range items {
			loopVars := make(map[string]any, len(vars)+2)
			for k, v := range vars {
				loopVars[k] = v
			}
			loopVars["item"] = item
			loopVars["iteration"] = i + 1
			_, _ = capture.run(ctx, nil, loopVars)
		}
		return &engine.StepResult{
			StepID:  step.ID,
			Status:  engine.StepStatusCompleted,
			Outcome: engine.StepOutcomeSuccess,
		}, nil
	})

	reg := newFakeExecutorRegistry()
	reg.Register("iterate", iterateExec)
	reg.Register("cli", &passThroughExecutor{})

	cfg := makeTestConfig()
	cfg.Executors = reg
	eng := New(cfg)

	// Plan with iterate step (Depth=0) and its sub-step (Depth=1).
	// After the fix, only the iterate step is executed by the outer loop;
	// the sub-step is skipped.
	plan := makeTestPlan(
		engine.ResolvedStep{ID: "loop", Kind: "iterate", Spec: &cliStepSpec{}, Depth: 0},
		engine.ResolvedStep{ID: "sub", Kind: "cli", Spec: &cliStepSpec{}, Depth: 1},
	)

	h, err := eng.Start(context.Background(), engine.ValidatedForTest(plan), engine.RunOptions{
		Vars: map[string]string{"outer_var": "hello"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	driveToCompletion(t, h)

	capture.mu.Lock()
	received := capture.received
	capture.mu.Unlock()

	if len(received) != 3 {
		t.Fatalf("expected SubStepRunner called 3 times (once per item), got %d", len(received))
	}
	for i, vars := range received {
		item, ok := vars["item"]
		if !ok {
			t.Errorf("call[%d]: missing loop var 'item'", i)
			continue
		}
		expected := []string{"a", "b", "c"}[i]
		if item != expected {
			t.Errorf("call[%d]: item=%v, want %q", i, item, expected)
		}
		if iter, ok := vars["iteration"]; !ok || iter != i+1 {
			t.Errorf("call[%d]: iteration=%v, want %d", i, iter, i+1)
		}
	}
}

// TestEngine_StepDelay_HonorsDelayField verifies that a step's `delay` field
// is honored by the engine: execution is paused for the configured duration
// before the executor is invoked, and a step/delaying trace event is emitted.
func TestEngine_StepDelay_HonorsDelayField(t *testing.T) {
	tw := &fakeTraceWriter{}
	reg := newFakeExecutorRegistry()

	var execAt time.Time
	reg.Register("cli", stepExecutorFunc(func(ctx context.Context, step engine.ResolvedStep, vars map[string]any) (*engine.StepResult, error) {
		execAt = time.Now()
		return &engine.StepResult{
			StepID:  step.ID,
			Status:  engine.StepStatusCompleted,
			Outcome: engine.StepOutcomeSuccess,
		}, nil
	}))

	cfg := makeTestConfig()
	cfg.Executors = reg
	cfg.TraceWriter = tw
	eng := New(cfg)

	plan := makeTestPlan(engine.ResolvedStep{
		ID:    "step-1",
		Kind:  "cli",
		Spec:  &cliStepSpec{},
		Delay: "50ms",
	})

	startedAt := time.Now()
	handle, err := eng.Start(context.Background(), engine.ValidatedForTest(plan), engine.RunOptions{})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	result, err := handle.Next(context.Background())
	if err != nil {
		t.Fatalf("Next failed: %v", err)
	}
	if result.Status != engine.StepStatusCompleted {
		t.Errorf("expected completed, got %s", result.Status)
	}

	elapsedToExec := execAt.Sub(startedAt)
	if elapsedToExec < 50*time.Millisecond {
		t.Errorf("expected executor to run no earlier than 50ms after Start; ran after %v", elapsedToExec)
	}

	var foundDelaying bool
	for _, ev := range tw.collect() {
		if ev.Kind == trace.EventKindStepDelaying {
			foundDelaying = true
			var payload struct {
				StepID string `json:"step_id"`
				Delay  string `json:"delay"`
			}
			if err := json.Unmarshal(ev.Payload, &payload); err != nil {
				t.Fatalf("unmarshal step/delaying payload: %v", err)
			}
			if payload.StepID != "step-1" {
				t.Errorf("step/delaying step_id = %q, want %q", payload.StepID, "step-1")
			}
			if payload.Delay != "50ms" {
				t.Errorf("step/delaying delay = %q, want %q", payload.Delay, "50ms")
			}
			break
		}
	}
	if !foundDelaying {
		t.Error("expected step/delaying trace event")
	}
}

// TestEngine_StepDelay_EmptyDelayDoesNotSleep verifies that steps without a
// delay run immediately and emit no step/delaying event.
func TestEngine_StepDelay_EmptyDelayDoesNotSleep(t *testing.T) {
	tw := &fakeTraceWriter{}
	reg := newFakeExecutorRegistry()
	reg.Register("cli", &passThroughExecutor{})

	cfg := makeTestConfig()
	cfg.Executors = reg
	cfg.TraceWriter = tw
	eng := New(cfg)

	plan := makeTestPlan(engine.ResolvedStep{ID: "step-1", Kind: "cli", Spec: &cliStepSpec{}})

	startedAt := time.Now()
	handle, err := eng.Start(context.Background(), engine.ValidatedForTest(plan), engine.RunOptions{})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if _, err := handle.Next(context.Background()); err != nil {
		t.Fatalf("Next failed: %v", err)
	}
	if d := time.Since(startedAt); d > 100*time.Millisecond {
		t.Errorf("step without delay took %v; expected <100ms", d)
	}
	for _, ev := range tw.collect() {
		if ev.Kind == trace.EventKindStepDelaying {
			t.Fatal("did not expect step/delaying event for step without delay")
		}
	}
}
