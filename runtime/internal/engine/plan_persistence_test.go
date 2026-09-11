package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	internalEventBus "github.com/ormasoftchile/yawr/runtime/internal/eventbus"
	internalexecutor "github.com/ormasoftchile/yawr/runtime/internal/executor"
	internalexpr "github.com/ormasoftchile/yawr/runtime/internal/expr"
	"github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	"github.com/ormasoftchile/yawr/runtime/internal/runstore"
	internaltrace "github.com/ormasoftchile/yawr/runtime/internal/trace"
	enginepkg "github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/eventbus"
	"github.com/ormasoftchile/yawr/runtime/pkg/governance"
	"github.com/ormasoftchile/yawr/runtime/pkg/input"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	tracepkg "github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

var errPlanPersistence = errors.New("plan persistence failed")

type noopRunLease struct{}

func (noopRunLease) Epoch() uint64  { return 1 }
func (noopRunLease) Release() error { return nil }

func acquireNoopRunLease(context.Context, string) (enginepkg.RunLease, error) {
	return noopRunLease{}, nil
}
func TestEngineResumeRecoversCheckpointTraceProjection(t *testing.T) {
	base := t.TempDir()
	const runID = "run-recover-checkpoint-trace"
	plan := &enginepkg.ExecutionPlan{
		RunID: runID, RunbookPath: "trace-recovery.runbook.yaml",
		Steps:    []enginepkg.ResolvedStep{{ID: "work", Kind: "noop", Spec: &schema.NoopSpec{}}},
		Metadata: enginepkg.PlanMetadata{RunbookID: "trace-recovery"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	firstStore := runstore.NewDirRunStore(base)
	firstConfig := makeTestConfig()
	firstRegistry := newFakeExecutorRegistry()
	firstRegistry.Register("noop", stepExecutorFunc(func(_ context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
		return &enginepkg.StepResult{
			StepID: step.ID, Status: enginepkg.StepStatusCompleted, Outcome: enginepkg.StepOutcomeSuccess,
			Output: map[string]any{"answer": "saved"}, Vars: map[string]any{"captured": "value"},
		}, nil
	}))
	firstConfig.Executors = firstRegistry
	firstConfig.TraceWriter = &kindFailingTraceWriter{kind: tracepkg.EventKind("execution/committed")}
	firstConfig.Store = firstStore
	handle, err := New(firstConfig).Start(context.Background(), plan, enginepkg.RunOptions{Store: firstStore})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	internalHandle := handle.(*runHandle)
	internalHandle.mu.Lock()
	_, commitErr := internalHandle.persistCompletedCheckpoint(context.Background())
	internalHandle.mu.Unlock()
	if !errors.Is(commitErr, enginepkg.ErrTraceCommit) {
		t.Fatalf("checkpoint projection error = %v, want ErrTraceCommit", commitErr)
	}
	checkpoint, err := firstStore.LoadState(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if checkpoint.CommittedTraceSequence < 1 || len(checkpoint.PendingTraceEvents) != 1 {
		t.Fatalf("checkpoint trace recovery state = sequence %d pending %#v", checkpoint.CommittedTraceSequence, checkpoint.PendingTraceEvents)
	}
	committedEvent := checkpoint.PendingTraceEvents[0]
	if committedEvent.Kind != "execution/committed" || committedEvent.Sequence != checkpoint.CommittedTraceSequence {
		t.Fatalf("committed checkpoint event = %#v", committedEvent)
	}
	if err := firstStore.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}

	resumeStore := runstore.NewDirRunStore(base)
	t.Cleanup(func() { _ = resumeStore.Close() })
	recoveryWriter := &fakeTraceWriter{}
	resumeConfig := makeTestConfig()
	resumeRegistry := newFakeExecutorRegistry()
	resumeRegistry.Register("noop", &passThroughExecutor{})
	resumeConfig.Executors = resumeRegistry
	resumeConfig.TraceWriter = recoveryWriter
	resumeConfig.Store = resumeStore
	resumed, err := New(resumeConfig).Resume(context.Background(), runID, enginepkg.RunOptions{Store: resumeStore})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumed.State().CheckpointSequence != checkpoint.CheckpointSequence {
		t.Fatal("resume changed checkpoint while recovering trace projection")
	}
	found := false
	for _, event := range recoveryWriter.collect() {
		if event.EventID == committedEvent.EventID && event.Sequence == committedEvent.Sequence {
			found = true
		}
	}
	if !found {
		t.Fatalf("resume did not recover committed trace event: %#v", recoveryWriter.collect())
	}
}

func TestEngineResumeRecoversExactStepLifecycleOnceAcrossRepeatedResume(t *testing.T) {
	base := t.TempDir()
	const runID = "run-recover-step-lifecycle"
	plan := &enginepkg.ExecutionPlan{
		RunID: runID, RunbookPath: "step-lifecycle.runbook.yaml",
		Steps:    []enginepkg.ResolvedStep{{ID: "work", Kind: "noop", Spec: &schema.NoopSpec{}}},
		Metadata: enginepkg.PlanMetadata{RunbookID: "step-lifecycle"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	firstStore := runstore.NewDirRunStore(base)
	firstConfig := makeTestConfig()
	firstRegistry := newFakeExecutorRegistry()
	firstRegistry.Register("noop", stepExecutorFunc(func(_ context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
		return &enginepkg.StepResult{
			StepID: step.ID, Status: enginepkg.StepStatusCompleted, Outcome: enginepkg.StepOutcomeSuccess,
			Output: map[string]any{"answer": "saved"}, Vars: map[string]any{"captured": "value"},
		}, nil
	}))
	firstConfig.Executors = firstRegistry
	firstConfig.TraceWriter = &kindFailingTraceWriter{kind: tracepkg.EventKindStepCompleted}
	firstConfig.Store = firstStore
	handle, err := New(firstConfig).Start(context.Background(), plan, enginepkg.RunOptions{Store: firstStore})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := handle.Next(context.Background()); !errors.Is(err, enginepkg.ErrTraceCommit) {
		t.Fatalf("Next error = %v, want ErrTraceCommit", err)
	}
	checkpoint, err := firstStore.LoadState(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	var committedStepEvent enginepkg.Event
	for _, event := range checkpoint.PendingTraceEvents {
		if event.Kind == string(tracepkg.EventKindStepCompleted) {
			committedStepEvent = event
		}
	}
	if committedStepEvent.EventID == "" || committedStepEvent.Payload["step_id"] != "work" {
		t.Fatalf("checkpoint omitted exact step completion: %#v", checkpoint.PendingTraceEvents)
	}
	if err := firstStore.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}

	projectionPath := filepath.Join(base, "projected-trace.jsonl")
	resumeOnce := func() {
		projectionWriter, err := internaltrace.NewJSONLWriter(projectionPath)
		if err != nil {
			t.Fatalf("NewJSONLWriter: %v", err)
		}
		resumeStore := runstore.NewDirRunStore(base)
		config := makeTestConfig()
		registry := newFakeExecutorRegistry()
		registry.Register("noop", &passThroughExecutor{})
		config.Executors = registry
		config.TraceWriter = projectionWriter
		config.Store = resumeStore
		if _, err := New(config).Resume(context.Background(), runID, enginepkg.RunOptions{
			Store: resumeStore, AcknowledgeIndeterminate: true,
		}); err != nil {
			t.Fatalf("Resume: %v", err)
		}
		if err := resumeStore.Close(); err != nil {
			t.Fatalf("close resume store: %v", err)
		}
		if err := projectionWriter.Close(); err != nil {
			t.Fatalf("close projection writer: %v", err)
		}
	}
	resumeOnce()
	resumeOnce()

	assertTraceEventCount := func(path string, want int) {
		t.Helper()
		events, err := internaltrace.NewJSONLReader(path).ReadAll(context.Background())
		if err != nil {
			t.Fatalf("read trace %s: %v", path, err)
		}
		count := 0
		for _, event := range events {
			if event.EventID == committedStepEvent.EventID {
				count++
				var payload map[string]any
				if err := json.Unmarshal(event.Payload, &payload); err != nil {
					t.Fatalf("decode recovered event: %v", err)
				}
				output, _ := payload["output"].(map[string]any)
				captures, _ := payload["captures"].(map[string]any)
				if output["answer"] != "saved" || captures["captured"] != "value" {
					t.Fatalf("recovered lifecycle payload = %#v", payload)
				}
			}
		}
		if count != want {
			t.Fatalf("event %s appears %d times in %s, want %d", committedStepEvent.EventID, count, path, want)
		}
	}
	assertTraceEventCount(projectionPath, 1)
	assertTraceEventCount(filepath.Join(base, runID, "trace.jsonl"), 1)
}

func TestEngineResumeDefersInitialCallbackUntilHandleIsObservable(t *testing.T) {
	base := t.TempDir()
	const runID = "run-resume-callback"
	plan := &enginepkg.ExecutionPlan{
		RunID: runID, RunbookPath: "callback.runbook.yaml",
		Steps: []enginepkg.ResolvedStep{
			{ID: "first", Kind: "noop", Spec: &schema.NoopSpec{}},
			{ID: "second", Kind: "noop", Spec: &schema.NoopSpec{}},
		},
		Metadata: enginepkg.PlanMetadata{RunbookID: "resume-callback"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	newEngine := func(store enginepkg.DurableRunStore) enginepkg.Engine {
		config := makeTestConfig()
		registry := newFakeExecutorRegistry()
		registry.Register("noop", &passThroughExecutor{})
		config.Executors = registry
		config.Store = store
		return New(config)
	}
	firstStore := runstore.NewDirRunStore(base)
	firstHandle, err := newEngine(firstStore).Start(context.Background(), plan, enginepkg.RunOptions{Store: firstStore})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := firstHandle.Next(context.Background()); err != nil {
		t.Fatalf("first Next: %v", err)
	}
	if err := firstStore.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}

	resumeStore := runstore.NewDirRunStore(base)
	t.Cleanup(func() { _ = resumeStore.Close() })
	callbackSawNil := make(chan bool, 1)
	var resumed enginepkg.RunHandle
	resumed, err = newEngine(resumeStore).Resume(context.Background(), runID, enginepkg.RunOptions{
		Store: resumeStore,
		OnEvent: func(event enginepkg.Event) {
			if event.Kind != string(tracepkg.EventKindStepResumed) {
				return
			}
			callbackSawNil <- resumed == nil
			if resumed != nil {
				_ = resumed.State()
			}
		},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	select {
	case sawNil := <-callbackSawNil:
		if sawNil {
			t.Fatal("resume callback ran before the returned handle was assigned")
		}
		t.Fatal("resume callback ran before the first handle operation")
	default:
	}
	if result, err := resumed.Next(context.Background()); err != nil || result.StepID != "second" {
		t.Fatalf("resumed Next = %#v, %v", result, err)
	}
	select {
	case sawNil := <-callbackSawNil:
		if sawNil {
			t.Fatal("deferred resume callback observed a nil handle")
		}
	case <-time.After(time.Second):
		t.Fatal("deferred resume callback was not delivered")
	}
}

type failingPlanStore struct{}

func (*failingPlanStore) SaveState(context.Context, enginepkg.RunState) error { return nil }
func (*failingPlanStore) LoadState(context.Context, string) (enginepkg.RunState, error) {
	return enginepkg.RunState{}, errors.New("not implemented")
}
func (*failingPlanStore) WriteTrace(context.Context, string, enginepkg.Event) error { return nil }
func (*failingPlanStore) Close() error                                              { return nil }
func (*failingPlanStore) SavePlan(context.Context, string, *enginepkg.ExecutionPlan) error {
	return errPlanPersistence
}
func (*failingPlanStore) LoadPlan(context.Context, string) (*enginepkg.ExecutionPlan, error) {
	return nil, errors.New("not implemented")
}
func (*failingPlanStore) PlanDigest(string) (string, bool) { return "", false }
func (*failingPlanStore) AcquireRunLease(ctx context.Context, runID string) (enginepkg.RunLease, error) {
	return acquireNoopRunLease(ctx, runID)
}

var errCheckpointPersistence = errors.New("checkpoint persistence failed")
var errTracePersistence = errors.New("trace persistence failed")

type failingTraceWriter struct{}

func (*failingTraceWriter) Append(tracepkg.TraceEvent) error { return errTracePersistence }
func (*failingTraceWriter) Close() error                     { return nil }

type kindFailingTraceWriter struct {
	kind tracepkg.EventKind
}

func (writer *kindFailingTraceWriter) Append(event tracepkg.TraceEvent) error {
	if event.Kind == writer.kind {
		return errTracePersistence
	}
	return nil
}
func (*kindFailingTraceWriter) Close() error { return nil }

type failingCheckpointStore struct {
	plan *enginepkg.ExecutionPlan
}

func (*failingCheckpointStore) SaveState(context.Context, enginepkg.RunState) error {
	return errCheckpointPersistence
}
func (*failingCheckpointStore) LoadState(context.Context, string) (enginepkg.RunState, error) {
	return enginepkg.RunState{}, errors.New("not implemented")
}
func (*failingCheckpointStore) WriteTrace(context.Context, string, enginepkg.Event) error { return nil }
func (*failingCheckpointStore) Close() error                                              { return nil }
func (store *failingCheckpointStore) SavePlan(_ context.Context, _ string, plan *enginepkg.ExecutionPlan) error {
	store.plan = plan
	return nil
}
func (store *failingCheckpointStore) LoadPlan(context.Context, string) (*enginepkg.ExecutionPlan, error) {
	return store.plan, nil
}
func (*failingCheckpointStore) PlanDigest(string) (string, bool) { return "", false }
func (*failingCheckpointStore) AcquireRunLease(ctx context.Context, runID string) (enginepkg.RunLease, error) {
	return acquireNoopRunLease(ctx, runID)
}

type capturingCheckpointStore struct {
	states []enginepkg.RunState
	plan   *enginepkg.ExecutionPlan
}

type kindFailingCheckpointTraceStore struct {
	*capturingCheckpointStore
	kind string
}

func (store *kindFailingCheckpointTraceStore) WriteTrace(_ context.Context, _ string, event enginepkg.Event) error {
	if event.Kind == store.kind {
		return errTracePersistence
	}
	return nil
}

type failOnceCheckpointStore struct {
	*runstore.DirRunStore
	failed bool
}

type checkpointErrorPromptProvider struct{}

func (checkpointErrorPromptProvider) PromptChoice(context.Context, input.ChoiceRequest) (*input.ChoiceResponse, error) {
	return nil, fmt.Errorf("%w: prepare choice", enginepkg.ErrCheckpointCommit)
}

func (checkpointErrorPromptProvider) PromptDecision(context.Context, input.DecisionRequest) (*input.DecisionResponse, error) {
	return nil, fmt.Errorf("%w: prepare decision", enginepkg.ErrCheckpointCommit)
}

func (checkpointErrorPromptProvider) PromptForm(context.Context, input.FormRequest) (*input.FormResponse, error) {
	return nil, fmt.Errorf("%w: prepare form", enginepkg.ErrCheckpointCommit)
}

type checkpointErrorApprovalGate struct{}

func (checkpointErrorApprovalGate) RequestApproval(context.Context, string, string) (governance.ApprovalRecord, error) {
	return governance.ApprovalRecord{}, fmt.Errorf("%w: prepare approval", enginepkg.ErrCheckpointCommit)
}

type gatedParallelDispatchExecutor struct {
	mu          sync.Mutex
	blockedStep string
	started     chan struct{}
	release     chan struct{}
	executed    []string
}

type barrierParallelExecutor struct {
	started chan string
	release chan struct{}
}

type cancellationBlockedDispatchExecutor struct {
	prepared chan struct{}
	release  chan struct{}
}

type siblingCancelledDispatchExecutor struct {
	prepared chan struct{}
}

type compensationTakeoverExecutor struct {
	mu           sync.Mutex
	blockUndoTwo bool
	undoStarted  chan struct{}
	releaseUndo  chan struct{}
	executed     []string
}

type notFoundThenResolvedDynamicResolver struct {
	calls  int
	result *internalexecutor.DynamicIncludeResult
}

func (resolver *notFoundThenResolvedDynamicResolver) Resolve(
	context.Context,
	string,
) (*internalexecutor.DynamicIncludeResult, error) {
	resolver.calls++
	if resolver.calls == 1 {
		return nil, errkit.New("DINC-002", "not found")
	}
	return resolver.result, nil
}

type boundaryRecordingApprovalGate struct {
	boundary enginepkg.DispatchExecutionBoundary
	found    bool
}

type inspectingWaitDispatcher struct {
	wait func(context.Context, string, eventbus.EventFilter, time.Duration) (*eventbus.InboundEvent, error)
}

type fileBackedLazyLoader struct{}

func (fileBackedLazyLoader) Load(_ context.Context, path string) (*internalexecutor.LoadedRunbook, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return &internalexecutor.LoadedRunbook{Flow: []schema.FlowNode{{Step: &schema.Step{
		ID: strings.TrimSpace(string(contents)), Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{},
	}}}}, nil
}

func (fileBackedLazyLoader) LoadSnapshot(_ context.Context, _ string, contents []byte) (*internalexecutor.LoadedRunbook, error) {
	return &internalexecutor.LoadedRunbook{Flow: []schema.FlowNode{{Step: &schema.Step{
		ID: strings.TrimSpace(string(contents)), Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{},
	}}}}, nil
}

func (dispatcher *inspectingWaitDispatcher) Dispatch(eventbus.InboundEvent) error { return nil }
func (dispatcher *inspectingWaitDispatcher) Wait(
	ctx context.Context,
	stepID string,
	filter eventbus.EventFilter,
	timeout time.Duration,
) (*eventbus.InboundEvent, error) {
	return dispatcher.wait(ctx, stepID, filter, timeout)
}
func (*inspectingWaitDispatcher) Cancel(string, string) {}

func (gate *boundaryRecordingApprovalGate) RequestApproval(
	ctx context.Context,
	_ string,
	_ string,
) (governance.ApprovalRecord, error) {
	gate.boundary, gate.found = enginepkg.DispatchExecutionBoundaryFromContext(ctx)
	return governance.ApprovalRecord{Approver: "test", Token: "approved"}, nil
}

func (executor *compensationTakeoverExecutor) Execute(ctx context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
	if step.ID == "undo-2" && executor.blockUndoTwo {
		close(executor.undoStarted)
		<-executor.releaseUndo
	}
	if _, err := enginepkg.PrepareExternalDispatch(ctx, enginepkg.DispatchRequest{
		Classification: "mutating", EndpointIdentity: "compensation-provider",
		RenderedRequest: map[string]any{"step": step.ID},
	}); err != nil {
		return nil, err
	}
	executor.mu.Lock()
	executor.executed = append(executor.executed, step.ID)
	executor.mu.Unlock()
	status := enginepkg.StepStatusCompleted
	var resultErr error
	if step.ID == "fail" {
		status = enginepkg.StepStatusFailed
		resultErr = errors.New("known external failure")
	}
	return &enginepkg.StepResult{
		StepID: step.ID, Status: status, Outcome: enginepkg.StepOutcomeSuccess,
		Error: resultErr, Vars: map[string]any{},
	}, nil
}

func (executor *compensationTakeoverExecutor) steps() []string {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return append([]string(nil), executor.executed...)
}

func (executor *cancellationBlockedDispatchExecutor) Execute(ctx context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
	if _, err := enginepkg.PrepareExternalDispatch(ctx, enginepkg.DispatchRequest{
		Classification: "mutating", EndpointIdentity: "cancel-provider",
		RenderedRequest: map[string]any{"step": step.ID},
	}); err != nil {
		return nil, err
	}
	close(executor.prepared)
	<-ctx.Done()
	<-executor.release
	return nil, ctx.Err()
}

func (executor *siblingCancelledDispatchExecutor) Execute(ctx context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
	if step.ID == "fail" {
		<-executor.prepared
		return nil, errors.New("sibling failed")
	}
	if _, err := enginepkg.PrepareExternalDispatch(ctx, enginepkg.DispatchRequest{
		Classification: "mutating", EndpointIdentity: "parallel-cancel-provider",
		RenderedRequest: map[string]any{"step": step.ID},
	}); err != nil {
		return nil, err
	}
	close(executor.prepared)
	<-ctx.Done()
	return nil, ctx.Err()
}

func (executor *barrierParallelExecutor) Execute(_ context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
	executor.started <- step.ID
	<-executor.release
	return &enginepkg.StepResult{
		StepID: step.ID, Status: enginepkg.StepStatusCompleted, Outcome: enginepkg.StepOutcomeSuccess,
		Vars: map[string]any{},
	}, nil
}

func (executor *gatedParallelDispatchExecutor) Execute(ctx context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
	if step.ID == executor.blockedStep {
		close(executor.started)
		<-executor.release
	}
	if _, err := enginepkg.PrepareExternalDispatch(ctx, enginepkg.DispatchRequest{
		Classification: "unspecified", EndpointIdentity: "parallel-provider",
		RenderedRequest: map[string]any{"step": step.ID},
	}); err != nil {
		return nil, err
	}
	executor.mu.Lock()
	executor.executed = append(executor.executed, step.ID)
	executor.mu.Unlock()
	return &enginepkg.StepResult{
		StepID: step.ID, Status: enginepkg.StepStatusCompleted, Outcome: enginepkg.StepOutcomeSuccess,
		Output: map[string]any{"step": step.ID}, Vars: map[string]any{},
	}, nil
}

func (executor *gatedParallelDispatchExecutor) executedSteps() []string {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return append([]string(nil), executor.executed...)
}

func (store *failOnceCheckpointStore) SaveState(ctx context.Context, state enginepkg.RunState) error {
	if !store.failed {
		store.failed = true
		return errCheckpointPersistence
	}
	return store.DirRunStore.SaveState(ctx, state)
}

func (store *capturingCheckpointStore) SaveState(_ context.Context, state enginepkg.RunState) error {
	store.states = append(store.states, state)
	return nil
}
func (*capturingCheckpointStore) LoadState(context.Context, string) (enginepkg.RunState, error) {
	return enginepkg.RunState{}, errors.New("not implemented")
}
func (*capturingCheckpointStore) WriteTrace(context.Context, string, enginepkg.Event) error {
	return nil
}
func (*capturingCheckpointStore) Close() error { return nil }
func (store *capturingCheckpointStore) SavePlan(_ context.Context, _ string, plan *enginepkg.ExecutionPlan) error {
	store.plan = plan
	return nil
}
func (store *capturingCheckpointStore) LoadPlan(context.Context, string) (*enginepkg.ExecutionPlan, error) {
	return store.plan, nil
}
func (*capturingCheckpointStore) PlanDigest(string) (string, bool) { return "", false }
func (*capturingCheckpointStore) AcquireRunLease(ctx context.Context, runID string) (enginepkg.RunLease, error) {
	return acquireNoopRunLease(ctx, runID)
}

func TestEngineStartPersistsPlanForFreshStore(t *testing.T) {
	base := t.TempDir()
	store := runstore.NewDirRunStore(base)
	eng := newPersistentPlanTestEngine()
	plan := persistentPlanForTest(t, "run-start-plan")

	handle, err := eng.Start(context.Background(), plan, enginepkg.RunOptions{Mode: enginepkg.RunModeReal, Store: store})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		_ = handle.Cancel(context.Background(), "test cleanup")
		_ = store.Close()
	})

	fresh := runstore.NewDirRunStore(base)
	restored, err := fresh.LoadPlan(context.Background(), plan.RunID)
	if err != nil {
		t.Fatalf("fresh store LoadPlan: %v", err)
	}
	if restored.Metadata.PlanHash != plan.Metadata.PlanHash {
		t.Fatalf("restored plan hash = %q, want %q", restored.Metadata.PlanHash, plan.Metadata.PlanHash)
	}
}

func TestEngineResumeLoadsPlanFromFreshStore(t *testing.T) {
	base := t.TempDir()
	const runID = "run-resume-plan"
	plan := persistentPlanForTest(t, runID)
	first := runstore.NewDirRunStore(base)
	if err := first.SavePlan(context.Background(), runID, plan); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	state := enginepkg.RunState{
		RunID: runID, RunbookPath: plan.RunbookPath, Status: enginepkg.RunStatusRunning,
		CurrentStep: "first", CurrentStepIndex: 0, Vars: map[string]any{},
		StartedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := first.SaveState(context.Background(), state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	if err := first.WriteTrace(context.Background(), runID, enginepkg.Event{
		EventID: "event-1", RunID: runID, RunbookID: "persisted", Kind: string(tracepkg.EventKindStepCompleted),
		Sequence: 1, Timestamp: time.Now().UTC().Format(time.RFC3339Nano), Payload: map[string]any{"step_id": "first"},
	}); err != nil {
		t.Fatalf("WriteTrace: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close first store: %v", err)
	}

	fresh := runstore.NewDirRunStore(base)
	handle, err := newPersistentPlanTestEngine().Resume(context.Background(), runID, enginepkg.RunOptions{
		Mode: enginepkg.RunModeReal, Store: fresh,
	})
	if err != nil {
		t.Fatalf("Resume with fresh store: %v", err)
	}
	t.Cleanup(func() {
		_ = handle.Cancel(context.Background(), "test cleanup")
		_ = fresh.Close()
	})
	if handle.State().RunID != runID {
		t.Fatalf("resumed run ID = %q, want %q", handle.State().RunID, runID)
	}
}

func TestEngineResumeRejectsCachedPlanForDigestBoundCheckpoint(t *testing.T) {
	base := t.TempDir()
	const runID = "run-missing-durable-plan"
	plan := persistentPlanForTest(t, runID)
	store := runstore.NewDirRunStore(base)
	store.RegisterPlan(runID, plan)
	state := enginepkg.RunState{
		RunID: runID, RunbookPath: plan.RunbookPath, Status: enginepkg.RunStatusRunning,
		PlanSnapshotDigest: "sha256:missing-plan", CurrentStep: "first", CurrentStepIndex: 0,
	}
	if err := store.SaveState(context.Background(), state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	_, err := newPersistentPlanTestEngine().Resume(context.Background(), runID, enginepkg.RunOptions{
		Mode: enginepkg.RunModeReal, Store: store,
	})
	if err == nil || !strings.Contains(err.Error(), "durable execution plan") {
		t.Fatalf("Resume error = %v, want missing durable execution plan", err)
	}
}

func TestEngineResumeAfterRealCheckpointWithFreshStore(t *testing.T) {
	base := t.TempDir()
	const runID = "run-real-restart"
	firstStore := runstore.NewDirRunStore(base)
	firstHandle, err := newPersistentPlanTestEngine().Start(
		context.Background(), persistentPlanForTest(t, runID),
		enginepkg.RunOptions{Mode: enginepkg.RunModeReal, Store: firstStore},
	)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	firstResult, err := firstHandle.Next(context.Background())
	if err != nil {
		t.Fatalf("first Next: %v", err)
	}
	if firstResult.StepID != "first" {
		t.Fatalf("first result = %q, want first", firstResult.StepID)
	}
	checkpoint, err := firstStore.LoadState(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadState after first step: %v", err)
	}
	if checkpoint.CursorSet == nil || checkpoint.CursorSet.SchemaVersion != enginepkg.ExecutionCursorSchemaV1 || len(checkpoint.CursorSet.Cursors) != 1 {
		t.Fatalf("checkpoint cursor set = %#v", checkpoint.CursorSet)
	}
	if checkpoint.CheckpointSequence != 1 {
		t.Fatalf("checkpoint sequence = %d, want 1", checkpoint.CheckpointSequence)
	}
	cursor := checkpoint.CursorSet.Cursors[0]
	if cursor.StepID != "second" || cursor.StepIndex != 1 || cursor.Phase != enginepkg.ExecutionPhaseBefore ||
		cursor.QualifiedNodeID != "second" || cursor.Invocation != 1 || cursor.RetryAttempt != 1 {
		t.Fatalf("next cursor = %#v, want second/before", cursor)
	}
	if err := firstStore.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}

	freshStore := runstore.NewDirRunStore(base)
	resumed, err := newPersistentPlanTestEngine().Resume(context.Background(), runID, enginepkg.RunOptions{
		Mode: enginepkg.RunModeReal, Store: freshStore,
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	secondResult, err := resumed.Next(context.Background())
	if err != nil {
		t.Fatalf("resumed Next: %v", err)
	}
	if secondResult.StepID != "second" {
		t.Fatalf("resumed result = %q, want second", secondResult.StepID)
	}
	secondCheckpoint, err := freshStore.LoadState(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadState after resume: %v", err)
	}
	if secondCheckpoint.CheckpointSequence != 2 {
		t.Fatalf("resumed checkpoint sequence = %d, want 2", secondCheckpoint.CheckpointSequence)
	}
	if _, err := resumed.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("final Next error = %v, want EOF", err)
	}
	if err := freshStore.Close(); err != nil {
		t.Fatalf("close fresh store: %v", err)
	}
}

func TestEngineResumeRejectsCompetingWriterLease(t *testing.T) {
	base := t.TempDir()
	const runID = "run-competing-resume"
	firstStore := runstore.NewDirRunStore(base)
	t.Cleanup(func() { _ = firstStore.Close() })
	plan := persistentPlanForTest(t, runID)
	if err := firstStore.SavePlan(context.Background(), runID, plan); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	if err := firstStore.SaveState(context.Background(), enginepkg.RunState{
		RunID: runID, RunbookPath: plan.RunbookPath, Status: enginepkg.RunStatusRunning,
		CurrentStep: "first", CurrentStepIndex: 0,
	}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	if err := firstStore.WriteTrace(context.Background(), runID, enginepkg.Event{
		EventID: "event-1", RunID: runID, RunbookID: "persisted", Kind: string(tracepkg.EventKindRunStarted),
		Sequence: 1, Timestamp: time.Now().UTC().Format(time.RFC3339Nano), Payload: map[string]any{},
	}); err != nil {
		t.Fatalf("WriteTrace: %v", err)
	}
	firstHandle, err := newPersistentPlanTestEngine().Resume(context.Background(), runID, enginepkg.RunOptions{Store: firstStore})
	if err != nil {
		t.Fatalf("first Resume: %v", err)
	}
	t.Cleanup(func() { _ = firstHandle.Cancel(context.Background(), "test cleanup") })

	secondStore := runstore.NewDirRunStore(base)
	t.Cleanup(func() { _ = secondStore.Close() })
	if _, err := newPersistentPlanTestEngine().Resume(context.Background(), runID, enginepkg.RunOptions{Store: secondStore}); !errors.Is(err, enginepkg.ErrRunLeaseHeld) {
		t.Fatalf("competing Resume error = %v, want ErrRunLeaseHeld", err)
	}
}

func TestEngineResumeMarksUnmatchedDispatchIndeterminateWithoutRedispatch(t *testing.T) {
	base := t.TempDir()
	const runID = "run-unmatched-dispatch"
	plan := &enginepkg.ExecutionPlan{
		RunID: runID, RunbookPath: "unmatched-dispatch.runbook.yaml",
		Steps: []enginepkg.ResolvedStep{
			{ID: "external", Kind: "cli", Spec: &schema.CLISpec{Command: "external"}},
			{ID: "after", Kind: "noop", Spec: &schema.NoopSpec{}},
		},
		Metadata: enginepkg.PlanMetadata{RunbookID: "unmatched-dispatch"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	firstStore := runstore.NewDirRunStore(base)
	t.Cleanup(func() { _ = firstStore.Close() })
	lease, err := firstStore.AcquireRunLease(context.Background(), runID)
	if err != nil {
		t.Fatalf("AcquireRunLease: %v", err)
	}
	t.Cleanup(func() { _ = lease.Release() })
	if err := firstStore.SavePlan(context.Background(), runID, plan); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	occurrenceID := enginepkg.InteractionPayloadDigest([]byte("unmatched occurrence"))
	state := enginepkg.RunState{
		RunID: runID, RunbookPath: plan.RunbookPath, WriterEpoch: lease.Epoch(),
		Status: enginepkg.RunStatusRunning, CheckpointSequence: 1,
		CurrentStep: "external", CurrentStepIndex: 0, StartedAt: time.Now().UTC(),
		CursorSet: &enginepkg.ExecutionCursorSet{
			SchemaVersion: enginepkg.ExecutionCursorSchemaV1,
			Cursors: []enginepkg.ExecutionCursor{{
				QualifiedNodeID: "external", StepID: "external", StepIndex: 0,
				Phase: enginepkg.ExecutionPhaseExecute, Invocation: 1, RetryAttempt: 1,
			}},
		},
		Dispatches: map[string]*enginepkg.DispatchState{
			occurrenceID: {
				SchemaVersion: enginepkg.DispatchStateSchemaV1, OccurrenceID: occurrenceID,
				WriterEpoch: lease.Epoch(), QualifiedNodeID: "external", StepID: "external",
				Phase: enginepkg.ExecutionPhaseExecute, Invocation: 1, RetryAttempt: 1, OccurrenceSequence: 1,
				Classification: "unspecified", EndpointIdentity: "external-provider",
				RequestDigest:  enginepkg.InteractionPayloadDigest([]byte("rendered request")),
				IdempotencyKey: enginepkg.InteractionPayloadDigest([]byte("idempotency key")),
				Status:         enginepkg.DispatchStatusPrepared, PreparedAt: time.Now().UTC().Format(time.RFC3339Nano),
			},
		},
	}
	if err := firstStore.SaveState(context.Background(), state); err != nil {
		t.Fatalf("SaveState prepared: %v", err)
	}
	traceCtx := enginepkg.WithRunWriterEpoch(context.Background(), lease.Epoch())
	if err := firstStore.WriteTrace(traceCtx, runID, enginepkg.Event{
		EventID: "event-1", RunID: runID, RunbookID: "unmatched-dispatch", Kind: string(tracepkg.EventKindRunStarted),
		Sequence: 1, Timestamp: time.Now().UTC().Format(time.RFC3339Nano), Payload: map[string]any{},
	}); err != nil {
		t.Fatalf("WriteTrace: %v", err)
	}
	if err := lease.Release(); err != nil {
		t.Fatalf("Release lease: %v", err)
	}
	if err := firstStore.Close(); err != nil {
		t.Fatalf("Close first store: %v", err)
	}

	external := &recordingExecutor{}
	after := &recordingExecutor{}
	config := makeTestConfig()
	registry := newFakeExecutorRegistry()
	registry.Register("cli", external)
	registry.Register("noop", after)
	config.Executors = registry
	recoveryStore := runstore.NewDirRunStore(base)
	if _, err := New(config).Resume(context.Background(), runID, enginepkg.RunOptions{
		Store: recoveryStore, AcknowledgeIndeterminate: true,
	}); !errors.Is(err, enginepkg.ErrIndeterminateAcknowledgmentRequired) {
		t.Fatalf("Resume that discovered unmatched dispatch error = %v", err)
	}
	if external.invoked {
		t.Fatal("unmatched external dispatch was invoked during recovery")
	}
	recovered, err := recoveryStore.LoadState(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadState recovered: %v", err)
	}
	if recovered.Status != enginepkg.RunStatusIndeterminate || recovered.CheckpointSequence != 2 ||
		recovered.Dispatches[occurrenceID].Status != enginepkg.DispatchStatusIndeterminate ||
		recovered.StepResults["external"] == nil || recovered.CursorSet.Cursors[0].StepID != "after" {
		t.Fatalf("recovered state = %#v", recovered)
	}
	if err := recoveryStore.Close(); err != nil {
		t.Fatalf("Close recovery store: %v", err)
	}

	acknowledgedStore := runstore.NewDirRunStore(base)
	handle, err := New(config).Resume(context.Background(), runID, enginepkg.RunOptions{
		Store: acknowledgedStore, AcknowledgeIndeterminate: true,
	})
	if err != nil {
		t.Fatalf("Resume acknowledged: %v", err)
	}
	t.Cleanup(func() {
		_ = handle.Cancel(context.Background(), "test cleanup")
		_ = acknowledgedStore.Close()
	})
	result, err := handle.Next(context.Background())
	if err != nil {
		t.Fatalf("Next after acknowledgment: %v", err)
	}
	if result.StepID != "after" || external.invoked || !after.invoked {
		t.Fatalf("resume result=%q external=%v after=%v", result.StepID, external.invoked, after.invoked)
	}
}

func TestRecoverUnmatchedNestedDispatchAdvancesExecutionFrame(t *testing.T) {
	base := t.TempDir()
	const runID = "run-unmatched-nested-dispatch"
	plan := persistentPlanForTest(t, runID)
	store := runstore.NewDirRunStore(base)
	if err := store.SavePlan(context.Background(), runID, plan); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	firstLease, err := store.AcquireRunLease(context.Background(), runID)
	if err != nil {
		t.Fatalf("AcquireRunLease first: %v", err)
	}
	frameID := enginepkg.InteractionPayloadDigest([]byte("nested frame"))
	occurrenceID := enginepkg.InteractionPayloadDigest([]byte("nested occurrence"))
	state := enginepkg.RunState{
		RunID: runID, RunbookPath: plan.RunbookPath, WriterEpoch: firstLease.Epoch(),
		Status: enginepkg.RunStatusRunning, CheckpointSequence: 1, CurrentStep: "first", CurrentStepIndex: 0,
		CursorSet: &enginepkg.ExecutionCursorSet{
			SchemaVersion: enginepkg.ExecutionCursorSchemaV1,
			Cursors: []enginepkg.ExecutionCursor{{
				QualifiedNodeID: "first/child-1", CallPath: []enginepkg.DebugCallFrame{{StepID: "first"}},
				StepID: "child-1", StepIndex: 0, FrameID: frameID,
				Phase: enginepkg.ExecutionPhaseExecute, Invocation: 1, RetryAttempt: 1,
			}},
		},
		ExecutionFrames: map[string]*enginepkg.ExecutionFrameState{
			frameID: {
				SchemaVersion: enginepkg.ExecutionFrameStateSchemaV1, FrameID: frameID,
				WriterEpoch: firstLease.Epoch(), ParentQualifiedNodeID: "first", ParentStepID: "first",
				Kind: "include", CallPath: []enginepkg.DebugCallFrame{{StepID: "first"}},
				Invocation: 1, DefinitionDigest: enginepkg.InteractionPayloadDigest([]byte("children")),
				StepCount: 2, StepIDs: []string{"child-1", "child-2"}, WorkingVars: map[string]any{"saved": true},
				Results: map[string]*enginepkg.StepResult{}, Status: enginepkg.ExecutionFrameStatusActive,
				StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
			},
		},
		Dispatches: map[string]*enginepkg.DispatchState{
			occurrenceID: {
				SchemaVersion: enginepkg.DispatchStateSchemaV1, OccurrenceID: occurrenceID,
				WriterEpoch: firstLease.Epoch(), QualifiedNodeID: "first/child-1",
				CallPath: []enginepkg.DebugCallFrame{{StepID: "first"}}, StepID: "child-1",
				FrameID: frameID, FrameStepIndex: 0, Phase: enginepkg.ExecutionPhaseExecute,
				Invocation: 1, RetryAttempt: 1, OccurrenceSequence: 1,
				Classification: "unspecified", EndpointIdentity: "nested-provider",
				RequestDigest:  enginepkg.InteractionPayloadDigest([]byte("request")),
				IdempotencyKey: enginepkg.InteractionPayloadDigest([]byte("key")),
				Status:         enginepkg.DispatchStatusPrepared, PreparedAt: time.Now().UTC().Format(time.RFC3339Nano),
			},
		},
	}
	if err := store.SaveState(context.Background(), state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	if err := firstLease.Release(); err != nil {
		t.Fatalf("Release first lease: %v", err)
	}
	secondLease, err := store.AcquireRunLease(context.Background(), runID)
	if err != nil {
		t.Fatalf("AcquireRunLease second: %v", err)
	}
	defer secondLease.Release()
	loaded, err := store.LoadState(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	recovered, changed, err := recoverUnmatchedDispatches(context.Background(), store, secondLease.Epoch(), plan, loaded)
	if err != nil {
		t.Fatalf("recoverUnmatchedDispatches: %v", err)
	}
	frame := recovered.ExecutionFrames[frameID]
	if !changed || frame == nil || frame.WriterEpoch != secondLease.Epoch() || frame.NextStepIndex != 1 ||
		frame.Results["0"] == nil || frame.Results["0"].Status != enginepkg.StepStatusIndeterminate {
		t.Fatalf("recovered frame = %#v changed=%v", frame, changed)
	}
	cursor := recovered.CursorSet.Cursors[0]
	if cursor.QualifiedNodeID != "first/child-2" || cursor.StepID != "child-2" ||
		cursor.FrameID != frameID || cursor.Phase != enginepkg.ExecutionPhaseBefore {
		t.Fatalf("recovered cursor = %#v, want next child", cursor)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close store: %v", err)
	}
}

func TestEngineStartFailsWhenPlanPersistenceFails(t *testing.T) {
	_, err := newPersistentPlanTestEngine().Start(
		context.Background(),
		persistentPlanForTest(t, "run-failed-plan"),
		enginepkg.RunOptions{Mode: enginepkg.RunModeReal, Store: &failingPlanStore{}},
	)
	if !errors.Is(err, errPlanPersistence) {
		t.Fatalf("Start error = %v, want plan persistence failure", err)
	}
}

func TestEngineStopsWhenCheckpointPersistenceFails(t *testing.T) {
	store := &failingCheckpointStore{}
	handle, err := newPersistentPlanTestEngine().Start(
		context.Background(), persistentPlanForTest(t, "run-failed-checkpoint"),
		enginepkg.RunOptions{Mode: enginepkg.RunModeReal, Store: store},
	)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := handle.Next(context.Background()); !errors.Is(err, enginepkg.ErrCheckpointCommit) || !errors.Is(err, errCheckpointPersistence) {
		t.Fatalf("Next error = %v, want checkpoint persistence failure", err)
	}
	if handle.State().Status != enginepkg.RunStatusIndeterminate {
		t.Fatalf("run status = %q, want indeterminate", handle.State().Status)
	}
	if _, err := handle.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("second Next error = %v, want EOF", err)
	}
}

func TestEngineStopsBeforeDispatchWhenTracePersistenceFails(t *testing.T) {
	store := &capturingCheckpointStore{}
	executor := &recordingExecutor{}
	config := makeTestConfig()
	registry := newFakeExecutorRegistry()
	registry.Register("work", executor)
	config.Executors = registry
	config.TraceWriter = &failingTraceWriter{}
	plan := enginepkg.ValidatedForTest(&enginepkg.ExecutionPlan{
		RunID: "run-failed-trace", RunbookPath: "trace.runbook.yaml",
		Steps:    []enginepkg.ResolvedStep{{ID: "work", Kind: "work", Spec: &cliStepSpec{}}},
		Metadata: enginepkg.PlanMetadata{RunbookID: "trace"},
	})
	handle, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{Store: store})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := handle.Next(context.Background()); !errors.Is(err, enginepkg.ErrTraceCommit) ||
		!errors.Is(err, errTracePersistence) {
		t.Fatalf("Next error = %v, want trace persistence failure", err)
	}
	if executor.invoked {
		t.Fatal("executor ran after trace persistence failure")
	}
	if len(store.states) != 1 || store.states[0].Status != enginepkg.RunStatusIndeterminate {
		t.Fatalf("trace failure checkpoint = %#v", store.states)
	}
}

func TestEngineStopsBeforeDispatchWhenRunTracePersistenceFails(t *testing.T) {
	store := &kindFailingCheckpointTraceStore{
		capturingCheckpointStore: &capturingCheckpointStore{}, kind: string(tracepkg.EventKindRunStarted),
	}
	executor := &recordingExecutor{}
	config := makeTestConfig()
	registry := newFakeExecutorRegistry()
	registry.Register("work", executor)
	config.Executors = registry
	plan := enginepkg.ValidatedForTest(&enginepkg.ExecutionPlan{
		RunID: "run-failed-run-trace", RunbookPath: "trace.runbook.yaml",
		Steps:    []enginepkg.ResolvedStep{{ID: "work", Kind: "work", Spec: &cliStepSpec{}}},
		Metadata: enginepkg.PlanMetadata{RunbookID: "trace"},
	})
	handle, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{Store: store})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := handle.Next(context.Background()); !errors.Is(err, enginepkg.ErrTraceCommit) ||
		!errors.Is(err, errTracePersistence) {
		t.Fatalf("Next error = %v, want run trace persistence failure", err)
	}
	if executor.invoked {
		t.Fatal("executor ran after run trace persistence failure")
	}
	if states := store.states; len(states) != 1 || states[0].Status != enginepkg.RunStatusIndeterminate {
		t.Fatalf("run trace failure checkpoints = %#v", states)
	}
}

func TestEngineStopsAfterSettledResultWhenCompletionTraceFails(t *testing.T) {
	store := &capturingCheckpointStore{}
	config := makeTestConfig()
	registry := newFakeExecutorRegistry()
	registry.Register("work", &recordingExecutor{})
	config.Executors = registry
	config.TraceWriter = &kindFailingTraceWriter{kind: tracepkg.EventKindStepCompleted}
	plan := enginepkg.ValidatedForTest(&enginepkg.ExecutionPlan{
		RunID: "run-failed-completion-trace", RunbookPath: "trace.runbook.yaml",
		Steps:    []enginepkg.ResolvedStep{{ID: "work", Kind: "work", Spec: &cliStepSpec{}}},
		Metadata: enginepkg.PlanMetadata{RunbookID: "trace"},
	})
	handle, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{Store: store})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := handle.Next(context.Background()); !errors.Is(err, enginepkg.ErrTraceCommit) ||
		!errors.Is(err, errTracePersistence) {
		t.Fatalf("Next error = %v, want completion trace failure", err)
	}
	if len(store.states) != 2 || store.states[0].StepResults["work"] == nil ||
		store.states[1].Status != enginepkg.RunStatusIndeterminate {
		t.Fatalf("trace failure checkpoints = %#v", store.states)
	}
}

func TestEngineStopsBeforeExecutorWhenStepStartedTraceFails(t *testing.T) {
	store := &capturingCheckpointStore{}
	executor := &recordingExecutor{}
	config := makeTestConfig()
	registry := newFakeExecutorRegistry()
	registry.Register("work", executor)
	config.Executors = registry
	config.TraceWriter = &kindFailingTraceWriter{kind: tracepkg.EventKindStepStarted}
	plan := enginepkg.ValidatedForTest(&enginepkg.ExecutionPlan{
		RunID: "run-failed-step-started-trace", RunbookPath: "trace.runbook.yaml",
		Steps:    []enginepkg.ResolvedStep{{ID: "work", Kind: "work", Spec: &cliStepSpec{}}},
		Metadata: enginepkg.PlanMetadata{RunbookID: "trace"},
	})
	handle, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{Store: store})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := handle.Next(context.Background()); !errors.Is(err, enginepkg.ErrTraceCommit) {
		t.Fatalf("Next error = %v, want ErrTraceCommit", err)
	}
	if executor.invoked {
		t.Fatal("executor ran after step/started trace failure")
	}
}

func TestEngineStopsBeforeWaitWhenStepStartedTraceFails(t *testing.T) {
	waitCalled := false
	config := makeTestConfig()
	config.TraceWriter = &kindFailingTraceWriter{kind: tracepkg.EventKindStepStarted}
	config.Dispatcher = &inspectingWaitDispatcher{wait: func(
		context.Context,
		string,
		eventbus.EventFilter,
		time.Duration,
	) (*eventbus.InboundEvent, error) {
		waitCalled = true
		return &eventbus.InboundEvent{EventID: "ready", Source: "signal"}, nil
	}}
	plan := enginepkg.ValidatedForTest(&enginepkg.ExecutionPlan{
		RunID: "run-failed-wait-started-trace", RunbookPath: "trace.runbook.yaml",
		Steps: []enginepkg.ResolvedStep{{
			ID: "wait", Kind: "wait_for_event",
			Spec: &testWaitEventSpec{filter: eventbus.EventFilter{Source: "signal", ID: "ready"}},
		}},
		Metadata: enginepkg.PlanMetadata{RunbookID: "trace"},
	})
	handle, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := handle.Next(context.Background()); !errors.Is(err, enginepkg.ErrTraceCommit) {
		t.Fatalf("Next error = %v, want ErrTraceCommit", err)
	}
	if waitCalled {
		t.Fatal("dispatcher wait ran after step/started trace failure")
	}
}

func TestEngineStopsBeforeProviderWhenDispatchPreparedTraceFails(t *testing.T) {
	store := &capturingCheckpointStore{}
	providerCalled := false
	config := makeTestConfig()
	registry := newFakeExecutorRegistry()
	registry.Register("external", stepExecutorFunc(func(ctx context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
		if _, err := enginepkg.PrepareExternalDispatch(ctx, enginepkg.DispatchRequest{
			Classification: "mutating", EndpointIdentity: "trace-provider",
			RenderedRequest: map[string]any{"step": step.ID},
		}); err != nil {
			return nil, err
		}
		providerCalled = true
		return &enginepkg.StepResult{StepID: step.ID, Status: enginepkg.StepStatusCompleted}, nil
	}))
	config.Executors = registry
	config.TraceWriter = &kindFailingTraceWriter{kind: tracepkg.EventKind("dispatch/prepared")}
	plan := enginepkg.ValidatedForTest(&enginepkg.ExecutionPlan{
		RunID: "run-failed-dispatch-trace", RunbookPath: "trace.runbook.yaml",
		Steps:    []enginepkg.ResolvedStep{{ID: "external", Kind: "external", Spec: &cliStepSpec{}}},
		Metadata: enginepkg.PlanMetadata{RunbookID: "trace"},
	})
	handle, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{Store: store})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := handle.Next(context.Background()); !errors.Is(err, enginepkg.ErrTraceCommit) {
		t.Fatalf("Next error = %v, want ErrTraceCommit", err)
	}
	if providerCalled {
		t.Fatal("provider ran after dispatch/prepared trace failure")
	}
}

func TestEngineCompletionTraceFailureOverridesEOF(t *testing.T) {
	store := &capturingCheckpointStore{}
	config := makeTestConfig()
	registry := newFakeExecutorRegistry()
	registry.Register("noop", &passThroughExecutor{})
	config.Executors = registry
	config.TraceWriter = &kindFailingTraceWriter{kind: tracepkg.EventKindRunCompleted}
	plan := enginepkg.ValidatedForTest(&enginepkg.ExecutionPlan{
		RunID: "run-completion-trace-failure", RunbookPath: "trace.runbook.yaml",
		Steps:    []enginepkg.ResolvedStep{{ID: "work", Kind: "noop", Spec: &schema.NoopSpec{}}},
		Metadata: enginepkg.PlanMetadata{RunbookID: "trace"},
	})
	handle, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{Store: store})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := handle.Next(context.Background()); err != nil {
		t.Fatalf("execute work: %v", err)
	}
	if _, err := handle.Next(context.Background()); !errors.Is(err, enginepkg.ErrTraceCommit) {
		t.Fatalf("completion error = %v, want ErrTraceCommit", err)
	}
	if store.states[len(store.states)-1].Status != enginepkg.RunStatusIndeterminate {
		t.Fatalf("completion trace state = %#v", store.states)
	}
}

func TestEngineCancellationTraceFailureOverridesSuccess(t *testing.T) {
	store := &capturingCheckpointStore{}
	config := makeTestConfig()
	config.TraceWriter = &kindFailingTraceWriter{kind: tracepkg.EventKindRunCancelled}
	plan := enginepkg.ValidatedForTest(&enginepkg.ExecutionPlan{
		RunID: "run-cancel-trace-failure", RunbookPath: "trace.runbook.yaml",
		Steps:    []enginepkg.ResolvedStep{{ID: "work", Kind: "noop", Spec: &schema.NoopSpec{}}},
		Metadata: enginepkg.PlanMetadata{RunbookID: "trace"},
	})
	handle, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{Store: store})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := handle.Cancel(context.Background(), "operator requested"); !errors.Is(err, enginepkg.ErrTraceCommit) {
		t.Fatalf("Cancel error = %v, want ErrTraceCommit", err)
	}
	if store.states[len(store.states)-1].Status != enginepkg.RunStatusIndeterminate {
		t.Fatalf("cancel trace state = %#v", store.states)
	}
}

func TestEngineCheckpointsSettledResultAfterErrorRouting(t *testing.T) {
	tests := []struct {
		name             string
		result           *enginepkg.StepResult
		onError          string
		steps            []enginepkg.ResolvedStep
		wantCursorStep   string
		wantCursorIndex  int
		wantResultStatus enginepkg.StepStatus
	}{
		{
			name:   "skipped",
			result: &enginepkg.StepResult{StepID: "action", Status: enginepkg.StepStatusSkipped},
			steps: []enginepkg.ResolvedStep{
				{ID: "action", Kind: "controlled", Spec: &cliStepSpec{}},
				{ID: "after", Kind: "noop", Spec: &schema.NoopSpec{}},
			},
			wantCursorStep: "after", wantCursorIndex: 1, wantResultStatus: enginepkg.StepStatusSkipped,
		},
		{
			name:    "failed continue",
			result:  &enginepkg.StepResult{StepID: "action", Status: enginepkg.StepStatusFailed, Error: errors.New("handled")},
			onError: "continue",
			steps: []enginepkg.ResolvedStep{
				{ID: "action", Kind: "controlled", Spec: &cliStepSpec{}, OnError: "continue"},
				{ID: "after", Kind: "noop", Spec: &schema.NoopSpec{}},
			},
			wantCursorStep: "after", wantCursorIndex: 1, wantResultStatus: enginepkg.StepStatusFailed,
		},
		{
			name:    "failed goto",
			result:  &enginepkg.StepResult{StepID: "action", Status: enginepkg.StepStatusFailed, Error: errors.New("routed")},
			onError: "goto:handler",
			steps: []enginepkg.ResolvedStep{
				{ID: "action", Kind: "controlled", Spec: &cliStepSpec{}, OnError: "goto:handler"},
				{ID: "skipped", Kind: "noop", Spec: &schema.NoopSpec{}},
				{ID: "handler", Kind: "noop", Spec: &schema.NoopSpec{}},
			},
			wantCursorStep: "handler", wantCursorIndex: 2, wantResultStatus: enginepkg.StepStatusFailed,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &capturingCheckpointStore{}
			config := makeTestConfig()
			registry := newFakeExecutorRegistry()
			registry.Register("controlled", stepExecutorFunc(func(context.Context, enginepkg.ResolvedStep, map[string]any) (*enginepkg.StepResult, error) {
				copy := *test.result
				return &copy, nil
			}))
			registry.Register("noop", &passThroughExecutor{})
			config.Executors = registry
			plan := enginepkg.ValidatedForTest(&enginepkg.ExecutionPlan{
				RunID:       "run-settled-" + strings.ReplaceAll(test.name, " ", "-"),
				RunbookPath: "settled.runbook.yaml", Steps: test.steps,
				Metadata: enginepkg.PlanMetadata{RunbookID: "settled"},
			})
			handle, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{Store: store})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if _, err := handle.Next(context.Background()); err != nil {
				t.Fatalf("Next: %v", err)
			}
			if len(store.states) == 0 {
				t.Fatal("composite produced no checkpoint")
			}
			state := store.states[len(store.states)-1]
			if state.CheckpointSequence != int64(len(store.states)) {
				t.Fatalf("final checkpoint sequence = %d, want %d", state.CheckpointSequence, len(store.states))
			}
			cursor := state.CursorSet.Cursors[0]
			if cursor.StepID != test.wantCursorStep || cursor.StepIndex != test.wantCursorIndex {
				t.Fatalf("cursor = %#v, want %s at %d", cursor, test.wantCursorStep, test.wantCursorIndex)
			}
			if state.StepResults["action"] == nil || state.StepResults["action"].Status != test.wantResultStatus {
				t.Fatalf("settled result = %#v, want status %s", state.StepResults["action"], test.wantResultStatus)
			}
			if test.onError != "" && state.Vars["__error_step_id"] != "action" {
				t.Fatalf("error routing vars = %#v", state.Vars)
			}
		})
	}
}

func TestEngineCommitsInteractionLifecycleBeforeAdvancing(t *testing.T) {
	store := &capturingCheckpointStore{}
	config := makeTestConfig()
	registry := newFakeExecutorRegistry()
	registry.Register("durable-interaction", stepExecutorFunc(func(ctx context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
		committer := enginepkg.InteractionCommitterFromContext(ctx)
		if committer == nil {
			return nil, errors.New("interaction committer missing")
		}
		request := json.RawMessage(`{"kind":"choice"}`)
		prepared, err := committer.PrepareInteraction(ctx, enginepkg.InteractionState{
			SchemaVersion: enginepkg.InteractionStateSchemaV1,
			TurnID:        "turn-one", NodeID: step.ID, StepID: step.ID, Kind: "choice",
			Ordinal:       enginepkg.InteractionInvocationTrackerFromContext(ctx).Next(step.ID, "choice"),
			Status:        enginepkg.InteractionStatusPending,
			RequestDigest: enginepkg.InteractionPayloadDigest(request), Request: request,
		})
		if err != nil {
			return nil, err
		}
		answer := json.RawMessage(`{"kind":"choice","selected":["one"]}`)
		if _, err := committer.AcceptInteraction(ctx, prepared.TurnID, enginepkg.InteractionPayloadDigest(answer), answer); err != nil {
			return nil, err
		}
		return &enginepkg.StepResult{StepID: step.ID, Status: enginepkg.StepStatusCompleted}, nil
	}))
	registry.Register("noop", &passThroughExecutor{})
	config.Executors = registry
	plan := enginepkg.ValidatedForTest(&enginepkg.ExecutionPlan{
		RunID: "run-interaction-checkpoints", RunbookPath: "interaction.runbook.yaml",
		Steps: []enginepkg.ResolvedStep{
			{ID: "choose", Kind: "durable-interaction", Spec: &cliStepSpec{}},
			{ID: "after", Kind: "noop", Spec: &schema.NoopSpec{}},
		},
		Metadata: enginepkg.PlanMetadata{RunbookID: "interaction"},
	})
	handle, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{Store: store})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := handle.Next(context.Background()); err != nil {
		t.Fatalf("Next: %v", err)
	}
	if len(store.states) != 3 {
		t.Fatalf("checkpoint count = %d, want prepare + accept + settle", len(store.states))
	}
	pending := store.states[0]
	if pending.Status != enginepkg.RunStatusWaiting || pending.Interactions["turn-one"] == nil ||
		pending.Interactions["turn-one"].Status != enginepkg.InteractionStatusPending {
		t.Fatalf("pending checkpoint = %#v", pending)
	}
	if cursor := pending.CursorSet.Cursors[0]; cursor.StepID != "choose" || cursor.StepIndex != 0 {
		t.Fatalf("pending cursor = %#v, want choose/before", cursor)
	}
	accepted := store.states[1]
	if accepted.Interactions["turn-one"] == nil || accepted.Interactions["turn-one"].Status != enginepkg.InteractionStatusAnswered {
		t.Fatalf("accepted checkpoint = %#v", accepted.Interactions)
	}
	if accepted.Interactions["turn-one"].AcceptedAt == "" || accepted.Interactions["turn-one"].AuditToken != "turn-one" {
		t.Fatalf("accepted audit metadata = %#v", accepted.Interactions["turn-one"])
	}
	settled := store.states[2]
	if settled.Status != enginepkg.RunStatusRunning || len(settled.Interactions) != 0 {
		t.Fatalf("settled interaction state = status %s interactions %#v", settled.Status, settled.Interactions)
	}
	if len(settled.InteractionInvocationCounts) != 1 {
		t.Fatalf("settled interaction invocation counts = %#v", settled.InteractionInvocationCounts)
	}
	for _, count := range settled.InteractionInvocationCounts {
		if count != 1 {
			t.Fatalf("settled interaction invocation count = %d, want 1", count)
		}
	}
	if cursor := settled.CursorSet.Cursors[0]; cursor.StepID != "after" || cursor.StepIndex != 1 {
		t.Fatalf("settled cursor = %#v, want after/before", cursor)
	}
}

func TestEngineCommitsDispatchIntentBeforeBoundaryAndSettlesWithResult(t *testing.T) {
	base := t.TempDir()
	store := runstore.NewDirRunStore(base)
	t.Cleanup(func() { _ = store.Close() })
	config := makeTestConfig()
	registry := newFakeExecutorRegistry()
	var prepared enginepkg.RunState
	registry.Register("cli", stepExecutorFunc(func(ctx context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
		if _, err := enginepkg.PrepareExternalDispatch(ctx, enginepkg.DispatchRequest{
			Classification: "unspecified", EndpointIdentity: "local-process",
			RenderedRequest: map[string]any{"command": "inspect", "args": []string{"--safe"}},
		}); err != nil {
			return nil, err
		}
		var err error
		prepared, err = store.LoadState(ctx, "run-dispatch-order")
		if err != nil {
			return nil, err
		}
		return &enginepkg.StepResult{
			StepID: step.ID, Status: enginepkg.StepStatusCompleted, Outcome: enginepkg.StepOutcomeSuccess,
			Output: map[string]any{"ok": true}, Vars: map[string]any{},
		}, nil
	}))
	registry.Register("noop", &passThroughExecutor{})
	config.Executors = registry
	plan := &enginepkg.ExecutionPlan{
		RunID: "run-dispatch-order", RunbookPath: "dispatch.runbook.yaml",
		Steps: []enginepkg.ResolvedStep{
			{ID: "external", Kind: "cli", Spec: &schema.CLISpec{Command: "inspect"}},
			{ID: "after", Kind: "noop", Spec: &schema.NoopSpec{}},
		},
		Metadata: enginepkg.PlanMetadata{RunbookID: "dispatch-order"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	handle, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{
		Mode: enginepkg.RunModeReal, Store: store,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = handle.Cancel(context.Background(), "test cleanup") })
	if _, err := handle.Next(context.Background()); err != nil {
		t.Fatalf("Next: %v", err)
	}
	if prepared.CheckpointSequence != 1 || len(prepared.Dispatches) != 1 ||
		prepared.CursorSet.Cursors[0].Phase != enginepkg.ExecutionPhaseExecute {
		t.Fatalf("prepared checkpoint = %#v", prepared)
	}
	settled, err := store.LoadState(context.Background(), plan.RunID)
	if err != nil {
		t.Fatalf("LoadState settled: %v", err)
	}
	if settled.CheckpointSequence != 2 || settled.StepResults["external"] == nil ||
		settled.CursorSet.Cursors[0].StepID != "after" {
		t.Fatalf("settled checkpoint = %#v", settled)
	}
	for _, dispatch := range settled.Dispatches {
		if dispatch.Status != enginepkg.DispatchStatusSettled || dispatch.ResultDigest == "" || dispatch.SettledAt == "" {
			t.Fatalf("settled dispatch = %#v", dispatch)
		}
	}
}

func TestEngineCommitsWaitIntentBeforeEventConsumptionAndSettlesWithResult(t *testing.T) {
	base := t.TempDir()
	store := runstore.NewDirRunStore(base)
	t.Cleanup(func() { _ = store.Close() })
	config := makeTestConfig()
	config.Dispatcher = &inspectingWaitDispatcher{wait: func(
		ctx context.Context,
		_ string,
		_ eventbus.EventFilter,
		_ time.Duration,
	) (*eventbus.InboundEvent, error) {
		prepared, err := store.LoadState(ctx, "run-wait-dispatch")
		if err != nil {
			return nil, err
		}
		if prepared.CheckpointSequence != 1 || len(prepared.Dispatches) != 1 ||
			prepared.CursorSet.Cursors[0].Phase != enginepkg.ExecutionPhaseExecute {
			return nil, fmt.Errorf("prepared wait checkpoint = %#v", prepared)
		}
		return &eventbus.InboundEvent{EventID: "ready", Source: "signal", Payload: map[string]any{"value": "saved"}}, nil
	}}
	plan := persistentPlanForTest(t, "run-wait-dispatch")
	plan.Steps = []enginepkg.ResolvedStep{
		{ID: "wait", Kind: "wait_for_event", Spec: &schema.WaitForEventSpec{Event: schema.WaitEventConfig{
			Source: schema.EventSourceSignal, ID: "ready",
		}}},
		{ID: "after", Kind: "noop", Spec: &schema.NoopSpec{}},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	handle, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{Store: store})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = handle.Cancel(context.Background(), "test cleanup") })
	result, err := handle.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if result.Output["value"] != "saved" {
		t.Fatalf("wait result = %#v", result)
	}
	settled, err := store.LoadState(context.Background(), plan.RunID)
	if err != nil {
		t.Fatalf("LoadState settled: %v", err)
	}
	if settled.CheckpointSequence != 2 || settled.StepResults["wait"] == nil ||
		settled.CursorSet.Cursors[0].StepID != "after" {
		t.Fatalf("settled wait checkpoint = %#v", settled)
	}
	for _, dispatch := range settled.Dispatches {
		if dispatch.Status != enginepkg.DispatchStatusSettled || dispatch.ResultDigest == "" {
			t.Fatalf("settled wait dispatch = %#v", dispatch)
		}
	}
}

func TestEngineNestedWaitFixtureIsAtomicWithFrameProgress(t *testing.T) {
	store := &capturingCheckpointStore{}
	config := makeTestConfig()
	config.Store = store
	registry := newFakeExecutorRegistry()
	registry.Register("noop", &passThroughExecutor{})
	config.Executors = registry
	plan := enginepkg.ValidatedForTest(&enginepkg.ExecutionPlan{
		RunID: "nested-wait-atomic", RunbookPath: "root.runbook.yaml",
		Steps:    []enginepkg.ResolvedStep{{ID: "container", Kind: "noop", Spec: &schema.NoopSpec{}}},
		Metadata: enginepkg.PlanMetadata{RunbookID: "nested-wait-atomic"},
	})
	handle, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{Store: store})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	internalHandle := handle.(*runHandle)
	waitStep := enginepkg.ResolvedStep{ID: "wait", Kind: "wait_for_event", Spec: &schema.WaitForEventSpec{
		Event: schema.WaitEventConfig{Source: schema.EventSourceSignal, ID: "ready"},
	}}
	definitionDigest, err := enginepkg.ExecutionFrameDefinitionDigest([]enginepkg.ResolvedStep{waitStep})
	if err != nil {
		t.Fatalf("ExecutionFrameDefinitionDigest: %v", err)
	}
	frame, err := internalHandle.BeginExecutionFrame(context.Background(), enginepkg.ExecutionFrameRequest{
		ParentQualifiedNodeID: "container", ParentStepID: "container", Kind: "include",
		CallPath: []enginepkg.DebugCallFrame{{StepID: "container"}}, DefinitionDigest: definitionDigest,
		StepCount: 1, StepIDs: []string{"wait"}, InitialVars: map[string]any{},
	})
	if err != nil {
		t.Fatalf("BeginExecutionFrame: %v", err)
	}
	ctx := enginepkg.WithDebugCallPath(context.Background(), frame.CallPath)
	ctx = enginepkg.WithDynamicIncludeStructuralPath(ctx, []schema.DynamicIncludeFrameIdentity{{
		QualifiedNodeID: "container", Kind: "include", Invocation: frame.Invocation,
	}})
	ctx = enginepkg.WithDispatchExecutionBoundary(ctx, enginepkg.DispatchExecutionBoundary{
		QualifiedNodeID: "container/wait", CallPath: frame.CallPath, StepID: "wait",
		FrameID: frame.FrameID, FrameStepIndex: 0, Invocation: frame.Invocation, RetryAttempt: 1,
	})
	if _, err := internalHandle.CommitExecutionFrameStep(ctx, enginepkg.ExecutionFrameStepCommit{
		FrameID: frame.FrameID, StepIndex: 0, StepKind: "wait_for_event",
		Result: &enginepkg.StepResult{
			StepID: "wait", Status: enginepkg.StepStatusCompleted, Outcome: enginepkg.StepOutcomeSuccess,
			Output: map[string]any{"value": "saved"}, Vars: map[string]any{},
		},
		WorkingVars: map[string]any{},
	}); err != nil {
		t.Fatalf("CommitExecutionFrameStep: %v", err)
	}
	checkpoint := store.states[len(store.states)-1]
	frameState := checkpoint.ExecutionFrames[frame.FrameID]
	if frameState == nil || frameState.NextStepIndex != 1 || frameState.Results["0"] == nil {
		t.Fatalf("committed frame = %#v", frameState)
	}
	foundEvent, foundResult := false, false
	for _, event := range checkpoint.PendingTraceEvents {
		foundEvent = foundEvent || event.Kind == string(tracepkg.EventKindEventReceived) && event.Payload["qualified_node_id"] == "container/wait"
		foundResult = foundResult || event.Kind == string(tracepkg.EventKindStepCompleted) && event.Payload["qualified_node_id"] == "container/wait"
	}
	if !foundEvent || !foundResult {
		t.Fatalf("atomic nested wait trace = %#v", checkpoint.PendingTraceEvents)
	}
}

func TestEngineGovernancePreflightSeesNestedExecutionBoundary(t *testing.T) {
	gate := &boundaryRecordingApprovalGate{}
	config := makeTestConfig()
	registry := newFakeExecutorRegistry()
	registry.Register("noop", &passThroughExecutor{})
	config.Executors = registry
	config.ApprovalGate = gate
	plan := enginepkg.ValidatedForTest(&enginepkg.ExecutionPlan{
		RunID: "run-governance-boundary", RunbookPath: "child.runbook.yaml",
		Steps:            []enginepkg.ResolvedStep{{ID: "child", Kind: "noop", Spec: &schema.NoopSpec{}}},
		GovernanceSource: &schema.GovernanceConfig{RequireApproval: true},
		Metadata:         enginepkg.PlanMetadata{RunbookID: "child"},
	})
	ctx := enginepkg.WithDebugCallPath(context.Background(), []enginepkg.DebugCallFrame{{StepID: "parent"}})
	frameID := enginepkg.InteractionPayloadDigest([]byte("governance frame"))
	ctx = enginepkg.WithExecutionFrameBinding(ctx, enginepkg.ExecutionFrameBinding{FrameID: frameID, StepOffset: 2})
	handle, err := New(config).Start(ctx, plan, enginepkg.RunOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := handle.Next(context.Background()); err != nil {
		t.Fatalf("Next: %v", err)
	}
	if !gate.found || gate.boundary.QualifiedNodeID != "parent/child" ||
		gate.boundary.FrameID != frameID || gate.boundary.FrameStepIndex != 2 {
		t.Fatalf("governance boundary = %#v found=%v", gate.boundary, gate.found)
	}
}

func TestEngineParallelBranchCommitsNestedDispatchesIndependently(t *testing.T) {
	store := &capturingCheckpointStore{}
	config := makeTestConfig()
	registry := newFakeExecutorRegistry()
	var executed []string
	registry.Register("external", stepExecutorFunc(func(ctx context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
		if _, err := enginepkg.PrepareExternalDispatch(ctx, enginepkg.DispatchRequest{
			Classification: "unspecified", EndpointIdentity: "parallel-provider",
			RenderedRequest: map[string]any{"step": step.ID},
		}); err != nil {
			return nil, err
		}
		executed = append(executed, step.ID)
		return &enginepkg.StepResult{
			StepID: step.ID, Status: enginepkg.StepStatusCompleted, Outcome: enginepkg.StepOutcomeSuccess,
			Output: map[string]any{"step": step.ID}, Vars: map[string]any{},
		}, nil
	}))
	config.Executors = registry
	plan := enginepkg.ValidatedForTest(&enginepkg.ExecutionPlan{
		RunID: "run-parallel-dispatch-frame", RunbookPath: "parallel.runbook.yaml",
		Steps: []enginepkg.ResolvedStep{{
			ID: "parallel", Kind: "parallel", Spec: &testParallelSpec{branches: []enginepkg.BranchSpec{{
				Label: "branch-a",
				Steps: []enginepkg.ResolvedStep{
					{ID: "child-1", Kind: "external", Spec: &cliStepSpec{}},
					{ID: "child-2", Kind: "external", Spec: &cliStepSpec{}},
				},
			}}},
		}},
		Metadata: enginepkg.PlanMetadata{RunbookID: "parallel-frame"},
	})
	handle, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{Store: store})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	result, err := handle.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if result.Status != enginepkg.StepStatusCompleted || len(executed) != 2 {
		t.Fatalf("result=%#v executed=%#v", result, executed)
	}
	state := handle.State()
	if len(state.ExecutionFrames) != 1 || len(state.Dispatches) != 2 {
		t.Fatalf("frames=%#v dispatches=%#v", state.ExecutionFrames, state.Dispatches)
	}
	for _, frame := range state.ExecutionFrames {
		if frame.Kind != "parallel" || frame.BranchLabel != "branch-a" ||
			frame.Status != enginepkg.ExecutionFrameStatusCompleted || frame.NextStepIndex != 2 {
			t.Fatalf("parallel frame = %#v", frame)
		}
	}
}

func TestEngineNestedDynamicIncludeNotFoundContinueCommitsCanonicalDigest(t *testing.T) {
	store := runstore.NewDirRunStore(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	config := makeTestConfig()
	registry := newFakeExecutorRegistry()
	registry.Register("include", stepExecutorFunc(func(ctx context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
		if _, err := enginepkg.PrepareExternalDispatch(ctx, enginepkg.DispatchRequest{
			Classification: "read-only", EndpointIdentity: "dynamic-include-resolver",
			RenderedRequest: map[string]any{"rendered_ref": "missing/child"},
		}); err != nil {
			return nil, err
		}
		return &enginepkg.StepResult{
			StepID: step.ID, Status: enginepkg.StepStatusSkipped, Outcome: enginepkg.StepOutcomeSkipped,
			Output: map[string]any{"skip_reason": "include_not_found"},
			Vars:   map[string]any{"runbook_found": false},
		}, nil
	}))
	config.Executors = registry
	includeSpec := &schema.IncludeSpec{Include: schema.IncludeConfig{
		RunbookRef: "missing/child", ResolveFrom: schema.ResolveFromCatalog,
		OnNotFound: schema.OnNotFoundContinue,
	}}
	parallelSpec := &schema.ParallelNode{
		ID: "parallel", Branches: []schema.ParallelBranch{{
			Label: "branch", Steps: []schema.FlowNode{{Step: &schema.Step{
				ID: "dynamic", Type: schema.StepTypeInclude, IncludeSpec: includeSpec,
			}}},
		}},
	}
	plan := &enginepkg.ExecutionPlan{
		RunID: "run-nested-dynamic-not-found", RunbookPath: "parallel.runbook.yaml",
		Steps: []enginepkg.ResolvedStep{
			{ID: "parallel", Kind: "parallel", Spec: parallelSpec},
			{ID: "dynamic", Kind: "include", Spec: includeSpec, Depth: 1, ParentID: "parallel", ParentKind: "parallel"},
		},
		Metadata: enginepkg.PlanMetadata{RunbookID: "nested-dynamic-not-found"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	handle, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{Store: store})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	result, err := handle.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if result.Status != enginepkg.StepStatusCompleted {
		t.Fatalf("parallel result = %#v", result)
	}
	state, err := store.LoadState(context.Background(), plan.RunID)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	for _, dispatch := range state.Dispatches {
		if dispatch.EndpointIdentity != "dynamic-include-resolver" {
			continue
		}
		var skipped *enginepkg.StepResult
		for _, frame := range state.ExecutionFrames {
			if frame.FrameID == dispatch.FrameID {
				skipped = frame.Results[strconv.Itoa(dispatch.FrameStepIndex)]
			}
		}
		digest, ok := enginepkg.DynamicIncludeNotFoundResultDigest(skipped)
		if !ok || dispatch.ResultDigest != digest {
			t.Fatalf("nested resolver dispatch/result = %#v/%#v", dispatch, skipped)
		}
		return
	}
	t.Fatal("nested resolver dispatch was not persisted")
}

func TestEngineDynamicIncludeNotFoundThenResolvedUsesDispatchInvocation(t *testing.T) {
	store := runstore.NewDirRunStore(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	resolver := &notFoundThenResolvedDynamicResolver{result: &internalexecutor.DynamicIncludeResult{
		Flow:        []schema.FlowNode{{Step: &schema.Step{ID: "child", Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{}}}},
		QualifiedID: "pkg/child", RunbookID: "child", RunbookName: "Child",
		ContentHash: strings.Repeat("a", 64), AbsPath: `C:\runbooks\child.runbook.yaml`,
		PackageName: "pkg", PackageVersion: "1.0.0",
		FileDigest:    enginepkg.InteractionPayloadDigest([]byte("file")),
		PackageDigest: enginepkg.InteractionPayloadDigest([]byte("package")),
	}}
	registry := internalexecutor.NewDefaultRegistry(internalexecutor.RegistryConfig{
		Evaluator: &internalexpr.TemplateEvaluator{}, DynamicIncludeResolver: resolver,
		SubStepRunner: func(
			context.Context, internalexecutor.SubStepParent, []schema.FlowNode, map[string]any,
		) ([]*enginepkg.StepResult, error) {
			return nil, nil
		},
	})
	triggerCalls := 0
	registry.Register("cli", stepExecutorFunc(func(_ context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
		triggerCalls++
		status, outcome := enginepkg.StepStatusCompleted, enginepkg.StepOutcomeSuccess
		var resultErr error
		if triggerCalls == 1 {
			status, outcome, resultErr = enginepkg.StepStatusFailed, enginepkg.StepOutcomeFailed, errors.New("retry dynamic include")
		}
		return &enginepkg.StepResult{
			StepID: step.ID, Status: status, Outcome: outcome, Error: resultErr, Vars: map[string]any{},
		}, nil
	}))
	config := makeTestConfig()
	config.Executors = registry
	plan := &enginepkg.ExecutionPlan{
		RunID: "run-dynamic-not-found-then-resolved", RunbookPath: "dynamic.runbook.yaml",
		Steps: []enginepkg.ResolvedStep{
			{ID: "dynamic", Kind: "include", Spec: &schema.IncludeSpec{Include: schema.IncludeConfig{
				RunbookRef: "pkg/child", ResolveFrom: schema.ResolveFromCatalog, OnNotFound: schema.OnNotFoundContinue,
			}}},
			{ID: "trigger", Kind: "cli", Spec: &schema.CLISpec{Command: "trigger"}, OnError: "goto:dynamic"},
		},
		Metadata: enginepkg.PlanMetadata{RunbookID: "dynamic-not-found-then-resolved"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	handle, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{Store: store})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result, err := handle.Next(context.Background()); err != nil || result.Status != enginepkg.StepStatusSkipped {
		t.Fatalf("first dynamic result/error = %#v/%v", result, err)
	}
	if result, err := handle.Next(context.Background()); err != nil || result.Status != enginepkg.StepStatusFailed {
		t.Fatalf("goto trigger result/error = %#v/%v", result, err)
	}
	if result, err := handle.Next(context.Background()); err != nil || result.Status != enginepkg.StepStatusCompleted {
		t.Fatalf("second dynamic result/error = %#v/%v", result, err)
	}
	state, err := store.LoadState(context.Background(), plan.RunID)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(state.DynamicIncludes) != 1 {
		t.Fatalf("dynamic resolutions = %#v", state.DynamicIncludes)
	}
	for _, resolution := range state.DynamicIncludes {
		if resolution.Invocation != 2 || resolution.Pin.Invocation != 2 {
			t.Fatalf("resolved invocation = %#v", resolution)
		}
	}
}

func TestEngineDynamicResolutionPinsUsePlanStructuralPaths(t *testing.T) {
	closure, err := plansnapshot.EncodeFlowClosure([]schema.FlowNode{{Step: &schema.Step{
		ID: "child-work", Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{},
	}}})
	if err != nil {
		t.Fatalf("EncodeFlowClosure: %v", err)
	}
	newRegistry := func() *fakeExecutorRegistry {
		registry := newFakeExecutorRegistry()
		registry.Register("include", stepExecutorFunc(func(ctx context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
			emitter := internalexecutor.EmitterFromContext(ctx)
			if emitter == nil {
				return nil, errors.New("dynamic include child has no event emitter")
			}
			emitter("include/runtimePath", map[string]any{"step_id": step.ID})
			if _, err := enginepkg.PrepareExternalDispatch(ctx, enginepkg.DispatchRequest{
				Classification: "read-only", EndpointIdentity: "dynamic-include-resolver",
				RenderedRequest: map[string]any{"rendered_ref": "pkg/child"},
			}); err != nil {
				return nil, err
			}
			_, err := enginepkg.CommitDynamicIncludeResolution(ctx, schema.LockedDynamicInclude{
				RenderedRef: "pkg/child", QualifiedID: "pkg/child",
				RunbookID: "child", RunbookName: "Child", RunbookContentHash: strings.Repeat("a", 64),
				AbsPath: `C:\runbooks\child.runbook.yaml`, PackageName: "pkg", PackageVersion: "1.0.0",
				FileDigest:    enginepkg.InteractionPayloadDigest([]byte("file")),
				PackageDigest: enginepkg.InteractionPayloadDigest([]byte("package")), ExecutableClosure: closure,
			})
			if err != nil {
				return nil, err
			}
			return &enginepkg.StepResult{
				StepID: step.ID, Status: enginepkg.StepStatusCompleted, Outcome: enginepkg.StepOutcomeSuccess,
				Output: map[string]any{}, Vars: map[string]any{},
			}, nil
		}))
		return registry
	}
	assertPin := func(t *testing.T, state enginepkg.RunState, wantContainer, wantKind, wantBranch string) {
		t.Helper()
		if len(state.DynamicIncludes) != 1 {
			t.Fatalf("dynamic resolutions = %#v", state.DynamicIncludes)
		}
		for _, resolution := range state.DynamicIncludes {
			if resolution.QualifiedNodeID != wantContainer+"/dynamic" || len(resolution.Pin.StructuralPath) != 1 {
				t.Fatalf("dynamic resolution = %#v", resolution)
			}
			identity := resolution.Pin.StructuralPath[0]
			if identity.QualifiedNodeID != wantContainer || identity.Kind != wantKind ||
				identity.BranchLabel != wantBranch || identity.Invocation != 1 {
				t.Fatalf("structural identity = %#v", identity)
			}
		}
	}

	t.Run("parallel", func(t *testing.T) {
		store := runstore.NewDirRunStore(t.TempDir())
		t.Cleanup(func() { _ = store.Close() })
		config := makeTestConfig()
		registry := newRegistry()
		config.Executors = registry
		includeSpec := &schema.IncludeSpec{Include: schema.IncludeConfig{RunbookRef: "pkg/child", ResolveFrom: schema.ResolveFromCatalog}}
		parallelSpec := &schema.ParallelNode{ID: "parallel", Branches: []schema.ParallelBranch{{
			Label: "left", Steps: []schema.FlowNode{{Step: &schema.Step{
				ID: "dynamic", Type: schema.StepTypeInclude, IncludeSpec: includeSpec,
			}}},
		}}}
		plan := &enginepkg.ExecutionPlan{
			RunID: "run-parallel-dynamic-path", RunbookPath: "parallel.runbook.yaml",
			Steps: []enginepkg.ResolvedStep{
				{ID: "parallel", Kind: "parallel", Spec: parallelSpec},
				{ID: "dynamic", Kind: "include", Spec: includeSpec, Depth: 1, ParentID: "parallel", ParentKind: "parallel", BranchLabel: "left"},
			},
			Metadata: enginepkg.PlanMetadata{RunbookID: "parallel-dynamic-path"},
		}
		if err := planner.ValidateExecutionPlan(plan); err != nil {
			t.Fatalf("ValidateExecutionPlan: %v", err)
		}
		handle, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{Store: store})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		if _, err := handle.Next(context.Background()); err != nil {
			t.Fatalf("Next: %v", err)
		}
		assertPin(t, handle.State(), "parallel", "parallel", "left")
	})

	t.Run("compensate", func(t *testing.T) {
		store := runstore.NewDirRunStore(t.TempDir())
		t.Cleanup(func() { _ = store.Close() })
		config := makeTestConfig()
		registry := newRegistry()
		registry.Register("compensate", internalexecutor.NewCompensateExecutor())
		registry.Register("cli", stepExecutorFunc(func(_ context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
			return &enginepkg.StepResult{
				StepID: step.ID, Status: enginepkg.StepStatusFailed, Outcome: enginepkg.StepOutcomeFailed,
				Error: errors.New("trigger compensation"), Vars: map[string]any{},
			}, nil
		}))
		config.Executors = registry
		includeStep := &schema.Step{ID: "dynamic", Type: schema.StepTypeInclude, IncludeSpec: &schema.IncludeSpec{
			Include: schema.IncludeConfig{RunbookRef: "pkg/child", ResolveFrom: schema.ResolveFromCatalog},
		}}
		compensateSpec := &schema.CompensateSpec{Compensate: schema.CompensateConfig{
			On: "failure", Steps: []schema.FlowNode{{Step: includeStep}},
		}}
		plan := &enginepkg.ExecutionPlan{
			RunID: "run-compensate-dynamic-path", RunbookPath: "compensate.runbook.yaml",
			Steps: []enginepkg.ResolvedStep{
				{ID: "register", Kind: "compensate", Spec: compensateSpec},
				{ID: "dynamic", Kind: "include", Spec: includeStep.IncludeSpec, Depth: 1, ParentID: "register", ParentKind: "compensate"},
				{ID: "fail", Kind: "cli", Spec: &schema.CLISpec{Command: "fail"}},
			},
			Metadata: enginepkg.PlanMetadata{RunbookID: "compensate-dynamic-path"},
		}
		if err := planner.ValidateExecutionPlan(plan); err != nil {
			t.Fatalf("ValidateExecutionPlan: %v", err)
		}
		handle, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{Store: store})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		if _, err := handle.Next(context.Background()); err != nil {
			t.Fatalf("register compensation: %v", err)
		}
		if _, err := handle.Next(context.Background()); err != nil {
			t.Fatalf("trigger compensation: %v", err)
		}
		assertPin(t, handle.State(), "register", "compensate", "")
	})
}

func TestEngineParallelSiblingCancellationLeavesMutatingDispatchIndeterminate(t *testing.T) {
	store := &capturingCheckpointStore{}
	executorImpl := &siblingCancelledDispatchExecutor{prepared: make(chan struct{})}
	config := makeTestConfig()
	registry := newFakeExecutorRegistry()
	registry.Register("controlled", executorImpl)
	config.Executors = registry
	plan := enginepkg.ValidatedForTest(&enginepkg.ExecutionPlan{
		RunID: "run-parallel-cancelled-dispatch", RunbookPath: "parallel.runbook.yaml",
		Steps: []enginepkg.ResolvedStep{{
			ID: "parallel", Kind: "parallel", Spec: &testParallelSpec{branches: []enginepkg.BranchSpec{
				{Label: "external", Steps: []enginepkg.ResolvedStep{{ID: "external", Kind: "controlled", Spec: &cliStepSpec{}}}},
				{Label: "failure", Steps: []enginepkg.ResolvedStep{{ID: "fail", Kind: "controlled", Spec: &cliStepSpec{}}}},
			}},
		}},
		Metadata: enginepkg.PlanMetadata{RunbookID: "parallel-cancelled-dispatch"},
	})
	handle, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{Store: store})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	result, err := handle.Next(context.Background())
	if !errors.Is(err, enginepkg.ErrIndeterminate) {
		t.Fatalf("Next result/error = %#v/%v, want ErrIndeterminate", result, err)
	}
	state := handle.State()
	if state.Status != enginepkg.RunStatusIndeterminate {
		t.Fatalf("run status = %s, want indeterminate", state.Status)
	}
	found := false
	for _, dispatch := range state.Dispatches {
		if dispatch.EndpointIdentity != "parallel-cancel-provider" {
			continue
		}
		found = true
		if dispatch.Status != enginepkg.DispatchStatusIndeterminate || dispatch.ResultDigest != "" {
			t.Fatalf("cancelled mutating dispatch = %#v, want indeterminate", dispatch)
		}
	}
	if !found {
		t.Fatal("cancelled mutating dispatch was not journaled")
	}
}

func TestEngineRejectsUnencodableFramedDispatchResult(t *testing.T) {
	store := &capturingCheckpointStore{}
	config := makeTestConfig()
	registry := newFakeExecutorRegistry()
	registry.Register("external", stepExecutorFunc(func(ctx context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
		if _, err := enginepkg.PrepareExternalDispatch(ctx, enginepkg.DispatchRequest{
			Classification: "mutating", EndpointIdentity: "parallel-provider",
			RenderedRequest: map[string]any{"step": step.ID},
		}); err != nil {
			return nil, err
		}
		return &enginepkg.StepResult{
			StepID: step.ID, Status: enginepkg.StepStatusCompleted, Outcome: enginepkg.StepOutcomeSuccess,
			Output: map[string]any{"invalid": make(chan struct{})}, Vars: map[string]any{},
		}, nil
	}))
	config.Executors = registry
	plan := enginepkg.ValidatedForTest(&enginepkg.ExecutionPlan{
		RunID: "run-unencodable-frame-result", RunbookPath: "parallel.runbook.yaml",
		Steps: []enginepkg.ResolvedStep{{
			ID: "parallel", Kind: "parallel", Spec: &testParallelSpec{branches: []enginepkg.BranchSpec{{
				Label: "branch", Steps: []enginepkg.ResolvedStep{{ID: "child", Kind: "external", Spec: &cliStepSpec{}}},
			}}},
		}},
		Metadata: enginepkg.PlanMetadata{RunbookID: "unencodable-frame-result"},
	})
	handle, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{Store: store})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := handle.Next(context.Background()); !errors.Is(err, enginepkg.ErrCheckpointCommit) {
		t.Fatalf("Next error = %v, want ErrCheckpointCommit", err)
	}
	state := handle.State()
	for _, dispatch := range state.Dispatches {
		if dispatch.Status == enginepkg.DispatchStatusPrepared {
			t.Fatalf("unencodable result left prepared dispatch: %#v", dispatch)
		}
	}
}

func TestEngineCompensationCommitsNestedDispatchesIndependently(t *testing.T) {
	store := &capturingCheckpointStore{}
	config := makeTestConfig()
	traceWriter := &fakeTraceWriter{}
	config.TraceWriter = traceWriter
	registry := newFakeExecutorRegistry()
	registry.Register("compensate", internalexecutor.NewCompensateExecutor())
	registry.Register("fail", stepExecutorFunc(func(_ context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
		return &enginepkg.StepResult{
			StepID: step.ID, Status: enginepkg.StepStatusFailed, Outcome: enginepkg.StepOutcomeFailed,
			Error: errors.New("trigger compensation"), Vars: map[string]any{},
		}, nil
	}))
	var compensated []string
	registry.Register("external", stepExecutorFunc(func(ctx context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
		if _, err := enginepkg.PrepareExternalDispatch(ctx, enginepkg.DispatchRequest{
			Classification: "mutating", EndpointIdentity: "compensation-provider",
			RenderedRequest: map[string]any{"step": step.ID},
		}); err != nil {
			return nil, err
		}
		compensated = append(compensated, step.ID)
		return &enginepkg.StepResult{
			StepID: step.ID, Status: enginepkg.StepStatusCompleted, Outcome: enginepkg.StepOutcomeSuccess,
			Vars: map[string]any{},
		}, nil
	}))
	config.Executors = registry
	plan := enginepkg.ValidatedForTest(&enginepkg.ExecutionPlan{
		RunID: "run-compensation-frame", RunbookPath: "compensation.runbook.yaml",
		Steps: []enginepkg.ResolvedStep{
			{ID: "register", Kind: "compensate", Spec: &schema.CompensateSpec{Compensate: schema.CompensateConfig{
				On: "failure", Steps: []schema.FlowNode{
					{Step: &schema.Step{ID: "undo-1", Type: "external"}},
					{Step: &schema.Step{ID: "undo-2", Type: "external"}},
				},
			}}},
			{ID: "fail", Kind: "fail", Spec: &cliStepSpec{}},
		},
		Metadata: enginepkg.PlanMetadata{RunbookID: "compensation-frame"},
	})
	handle, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{Store: store})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := handle.Next(context.Background()); err != nil {
		t.Fatalf("register compensation: %v", err)
	}
	if _, err := handle.Next(context.Background()); err != nil {
		t.Fatalf("trigger compensation: %v", err)
	}
	if len(compensated) != 2 || compensated[0] != "undo-1" || compensated[1] != "undo-2" {
		t.Fatalf("compensated steps = %#v", compensated)
	}
	state := handle.State()
	if len(state.ExecutionFrames) != 1 || len(state.Dispatches) != 2 {
		t.Fatalf("compensation frames=%#v dispatches=%#v", state.ExecutionFrames, state.Dispatches)
	}
	for _, frame := range state.ExecutionFrames {
		if frame.Kind != "compensate" || frame.Status != enginepkg.ExecutionFrameStatusCompleted || frame.NextStepIndex != 2 {
			t.Fatalf("compensation frame = %#v", frame)
		}
	}
	completed := make(map[string]bool)
	for _, event := range traceWriter.collect() {
		if event.Kind != tracepkg.EventKindStepCompleted {
			continue
		}
		var payload struct {
			StepID          string `json:"step_id"`
			QualifiedNodeID string `json:"qualified_node_id"`
			Invocation      int    `json:"invocation"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("decode compensation completion: %v", err)
		}
		if payload.StepID == "undo-1" || payload.StepID == "undo-2" {
			if payload.QualifiedNodeID != "register/"+payload.StepID || payload.Invocation != 1 {
				t.Fatalf("compensation completion = %#v", payload)
			}
			completed[payload.StepID] = true
		}
	}
	if !completed["undo-1"] || !completed["undo-2"] {
		t.Fatalf("compensation completion events = %#v", completed)
	}
}

func TestEngineResumeCompensationSkipsFailedRootAndCommittedUndo(t *testing.T) {
	base := t.TempDir()
	const runID = "run-resume-compensation"
	plan := persistentCompensationPlanForTest(t, runID)
	firstStore := runstore.NewDirRunStore(base)
	firstExecutor := &compensationTakeoverExecutor{
		blockUndoTwo: true, undoStarted: make(chan struct{}), releaseUndo: make(chan struct{}),
	}
	var releaseFirst sync.Once
	t.Cleanup(func() {
		releaseFirst.Do(func() { close(firstExecutor.releaseUndo) })
		_ = firstStore.Close()
	})
	firstConfig := makeTestConfig()
	firstRegistry := newFakeExecutorRegistry()
	firstRegistry.Register("compensate", internalexecutor.NewCompensateExecutor())
	firstRegistry.Register("cli", firstExecutor)
	firstConfig.Executors = firstRegistry
	firstHandle, err := New(firstConfig).Start(context.Background(), plan, enginepkg.RunOptions{Store: firstStore})
	if err != nil {
		t.Fatalf("Start first: %v", err)
	}
	if _, err := firstHandle.Next(context.Background()); err != nil {
		t.Fatalf("register compensation: %v", err)
	}
	firstDone := make(chan error, 1)
	go func() {
		_, nextErr := firstHandle.Next(context.Background())
		firstDone <- nextErr
	}()
	select {
	case <-firstExecutor.undoStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first process did not reach undo-2")
	}
	if err := firstStore.Close(); err != nil {
		t.Fatalf("Close first process: %v", err)
	}

	secondStore := runstore.NewDirRunStore(base)
	t.Cleanup(func() { _ = secondStore.Close() })
	secondExecutor := &compensationTakeoverExecutor{}
	secondConfig := makeTestConfig()
	secondRegistry := newFakeExecutorRegistry()
	secondRegistry.Register("compensate", internalexecutor.NewCompensateExecutor())
	secondRegistry.Register("cli", secondExecutor)
	secondConfig.Executors = secondRegistry
	secondHandle, err := New(secondConfig).Resume(context.Background(), runID, enginepkg.RunOptions{Store: secondStore})
	if err != nil {
		t.Fatalf("Resume second: %v", err)
	}
	t.Cleanup(func() { _ = secondHandle.Cancel(context.Background(), "test cleanup") })
	result, err := secondHandle.Next(context.Background())
	if err != nil {
		t.Fatalf("resumed compensation Next: %v", err)
	}
	if result.StepID != "fail" || result.Status != enginepkg.StepStatusFailed {
		t.Fatalf("resumed root result = %#v", result)
	}
	if got := secondExecutor.steps(); len(got) != 1 || got[0] != "undo-2" {
		t.Fatalf("resumed compensation executed = %#v, want only undo-2", got)
	}

	releaseFirst.Do(func() { close(firstExecutor.releaseUndo) })
	select {
	case staleErr := <-firstDone:
		if !errors.Is(staleErr, enginepkg.ErrCheckpointCommit) && !errors.Is(staleErr, enginepkg.ErrRunLeaseStale) {
			t.Fatalf("stale compensation error = %v", staleErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stale compensation process did not stop")
	}
	if got := firstExecutor.steps(); len(got) != 2 || got[0] != "fail" || got[1] != "undo-1" {
		t.Fatalf("stale compensation dispatched after takeover: %#v", got)
	}
	_ = firstStore.Close()
	_ = secondHandle.Cancel(context.Background(), "test cleanup")
	_ = secondStore.Close()
}

func persistentCompensationPlanForTest(t *testing.T, runID string) *enginepkg.ExecutionPlan {
	t.Helper()
	plan := &enginepkg.ExecutionPlan{
		RunID: runID, RunbookPath: "compensation.runbook.yaml",
		Steps: []enginepkg.ResolvedStep{
			{ID: "register", Kind: "compensate", Spec: &schema.CompensateSpec{Compensate: schema.CompensateConfig{
				On: "failure", Steps: []schema.FlowNode{
					{Step: &schema.Step{ID: "undo-1", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "undo-1"}}},
					{Step: &schema.Step{ID: "undo-2", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "undo-2"}}},
				},
			}}},
			{ID: "fail", Kind: "cli", Spec: &schema.CLISpec{Command: "fail"}},
		},
		Metadata: enginepkg.PlanMetadata{RunbookID: "compensation-resume"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	return plan
}

func TestEngineResumeParallelFramesExecutesOnlyUnfinishedChildren(t *testing.T) {
	base := t.TempDir()
	const runID = "run-resume-parallel-frames"
	plan := persistentParallelPlanForTest(t, runID)
	firstStore := runstore.NewDirRunStore(base)
	firstExecutor := &gatedParallelDispatchExecutor{
		blockedStep: "a-2", started: make(chan struct{}), release: make(chan struct{}),
	}
	var releaseFirst sync.Once
	t.Cleanup(func() {
		releaseFirst.Do(func() { close(firstExecutor.release) })
		_ = firstStore.Close()
	})
	firstConfig := makeTestConfig()
	firstRegistry := newFakeExecutorRegistry()
	firstRegistry.Register("cli", firstExecutor)
	firstConfig.Executors = firstRegistry
	firstHandle, err := New(firstConfig).Start(context.Background(), plan, enginepkg.RunOptions{Store: firstStore})
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
		t.Fatal("first process did not reach a-2")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		state, loadErr := firstStore.LoadState(context.Background(), runID)
		if loadErr == nil && len(state.ExecutionFrames) == 2 {
			ready := true
			for _, frame := range state.ExecutionFrames {
				if frame.NextStepIndex != 1 {
					ready = false
				}
			}
			if ready {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("parallel frames did not commit their first children")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := firstStore.Close(); err != nil {
		t.Fatalf("Close first process store: %v", err)
	}

	secondStore := runstore.NewDirRunStore(base)
	t.Cleanup(func() { _ = secondStore.Close() })
	secondExecutor := &gatedParallelDispatchExecutor{}
	secondConfig := makeTestConfig()
	secondRegistry := newFakeExecutorRegistry()
	secondRegistry.Register("cli", secondExecutor)
	secondConfig.Executors = secondRegistry
	secondHandle, err := New(secondConfig).Resume(context.Background(), runID, enginepkg.RunOptions{Store: secondStore})
	if err != nil {
		t.Fatalf("Resume second: %v", err)
	}
	t.Cleanup(func() { _ = secondHandle.Cancel(context.Background(), "test cleanup") })
	result, err := secondHandle.Next(context.Background())
	if err != nil {
		t.Fatalf("resumed Next: %v", err)
	}
	if result.Status != enginepkg.StepStatusCompleted {
		t.Fatalf("resumed parallel result status=%s error=%v", result.Status, result.Error)
	}
	if got := secondExecutor.executedSteps(); len(got) != 1 || got[0] != "a-2" {
		t.Fatalf("resumed parallel executed = %#v, want only a-2", got)
	}

	releaseFirst.Do(func() { close(firstExecutor.release) })
	select {
	case staleErr := <-firstDone:
		if !errors.Is(staleErr, enginepkg.ErrCheckpointCommit) && !errors.Is(staleErr, enginepkg.ErrRunLeaseStale) {
			t.Fatalf("stale process error = %v", staleErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stale process did not stop")
	}
	if got := firstExecutor.executedSteps(); len(got) != 2 {
		t.Fatalf("stale process dispatched after takeover: %#v", got)
	}
	_ = firstStore.Close()
	_ = secondHandle.Cancel(context.Background(), "test cleanup")
	_ = secondStore.Close()
}

func TestEngineParallelStateExposesAllActiveLeafCursors(t *testing.T) {
	store := &capturingCheckpointStore{}
	executorImpl := &barrierParallelExecutor{started: make(chan string, 2), release: make(chan struct{})}
	config := makeTestConfig()
	registry := newFakeExecutorRegistry()
	registry.Register("cli", executorImpl)
	config.Executors = registry
	parallel := &schema.ParallelNode{
		ID: "parallel",
		Branches: []schema.ParallelBranch{
			{Label: "a", Steps: []schema.FlowNode{{Step: &schema.Step{ID: "a-1", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "a"}}}}},
			{Label: "b", Steps: []schema.FlowNode{{Step: &schema.Step{ID: "b-1", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "b"}}}}},
		},
	}
	plan := enginepkg.ValidatedForTest(&enginepkg.ExecutionPlan{
		RunID: "run-parallel-cursors", RunbookPath: "parallel.runbook.yaml",
		Steps: []enginepkg.ResolvedStep{
			{ID: "parallel", Kind: "parallel", Spec: parallel},
			{ID: "a-1", Kind: "cli", Spec: &schema.CLISpec{Command: "a"}, Depth: 1, ParentID: "parallel", ParentKind: "parallel"},
			{ID: "b-1", Kind: "cli", Spec: &schema.CLISpec{Command: "b"}, Depth: 1, ParentID: "parallel", ParentKind: "parallel"},
		},
		Metadata: enginepkg.PlanMetadata{RunbookID: "parallel-cursors"},
	})
	handle, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{Store: store})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, nextErr := handle.Next(context.Background())
		done <- nextErr
	}()
	for range 2 {
		select {
		case <-executorImpl.started:
		case <-time.After(time.Second):
			close(executorImpl.release)
			t.Fatal("parallel children did not reach barrier")
		}
	}
	cursors := handle.State().CursorSet.Cursors
	if len(cursors) != 2 {
		close(executorImpl.release)
		t.Fatalf("active cursor count = %d, want 2: %#v", len(cursors), cursors)
	}
	want := []string{"parallel/a-1", "parallel/b-1"}
	for index, cursor := range cursors {
		if cursor.QualifiedNodeID != want[index] || cursor.StepID == "parallel" ||
			len(cursor.CallPath) != 1 || cursor.CallPath[0].StepID != "parallel" {
			close(executorImpl.release)
			t.Fatalf("cursor %d = %#v, want %s leaf", index, cursor, want[index])
		}
	}
	close(executorImpl.release)
	if err := <-done; err != nil {
		t.Fatalf("parallel Next: %v", err)
	}
}

func persistentParallelPlanForTest(t *testing.T, runID string) *enginepkg.ExecutionPlan {
	t.Helper()
	parallel := &schema.ParallelNode{
		ID: "parallel",
		Branches: []schema.ParallelBranch{
			{Label: "a", Steps: []schema.FlowNode{
				{Step: &schema.Step{ID: "a-1", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "a-1"}}},
				{Step: &schema.Step{ID: "a-2", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "a-2"}}},
			}},
			{Label: "b", Steps: []schema.FlowNode{
				{Step: &schema.Step{ID: "b-1", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "b-1"}}},
			}},
		},
	}
	plan := &enginepkg.ExecutionPlan{
		RunID: runID, RunbookPath: "parallel.runbook.yaml",
		Steps: []enginepkg.ResolvedStep{
			{ID: "parallel", Kind: "parallel", Spec: parallel},
			{ID: "a-1", Kind: "cli", Spec: &schema.CLISpec{Command: "a-1"}, Depth: 1, ParentID: "parallel", ParentKind: "parallel"},
			{ID: "a-2", Kind: "cli", Spec: &schema.CLISpec{Command: "a-2"}, Depth: 1, ParentID: "parallel", ParentKind: "parallel"},
			{ID: "b-1", Kind: "cli", Spec: &schema.CLISpec{Command: "b-1"}, Depth: 1, ParentID: "parallel", ParentKind: "parallel"},
		},
		Metadata: enginepkg.PlanMetadata{RunbookID: "parallel-frame"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	return plan
}

func TestInteractionInvocationTrackerForResumePreservesActiveOrdinal(t *testing.T) {
	persisted := enginepkg.NewInteractionInvocationTracker()
	persisted.Next("choose", "choice")
	activeOrdinal := persisted.Next("choose", "choice")
	state := enginepkg.RunState{
		InteractionInvocationCounts: persisted.Snapshot(),
		Interactions: map[string]*enginepkg.InteractionState{
			"turn-two": {NodeID: "choose", Kind: "choice", Ordinal: activeOrdinal},
		},
	}

	resumed := interactionInvocationTrackerForResume(context.Background(), state)
	for _, count := range resumed.Snapshot() {
		if count != activeOrdinal {
			t.Fatalf("durable count during active ordinal reuse = %d, want %d", count, activeOrdinal)
		}
	}
	if got := resumed.Next("choose", "choice"); got != activeOrdinal {
		t.Fatalf("active invocation ordinal = %d, want %d", got, activeOrdinal)
	}
	if got := resumed.Next("choose", "choice"); got != activeOrdinal+1 {
		t.Fatalf("next invocation ordinal = %d, want %d", got, activeOrdinal+1)
	}
}

func TestInteractionInvocationTrackerForResumeScopesConcurrentFrames(t *testing.T) {
	firstFrame := enginepkg.InteractionPayloadDigest([]byte("first interaction frame"))
	secondFrame := enginepkg.InteractionPayloadDigest([]byte("second interaction frame"))
	persisted := enginepkg.NewInteractionInvocationTracker()
	if got := persisted.NextOccurrence("iterate/choose", "choice", firstFrame, 0); got != 1 {
		t.Fatalf("first frame ordinal = %d, want 1", got)
	}
	if got := persisted.NextOccurrence("iterate/choose", "choice", secondFrame, 0); got != 1 {
		t.Fatalf("second frame ordinal = %d, want 1", got)
	}
	state := enginepkg.RunState{
		InteractionInvocationCounts: persisted.Snapshot(),
		Interactions: map[string]*enginepkg.InteractionState{
			"turn-first": {
				NodeID: "iterate/choose", Kind: "choice", FrameID: firstFrame, FrameStepIndex: 0, Ordinal: 1,
			},
			"turn-second": {
				NodeID: "iterate/choose", Kind: "choice", FrameID: secondFrame, FrameStepIndex: 0, Ordinal: 1,
			},
		},
	}
	resumed := interactionInvocationTrackerForResume(context.Background(), state)
	if got := resumed.NextOccurrence("iterate/choose", "choice", secondFrame, 0); got != 1 {
		t.Fatalf("resumed second frame ordinal = %d, want 1", got)
	}
	if got := resumed.NextOccurrence("iterate/choose", "choice", firstFrame, 0); got != 1 {
		t.Fatalf("resumed first frame ordinal = %d, want 1", got)
	}
	for key, count := range resumed.Snapshot() {
		if count != 1 {
			t.Fatalf("resumed scoped count %q = %d, want 1", key, count)
		}
	}
}

func TestEngineTreatsInteractionCheckpointErrorsAsFatal(t *testing.T) {
	tests := []struct {
		name     string
		step     enginepkg.ResolvedStep
		executor enginepkg.StepExecutor
	}{
		{
			name: "choice",
			step: enginepkg.ResolvedStep{
				ID: "interact", Kind: "choice", OnError: "continue",
				Spec: &schema.ChoiceSpec{
					Prompt: "Choose", Variable: "choice",
					Options: []schema.ChoiceOption{{Label: "One", Value: "one"}},
				},
			},
			executor: internalexecutor.NewChoiceExecutor(checkpointErrorPromptProvider{}, nil),
		},
		{
			name: "approval",
			step: enginepkg.ResolvedStep{
				ID: "interact", Kind: "approve", OnError: "continue", Spec: &schema.ApproveSpec{},
			},
			executor: internalexecutor.NewApproveExecutor(checkpointErrorApprovalGate{}),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &capturingCheckpointStore{}
			config := makeTestConfig()
			registry := newFakeExecutorRegistry()
			registry.Register(test.step.Kind, test.executor)
			following := &recordingExecutor{}
			registry.Register("after", following)
			config.Executors = registry
			plan := enginepkg.ValidatedForTest(&enginepkg.ExecutionPlan{
				RunID: "run-fatal-interaction-" + test.name, RunbookPath: "interaction.runbook.yaml",
				Steps: []enginepkg.ResolvedStep{
					test.step,
					{ID: "after", Kind: "after", Spec: &cliStepSpec{}},
				},
				Metadata: enginepkg.PlanMetadata{RunbookID: "fatal-interaction"},
			})
			handle, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{Store: store})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if _, err := handle.Next(context.Background()); !errors.Is(err, enginepkg.ErrCheckpointCommit) {
				t.Fatalf("Next error = %v, want ErrCheckpointCommit", err)
			}
			if following.invoked {
				t.Fatal("following action executed after interaction checkpoint failure")
			}
			if len(store.states) != 1 || store.states[0].Status != enginepkg.RunStatusIndeterminate {
				t.Fatalf("durability marker = %#v, want one indeterminate checkpoint", store.states)
			}
			if _, err := handle.Next(context.Background()); !errors.Is(err, io.EOF) {
				t.Fatalf("second Next error = %v, want EOF", err)
			}
			if following.invoked {
				t.Fatal("following action executed after terminal durability failure")
			}
		})
	}
}

func TestEngineDoesNotPublishCompositeSuccessBeforeCheckpointCommit(t *testing.T) {
	tests := []struct {
		name          string
		step          enginepkg.ResolvedStep
		prepare       func(*enginepkg.EngineConfig)
		forbiddenKind tracepkg.EventKind
	}{
		{
			name: "parallel completion",
			step: enginepkg.ResolvedStep{ID: "composite", Kind: "parallel", Spec: &testParallelSpec{branches: []enginepkg.BranchSpec{{
				Label: "branch", Steps: []enginepkg.ResolvedStep{{ID: "child", Kind: "noop", Spec: &schema.NoopSpec{}}},
			}}}},
			prepare:       func(config *enginepkg.EngineConfig) { config.Executors.Register("noop", &passThroughExecutor{}) },
			forbiddenKind: tracepkg.EventKindStepCompleted,
		},
		{
			name: "wait event receipt",
			step: enginepkg.ResolvedStep{ID: "composite", Kind: "wait_for_event", Spec: &testWaitEventSpec{
				filter: eventbus.EventFilter{Source: "signal", ID: "ready"},
			}},
			prepare: func(config *enginepkg.EngineConfig) {
				go func() {
					time.Sleep(20 * time.Millisecond)
					_ = config.Dispatcher.Dispatch(eventbus.InboundEvent{EventID: "ready", Source: "signal"})
				}()
			},
			forbiddenKind: tracepkg.EventKindEventReceived,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			traceWriter := &fakeTraceWriter{}
			config := makeTestConfig()
			config.Executors = newFakeExecutorRegistry()
			config.TraceWriter = traceWriter
			test.prepare(&config)
			plan := enginepkg.ValidatedForTest(&enginepkg.ExecutionPlan{
				RunID:       "run-publish-" + strings.ReplaceAll(test.name, " ", "-"),
				RunbookPath: "publish.runbook.yaml", Steps: []enginepkg.ResolvedStep{test.step},
				Metadata: enginepkg.PlanMetadata{RunbookID: "publish"},
			})
			handle, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{Store: &failingCheckpointStore{}})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if _, err := handle.Next(context.Background()); !errors.Is(err, enginepkg.ErrCheckpointCommit) {
				t.Fatalf("Next error = %v, want ErrCheckpointCommit", err)
			}
			for _, event := range traceWriter.collect() {
				var payload struct {
					StepID string `json:"step_id"`
				}
				_ = json.Unmarshal(event.Payload, &payload)
				if event.Kind == test.forbiddenKind && payload.StepID == "composite" {
					t.Fatalf("published %s before checkpoint commit", event.Kind)
				}
			}
		})
	}
}

func TestEngineCheckpointsCompositeErrorRouting(t *testing.T) {
	tests := []struct {
		name       string
		step       enginepkg.ResolvedStep
		prepare    func(*enginepkg.EngineConfig)
		wantTarget string
		wantIndex  int
	}{
		{
			name: "parallel goto",
			step: enginepkg.ResolvedStep{
				ID: "composite", Kind: "parallel", OnError: "goto:handler",
				Spec: &testParallelSpec{branches: []enginepkg.BranchSpec{{
					Label: "branch", Steps: []enginepkg.ResolvedStep{{ID: "child", Kind: "failing", Spec: &cliStepSpec{}}},
				}}},
			},
			prepare: func(config *enginepkg.EngineConfig) {
				config.Executors.Register("failing", &failingExecutor{err: errors.New("branch failed")})
			},
			wantTarget: "handler", wantIndex: 2,
		},
		{
			name: "wait timeout continue",
			step: enginepkg.ResolvedStep{
				ID: "composite", Kind: "wait_for_event", OnError: "continue",
				Spec: &testWaitEventSpec{filter: eventbus.EventFilter{Source: "signal", ID: "never"}, timeout: time.Millisecond},
			},
			prepare: func(config *enginepkg.EngineConfig) {
				config.Dispatcher = internalEventBus.NewDispatcher()
			},
			wantTarget: "skipped", wantIndex: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &capturingCheckpointStore{}
			config := makeTestConfig()
			config.Executors = newFakeExecutorRegistry()
			test.prepare(&config)
			plan := enginepkg.ValidatedForTest(&enginepkg.ExecutionPlan{
				RunID:       "run-composite-routing-" + strings.ReplaceAll(test.name, " ", "-"),
				RunbookPath: "composite-routing.runbook.yaml",
				Steps: []enginepkg.ResolvedStep{
					test.step,
					{ID: "skipped", Kind: "noop", Spec: &schema.NoopSpec{}},
					{ID: "handler", Kind: "noop", Spec: &schema.NoopSpec{}},
				},
				Metadata: enginepkg.PlanMetadata{RunbookID: "composite-routing"},
			})
			handle, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{Store: store})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if _, err := handle.Next(context.Background()); err != nil {
				t.Fatalf("Next: %v", err)
			}
			if len(store.states) == 0 {
				t.Fatal("composite produced no checkpoint")
			}
			state := store.states[len(store.states)-1]
			if state.CheckpointSequence != int64(len(store.states)) {
				t.Fatalf("final checkpoint sequence = %d, want %d", state.CheckpointSequence, len(store.states))
			}
			cursor := state.CursorSet.Cursors[0]
			if cursor.StepID != test.wantTarget || cursor.StepIndex != test.wantIndex {
				t.Fatalf("cursor = %#v, want %s at %d", cursor, test.wantTarget, test.wantIndex)
			}
			if state.StepResults["composite"] == nil || state.Vars["__error_step_id"] != "composite" {
				t.Fatalf("settled composite state = %#v", state)
			}
		})
	}
}

func TestEngineCheckpointFailurePersistsIndeterminateResumeBoundary(t *testing.T) {
	base := t.TempDir()
	const runID = "run-transient-checkpoint"
	store := &failOnceCheckpointStore{DirRunStore: runstore.NewDirRunStore(base)}
	handle, err := newPersistentPlanTestEngine().Start(
		context.Background(), persistentPlanForTest(t, runID),
		enginepkg.RunOptions{Mode: enginepkg.RunModeReal, Store: store},
	)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := handle.Next(context.Background()); !errors.Is(err, enginepkg.ErrCheckpointCommit) {
		t.Fatalf("Next error = %v, want ErrCheckpointCommit", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	fresh := runstore.NewDirRunStore(base)
	state, err := fresh.LoadState(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if state.Status != enginepkg.RunStatusIndeterminate || state.StepResults["first"] == nil {
		t.Fatalf("durable blocked state = %#v", state)
	}
	if cursor := state.CursorSet.Cursors[0]; cursor.StepID != "second" || cursor.StepIndex != 1 {
		t.Fatalf("durable blocked cursor = %#v, want second", cursor)
	}
	if _, err := newPersistentPlanTestEngine().Resume(context.Background(), runID, enginepkg.RunOptions{
		Mode: enginepkg.RunModeReal, Store: fresh,
	}); !errors.Is(err, enginepkg.ErrIndeterminateAcknowledgmentRequired) {
		t.Fatalf("Resume without acknowledgment error = %v", err)
	}
	resumed, err := newPersistentPlanTestEngine().Resume(context.Background(), runID, enginepkg.RunOptions{
		Mode: enginepkg.RunModeReal, Store: fresh, AcknowledgeIndeterminate: true,
	})
	if err != nil {
		t.Fatalf("Resume acknowledged: %v", err)
	}
	result, err := resumed.Next(context.Background())
	if err != nil {
		t.Fatalf("resumed Next: %v", err)
	}
	if result.StepID != "second" {
		t.Fatalf("resumed step = %q, want second", result.StepID)
	}
	if err := fresh.Close(); err != nil {
		t.Fatalf("Close fresh: %v", err)
	}
}

func TestEngineCheckpointsTerminalRunState(t *testing.T) {
	store := &capturingCheckpointStore{}
	config := makeTestConfig()
	registry := newFakeExecutorRegistry()
	registry.Register("terminal", stepExecutorFunc(func(_ context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
		return &enginepkg.StepResult{
			StepID: step.ID, Status: enginepkg.StepStatusCompleted, Outcome: enginepkg.StepOutcomeSuccess,
			Output: map[string]any{"terminal": true},
		}, nil
	}))
	config.Executors = registry
	plan := enginepkg.ValidatedForTest(&enginepkg.ExecutionPlan{
		RunID: "run-terminal-checkpoint", RunbookPath: "terminal.runbook.yaml",
		Steps:    []enginepkg.ResolvedStep{{ID: "end", Kind: "terminal", Spec: &cliStepSpec{}}},
		Metadata: enginepkg.PlanMetadata{RunbookID: "terminal"},
	})
	handle, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{Store: store})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := handle.Next(context.Background()); err != nil {
		t.Fatalf("Next: %v", err)
	}
	if len(store.states) != 1 || store.states[0].Status != enginepkg.RunStatusCompleted {
		t.Fatalf("terminal checkpoint = %#v, want completed", store.states)
	}
}

func TestEngineCheckpointsOrdinaryEndOfPlan(t *testing.T) {
	store := &capturingCheckpointStore{}
	config := makeTestConfig()
	registry := newFakeExecutorRegistry()
	registry.Register("noop", &passThroughExecutor{})
	config.Executors = registry
	plan := enginepkg.ValidatedForTest(&enginepkg.ExecutionPlan{
		RunID: "run-ordinary-completion", RunbookPath: "ordinary.runbook.yaml",
		Steps:    []enginepkg.ResolvedStep{{ID: "work", Kind: "noop", Spec: &schema.NoopSpec{}}},
		Metadata: enginepkg.PlanMetadata{RunbookID: "ordinary"},
	})
	handle, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{Store: store})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := handle.Next(context.Background()); err != nil {
		t.Fatalf("execute work: %v", err)
	}
	if _, err := handle.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("complete run: %v", err)
	}
	if len(store.states) != 2 {
		t.Fatalf("checkpoint count = %d, want step + run completion", len(store.states))
	}
	completed := store.states[1]
	if completed.Status != enginepkg.RunStatusCompleted || completed.CheckpointSequence != 2 {
		t.Fatalf("completion checkpoint = %#v", completed)
	}
	cursor := completed.CursorSet.Cursors[0]
	if !cursor.AtEnd || cursor.StepIndex != len(plan.Steps) {
		t.Fatalf("completion cursor = %#v, want exact end boundary", cursor)
	}
}

func TestEngineCheckpointsCancellationBeforeReturning(t *testing.T) {
	store := &capturingCheckpointStore{}
	plan := enginepkg.ValidatedForTest(&enginepkg.ExecutionPlan{
		RunID: "run-cancel-checkpoint", RunbookPath: "cancel.runbook.yaml",
		Steps:    []enginepkg.ResolvedStep{{ID: "work", Kind: "noop", Spec: &schema.NoopSpec{}}},
		Metadata: enginepkg.PlanMetadata{RunbookID: "cancel"},
	})
	handle, err := New(makeTestConfig()).Start(context.Background(), plan, enginepkg.RunOptions{Store: store})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := handle.Cancel(context.Background(), "operator requested"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if len(store.states) != 1 || store.states[0].Status != enginepkg.RunStatusCancelled ||
		store.states[0].CompletedAt.IsZero() {
		t.Fatalf("cancel checkpoint = %#v", store.states)
	}
}

func TestEngineCancellationWithPreparedDispatchBecomesIndeterminate(t *testing.T) {
	store := &capturingCheckpointStore{}
	executorImpl := &cancellationBlockedDispatchExecutor{prepared: make(chan struct{}), release: make(chan struct{})}
	config := makeTestConfig()
	registry := newFakeExecutorRegistry()
	registry.Register("external", executorImpl)
	config.Executors = registry
	plan := enginepkg.ValidatedForTest(&enginepkg.ExecutionPlan{
		RunID: "run-cancel-dispatch", RunbookPath: "cancel-dispatch.runbook.yaml",
		Steps:    []enginepkg.ResolvedStep{{ID: "external", Kind: "external", Spec: &cliStepSpec{}}},
		Metadata: enginepkg.PlanMetadata{RunbookID: "cancel-dispatch"},
	})
	handle, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{Store: store})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	nextDone := make(chan error, 1)
	go func() {
		_, nextErr := handle.Next(context.Background())
		nextDone <- nextErr
	}()
	select {
	case <-executorImpl.prepared:
	case <-time.After(time.Second):
		t.Fatal("external dispatch was not prepared")
	}
	if err := handle.Cancel(context.Background(), "operator requested"); !errors.Is(err, enginepkg.ErrIndeterminate) {
		t.Fatalf("Cancel error = %v, want ErrIndeterminate", err)
	}
	state := store.states[len(store.states)-1]
	if state.Status != enginepkg.RunStatusIndeterminate || state.StepResults["external"] == nil {
		t.Fatalf("cancelled dispatch checkpoint = %#v", state)
	}
	for _, dispatch := range state.Dispatches {
		if dispatch.Status != enginepkg.DispatchStatusIndeterminate {
			t.Fatalf("cancelled dispatch = %#v", dispatch)
		}
	}
	close(executorImpl.release)
	select {
	case <-nextDone:
	case <-time.After(time.Second):
		t.Fatal("cancelled executor did not stop")
	}
}

func TestEngineAmbiguousErrorWithPreparedDispatchBecomesIndeterminate(t *testing.T) {
	store := &capturingCheckpointStore{}
	config := makeTestConfig()
	registry := newFakeExecutorRegistry()
	registry.Register("external", stepExecutorFunc(func(ctx context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
		if _, err := enginepkg.PrepareExternalDispatch(ctx, enginepkg.DispatchRequest{
			Classification: "unspecified", EndpointIdentity: "ambiguous-provider",
			RenderedRequest: map[string]any{"step": step.ID},
		}); err != nil {
			return nil, err
		}
		return nil, errors.New("provider acknowledgement was not received")
	}))
	config.Executors = registry
	plan := enginepkg.ValidatedForTest(&enginepkg.ExecutionPlan{
		RunID: "run-ambiguous-dispatch", RunbookPath: "ambiguous-dispatch.runbook.yaml",
		Steps:    []enginepkg.ResolvedStep{{ID: "external", Kind: "external", Spec: &cliStepSpec{}}},
		Metadata: enginepkg.PlanMetadata{RunbookID: "ambiguous-dispatch"},
	})
	handle, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{Store: store})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	result, err := handle.Next(context.Background())
	if !errors.Is(err, enginepkg.ErrIndeterminate) {
		t.Fatalf("Next error = %v, want ErrIndeterminate", err)
	}
	if result == nil || result.Status != enginepkg.StepStatusIndeterminate {
		t.Fatalf("result = %#v, want indeterminate", result)
	}
	state := store.states[len(store.states)-1]
	for _, dispatch := range state.Dispatches {
		if dispatch.EndpointIdentity == "ambiguous-provider" && dispatch.Status != enginepkg.DispatchStatusIndeterminate {
			t.Fatalf("dispatch = %#v, want indeterminate", dispatch)
		}
	}
}

func TestEngineCheckpointsSignalCancellationBeforeClosingEvents(t *testing.T) {
	store := &capturingCheckpointStore{}
	platformFake := platform.NewFakePlatform()
	platformFake.SignalCh = make(chan platform.Signal, 1)
	config := makeTestConfig()
	config.Platform = platformFake
	plan := enginepkg.ValidatedForTest(&enginepkg.ExecutionPlan{
		RunID: "run-signal-checkpoint", RunbookPath: "signal.runbook.yaml",
		Steps:    []enginepkg.ResolvedStep{{ID: "work", Kind: "noop", Spec: &schema.NoopSpec{}}},
		Metadata: enginepkg.PlanMetadata{RunbookID: "signal"},
	})
	handle, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{Store: store})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	platformFake.SignalCh <- platform.Signal{Name: "SIGTERM"}
	deadline := time.After(time.Second)
	for {
		select {
		case _, open := <-handle.Events():
			if open {
				continue
			}
			if len(store.states) != 1 || store.states[0].Status != enginepkg.RunStatusCancelled {
				t.Fatalf("signal checkpoint = %#v", store.states)
			}
			return
		case <-deadline:
			t.Fatal("signal cancellation did not close events")
		}
	}
}

func TestEngineCheckpointsDirectFailureBeforeReturning(t *testing.T) {
	store := &capturingCheckpointStore{}
	config := makeTestConfig()
	config.Executors.Register("noop", &passThroughExecutor{})
	plan := enginepkg.ValidatedForTest(&enginepkg.ExecutionPlan{
		RunID: "run-direct-failure", RunbookPath: "direct-failure.runbook.yaml",
		Steps:    []enginepkg.ResolvedStep{{ID: "work", Kind: "noop", Spec: &schema.NoopSpec{}}},
		Metadata: enginepkg.PlanMetadata{RunbookID: "direct-failure"},
	})
	directFailure := errors.New("route controller failed")
	handle, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{
		Store: store,
		RouteTest: routeControllerFunc(func(context.Context, enginepkg.DebugLocation, enginepkg.ResolvedStep) (enginepkg.RouteTestDecision, error) {
			return "", directFailure
		}),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := handle.Next(context.Background()); !errors.Is(err, directFailure) {
		t.Fatalf("Next error = %v, want direct failure", err)
	}
	if len(store.states) != 1 {
		t.Fatalf("checkpoint count = %d, want one terminal checkpoint", len(store.states))
	}
	failed := store.states[0]
	if failed.Status != enginepkg.RunStatusFailed || failed.StepResults["work"] == nil ||
		failed.StepResults["work"].Status != enginepkg.StepStatusFailed {
		t.Fatalf("failed checkpoint = %#v", failed)
	}
}

func TestEngineCheckpointsIndeterminateOccurrenceBeforeReturning(t *testing.T) {
	store := &capturingCheckpointStore{}
	config := makeTestConfig()
	registry := newFakeExecutorRegistry()
	registry.Register("tool", &timeoutExecErr{})
	config.Executors = registry
	plan := enginepkg.ValidatedForTest(makeToolPlan(
		"uncertain", "writer", "mutate", classificationPtr("mutating"), nil,
		"https://example.com", boolPtr(false),
	))
	handle, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{Store: store})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := handle.Next(context.Background()); !errors.Is(err, enginepkg.ErrIndeterminate) {
		t.Fatalf("Next error = %v, want ErrIndeterminate", err)
	}
	if len(store.states) != 1 {
		t.Fatalf("checkpoint count = %d, want one terminal checkpoint", len(store.states))
	}
	indeterminate := store.states[0]
	result := indeterminate.StepResults["uncertain"]
	if indeterminate.Status != enginepkg.RunStatusIndeterminate || result == nil ||
		result.Status != enginepkg.StepStatusIndeterminate || result.Indeterminate == nil {
		t.Fatalf("indeterminate checkpoint = %#v", indeterminate)
	}
}

func TestEngineCheckpointsMissingExecutorFailure(t *testing.T) {
	store := &capturingCheckpointStore{}
	config := makeTestConfig()
	config.Executors = newFakeExecutorRegistry()
	plan := enginepkg.ValidatedForTest(&enginepkg.ExecutionPlan{
		RunID: "run-missing-executor", RunbookPath: "missing-executor.runbook.yaml",
		Steps:    []enginepkg.ResolvedStep{{ID: "missing", Kind: "unknown", Spec: &cliStepSpec{}}},
		Metadata: enginepkg.PlanMetadata{RunbookID: "missing-executor"},
	})
	handle, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{Store: store})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	result, err := handle.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if result.Status != enginepkg.StepStatusFailed {
		t.Fatalf("result status = %s, want failed", result.Status)
	}
	if len(store.states) != 1 || store.states[0].Status != enginepkg.RunStatusFailed || store.states[0].StepResults["missing"] == nil {
		t.Fatalf("missing-executor checkpoint = %#v", store.states)
	}
}

func TestEngineCheckpointsGovernanceDeniedResult(t *testing.T) {
	store := &capturingCheckpointStore{}
	traceWriter := &fakeTraceWriter{}
	config := makeTestConfig()
	registry := newFakeExecutorRegistry()
	registry.Register("cli", &passThroughExecutor{})
	config.Executors = registry
	config.TraceWriter = traceWriter
	plan := enginepkg.ValidatedForTest(&enginepkg.ExecutionPlan{
		RunID: "run-governance-denied", RunbookPath: "governance.runbook.yaml",
		Steps: []enginepkg.ResolvedStep{
			{ID: "denied", Kind: "cli", Spec: &schema.CLISpec{Command: "blocked"}},
			{ID: "after", Kind: "cli", Spec: &schema.CLISpec{Command: "allowed"}},
		},
		GovernanceSource: &schema.GovernanceConfig{DenyCommands: []string{"blocked"}},
		Metadata:         enginepkg.PlanMetadata{RunbookID: "governance"},
	})
	handle, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{Store: store})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	result, err := handle.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if result.Status != enginepkg.StepStatusDenied {
		t.Fatalf("result status = %s, want denied", result.Status)
	}
	if len(store.states) != 1 || store.states[0].StepResults["denied"] == nil {
		t.Fatalf("denied checkpoint = %#v", store.states)
	}
	if cursor := store.states[0].CursorSet.Cursors[0]; cursor.StepID != "after" || cursor.StepIndex != 1 {
		t.Fatalf("denied cursor = %#v, want after", cursor)
	}
	foundFailure := false
	for _, event := range traceWriter.collect() {
		if event.Kind == tracepkg.EventKindStepFailed {
			foundFailure = true
		}
	}
	if !foundFailure {
		t.Fatal("denied result did not emit step/failed")
	}
}

func TestEngineCheckpointsSuccessfulCompositeSteps(t *testing.T) {
	tests := []struct {
		name    string
		step    enginepkg.ResolvedStep
		prepare func(*enginepkg.EngineConfig)
	}{
		{
			name: "parallel",
			step: enginepkg.ResolvedStep{
				ID: "composite", Kind: "parallel",
				Spec: &testParallelSpec{branches: []enginepkg.BranchSpec{{
					Label: "branch", Steps: []enginepkg.ResolvedStep{{ID: "branch-step", Kind: "noop", Spec: &schema.NoopSpec{}}},
				}}},
			},
			prepare: func(config *enginepkg.EngineConfig) {
				config.Executors.Register("noop", &passThroughExecutor{})
			},
		},
		{
			name: "wait for event",
			step: enginepkg.ResolvedStep{
				ID: "composite", Kind: "wait_for_event",
				Spec: &testWaitEventSpec{filter: eventbus.EventFilter{Source: "signal", ID: "ready"}},
			},
			prepare: func(config *enginepkg.EngineConfig) {
				go func() {
					time.Sleep(20 * time.Millisecond)
					_ = config.Dispatcher.Dispatch(eventbus.InboundEvent{EventID: "ready", Source: "signal"})
				}()
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &capturingCheckpointStore{}
			config := makeTestConfig()
			config.Executors = newFakeExecutorRegistry()
			test.prepare(&config)
			plan := enginepkg.ValidatedForTest(&enginepkg.ExecutionPlan{
				RunID:       "run-composite-" + strings.ReplaceAll(test.name, " ", "-"),
				RunbookPath: "composite.runbook.yaml",
				Steps: []enginepkg.ResolvedStep{
					test.step,
					{ID: "after", Kind: "noop", Spec: &schema.NoopSpec{}},
				},
				Metadata: enginepkg.PlanMetadata{RunbookID: "composite"},
			})
			handle, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{Store: store})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if _, err := handle.Next(context.Background()); err != nil {
				t.Fatalf("Next: %v", err)
			}
			if len(store.states) == 0 {
				t.Fatal("composite produced no checkpoint")
			}
			if test.name == "parallel" {
				foundJoinBarrier := false
				for _, checkpoint := range store.states {
					for _, frame := range checkpoint.ExecutionFrames {
						if frame == nil || frame.Status != enginepkg.ExecutionFrameStatusActive || frame.NextStepIndex != frame.StepCount {
							continue
						}
						foundJoinBarrier = true
						cursor := checkpoint.CursorSet.Cursors[0]
						if cursor.StepID != "composite" || cursor.StepIndex != 0 || cursor.Phase != enginepkg.ExecutionPhaseBefore {
							t.Fatalf("unsettled parent cursor = %#v, want composite/before", cursor)
						}
					}
				}
				if !foundJoinBarrier {
					t.Fatal("parallel produced no durable join barrier")
				}
			}
			state := store.states[len(store.states)-1]
			cursor := state.CursorSet.Cursors[0]
			if state.CheckpointSequence != int64(len(store.states)) {
				t.Fatalf("checkpoint sequence = %d, want %d", state.CheckpointSequence, len(store.states))
			}
			if cursor.StepID != "after" || cursor.StepIndex != 1 || cursor.Phase != enginepkg.ExecutionPhaseBefore {
				t.Fatalf("next cursor = %#v, want after/before", cursor)
			}
		})
	}
}

func TestEngineResumeRejectsUnpinnedLazyInclude(t *testing.T) {
	base := t.TempDir()
	const runID = "run-lazy-resume"
	plan := &enginepkg.ExecutionPlan{
		RunID: runID, RunbookPath: "lazy.runbook.yaml",
		Steps: []enginepkg.ResolvedStep{{
			ID: "lazy", Name: "Lazy", Kind: "include",
			Spec: &schema.IncludeSpec{
				Include:         schema.IncludeConfig{Runbook: "child.runbook.yaml", Expand: "lazy"},
				LazyRunbookPath: "C:/repo/child.runbook.yaml",
			},
		}},
		Metadata: enginepkg.PlanMetadata{RunbookID: "lazy", RunbookName: "Lazy"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	store := runstore.NewDirRunStore(base)
	if err := store.SavePlan(context.Background(), runID, plan); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	if _, err := newPersistentPlanTestEngine().Resume(context.Background(), runID, enginepkg.RunOptions{
		Mode: enginepkg.RunModeReal, Store: runstore.NewDirRunStore(base),
	}); !errors.Is(err, plansnapshot.ErrUnpinnedInclude) {
		t.Fatalf("Resume error = %v, want ErrUnpinnedInclude", err)
	}
}

func TestEngineResumeUsesCapturedLazyClosureAfterSourceDeletion(t *testing.T) {
	base := t.TempDir()
	childPath := base + "/child.runbook.yaml"
	if err := os.WriteFile(childPath, []byte("original-child"), 0o600); err != nil {
		t.Fatalf("write child: %v", err)
	}
	const runID = "run-lazy-content-binding"
	plan := &enginepkg.ExecutionPlan{
		RunID: runID, RunbookPath: "lazy-parent.runbook.yaml",
		Steps: []enginepkg.ResolvedStep{
			{ID: "before", Kind: "noop", Spec: &schema.NoopSpec{}},
			{ID: "lazy", Kind: "include", Spec: &schema.IncludeSpec{
				Include: schema.IncludeConfig{Runbook: "child.runbook.yaml", Expand: "lazy"}, LazyRunbookPath: childPath,
			}},
		},
		Metadata: enginepkg.PlanMetadata{RunbookID: "lazy-content-binding"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	executedChildren := make([]string, 0, 1)
	runner := func(_ context.Context, _ internalexecutor.SubStepParent, nodes []schema.FlowNode, _ map[string]any) ([]*enginepkg.StepResult, error) {
		for _, node := range nodes {
			if node.Step != nil {
				executedChildren = append(executedChildren, node.Step.ID)
			}
		}
		return nil, nil
	}
	newEngine := func(store enginepkg.DurableRunStore) enginepkg.Engine {
		config := makeTestConfig()
		registry := newFakeExecutorRegistry()
		registry.Register("noop", &passThroughExecutor{})
		registry.Register("include", internalexecutor.NewIncludeExecutor(nil, runner, fileBackedLazyLoader{}))
		config.Executors = registry
		config.Store = store
		return New(config)
	}
	firstStore := runstore.NewDirRunStore(base + "/runs")
	firstHandle, err := newEngine(firstStore).Start(context.Background(), plan, enginepkg.RunOptions{Store: firstStore})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result, err := firstHandle.Next(context.Background()); err != nil || result.StepID != "before" {
		t.Fatalf("first Next = %#v, %v", result, err)
	}
	if err := firstStore.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}
	if err := os.Remove(childPath); err != nil {
		t.Fatalf("remove child: %v", err)
	}
	resumeStore := runstore.NewDirRunStore(base + "/runs")
	t.Cleanup(func() { _ = resumeStore.Close() })
	resumed, err := newEngine(resumeStore).Resume(context.Background(), runID, enginepkg.RunOptions{Store: resumeStore})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	result, err := resumed.Next(context.Background())
	if err != nil || result.StepID != "lazy" || result.Status != enginepkg.StepStatusCompleted {
		t.Fatalf("captured lazy include result = %#v, %v", result, err)
	}
	if fmt.Sprint(executedChildren) != "[original-child]" {
		t.Fatalf("captured lazy child execution = %v, want original-child", executedChildren)
	}
}

func newPersistentPlanTestEngine() enginepkg.Engine {
	config := makeTestConfig()
	registry := newFakeExecutorRegistry()
	registry.Register("noop", &passThroughExecutor{})
	config.Executors = registry
	return New(config)
}

func persistentPlanForTest(t *testing.T, runID string) *enginepkg.ExecutionPlan {
	t.Helper()
	plan := &enginepkg.ExecutionPlan{
		RunID: runID, RunbookPath: "persisted.runbook.yaml",
		Steps: []enginepkg.ResolvedStep{
			{ID: "first", Name: "First", Kind: "noop", Spec: &schema.NoopSpec{}},
			{ID: "second", Name: "Second", Kind: "noop", Spec: &schema.NoopSpec{}},
		},
		Metadata: enginepkg.PlanMetadata{RunbookID: "persisted", RunbookName: "Persisted"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	return plan
}
