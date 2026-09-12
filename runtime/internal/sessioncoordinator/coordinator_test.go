package sessioncoordinator_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	internalengine "github.com/ormasoftchile/yawr/runtime/internal/engine"
	"github.com/ormasoftchile/yawr/runtime/internal/eventbus"
	"github.com/ormasoftchile/yawr/runtime/internal/executor"
	internalexpr "github.com/ormasoftchile/yawr/runtime/internal/expr"
	"github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/internal/runstore"
	"github.com/ormasoftchile/yawr/runtime/internal/sessioncoordinator"
	"github.com/ormasoftchile/yawr/runtime/internal/sessionstore"
	internaltrace "github.com/ormasoftchile/yawr/runtime/internal/trace"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/session"
	"github.com/ormasoftchile/yawr/runtime/pkg/testutil"
	"github.com/ormasoftchile/yawr/runtime/pkg/tool"
	"github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

type noopExecutor struct{}

func (noopExecutor) Execute(_ context.Context, step engine.ResolvedStep, _ map[string]any) (*engine.StepResult, error) {
	return &engine.StepResult{StepID: step.ID, Status: engine.StepStatusCompleted, Outcome: engine.StepOutcomeSuccess}, nil
}

type detachBlockingExecutor struct{ started chan struct{} }

func (executor *detachBlockingExecutor) Execute(ctx context.Context, step engine.ResolvedStep, _ map[string]any) (*engine.StepResult, error) {
	close(executor.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

type discardTraceWriter struct{}

func (discardTraceWriter) Append(trace.TraceEvent) error { return nil }
func (discardTraceWriter) Close() error                  { return nil }

func TestCoordinatorPersistsCompletedStepOccurrences(t *testing.T) {
	root := t.TempDir()
	sessions := sessionstore.NewDirStore(filepath.Join(root, "sessions"))
	runs := runstore.NewDirRunStore(filepath.Join(root, "runs"))
	t.Cleanup(func() {
		_ = runs.Close()
		_ = sessions.Close()
	})
	registry := executor.NewMapRegistry()
	registry.Register("noop", noopExecutor{})
	runtime := internalengine.New(engine.EngineConfig{
		Executors: registry, Dispatcher: eventbus.NewDispatcher(), TraceWriter: discardTraceWriter{},
		Platform: platform.NewFakePlatform(), Store: runs,
	})
	coordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
		Sessions: sessions, Runs: runs, Engine: runtime,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sessionID := uuid.NewString()
	plan := engine.ValidatedForTest(&engine.ExecutionPlan{
		RunID: uuid.NewString(), RunbookPath: "occurrences.runbook.yaml",
		Steps: []engine.ResolvedStep{
			{ID: "first", Kind: "noop", Spec: &schema.NoopSpec{}},
			{ID: "second", Kind: "noop", Spec: &schema.NoopSpec{}},
		},
		Metadata: engine.PlanMetadata{RunbookID: "occurrences", RunbookName: "Occurrences"},
	})
	handle, err := coordinator.Start(context.Background(), sessioncoordinator.StartRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), SegmentID: uuid.NewString(), RunID: plan.RunID,
		Plan: plan, Graph: targetGraphJSON(t, plan), RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	for _, stepID := range []string{"first", "second"} {
		result, nextErr := handle.Next(context.Background())
		if nextErr != nil || result == nil || result.StepID != stepID {
			t.Fatalf("Next %s = %#v, %v", stepID, result, nextErr)
		}
	}
	manifest, err := sessions.LoadManifest(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(manifest.Occurrences) != 2 {
		t.Fatalf("occurrences = %#v, want two completed steps", manifest.Occurrences)
	}
	qualified := make(map[string]bool)
	for occurrenceID, occurrence := range manifest.Occurrences {
		if occurrenceID == "" || occurrence.SessionID != sessionID || occurrence.RunID != plan.RunID ||
			occurrence.SegmentID == "" || occurrence.Status != "completed" || occurrence.Phase != "execute" ||
			occurrence.Invocation < 1 || occurrence.RetryAttempt < 1 || occurrence.OccurrenceSequence < 1 ||
			occurrence.EventSequence < 1 {
			t.Fatalf("invalid occurrence %q = %#v", occurrenceID, occurrence)
		}
		qualified[occurrence.QualifiedNodeID] = true
	}
	if !qualified["first"] || !qualified["second"] {
		t.Fatalf("qualified occurrence nodes = %#v", qualified)
	}
}

type failingConfiguredTraceWriter struct {
	mu     sync.Mutex
	fail   bool
	events []trace.TraceEvent
}

func (writer *failingConfiguredTraceWriter) Append(event trace.TraceEvent) error {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.fail {
		return errors.New("simulated configured trace projection failure")
	}
	writer.events = append(writer.events, event)
	return nil
}

func (*failingConfiguredTraceWriter) Close() error { return nil }

func (writer *failingConfiguredTraceWriter) collected() []trace.TraceEvent {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return append([]trace.TraceEvent(nil), writer.events...)
}

type failingRunTraceProjectionStore struct {
	*runstore.DirRunStore
	fail bool
}

func (store *failingRunTraceProjectionStore) WriteTrace(
	ctx context.Context,
	runID string,
	event engine.Event,
) error {
	if store.fail {
		return errors.New("simulated run trace projection failure")
	}
	return store.DirRunStore.WriteTrace(ctx, runID, event)
}

type recordingExecutor struct {
	mu    sync.Mutex
	steps []string
}

type failFirstStartEngine struct {
	inner  engine.Engine
	failed bool
}

type engineWithoutSessionValidation struct {
	inner engine.Engine
}

func (runtime *engineWithoutSessionValidation) Start(
	ctx context.Context,
	plan *engine.ExecutionPlan,
	options engine.RunOptions,
) (engine.RunHandle, error) {
	return runtime.inner.Start(ctx, plan, options)
}

func (runtime *engineWithoutSessionValidation) Resume(
	ctx context.Context,
	runID string,
	options engine.RunOptions,
) (engine.RunHandle, error) {
	return runtime.inner.Resume(ctx, runID, options)
}

func TestCoordinatorRequiresDurableHandoffValidationCapabilities(t *testing.T) {
	runs := runstore.NewDirRunStore(t.TempDir())
	t.Cleanup(func() { _ = runs.Close() })
	registry := executor.NewMapRegistry()
	registry.Register("noop", noopExecutor{})
	inner := internalengine.New(engine.EngineConfig{
		Executors: registry, Dispatcher: eventbus.NewDispatcher(), TraceWriter: discardTraceWriter{},
		Platform: platform.NewFakePlatform(), Store: runs,
	})
	sessions := sessionstore.NewDirStore(t.TempDir())
	t.Cleanup(func() { _ = sessions.Close() })
	_, err := sessioncoordinator.New(sessioncoordinator.Config{
		Sessions: sessions, Runs: runs, Engine: &engineWithoutSessionValidation{inner: inner},
	})
	if err == nil {
		t.Fatal("New accepted an engine without mandatory durable handoff validation capabilities")
	}
}

func TestCoordinatorDetachPausesBlockedRunAndReleasesSessionLease(t *testing.T) {
	root := t.TempDir()
	sessions := sessionstore.NewDirStore(filepath.Join(root, "sessions"))
	runs := runstore.NewDirRunStore(filepath.Join(root, "runs"))
	t.Cleanup(func() {
		_ = runs.Close()
		_ = sessions.Close()
	})
	blocking := &detachBlockingExecutor{started: make(chan struct{})}
	registry := executor.NewMapRegistry()
	registry.Register("cli", blocking)
	runtime := internalengine.New(engine.EngineConfig{
		Executors: registry, Dispatcher: eventbus.NewDispatcher(), TraceWriter: discardTraceWriter{},
		Platform: platform.NewFakePlatform(), Store: runs,
	})
	coordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
		Sessions: sessions, Runs: runs, Engine: runtime,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sessionID := uuid.NewString()
	plan := engine.ValidatedForTest(&engine.ExecutionPlan{
		RunID: uuid.NewString(), RunbookPath: "detach.runbook.yaml",
		Steps:    []engine.ResolvedStep{{ID: "work", Kind: "cli", Spec: &schema.CLISpec{Command: "blocked"}}},
		Metadata: engine.PlanMetadata{RunbookID: "detach", RunbookName: "Detach"},
	})
	handle, err := coordinator.Start(context.Background(), sessioncoordinator.StartRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), SegmentID: uuid.NewString(), RunID: plan.RunID,
		Plan: plan, Graph: targetGraphJSON(t, plan), RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	nextDone := make(chan error, 1)
	go func() {
		_, nextErr := handle.Next(context.Background())
		nextDone <- nextErr
	}()
	<-blocking.started
	detachCommandID := uuid.NewString()
	manifest, err := handle.Detach(context.Background(), detachCommandID, "transport lost")
	if err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if err := <-nextDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Next error = %v", err)
	}
	if manifest.Session.Status != session.StatusPaused ||
		manifest.Attempts[manifest.Session.ActiveRunID].Status != session.AttemptStatusPausedAtBoundary {
		t.Fatalf("detached manifest = %#v", manifest)
	}
	events, err := sessions.ReadEvents(context.Background(), sessionID, 0)
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	pauseEvents := 0
	for _, event := range events {
		if event.Kind == session.EventSessionPaused {
			pauseEvents++
			if event.CommandID != detachCommandID {
				t.Fatalf("pause command id = %s", event.CommandID)
			}
		}
	}
	if pauseEvents != 1 {
		t.Fatalf("pause event count = %d", pauseEvents)
	}
	lease, err := sessions.AcquireSessionLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("detached session lease remained held: %v", err)
	}
	_ = lease.Release()
}

type callCountingEngine struct {
	startCalls  int
	resumeCalls int
}

type staticHandoffResolver struct {
	target   sessioncoordinator.HandoffTarget
	requests []sessioncoordinator.HandoffResolveRequest
}

type fixedDynamicIncludeResolver struct {
	result *executor.DynamicIncludeResult
	calls  int
}

func (resolver *fixedDynamicIncludeResolver) Resolve(
	context.Context,
	string,
) (*executor.DynamicIncludeResult, error) {
	resolver.calls++
	return resolver.result, nil
}

func targetGraphJSON(t *testing.T, plan *engine.ExecutionPlan) json.RawMessage {
	t.Helper()
	encoded, err := sessioncoordinator.BuildExecutionPlanGraph(plan)
	if err != nil {
		t.Fatalf("encode target GraphJSON: %v", err)
	}
	return encoded
}

func (resolver *staticHandoffResolver) ResolveHandoff(
	_ context.Context,
	request sessioncoordinator.HandoffResolveRequest,
) (sessioncoordinator.HandoffTarget, error) {
	resolver.requests = append(resolver.requests, request)
	return resolver.target, nil
}

type projectionErrorStore struct {
	session.Store
	failCreate                bool
	failUpdate                bool
	failPause                 bool
	failResume                bool
	failMutationBeforeCommit  bool
	failMutationAfterCommit   bool
	failMutationWhen          func(session.ExecutionMutationRequest) bool
	failMutationAfterWhen     func(session.ExecutionMutationRequest) bool
	failPrepareBeforeCommit   bool
	failPrepareAfterCommit    bool
	failTransitionAfterCommit bool
	mutatePrepare             func(*session.PrepareTransitionRequest)
	failReadBlobOnce          bool
}

func (store *projectionErrorStore) PauseSession(
	ctx context.Context,
	request session.PauseRequest,
) (session.Manifest, error) {
	manifest, err := store.Store.PauseSession(ctx, request)
	if err == nil && store.failPause {
		store.failPause = false
		return manifest, session.ErrProjection
	}
	return manifest, err
}

func (store *projectionErrorStore) ResumeSession(
	ctx context.Context,
	request session.ResumeSessionRequest,
) (session.Manifest, error) {
	manifest, err := store.Store.ResumeSession(ctx, request)
	if err == nil && store.failResume {
		store.failResume = false
		return manifest, session.ErrProjection
	}
	return manifest, err
}

type varsRecordingExecutor struct {
	mu   sync.Mutex
	vars map[string]any
}

func (executorImpl *varsRecordingExecutor) Execute(
	_ context.Context,
	step engine.ResolvedStep,
	vars map[string]any,
) (*engine.StepResult, error) {
	executorImpl.mu.Lock()
	executorImpl.vars = make(map[string]any, len(vars))
	for name, value := range vars {
		executorImpl.vars[name] = value
	}
	executorImpl.mu.Unlock()
	return &engine.StepResult{StepID: step.ID, Status: engine.StepStatusCompleted, Outcome: engine.StepOutcomeSuccess}, nil
}

func (executorImpl *varsRecordingExecutor) values() map[string]any {
	executorImpl.mu.Lock()
	defer executorImpl.mu.Unlock()
	result := make(map[string]any, len(executorImpl.vars))
	for name, value := range executorImpl.vars {
		result[name] = value
	}
	return result
}

func (store *projectionErrorStore) CreateSession(
	ctx context.Context,
	epoch uint64,
	request session.CreateRequest,
) (session.Manifest, error) {
	manifest, err := store.Store.CreateSession(ctx, epoch, request)
	if err == nil && store.failCreate {
		store.failCreate = false
		return manifest, session.ErrProjection
	}
	return manifest, err
}

func (store *projectionErrorStore) UpdateAttempt(
	ctx context.Context,
	request session.AttemptUpdateRequest,
) (session.Manifest, error) {
	manifest, err := store.Store.UpdateAttempt(ctx, request)
	if err == nil && store.failUpdate {
		store.failUpdate = false
		return manifest, session.ErrProjection
	}
	return manifest, err
}

func (store *projectionErrorStore) CommitExecutionMutation(
	ctx context.Context,
	request session.ExecutionMutationRequest,
) (session.Manifest, error) {
	if store.failMutationBeforeCommit || store.failMutationWhen != nil && store.failMutationWhen(request) {
		store.failMutationBeforeCommit = false
		return session.Manifest{}, errors.New("simulated process loss before session execution commit")
	}
	manifest, err := store.Store.CommitExecutionMutation(ctx, request)
	if err == nil && (store.failMutationAfterCommit ||
		store.failMutationAfterWhen != nil && store.failMutationAfterWhen(request)) {
		store.failMutationAfterCommit = false
		store.failMutationAfterWhen = nil
		return manifest, errors.New("simulated process loss after session execution commit")
	}
	return manifest, err
}

func (store *projectionErrorStore) PrepareTransition(
	ctx context.Context,
	request session.PrepareTransitionRequest,
) (session.Manifest, error) {
	if store.failPrepareBeforeCommit {
		store.failPrepareBeforeCommit = false
		return session.Manifest{}, errors.New("simulated process loss before transition prepare")
	}
	if store.mutatePrepare != nil {
		store.mutatePrepare(&request)
		store.mutatePrepare = nil
	}
	manifest, err := store.Store.PrepareTransition(ctx, request)
	if err == nil && store.failPrepareAfterCommit {
		store.failPrepareAfterCommit = false
		return manifest, errors.New("simulated process loss after transition prepare")
	}
	return manifest, err
}

func (store *projectionErrorStore) CommitTransition(
	ctx context.Context,
	request session.CommitTransitionRequest,
) (session.Manifest, error) {
	manifest, err := store.Store.CommitTransition(ctx, request)
	if err == nil && store.failTransitionAfterCommit {
		store.failTransitionAfterCommit = false
		return manifest, errors.New("simulated process loss after transition commit")
	}
	return manifest, err
}

func (store *projectionErrorStore) ReadBlob(
	ctx context.Context,
	sessionID string,
	digest string,
) (json.RawMessage, error) {
	if store.failReadBlobOnce {
		store.failReadBlobOnce = false
		return nil, errors.New("simulated transient blob read failure")
	}
	return store.Store.ReadBlob(ctx, sessionID, digest)
}

func (runtime *failFirstStartEngine) Start(ctx context.Context, plan *engine.ExecutionPlan, options engine.RunOptions) (engine.RunHandle, error) {
	if !runtime.failed {
		runtime.failed = true
		return nil, errors.New("simulated process loss after session creation")
	}
	return runtime.inner.Start(ctx, plan, options)
}

func (runtime *failFirstStartEngine) Resume(ctx context.Context, runID string, options engine.RunOptions) (engine.RunHandle, error) {
	return runtime.inner.Resume(ctx, runID, options)
}

func (runtime *failFirstStartEngine) PrepareDurablePlan(ctx context.Context, plan *engine.ExecutionPlan) error {
	return runtime.inner.(interface {
		PrepareDurablePlan(context.Context, *engine.ExecutionPlan) error
	}).PrepareDurablePlan(ctx, plan)
}

func (runtime *failFirstStartEngine) ValidateDurableHandoffPlan(
	ctx context.Context,
	plan *engine.ExecutionPlan,
	options engine.RunOptions,
) error {
	return runtime.inner.(interface {
		ValidateDurableHandoffPlan(context.Context, *engine.ExecutionPlan, engine.RunOptions) error
	}).ValidateDurableHandoffPlan(ctx, plan, options)
}

func (runtime *failFirstStartEngine) ValidateDurableHandoffArtifacts(
	ctx context.Context,
	plan *engine.ExecutionPlan,
	options engine.RunOptions,
	artifacts ...json.RawMessage,
) error {
	return runtime.inner.(interface {
		ValidateDurableHandoffArtifacts(context.Context, *engine.ExecutionPlan, engine.RunOptions, ...json.RawMessage) error
	}).ValidateDurableHandoffArtifacts(ctx, plan, options, artifacts...)
}

func (runtime *callCountingEngine) Start(context.Context, *engine.ExecutionPlan, engine.RunOptions) (engine.RunHandle, error) {
	runtime.startCalls++
	return nil, errors.New("unexpected engine Start")
}

func (runtime *callCountingEngine) Resume(context.Context, string, engine.RunOptions) (engine.RunHandle, error) {
	runtime.resumeCalls++
	return nil, errors.New("unexpected engine Resume")
}

func (*callCountingEngine) PrepareDurablePlan(context.Context, *engine.ExecutionPlan) error {
	return nil
}

func (*callCountingEngine) ValidateDurableHandoffPlan(
	context.Context,
	*engine.ExecutionPlan,
	engine.RunOptions,
) error {
	return nil
}

func (*callCountingEngine) ValidateDurableHandoffArtifacts(
	context.Context,
	*engine.ExecutionPlan,
	engine.RunOptions,
	...json.RawMessage,
) error {
	return nil
}

func TestCoordinatorRejectsSourceGraphNotBoundToPlan(t *testing.T) {
	root := t.TempDir()
	sessions := sessionstore.NewDirStore(root + "/sessions")
	runs := runstore.NewDirRunStore(root + "/runs")
	t.Cleanup(func() {
		_ = runs.Close()
		_ = sessions.Close()
	})
	runtime := &callCountingEngine{}
	coordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
		Sessions: sessions, Runs: runs, Engine: runtime,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	plan := &engine.ExecutionPlan{
		RunbookPath: "root.runbook.yaml",
		Steps:       []engine.ResolvedStep{{ID: "inspect", Kind: "noop", Spec: &schema.NoopSpec{}}},
		Metadata:    engine.PlanMetadata{RunbookID: "root", RunbookName: "Root"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	_, err = coordinator.Start(context.Background(), sessioncoordinator.StartRequest{
		SessionID: uuid.NewString(), CommandID: uuid.NewString(), SegmentID: uuid.NewString(), RunID: uuid.NewString(),
		Plan: plan, Graph: json.RawMessage(`{}`), RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
	})
	if err == nil || !strings.Contains(err.Error(), "source graph") || runtime.startCalls != 0 {
		t.Fatalf("Start error = %v, engine starts = %d", err, runtime.startCalls)
	}
}

func TestCoordinatorCommitsDynamicIncludeSegmentRevisionBeforeChildExecution(t *testing.T) {
	root := t.TempDir()
	baseSessions := sessionstore.NewDirStore(root + "/sessions")
	sessions := &projectionErrorStore{Store: baseSessions}
	runs := runstore.NewDirRunStore(root + "/runs")
	resolved := &executor.DynamicIncludeResult{
		Flow: []schema.FlowNode{{Step: &schema.Step{
			ID: "child_work", Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{},
		}}},
		QualifiedID: "pkg/child", RunbookID: "child", RunbookName: "Child",
		ContentHash: strings.Repeat("a", 64), AbsPath: "C:/runbooks/child.runbook.yaml",
		PackageName: "pkg", PackageVersion: "1.0.0",
		FileDigest:    engine.InteractionPayloadDigest([]byte("child-file")),
		PackageDigest: engine.InteractionPayloadDigest([]byte("child-package")),
	}
	resolver := &fixedDynamicIncludeResolver{result: resolved}
	registry := executor.NewDefaultRegistry(executor.RegistryConfig{
		Evaluator: &internalexpr.TemplateEvaluator{},
		SubStepRunner: func(
			context.Context, executor.SubStepParent, []schema.FlowNode, map[string]any,
		) ([]*engine.StepResult, error) {
			return nil, nil
		},
		DynamicIncludeResolver: resolver,
	})
	runtime := internalengine.New(engine.EngineConfig{
		Executors: registry, Dispatcher: eventbus.NewDispatcher(), TraceWriter: discardTraceWriter{},
		Platform: platform.NewFakePlatform(), Store: runs,
	})
	coordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
		Sessions: sessions, Runs: runs, Engine: runtime,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	plan := &engine.ExecutionPlan{
		RunbookPath: "root.runbook.yaml",
		Steps: []engine.ResolvedStep{{
			ID: "child", Kind: "include", Spec: &schema.IncludeSpec{Include: schema.IncludeConfig{
				RunbookRef: "pkg/child", ResolveFrom: schema.ResolveFromCatalog,
			}},
		}},
		Metadata: engine.PlanMetadata{RunbookID: "root", RunbookName: "Root"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	sessionID := uuid.NewString()
	handle, err := coordinator.Start(context.Background(), sessioncoordinator.StartRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), SegmentID: uuid.NewString(), RunID: uuid.NewString(),
		Plan: plan, Graph: targetGraphJSON(t, plan), RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	sessions.failMutationWhen = func(session.ExecutionMutationRequest) bool {
		current, loadErr := baseSessions.LoadManifest(context.Background(), sessionID)
		if loadErr != nil {
			return false
		}
		segment := current.Segments[current.Session.ActiveSegmentID]
		return segment.ExecutableRevision == 2 && segment.GraphRevision == 2
	}
	if _, err := handle.Next(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "simulated process loss before session execution commit") {
		t.Fatalf("dynamic revision crash = %v", err)
	}
	manifest, err := sessions.LoadManifest(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("LoadManifest after revision crash: %v", err)
	}
	segment := manifest.Segments[manifest.Session.ActiveSegmentID]
	if segment.ExecutableRevision != 2 || segment.GraphRevision != 2 {
		t.Fatalf("dynamic segment revisions = executable %d graph %d", segment.ExecutableRevision, segment.GraphRevision)
	}
	planData, err := sessions.ReadBlob(context.Background(), sessionID, segment.ExecutableSnapshotHash)
	if err != nil || !strings.Contains(string(planData), "pkg/child") {
		t.Fatalf("dynamic plan revision = %s, %v", planData, err)
	}
	graphData, err := sessions.ReadBlob(context.Background(), sessionID, segment.GraphHash)
	if err != nil || !strings.Contains(string(graphData), "child_work") {
		t.Fatalf("dynamic graph revision = %s, %v", graphData, err)
	}
	var revisedGraph struct {
		Frames []graphdoc.Frame `json:"frames"`
		Nodes  []struct {
			Data map[string]any `json:"data"`
		} `json:"nodes"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(graphData)))
	decoder.UseNumber()
	if err := decoder.Decode(&revisedGraph); err != nil {
		t.Fatalf("decode dynamic graph revision: %v", err)
	}
	foundChildFrame, foundChildDetails := false, false
	for _, frame := range revisedGraph.Frames {
		if frame.ParentIncludeNodeID == "child" {
			foundChildFrame = frame.RunbookID == resolved.RunbookID &&
				frame.ContentHash == resolved.ContentHash
		}
	}
	for _, node := range revisedGraph.Nodes {
		if node.Data["step_id"] != "child_work" {
			continue
		}
		details, _ := node.Data["details"].(map[string]any)
		foundChildDetails = details["kind"] == "noop"
	}
	if !foundChildFrame || !foundChildDetails {
		t.Fatalf("dynamic graph lost frame identity or details: frames=%#v nodes=%#v", revisedGraph.Frames, revisedGraph.Nodes)
	}
	events, err := sessions.ReadEvents(context.Background(), sessionID, 0)
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	revisionSequence := int64(0)
	for _, event := range events {
		if event.Kind == session.EventSegmentRevised {
			revisionSequence = event.Sequence
		}
		if event.Kind == session.EventExecutionCommitted && revisionSequence != 0 && event.Sequence > revisionSequence {
			t.Fatal("dynamic execution mutation committed after injected crash")
		}
	}
	if revisionSequence == 0 {
		t.Fatal("dynamic segment revision event is missing before injected crash")
	}
	if err := runs.Close(); err != nil {
		t.Fatalf("close runs: %v", err)
	}
	if err := baseSessions.Close(); err != nil {
		t.Fatalf("close sessions: %v", err)
	}
	reopenedSessions := sessionstore.NewDirStore(root + "/sessions")
	reopenedRuns := runstore.NewDirRunStore(root + "/runs")
	t.Cleanup(func() {
		_ = reopenedRuns.Close()
		_ = reopenedSessions.Close()
	})
	reopenedRuntime := internalengine.New(engine.EngineConfig{
		Executors: registry, Dispatcher: eventbus.NewDispatcher(), TraceWriter: discardTraceWriter{},
		Platform: platform.NewFakePlatform(), Store: reopenedRuns,
	})
	reopenedCoordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
		Sessions: reopenedSessions, Runs: reopenedRuns, Engine: reopenedRuntime,
	})
	if err != nil {
		t.Fatalf("New reopened: %v", err)
	}
	resumed, err := reopenedCoordinator.Resume(context.Background(), sessioncoordinator.ResumeRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
	})
	if err != nil {
		t.Fatalf("Resume dynamic revision: %v", err)
	}
	if result, err := resumed.Next(context.Background()); err != nil || result == nil || result.StepID != "child" {
		t.Fatalf("resumed dynamic Next = %#v, %v", result, err)
	}
	if _, err := resumed.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("complete resumed dynamic run = %v, want EOF", err)
	}
	if resolver.calls != 1 {
		t.Fatalf("dynamic resolver calls = %d, want 1", resolver.calls)
	}
}

func (executorImpl *recordingExecutor) Execute(_ context.Context, step engine.ResolvedStep, _ map[string]any) (*engine.StepResult, error) {
	executorImpl.mu.Lock()
	executorImpl.steps = append(executorImpl.steps, step.ID)
	executorImpl.mu.Unlock()
	return &engine.StepResult{StepID: step.ID, Status: engine.StepStatusCompleted, Outcome: engine.StepOutcomeSuccess}, nil
}

func (executorImpl *recordingExecutor) executed() []string {
	executorImpl.mu.Lock()
	defer executorImpl.mu.Unlock()
	return append([]string(nil), executorImpl.steps...)
}

func TestCoordinatorCompletedAttemptRemainsPausedUntilExplicitClose(t *testing.T) {
	root := t.TempDir()
	sessions := sessionstore.NewDirStore(root + "/sessions")
	runs := runstore.NewDirRunStore(root + "/runs")
	registry := executor.NewMapRegistry()
	registry.Register("noop", noopExecutor{})
	runtime := internalengine.New(engine.EngineConfig{
		Executors: registry, Dispatcher: eventbus.NewDispatcher(), TraceWriter: discardTraceWriter{},
		Platform: platform.NewFakePlatform(), Store: runs,
	})
	coordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
		Sessions: sessions, Runs: runs, Engine: runtime,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	plan := &engine.ExecutionPlan{
		RunbookPath: "root.runbook.yaml",
		Steps:       []engine.ResolvedStep{{ID: "inspect", Kind: "noop", Spec: &schema.NoopSpec{}}},
		Metadata:    engine.PlanMetadata{RunbookID: "root", RunbookName: "Root"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	sessionID := uuid.NewString()
	segmentID := uuid.NewString()
	runID := uuid.NewString()
	handle, err := coordinator.Start(context.Background(), sessioncoordinator.StartRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), SegmentID: segmentID, RunID: runID,
		Plan: plan, Graph: targetGraphJSON(t, plan),
		RunOptions: engine.RunOptions{Mode: engine.RunModeReal, Actor: "operator", Client: "test"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result, err := handle.Next(context.Background()); err != nil || result.StepID != "inspect" {
		t.Fatalf("first Next = %#v, %v", result, err)
	}
	if _, err := handle.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("complete Next = %v, want EOF", err)
	}
	inspection, err := coordinator.Inspect(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if inspection.Manifest.Session.Status != session.StatusPaused ||
		inspection.Manifest.Segments[segmentID].Status != session.SegmentStatusCompleted ||
		inspection.Manifest.Attempts[runID].Status != session.AttemptStatusCompleted {
		t.Fatalf("completed investigation = %#v", inspection.Manifest)
	}
	if inspection.Run == nil || inspection.Run.Status != engine.RunStatusCompleted || inspection.Run.StepResults["inspect"] == nil {
		t.Fatalf("inspectable run state = %#v", inspection.Run)
	}
	closed, err := handle.CloseInvestigation(context.Background(), uuid.NewString(), session.StatusResolved)
	if err != nil {
		t.Fatalf("CloseInvestigation: %v", err)
	}
	if closed.Session.Status != session.StatusResolved {
		t.Fatalf("closed session status = %s", closed.Session.Status)
	}
	if err := handle.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := runs.Close(); err != nil {
		t.Fatalf("close runs: %v", err)
	}
	if err := sessions.Close(); err != nil {
		t.Fatalf("close sessions: %v", err)
	}

	reopenedSessions := sessionstore.NewDirStore(root + "/sessions")
	reopenedRuns := runstore.NewDirRunStore(root + "/runs")
	t.Cleanup(func() {
		_ = reopenedRuns.Close()
		_ = reopenedSessions.Close()
	})
	reopened, err := sessioncoordinator.New(sessioncoordinator.Config{
		Sessions: reopenedSessions, Runs: reopenedRuns, Engine: runtime,
	})
	if err != nil {
		t.Fatalf("New reopened: %v", err)
	}
	inspection, err = reopened.Inspect(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("Inspect reopened: %v", err)
	}
	if inspection.Manifest.Session.Status != session.StatusResolved || inspection.Run == nil ||
		inspection.Run.StepResults["inspect"] == nil {
		t.Fatalf("reopened inspection = %#v", inspection)
	}
}

func TestCoordinatorPersistsHandoffPendingBeforeTransitionResolution(t *testing.T) {
	root := t.TempDir()
	sessions := sessionstore.NewDirStore(root + "/sessions")
	runs := runstore.NewDirRunStore(root + "/runs")
	t.Cleanup(func() {
		_ = runs.Close()
		_ = sessions.Close()
	})
	registry := executor.NewMapRegistry()
	registry.Register("handoff", executor.NewHandoffExecutor(&internalexpr.TemplateEvaluator{}))
	runtime := internalengine.New(engine.EngineConfig{
		Executors: registry, Dispatcher: eventbus.NewDispatcher(), TraceWriter: discardTraceWriter{},
		Platform: platform.NewFakePlatform(), Store: runs,
	})
	coordinator, err := sessioncoordinator.New(sessioncoordinator.Config{Sessions: sessions, Runs: runs, Engine: runtime})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	plan := &engine.ExecutionPlan{
		RunbookPath: "root.runbook.yaml",
		Steps: []engine.ResolvedStep{{
			ID: "continue", Kind: "handoff", Spec: &schema.HandoffSpec{Handoff: schema.HandoffConfig{
				Runbook: "target.runbook.yaml",
				Reason:  schema.HandoffReason{Code: "continue", Summary: "Continue investigation"},
				With:    map[string]string{"server": "${server}"},
			}},
		}},
		Metadata: engine.PlanMetadata{RunbookID: "root", RunbookName: "Root"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	runID := uuid.NewString()
	handle, err := coordinator.Start(context.Background(), sessioncoordinator.StartRequest{
		SessionID: uuid.NewString(), CommandID: uuid.NewString(), SegmentID: uuid.NewString(), RunID: runID,
		Plan: plan, Graph: targetGraphJSON(t, plan),
		RunOptions: engine.RunOptions{Mode: engine.RunModeReal, RuntimeVars: map[string]any{"server": "db01"}},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := handle.Next(context.Background()); err == nil {
		t.Fatal("Next did not return handoff signal")
	} else if request, ok := engine.HandoffRequestFromError(err); !ok || request.Context["server"] != "db01" {
		t.Fatalf("handoff signal = %#v, %v", request, err)
	}
	manifest := handle.Manifest()
	if manifest.Attempts[runID].Status != session.AttemptStatusHandoffPending ||
		manifest.Session.Status != session.StatusPaused || manifest.Session.ActiveRunID != runID {
		t.Fatalf("handoff-pending manifest = %#v", manifest)
	}
}

func TestCoordinatorRejectsProtectedSourceGraphBeforeSessionCreation(t *testing.T) {
	const secret = "a\"b"
	root := t.TempDir()
	sessions := sessionstore.NewDirStore(filepath.Join(root, "sessions"))
	runs := runstore.NewDirRunStore(filepath.Join(root, "runs"))
	t.Cleanup(func() {
		_ = runs.Close()
		_ = sessions.Close()
	})
	registry := executor.NewMapRegistry()
	registry.Register("noop", noopExecutor{})
	runtime := internalengine.New(engine.EngineConfig{
		Executors: registry, Dispatcher: eventbus.NewDispatcher(), TraceWriter: discardTraceWriter{},
		Platform: platform.NewFakePlatform(), Store: runs,
	})
	coordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
		Sessions: sessions, Runs: runs, Engine: runtime,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	plan := &engine.ExecutionPlan{
		RunbookPath: "root.runbook.yaml",
		Steps:       []engine.ResolvedStep{{ID: "work", Name: secret, Kind: "noop", Spec: &schema.NoopSpec{}}},
		Inputs:      map[string]*schema.Input{"credential": {Type: "secret"}},
		Metadata:    engine.PlanMetadata{RunbookID: "root", RunbookName: "Root"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	sessionID := uuid.NewString()
	graphPlan := *plan
	graphPlan.Steps = append([]engine.ResolvedStep(nil), plan.Steps...)
	graphPlan.Steps[0].Name = ""
	graph := targetGraphJSON(t, &graphPlan)
	plan.Metadata.GraphContentHash = graphPlan.Metadata.GraphContentHash
	_, err = coordinator.Start(context.Background(), sessioncoordinator.StartRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), SegmentID: uuid.NewString(), RunID: uuid.NewString(),
		Plan: plan, Graph: graph,
		RunOptions: engine.RunOptions{Mode: engine.RunModeReal, RuntimeVars: map[string]any{"credential": secret}},
	})
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("Start error = %v, want value-free source graph refusal", err)
	}
	if _, loadErr := sessions.LoadManifest(context.Background(), sessionID); !errors.Is(loadErr, os.ErrNotExist) {
		t.Fatalf("protected source session was created: %v", loadErr)
	}
}

func TestCoordinatorCommitsStaticHandoffAndContinuesInTarget(t *testing.T) {
	root := t.TempDir()
	sessions := sessionstore.NewDirStore(root + "/sessions")
	runs := runstore.NewDirRunStore(root + "/runs")
	t.Cleanup(func() {
		_ = runs.Close()
		_ = sessions.Close()
	})
	registry := executor.NewMapRegistry()
	registry.Register("handoff", executor.NewHandoffExecutor(&internalexpr.TemplateEvaluator{}))
	registry.Register("noop", noopExecutor{})
	runtime := internalengine.New(engine.EngineConfig{
		Executors: registry, Dispatcher: eventbus.NewDispatcher(), TraceWriter: discardTraceWriter{},
		Platform: platform.NewFakePlatform(), Store: runs,
	})
	targetPlan := &engine.ExecutionPlan{
		RunbookPath: "target.runbook.yaml",
		Steps:       []engine.ResolvedStep{{ID: "target_work", Kind: "noop", Spec: &schema.NoopSpec{}}},
		Inputs: map[string]*schema.Input{
			"server": {Type: "string", Required: true},
		},
		Metadata: engine.PlanMetadata{RunbookID: "target", RunbookName: "Target"},
	}
	if err := planner.ValidateExecutionPlan(targetPlan); err != nil {
		t.Fatalf("ValidateExecutionPlan target: %v", err)
	}
	resolver := &staticHandoffResolver{target: sessioncoordinator.HandoffTarget{
		Plan: targetPlan, Graph: targetGraphJSON(t, targetPlan),
	}}
	coordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
		Sessions: sessions, Runs: runs, Engine: runtime, HandoffResolver: resolver,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sourcePlan := &engine.ExecutionPlan{
		RunbookPath: "root.runbook.yaml",
		Steps: []engine.ResolvedStep{{
			ID: "continue", Kind: "handoff", Spec: &schema.HandoffSpec{Handoff: schema.HandoffConfig{
				Runbook: "target.runbook.yaml",
				Reason:  schema.HandoffReason{Code: "continue", Summary: "Continue investigation"},
				With:    map[string]string{"server": "${server}"},
			}},
		}},
		Metadata: engine.PlanMetadata{RunbookID: "root", RunbookName: "Root"},
	}
	if err := planner.ValidateExecutionPlan(sourcePlan); err != nil {
		t.Fatalf("ValidateExecutionPlan source: %v", err)
	}
	sessionID, sourceSegmentID, sourceRunID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	handle, err := coordinator.Start(context.Background(), sessioncoordinator.StartRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), SegmentID: sourceSegmentID, RunID: sourceRunID,
		Plan: sourcePlan, Graph: targetGraphJSON(t, sourcePlan),
		RunOptions: engine.RunOptions{
			Mode: engine.RunModeReal, Actor: "operator", Client: "test",
			RuntimeVars: map[string]any{"server": "db01"},
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result, err := handle.Next(context.Background()); err != nil || result != nil {
		t.Fatalf("handoff Next = %#v, %v", result, err)
	}
	manifest := handle.Manifest()
	if len(resolver.requests) != 1 || resolver.requests[0].Handoff.TargetRunbook != "target.runbook.yaml" ||
		len(manifest.Segments) != 2 || len(manifest.Attempts) != 2 || len(manifest.Transitions) != 1 ||
		manifest.Segments[sourceSegmentID].Status != session.SegmentStatusHandedOff ||
		manifest.Attempts[sourceRunID].Status != session.AttemptStatusCompleted ||
		manifest.Session.ActiveSegmentID == sourceSegmentID || manifest.Session.ActiveRunID == sourceRunID {
		t.Fatalf("committed handoff = %#v, requests = %#v", manifest, resolver.requests)
	}
	targetRunID := manifest.Session.ActiveRunID
	if manifest.Attempts[targetRunID].Status != session.AttemptStatusRunning {
		t.Fatalf("target attempt = %#v", manifest.Attempts[targetRunID])
	}
	if result, err := handle.Next(context.Background()); err != nil || result == nil || result.StepID != "target_work" {
		t.Fatalf("target Next = %#v, %v", result, err)
	}
	if _, err := handle.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("target completion = %v", err)
	}
	manifest = handle.Manifest()
	if manifest.Attempts[targetRunID].Status != session.AttemptStatusCompleted ||
		manifest.Session.Status != session.StatusPaused {
		t.Fatalf("completed target manifest = %#v", manifest)
	}
}

func TestCoordinatorRecoversStaticHandoffWithoutDuplicatingTarget(t *testing.T) {
	for _, testCase := range []struct {
		name                string
		configure           func(*projectionErrorStore)
		configureAfterStart func(*projectionErrorStore, string)
		preparedOnly        bool
		noTransition        bool
		transientRead       bool
		wantResolverCalls   int
	}{
		{
			name: "before prepare", configure: func(store *projectionErrorStore) { store.failPrepareBeforeCommit = true },
			noTransition: true, wantResolverCalls: 2,
		},
		{
			name: "after prepare", configure: func(store *projectionErrorStore) { store.failPrepareAfterCommit = true },
			preparedOnly: true, wantResolverCalls: 1,
		},
		{
			name: "transient verification read", configure: func(store *projectionErrorStore) { store.failPrepareAfterCommit = true },
			preparedOnly: true, transientRead: true, wantResolverCalls: 1,
		},
		{
			name: "after commit", configure: func(store *projectionErrorStore) { store.failTransitionAfterCommit = true },
			wantResolverCalls: 1,
		},
		{
			name: "after target start commit", configure: func(*projectionErrorStore) {},
			configureAfterStart: func(store *projectionErrorStore, sourceRunID string) {
				store.failMutationAfterWhen = func(request session.ExecutionMutationRequest) bool {
					return request.Mutation.RunID != sourceRunID
				}
			},
			wantResolverCalls: 1,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			baseSessions := sessionstore.NewDirStore(root + "/sessions")
			sessions := &projectionErrorStore{Store: baseSessions}
			testCase.configure(sessions)
			runs := runstore.NewDirRunStore(root + "/runs")
			t.Cleanup(func() {
				_ = runs.Close()
				_ = baseSessions.Close()
			})
			recorder := &varsRecordingExecutor{}
			registry := executor.NewMapRegistry()
			registry.Register("handoff", executor.NewHandoffExecutor(&internalexpr.TemplateEvaluator{}))
			registry.Register("noop", recorder)
			newRuntime := func() engine.Engine {
				return internalengine.New(engine.EngineConfig{
					Executors: registry, Dispatcher: eventbus.NewDispatcher(), TraceWriter: discardTraceWriter{},
					Platform: platform.NewFakePlatform(), Store: runs,
				})
			}
			targetPlan := &engine.ExecutionPlan{
				RunbookPath: "target.runbook.yaml",
				Steps:       []engine.ResolvedStep{{ID: "target_work", Kind: "noop", Spec: &schema.NoopSpec{}}},
				Inputs: map[string]*schema.Input{
					"server": {Type: "string", Required: true},
				},
				Metadata: engine.PlanMetadata{RunbookID: "target", RunbookName: "Target"},
			}
			if err := planner.ValidateExecutionPlan(targetPlan); err != nil {
				t.Fatalf("ValidateExecutionPlan target: %v", err)
			}
			resolver := &staticHandoffResolver{target: sessioncoordinator.HandoffTarget{
				Plan: targetPlan, Graph: targetGraphJSON(t, targetPlan),
			}}
			firstCoordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
				Sessions: sessions, Runs: runs, Engine: newRuntime(), HandoffResolver: resolver,
			})
			if err != nil {
				t.Fatalf("New first: %v", err)
			}
			sourcePlan := &engine.ExecutionPlan{
				RunbookPath: "root.runbook.yaml",
				Steps: []engine.ResolvedStep{{
					ID: "continue", Kind: "handoff", Spec: &schema.HandoffSpec{Handoff: schema.HandoffConfig{
						Runbook: "target.runbook.yaml",
						Reason:  schema.HandoffReason{Code: "continue", Summary: "Continue investigation"},
						With:    map[string]string{"server": "${server}"},
					}},
				}},
				Metadata: engine.PlanMetadata{RunbookID: "root", RunbookName: "Root"},
			}
			if err := planner.ValidateExecutionPlan(sourcePlan); err != nil {
				t.Fatalf("ValidateExecutionPlan source: %v", err)
			}
			sessionID, sourceSegmentID, sourceRunID := uuid.NewString(), uuid.NewString(), uuid.NewString()
			firstHandle, err := firstCoordinator.Start(context.Background(), sessioncoordinator.StartRequest{
				SessionID: sessionID, CommandID: uuid.NewString(), SegmentID: sourceSegmentID, RunID: sourceRunID,
				Plan: sourcePlan, Graph: targetGraphJSON(t, sourcePlan),
				RunOptions: engine.RunOptions{
					Mode: engine.RunModeReal, RuntimeVars: map[string]any{"server": "db01"},
				},
			})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if testCase.configureAfterStart != nil {
				testCase.configureAfterStart(sessions, sourceRunID)
			}
			if _, err := firstHandle.Next(context.Background()); err == nil || !strings.Contains(err.Error(), "simulated process loss") {
				t.Fatalf("handoff crash = %v", err)
			}
			crashed, err := sessions.LoadManifest(context.Background(), sessionID)
			if err != nil {
				t.Fatalf("LoadManifest after crash: %v", err)
			}
			wantCrashedTransitions := 1
			if testCase.noTransition {
				wantCrashedTransitions = 0
			}
			if len(crashed.Transitions) != wantCrashedTransitions {
				t.Fatalf("transition count after crash = %d, want %d", len(crashed.Transitions), wantCrashedTransitions)
			}
			var transitionID, targetSegmentID, targetRunID string
			for id, transition := range crashed.Transitions {
				transitionID, targetSegmentID, targetRunID = id, transition.TargetSegmentID, transition.TargetRunID
				wantStatus := session.TransitionStatusCommitted
				if testCase.preparedOnly {
					wantStatus = session.TransitionStatusPrepared
				}
				if transition.Status != wantStatus {
					t.Fatalf("transition status = %s, want %s", transition.Status, wantStatus)
				}
			}
			if err := firstHandle.Release(); err != nil {
				t.Fatalf("Release crashed handle: %v", err)
			}
			secondCoordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
				Sessions: sessions, Runs: runs, Engine: newRuntime(), HandoffResolver: resolver,
			})
			if err != nil {
				t.Fatalf("New second: %v", err)
			}
			if testCase.transientRead {
				sessions.failReadBlobOnce = true
				if _, err := secondCoordinator.Resume(context.Background(), sessioncoordinator.ResumeRequest{
					SessionID: sessionID, CommandID: uuid.NewString(), RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
				}); err == nil || !strings.Contains(err.Error(), "transient blob read failure") {
					t.Fatalf("transient Resume = %v", err)
				}
				retryable, err := sessions.LoadManifest(context.Background(), sessionID)
				if err != nil || retryable.Transitions[transitionID].Status != session.TransitionStatusPrepared {
					t.Fatalf("transition after transient read = %#v, %v", retryable.Transitions[transitionID], err)
				}
			}
			secondHandle, err := secondCoordinator.Resume(context.Background(), sessioncoordinator.ResumeRequest{
				SessionID: sessionID, CommandID: uuid.NewString(), RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
			})
			if err != nil {
				t.Fatalf("Resume after crash: %v", err)
			}
			if result, err := secondHandle.Next(context.Background()); err != nil || result == nil || result.StepID != "target_work" {
				t.Fatalf("target Next = %#v, %v", result, err)
			}
			recovered := secondHandle.Manifest()
			if testCase.noTransition {
				for id, transition := range recovered.Transitions {
					transitionID, targetSegmentID, targetRunID = id, transition.TargetSegmentID, transition.TargetRunID
				}
			}
			if len(recovered.Transitions) != 1 || len(recovered.Segments) != 2 || len(recovered.Attempts) != 2 ||
				recovered.Transitions[transitionID].Status != session.TransitionStatusCommitted ||
				recovered.Segments[targetSegmentID].SegmentID != targetSegmentID ||
				recovered.Attempts[targetRunID].RunID != targetRunID || recorder.values()["server"] != "db01" ||
				len(resolver.requests) != testCase.wantResolverCalls {
				t.Fatalf("recovered handoff = %#v, vars = %#v, resolver calls = %d", recovered, recorder.values(), len(resolver.requests))
			}
		})
	}
}

func TestCoordinatorRetainsInvalidPreparedHandoffAndFailsClosed(t *testing.T) {
	root := t.TempDir()
	baseSessions := sessionstore.NewDirStore(root + "/sessions")
	sessions := &projectionErrorStore{
		Store:                  baseSessions,
		failPrepareAfterCommit: true,
		mutatePrepare: func(request *session.PrepareTransitionRequest) {
			request.Transition.ReasonSummary = "forged prepared reason"
		},
	}
	runs := runstore.NewDirRunStore(root + "/runs")
	t.Cleanup(func() {
		_ = runs.Close()
		_ = baseSessions.Close()
	})
	registry := executor.NewMapRegistry()
	registry.Register("handoff", executor.NewHandoffExecutor(&internalexpr.TemplateEvaluator{}))
	registry.Register("noop", noopExecutor{})
	newRuntime := func() engine.Engine {
		return internalengine.New(engine.EngineConfig{
			Executors: registry, Dispatcher: eventbus.NewDispatcher(), TraceWriter: discardTraceWriter{},
			Platform: platform.NewFakePlatform(), Store: runs,
		})
	}
	targetPlan := &engine.ExecutionPlan{
		RunbookPath: "target.runbook.yaml",
		Steps:       []engine.ResolvedStep{{ID: "target_work", Kind: "noop", Spec: &schema.NoopSpec{}}},
		Inputs:      map[string]*schema.Input{"server": {Type: "string", Required: true}},
		Metadata:    engine.PlanMetadata{RunbookID: "target", RunbookName: "Target"},
	}
	if err := planner.ValidateExecutionPlan(targetPlan); err != nil {
		t.Fatalf("ValidateExecutionPlan target: %v", err)
	}
	resolver := &staticHandoffResolver{target: sessioncoordinator.HandoffTarget{
		Plan: targetPlan, Graph: targetGraphJSON(t, targetPlan),
	}}
	firstCoordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
		Sessions: sessions, Runs: runs, Engine: newRuntime(), HandoffResolver: resolver,
	})
	if err != nil {
		t.Fatalf("New first: %v", err)
	}
	sourcePlan := &engine.ExecutionPlan{
		RunbookPath: "root.runbook.yaml",
		Steps: []engine.ResolvedStep{{
			ID: "continue", Kind: "handoff", Spec: &schema.HandoffSpec{Handoff: schema.HandoffConfig{
				Runbook: "target.runbook.yaml",
				Reason:  schema.HandoffReason{Code: "continue", Summary: "Continue investigation"},
				With:    map[string]string{"server": "${server}"},
			}},
		}},
		Metadata: engine.PlanMetadata{RunbookID: "root", RunbookName: "Root"},
	}
	if err := planner.ValidateExecutionPlan(sourcePlan); err != nil {
		t.Fatalf("ValidateExecutionPlan source: %v", err)
	}
	sessionID, sourceSegmentID, sourceRunID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	firstHandle, err := firstCoordinator.Start(context.Background(), sessioncoordinator.StartRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), SegmentID: sourceSegmentID, RunID: sourceRunID,
		Plan: sourcePlan, Graph: targetGraphJSON(t, sourcePlan),
		RunOptions: engine.RunOptions{Mode: engine.RunModeReal, RuntimeVars: map[string]any{"server": "db01"}},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := firstHandle.Next(context.Background()); err == nil || !strings.Contains(err.Error(), "simulated process loss") {
		t.Fatalf("handoff crash = %v", err)
	}
	prepared, err := sessions.LoadManifest(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("LoadManifest after prepare loss: %v", err)
	}
	if len(prepared.Transitions) != 1 {
		t.Fatalf("prepared transitions = %#v", prepared.Transitions)
	}
	if err := firstHandle.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	secondCoordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
		Sessions: sessions, Runs: runs, Engine: newRuntime(), HandoffResolver: resolver,
	})
	if err != nil {
		t.Fatalf("New second: %v", err)
	}
	if _, err := secondCoordinator.Resume(context.Background(), sessioncoordinator.ResumeRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
	}); err == nil || !strings.Contains(err.Error(), "prepared handoff recovery blocked") {
		t.Fatalf("Resume invalid preparation = %v", err)
	}
	blocked, err := sessions.LoadManifest(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	var transition session.TransitionRecord
	for _, candidate := range blocked.Transitions {
		transition = candidate
	}
	if transition.Status != session.TransitionStatusPrepared || len(blocked.Segments) != 1 || len(blocked.Attempts) != 1 ||
		blocked.Session.ActiveSegmentID != sourceSegmentID || blocked.Session.ActiveRunID != sourceRunID ||
		blocked.Attempts[sourceRunID].Status != session.AttemptStatusHandoffPending || len(resolver.requests) != 1 {
		t.Fatalf("blocked transition = %#v, resolver calls = %d", blocked, len(resolver.requests))
	}
	inspection, err := secondCoordinator.Inspect(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("Inspect source after blocked recovery: %v", err)
	}
	if inspection.Run == nil || inspection.Run.Status != engine.RunStatusHandoffPending ||
		inspection.Run.CursorSet == nil || len(inspection.Run.CursorSet.Cursors) != 1 ||
		inspection.Run.CursorSet.Cursors[0].StepID != "continue" {
		t.Fatalf("source cursor after blocked recovery = %#v", inspection.Run)
	}
	editedTarget := *targetPlan
	editedTarget.Steps = append([]engine.ResolvedStep(nil), targetPlan.Steps...)
	editedTarget.Steps[0].Name = "Edited after prepared transition"
	editedTarget.Metadata.PlanHash = ""
	editedTarget.Validation = nil
	if err := planner.ValidateExecutionPlan(&editedTarget); err != nil {
		t.Fatalf("ValidateExecutionPlan edited target: %v", err)
	}
	resolver.target = sessioncoordinator.HandoffTarget{Plan: &editedTarget, Graph: targetGraphJSON(t, &editedTarget)}
	if _, err := secondCoordinator.Resume(context.Background(), sessioncoordinator.ResumeRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
	}); err == nil || !strings.Contains(err.Error(), "prepared handoff recovery blocked") {
		t.Fatalf("second Resume invalid preparation = %v", err)
	}
	stillBlocked, err := sessions.LoadManifest(context.Background(), sessionID)
	if err != nil || stillBlocked.Transitions[transition.TransitionID].Status != session.TransitionStatusPrepared ||
		len(stillBlocked.Segments) != 1 || len(resolver.requests) != 1 {
		t.Fatalf("prepared transition changed after second resume = %#v, %v; resolver calls = %d", stillBlocked, err, len(resolver.requests))
	}
}

func TestCoordinatorUsesSessionWriterEpochForEngineState(t *testing.T) {
	root := t.TempDir()
	sessions := sessionstore.NewDirStore(root + "/sessions")
	runs := runstore.NewDirRunStore(root + "/runs")
	t.Cleanup(func() {
		_ = runs.Close()
		_ = sessions.Close()
	})
	sessionID := uuid.NewString()
	for range 2 {
		lease, err := sessions.AcquireSessionLease(context.Background(), sessionID)
		if err != nil {
			t.Fatalf("advance session epoch: %v", err)
		}
		if err := lease.Release(); err != nil {
			t.Fatalf("release advanced epoch: %v", err)
		}
	}
	registry := executor.NewMapRegistry()
	registry.Register("noop", noopExecutor{})
	runtime := internalengine.New(engine.EngineConfig{
		Executors: registry, Dispatcher: eventbus.NewDispatcher(), TraceWriter: discardTraceWriter{},
		Platform: platform.NewFakePlatform(), Store: runs,
	})
	coordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
		Sessions: sessions, Runs: runs, Engine: runtime,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	plan := &engine.ExecutionPlan{
		RunbookPath: "session-epoch.runbook.yaml",
		Steps:       []engine.ResolvedStep{{ID: "work", Kind: "noop", Spec: &schema.NoopSpec{}}},
		Metadata:    engine.PlanMetadata{RunbookID: "session-epoch", RunbookName: "Session Epoch"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	handle, err := coordinator.Start(context.Background(), sessioncoordinator.StartRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), SegmentID: uuid.NewString(), RunID: uuid.NewString(),
		Plan: plan, Graph: targetGraphJSON(t, plan),
		RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	inspection, err := coordinator.Inspect(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if inspection.Run == nil || inspection.Run.WriterEpoch != handle.Manifest().Session.WriterEpoch {
		t.Fatalf("run epoch %v does not match session epoch %d", inspection.Run, handle.Manifest().Session.WriterEpoch)
	}
}

func TestCoordinatorResumeKeepsRunIdentityAndSkipsCommittedStep(t *testing.T) {
	root := t.TempDir()
	plan := &engine.ExecutionPlan{
		RunbookPath: "resume.runbook.yaml",
		Steps: []engine.ResolvedStep{
			{ID: "first", Kind: "noop", Spec: &schema.NoopSpec{}},
			{ID: "second", Kind: "noop", Spec: &schema.NoopSpec{}},
		},
		Metadata: engine.PlanMetadata{RunbookID: "resume", RunbookName: "Resume"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	sessionID, segmentID, runID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	firstSessions := sessionstore.NewDirStore(root + "/sessions")
	firstRuns := runstore.NewDirRunStore(root + "/runs")
	firstExecutor := &recordingExecutor{}
	firstRegistry := executor.NewMapRegistry()
	firstRegistry.Register("noop", firstExecutor)
	firstRuntime := internalengine.New(engine.EngineConfig{
		Executors: firstRegistry, Dispatcher: eventbus.NewDispatcher(), TraceWriter: discardTraceWriter{},
		Platform: platform.NewFakePlatform(), Store: firstRuns,
	})
	firstCoordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
		Sessions: firstSessions, Runs: firstRuns, Engine: firstRuntime,
	})
	if err != nil {
		t.Fatalf("New first: %v", err)
	}
	firstHandle, err := firstCoordinator.Start(context.Background(), sessioncoordinator.StartRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), SegmentID: segmentID, RunID: runID,
		Plan: plan, Graph: targetGraphJSON(t, plan),
		RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result, err := firstHandle.Next(context.Background()); err != nil || result.StepID != "first" {
		t.Fatalf("first Next = %#v, %v", result, err)
	}
	if firstHandle.Manifest().Attempts[runID].RunProjectionHash == "" {
		t.Fatal("first Next did not commit a session-owned run projection")
	}
	if err := firstRuns.Close(); err != nil {
		t.Fatalf("close first runs: %v", err)
	}
	if err := firstSessions.Close(); err != nil {
		t.Fatalf("close first sessions: %v", err)
	}
	checkpointSequence := firstHandle.Manifest().Attempts[runID].CheckpointSequence
	currentCheckpoint := filepath.Join(
		root, "runs", runID, "snapshots", fmt.Sprintf("checkpoint-%020d.json", checkpointSequence),
	)
	newerCheckpoint := filepath.Join(
		root, "runs", runID, "snapshots", fmt.Sprintf("checkpoint-%020d.json", checkpointSequence+1),
	)
	checkpointData, err := os.ReadFile(currentCheckpoint)
	if err != nil {
		t.Fatalf("read current checkpoint: %v", err)
	}
	var forgedCheckpoint map[string]any
	if err := json.Unmarshal(checkpointData, &forgedCheckpoint); err != nil {
		t.Fatalf("decode current checkpoint: %v", err)
	}
	forgedCheckpoint["CheckpointSequence"] = checkpointSequence + 1
	checkpointData, err = json.Marshal(forgedCheckpoint)
	if err != nil {
		t.Fatalf("encode newer checkpoint: %v", err)
	}
	if err := os.WriteFile(newerCheckpoint, checkpointData, 0o600); err != nil {
		t.Fatalf("write newer checkpoint: %v", err)
	}
	inspectionSessions := sessionstore.NewDirStore(root + "/sessions")
	inspectionRuns := runstore.NewDirRunStore(root + "/runs")
	inspectionCoordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
		Sessions: inspectionSessions, Runs: inspectionRuns, Engine: &callCountingEngine{},
	})
	if err != nil {
		t.Fatalf("New inspection coordinator: %v", err)
	}
	inspection, err := inspectionCoordinator.Inspect(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("Inspect newer local checkpoint: %v", err)
	}
	if inspection.Run == nil || inspection.Run.CheckpointSequence != checkpointSequence ||
		inspection.Run.StepResults["first"] == nil || inspection.Run.StepResults["second"] != nil {
		t.Fatalf("Inspect trusted newer local checkpoint: %#v", inspection.Run)
	}
	if err := inspectionRuns.Close(); err != nil {
		t.Fatalf("close inspection runs: %v", err)
	}
	if err := inspectionSessions.Close(); err != nil {
		t.Fatalf("close inspection sessions: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(root, "runs", runID)); err != nil {
		t.Fatalf("remove run projection: %v", err)
	}

	secondSessions := sessionstore.NewDirStore(root + "/sessions")
	secondRuns := runstore.NewDirRunStore(root + "/runs")
	t.Cleanup(func() {
		_ = secondRuns.Close()
		_ = secondSessions.Close()
	})
	secondExecutor := &recordingExecutor{}
	secondRegistry := executor.NewMapRegistry()
	secondRegistry.Register("noop", secondExecutor)
	secondRuntime := internalengine.New(engine.EngineConfig{
		Executors: secondRegistry, Dispatcher: eventbus.NewDispatcher(), TraceWriter: discardTraceWriter{},
		Platform: platform.NewFakePlatform(), Store: secondRuns,
	})
	secondCoordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
		Sessions: secondSessions, Runs: secondRuns, Engine: secondRuntime,
	})
	if err != nil {
		t.Fatalf("New second: %v", err)
	}
	inspection, err = secondCoordinator.Inspect(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("Inspect restored projection: %v", err)
	}
	if inspection.Run == nil || inspection.Run.StepResults["first"] == nil ||
		inspection.Run.StepResults["second"] != nil {
		t.Fatalf("inspection did not restore the authoritative first checkpoint: %#v", inspection.Run)
	}
	if _, err := secondCoordinator.Resume(context.Background(), sessioncoordinator.ResumeRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), RunOptions: engine.RunOptions{Mode: engine.RunModeDryRun},
	}); err == nil {
		t.Fatal("Resume unexpectedly changed the attempt mode from real to dry-run")
	}
	if _, err := secondCoordinator.Resume(context.Background(), sessioncoordinator.ResumeRequest{
		SessionID: sessionID, CommandID: firstHandle.Manifest().Session.CreationCommandID,
		RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
	}); !errors.Is(err, session.ErrCommandConflict) {
		t.Fatalf("conflicting Resume error = %v, want ErrCommandConflict", err)
	}
	resumed, err := secondCoordinator.Resume(context.Background(), sessioncoordinator.ResumeRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if result, err := resumed.Next(context.Background()); err != nil || result.StepID != "second" {
		t.Fatalf("resumed Next = %#v, %v", result, err)
	}
	if _, err := resumed.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("complete resumed run = %v", err)
	}
	if got := firstExecutor.executed(); len(got) != 1 || got[0] != "first" {
		t.Fatalf("first process executed %v", got)
	}
	if got := secondExecutor.executed(); len(got) != 1 || got[0] != "second" {
		t.Fatalf("resumed process executed %v, want only second", got)
	}
	manifest := resumed.Manifest()
	if manifest.Session.SessionID != sessionID || manifest.Attempts[runID].Status != session.AttemptStatusCompleted {
		t.Fatalf("resumed manifest = %#v", manifest)
	}
	if err := resumed.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

func TestCoordinatorResumeRepairsMissingRunPlanFromSessionSnapshot(t *testing.T) {
	root := t.TempDir()
	plan := &engine.ExecutionPlan{
		RunbookPath: "repair-plan.runbook.yaml",
		Steps: []engine.ResolvedStep{
			{ID: "first", Kind: "noop", Spec: &schema.NoopSpec{}},
			{ID: "second", Kind: "noop", Spec: &schema.NoopSpec{}},
		},
		Metadata: engine.PlanMetadata{RunbookID: "repair-plan", RunbookName: "Repair Plan"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	sessionID, segmentID, runID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	firstSessions := sessionstore.NewDirStore(root + "/sessions")
	firstRuns := runstore.NewDirRunStore(root + "/runs")
	firstRegistry := executor.NewMapRegistry()
	firstRegistry.Register("noop", noopExecutor{})
	firstRuntime := internalengine.New(engine.EngineConfig{
		Executors: firstRegistry, Dispatcher: eventbus.NewDispatcher(), TraceWriter: discardTraceWriter{},
		Platform: platform.NewFakePlatform(), Store: firstRuns,
	})
	firstCoordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
		Sessions: firstSessions, Runs: firstRuns, Engine: firstRuntime,
	})
	if err != nil {
		t.Fatalf("New first: %v", err)
	}
	firstHandle, err := firstCoordinator.Start(context.Background(), sessioncoordinator.StartRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), SegmentID: segmentID, RunID: runID,
		Plan: plan, Graph: targetGraphJSON(t, plan),
		RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result, err := firstHandle.Next(context.Background()); err != nil || result.StepID != "first" {
		t.Fatalf("first Next = %#v, %v", result, err)
	}
	if err := firstRuns.Close(); err != nil {
		t.Fatalf("close first runs: %v", err)
	}
	if err := firstSessions.Close(); err != nil {
		t.Fatalf("close first sessions: %v", err)
	}
	if err := os.Remove(firstRuns.PlanPath(runID)); err != nil {
		t.Fatalf("remove run plan: %v", err)
	}

	secondSessions := sessionstore.NewDirStore(root + "/sessions")
	secondRuns := runstore.NewDirRunStore(root + "/runs")
	t.Cleanup(func() {
		_ = secondRuns.Close()
		_ = secondSessions.Close()
	})
	secondExecutor := &recordingExecutor{}
	secondRegistry := executor.NewMapRegistry()
	secondRegistry.Register("noop", secondExecutor)
	secondRuntime := internalengine.New(engine.EngineConfig{
		Executors: secondRegistry, Dispatcher: eventbus.NewDispatcher(), TraceWriter: discardTraceWriter{},
		Platform: platform.NewFakePlatform(), Store: secondRuns,
	})
	secondCoordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
		Sessions: secondSessions, Runs: secondRuns, Engine: secondRuntime,
	})
	if err != nil {
		t.Fatalf("New second: %v", err)
	}
	resumed, err := secondCoordinator.Resume(context.Background(), sessioncoordinator.ResumeRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if result, err := resumed.Next(context.Background()); err != nil || result.StepID != "second" {
		t.Fatalf("resumed Next = %#v, %v", result, err)
	}
	if _, err := resumed.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("complete resumed run = %v", err)
	}
	if got := secondExecutor.executed(); len(got) != 1 || got[0] != "second" {
		t.Fatalf("resumed process executed %v, want only second", got)
	}
	if _, err := os.Stat(secondRuns.PlanPath(runID)); err != nil {
		t.Fatalf("repaired run plan: %v", err)
	}
	if err := resumed.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

func TestCoordinatorResumeBeforeFirstNextRestoresInitialMutation(t *testing.T) {
	root := t.TempDir()
	sessions := sessionstore.NewDirStore(root + "/sessions")
	runs := runstore.NewDirRunStore(root + "/runs")
	registry := executor.NewMapRegistry()
	registry.Register("noop", noopExecutor{})
	runtime := internalengine.New(engine.EngineConfig{
		Executors: registry, Dispatcher: eventbus.NewDispatcher(), TraceWriter: discardTraceWriter{},
		Platform: platform.NewFakePlatform(), Store: runs,
	})
	coordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
		Sessions: sessions, Runs: runs, Engine: runtime,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	plan := &engine.ExecutionPlan{
		RunbookPath: "missing-projection.runbook.yaml",
		Steps:       []engine.ResolvedStep{{ID: "work", Kind: "noop", Spec: &schema.NoopSpec{}}},
		Metadata:    engine.PlanMetadata{RunbookID: "missing-projection", RunbookName: "Missing Projection"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	sessionID, segmentID, runID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	handle, err := coordinator.Start(context.Background(), sessioncoordinator.StartRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), SegmentID: segmentID, RunID: runID,
		Plan: plan, Graph: targetGraphJSON(t, plan),
		RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	attempt := handle.Manifest().Attempts[runID]
	if attempt.RunProjectionHash == "" || attempt.ExecutionMutationHash == "" || attempt.CheckpointSequence != 0 {
		t.Fatalf("initial execution mutation = %#v", attempt)
	}
	if err := runs.Close(); err != nil {
		t.Fatalf("close runs: %v", err)
	}
	if err := sessions.Close(); err != nil {
		t.Fatalf("close sessions: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(root, "runs", runID)); err != nil {
		t.Fatalf("remove run projection: %v", err)
	}

	reopenedSessions := sessionstore.NewDirStore(root + "/sessions")
	reopenedRuns := runstore.NewDirRunStore(root + "/runs")
	t.Cleanup(func() {
		_ = reopenedRuns.Close()
		_ = reopenedSessions.Close()
	})
	reopenedExecutor := &recordingExecutor{}
	reopenedRegistry := executor.NewMapRegistry()
	reopenedRegistry.Register("noop", reopenedExecutor)
	reopenedRuntime := internalengine.New(engine.EngineConfig{
		Executors: reopenedRegistry, Dispatcher: eventbus.NewDispatcher(), TraceWriter: discardTraceWriter{},
		Platform: platform.NewFakePlatform(), Store: reopenedRuns,
	})
	reopenedCoordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
		Sessions: reopenedSessions, Runs: reopenedRuns, Engine: reopenedRuntime,
	})
	if err != nil {
		t.Fatalf("New reopened: %v", err)
	}
	resumed, err := reopenedCoordinator.Resume(context.Background(), sessioncoordinator.ResumeRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if result, err := resumed.Next(context.Background()); err != nil || result.StepID != "work" {
		t.Fatalf("resumed first Next = %#v, %v", result, err)
	}
	if _, err := resumed.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("complete resumed run = %v", err)
	}
	if got := reopenedExecutor.executed(); len(got) != 1 || got[0] != "work" {
		t.Fatalf("resumed process executed %v", got)
	}
	if err := resumed.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

func TestCoordinatorResumeRejectsProjectionWithoutMutationBlob(t *testing.T) {
	root := t.TempDir()
	sessions := sessionstore.NewDirStore(root + "/sessions")
	runs := runstore.NewDirRunStore(root + "/runs")
	registry := executor.NewMapRegistry()
	registry.Register("noop", noopExecutor{})
	runtime := internalengine.New(engine.EngineConfig{
		Executors: registry, Dispatcher: eventbus.NewDispatcher(), TraceWriter: discardTraceWriter{},
		Platform: platform.NewFakePlatform(), Store: runs,
	})
	coordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
		Sessions: sessions, Runs: runs, Engine: runtime,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	plan := &engine.ExecutionPlan{
		RunbookPath: "missing-mutation.runbook.yaml",
		Steps:       []engine.ResolvedStep{{ID: "work", Kind: "noop", Spec: &schema.NoopSpec{}}},
		Metadata:    engine.PlanMetadata{RunbookID: "missing-mutation", RunbookName: "Missing Mutation"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	sessionID, segmentID, runID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	handle, err := coordinator.Start(context.Background(), sessioncoordinator.StartRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), SegmentID: segmentID, RunID: runID,
		Plan: plan, Graph: targetGraphJSON(t, plan),
		RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	mutationHash := handle.Manifest().Attempts[runID].ExecutionMutationHash
	if mutationHash == "" {
		t.Fatal("initial execution mutation is missing")
	}
	if err := runs.Close(); err != nil {
		t.Fatalf("close runs: %v", err)
	}
	if err := sessions.Close(); err != nil {
		t.Fatalf("close sessions: %v", err)
	}
	mutationPath := filepath.Join(
		root, "sessions", sessionID, "blobs", strings.TrimPrefix(mutationHash, "sha256:")+".json",
	)
	if err := os.Remove(mutationPath); err != nil {
		t.Fatalf("remove mutation blob: %v", err)
	}

	reopenedSessions := sessionstore.NewDirStore(root + "/sessions")
	reopenedRuns := runstore.NewDirRunStore(root + "/runs")
	t.Cleanup(func() {
		_ = reopenedRuns.Close()
		_ = reopenedSessions.Close()
	})
	forbiddenRuntime := &callCountingEngine{}
	reopenedCoordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
		Sessions: reopenedSessions, Runs: reopenedRuns, Engine: forbiddenRuntime,
	})
	if err != nil {
		t.Fatalf("New reopened: %v", err)
	}
	if _, err := reopenedCoordinator.Resume(context.Background(), sessioncoordinator.ResumeRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
	}); err == nil {
		t.Fatal("Resume unexpectedly trusted a projection without its mutation blob")
	}
	if forbiddenRuntime.startCalls != 0 || forbiddenRuntime.resumeCalls != 0 {
		t.Fatalf("engine calls = Start %d, Resume %d; want neither", forbiddenRuntime.startCalls, forbiddenRuntime.resumeCalls)
	}
}

func TestCoordinatorCrashAfterEngineSaveBeforeSessionCommitDoesNotRedispatch(t *testing.T) {
	root := t.TempDir()
	baseSessions := sessionstore.NewDirStore(root + "/sessions")
	firstSessions := &projectionErrorStore{Store: baseSessions}
	firstRuns := runstore.NewDirRunStore(root + "/runs")
	firstExecutor := &recordingExecutor{}
	firstRegistry := executor.NewMapRegistry()
	firstRegistry.Register("noop", firstExecutor)
	firstRuntime := internalengine.New(engine.EngineConfig{
		Executors: firstRegistry, Dispatcher: eventbus.NewDispatcher(), TraceWriter: discardTraceWriter{},
		Platform: platform.NewFakePlatform(), Store: firstRuns,
	})
	firstCoordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
		Sessions: firstSessions, Runs: firstRuns, Engine: firstRuntime,
	})
	if err != nil {
		t.Fatalf("New first: %v", err)
	}
	plan := &engine.ExecutionPlan{
		RunbookPath: "commit-authority.runbook.yaml",
		Steps: []engine.ResolvedStep{
			{ID: "first", Kind: "noop", Spec: &schema.NoopSpec{}},
			{ID: "second", Kind: "noop", Spec: &schema.NoopSpec{}},
		},
		Metadata: engine.PlanMetadata{RunbookID: "commit-authority", RunbookName: "Commit Authority"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	sessionID, segmentID, runID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	handle, err := firstCoordinator.Start(context.Background(), sessioncoordinator.StartRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), SegmentID: segmentID, RunID: runID,
		Plan: plan, Graph: targetGraphJSON(t, plan),
		RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result, err := handle.Next(context.Background()); err != nil || result.StepID != "first" {
		t.Fatalf("first Next = %#v, %v", result, err)
	}
	firstSessions.failMutationAfterCommit = true
	if result, err := handle.Next(context.Background()); result == nil || result.StepID != "second" || err == nil {
		t.Fatalf("second Next at crash boundary = %#v, %v", result, err)
	}
	if got := firstExecutor.executed(); len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("first process executed %v", got)
	}
	if err := firstRuns.Close(); err != nil {
		t.Fatalf("close first runs: %v", err)
	}
	if err := baseSessions.Close(); err != nil {
		t.Fatalf("close first sessions: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(root, "runs", runID)); err != nil {
		t.Fatalf("remove run projection: %v", err)
	}

	secondSessions := sessionstore.NewDirStore(root + "/sessions")
	secondRuns := runstore.NewDirRunStore(root + "/runs")
	t.Cleanup(func() {
		_ = secondRuns.Close()
		_ = secondSessions.Close()
	})
	secondExecutor := &recordingExecutor{}
	secondRegistry := executor.NewMapRegistry()
	secondRegistry.Register("noop", secondExecutor)
	secondRuntime := internalengine.New(engine.EngineConfig{
		Executors: secondRegistry, Dispatcher: eventbus.NewDispatcher(), TraceWriter: discardTraceWriter{},
		Platform: platform.NewFakePlatform(), Store: secondRuns,
	})
	secondCoordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
		Sessions: secondSessions, Runs: secondRuns, Engine: secondRuntime,
	})
	if err != nil {
		t.Fatalf("New second: %v", err)
	}
	resumed, err := secondCoordinator.Resume(context.Background(), sessioncoordinator.ResumeRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if _, err := resumed.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("resumed Next = %v, want EOF without redispatch", err)
	}
	if got := secondExecutor.executed(); len(got) != 0 {
		t.Fatalf("resumed process redispatched %v", got)
	}
}

func TestCoordinatorDispatchRequiresAuthoritativeIntentCommit(t *testing.T) {
	for _, test := range []struct {
		name         string
		beforeCommit bool
	}{
		{name: "commit fails before append", beforeCommit: true},
		{name: "commit succeeds before acknowledgment", beforeCommit: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			baseSessions := sessionstore.NewDirStore(root + "/sessions")
			firstSessions := &projectionErrorStore{Store: baseSessions}
			firstRuns := runstore.NewDirRunStore(root + "/runs")
			firstTools := testutil.NewFakeToolRuntime()
			firstTools.RegisterResult("remote", "mutate", &tool.ToolResult{Stdout: "done"})
			firstRegistry := executor.NewMapRegistry()
			firstRegistry.Register("tool", executor.NewToolExecutor(firstTools, nil))
			firstRegistry.Register("noop", noopExecutor{})
			firstRuntime := internalengine.New(engine.EngineConfig{
				Executors: firstRegistry, Dispatcher: eventbus.NewDispatcher(), TraceWriter: discardTraceWriter{},
				Platform: platform.NewFakePlatform(), Store: firstRuns,
			})
			firstCoordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
				Sessions: firstSessions, Runs: firstRuns, Engine: firstRuntime,
			})
			if err != nil {
				t.Fatalf("New first: %v", err)
			}
			plan := &engine.ExecutionPlan{
				RunbookPath: "dispatch-authority.runbook.yaml",
				Steps: []engine.ResolvedStep{
					{
						ID: "mutate", Kind: "tool", Spec: &schema.ToolCallSpec{
							Tool: schema.ToolInvocation{Name: "remote", Action: "mutate"},
						},
					},
					{ID: "inspect-after", Kind: "noop", Spec: &schema.NoopSpec{}},
				},
				Metadata: engine.PlanMetadata{RunbookID: "dispatch-authority", RunbookName: "Dispatch Authority"},
			}
			if err := planner.ValidateExecutionPlan(plan); err != nil {
				t.Fatalf("ValidateExecutionPlan: %v", err)
			}
			sessionID, segmentID, runID := uuid.NewString(), uuid.NewString(), uuid.NewString()
			handle, err := firstCoordinator.Start(context.Background(), sessioncoordinator.StartRequest{
				SessionID: sessionID, CommandID: uuid.NewString(), SegmentID: segmentID, RunID: runID,
				Plan: plan, Graph: targetGraphJSON(t, plan),
				RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
			})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			firstSessions.failMutationBeforeCommit = test.beforeCommit
			firstSessions.failMutationAfterCommit = !test.beforeCommit
			if _, err := handle.Next(context.Background()); err == nil {
				t.Fatal("Next unexpectedly survived injected intent commit crash")
			}
			if len(firstTools.Calls) != 0 {
				t.Fatalf("tool dispatched before authoritative intent acknowledgment: %#v", firstTools.Calls)
			}
			if err := firstRuns.Close(); err != nil {
				t.Fatalf("close first runs: %v", err)
			}
			if err := baseSessions.Close(); err != nil {
				t.Fatalf("close first sessions: %v", err)
			}

			secondSessions := sessionstore.NewDirStore(root + "/sessions")
			secondRuns := runstore.NewDirRunStore(root + "/runs")
			t.Cleanup(func() {
				_ = secondRuns.Close()
				_ = secondSessions.Close()
			})
			secondTools := testutil.NewFakeToolRuntime()
			secondTools.RegisterResult("remote", "mutate", &tool.ToolResult{Stdout: "done"})
			secondNoops := &recordingExecutor{}
			secondRegistry := executor.NewMapRegistry()
			secondRegistry.Register("tool", executor.NewToolExecutor(secondTools, nil))
			secondRegistry.Register("noop", secondNoops)
			secondRuntime := internalengine.New(engine.EngineConfig{
				Executors: secondRegistry, Dispatcher: eventbus.NewDispatcher(), TraceWriter: discardTraceWriter{},
				Platform: platform.NewFakePlatform(), Store: secondRuns,
			})
			secondCoordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
				Sessions: secondSessions, Runs: secondRuns, Engine: secondRuntime,
			})
			if err != nil {
				t.Fatalf("New second: %v", err)
			}
			if test.beforeCommit {
				inspection, inspectErr := secondCoordinator.Inspect(context.Background(), sessionID)
				if inspectErr != nil {
					t.Fatalf("Inspect authoritative checkpoint: %v", inspectErr)
				}
				if inspection.Run == nil || inspection.Run.CheckpointSequence != 0 || len(inspection.Run.Dispatches) != 0 {
					t.Fatalf("Inspect trusted unjournaled intent: %#v", inspection.Run)
				}
			}
			resumed, err := secondCoordinator.Resume(context.Background(), sessioncoordinator.ResumeRequest{
				SessionID: sessionID, CommandID: uuid.NewString(), RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
			})
			if !test.beforeCommit {
				if !errors.Is(err, engine.ErrIndeterminateAcknowledgmentRequired) {
					t.Fatalf("Resume error = %v, want ErrIndeterminateAcknowledgmentRequired", err)
				}
				if len(secondTools.Calls) != 0 {
					t.Fatalf("committed unmatched intent was redispatched: %#v", secondTools.Calls)
				}
				acknowledged, acknowledgeErr := secondCoordinator.Resume(context.Background(), sessioncoordinator.ResumeRequest{
					SessionID: sessionID, CommandID: uuid.NewString(), RunOptions: engine.RunOptions{
						Mode: engine.RunModeReal, AcknowledgeIndeterminate: true,
					},
				})
				if acknowledgeErr != nil {
					t.Fatalf("acknowledged Resume: %v", acknowledgeErr)
				}
				if result, nextErr := acknowledged.Next(context.Background()); nextErr != nil || result.StepID != "inspect-after" {
					t.Fatalf("acknowledged Next = %#v, %v", result, nextErr)
				}
				if len(secondTools.Calls) != 0 {
					t.Fatalf("acknowledged resume redispatched tool: %#v", secondTools.Calls)
				}
				if got := secondNoops.executed(); len(got) != 1 || got[0] != "inspect-after" {
					t.Fatalf("acknowledged resume executed %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resume after rejected intent: %v", err)
			}
			if result, err := resumed.Next(context.Background()); err != nil || result.StepID != "mutate" {
				t.Fatalf("resumed Next = %#v, %v", result, err)
			}
			if len(secondTools.Calls) != 1 {
				t.Fatalf("accepted retry dispatched %d times", len(secondTools.Calls))
			}
		})
	}
}

func TestCoordinatorResultCommitFailureAfterDispatchDoesNotRedispatch(t *testing.T) {
	root := t.TempDir()
	baseSessions := sessionstore.NewDirStore(root + "/sessions")
	firstSessions := &projectionErrorStore{Store: baseSessions}
	firstRuns := runstore.NewDirRunStore(root + "/runs")
	firstTools := testutil.NewFakeToolRuntime()
	firstTools.RegisterResult("remote", "mutate", &tool.ToolResult{Stdout: "done"})
	firstRegistry := executor.NewMapRegistry()
	firstRegistry.Register("tool", executor.NewToolExecutor(firstTools, nil))
	firstRegistry.Register("noop", noopExecutor{})
	firstRuntime := internalengine.New(engine.EngineConfig{
		Executors: firstRegistry, Dispatcher: eventbus.NewDispatcher(), TraceWriter: discardTraceWriter{},
		Platform: platform.NewFakePlatform(), Store: firstRuns,
	})
	firstCoordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
		Sessions: firstSessions, Runs: firstRuns, Engine: firstRuntime,
	})
	if err != nil {
		t.Fatalf("New first: %v", err)
	}
	plan := &engine.ExecutionPlan{
		RunbookPath: "result-commit.runbook.yaml",
		Steps: []engine.ResolvedStep{
			{ID: "mutate", Kind: "tool", Spec: &schema.ToolCallSpec{
				Tool: schema.ToolInvocation{Name: "remote", Action: "mutate"},
			}},
			{ID: "inspect-after", Kind: "noop", Spec: &schema.NoopSpec{}},
		},
		Metadata: engine.PlanMetadata{RunbookID: "result-commit", RunbookName: "Result Commit"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	sessionID, segmentID, runID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	handle, err := firstCoordinator.Start(context.Background(), sessioncoordinator.StartRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), SegmentID: segmentID, RunID: runID,
		Plan: plan, Graph: targetGraphJSON(t, plan),
		RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	failedResultCommit := false
	firstSessions.failMutationWhen = func(session.ExecutionMutationRequest) bool {
		if !failedResultCommit && len(firstTools.Calls) > 0 {
			failedResultCommit = true
			return true
		}
		return false
	}
	if _, err := handle.Next(context.Background()); err == nil {
		t.Fatal("Next unexpectedly survived rejected result commit")
	}
	if !failedResultCommit || len(firstTools.Calls) != 1 {
		t.Fatalf("provider calls=%d failed result commit=%t", len(firstTools.Calls), failedResultCommit)
	}
	if err := firstRuns.Close(); err != nil {
		t.Fatalf("close first runs: %v", err)
	}
	if err := baseSessions.Close(); err != nil {
		t.Fatalf("close first sessions: %v", err)
	}

	secondSessions := sessionstore.NewDirStore(root + "/sessions")
	secondRuns := runstore.NewDirRunStore(root + "/runs")
	t.Cleanup(func() {
		_ = secondRuns.Close()
		_ = secondSessions.Close()
	})
	secondTools := testutil.NewFakeToolRuntime()
	secondTools.RegisterResult("remote", "mutate", &tool.ToolResult{Stdout: "done"})
	secondNoops := &recordingExecutor{}
	secondRegistry := executor.NewMapRegistry()
	secondRegistry.Register("tool", executor.NewToolExecutor(secondTools, nil))
	secondRegistry.Register("noop", secondNoops)
	secondRuntime := internalengine.New(engine.EngineConfig{
		Executors: secondRegistry, Dispatcher: eventbus.NewDispatcher(), TraceWriter: discardTraceWriter{},
		Platform: platform.NewFakePlatform(), Store: secondRuns,
	})
	secondCoordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
		Sessions: secondSessions, Runs: secondRuns, Engine: secondRuntime,
	})
	if err != nil {
		t.Fatalf("New second: %v", err)
	}
	if _, err := secondCoordinator.Resume(context.Background(), sessioncoordinator.ResumeRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
	}); !errors.Is(err, engine.ErrIndeterminateAcknowledgmentRequired) {
		t.Fatalf("Resume error = %v, want ErrIndeterminateAcknowledgmentRequired", err)
	}
	if len(secondTools.Calls) != 0 {
		t.Fatalf("result recovery redispatched tool: %#v", secondTools.Calls)
	}
	acknowledged, err := secondCoordinator.Resume(context.Background(), sessioncoordinator.ResumeRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), RunOptions: engine.RunOptions{
			Mode: engine.RunModeReal, AcknowledgeIndeterminate: true,
		},
	})
	if err != nil {
		t.Fatalf("acknowledged Resume: %v", err)
	}
	if result, err := acknowledged.Next(context.Background()); err != nil || result.StepID != "inspect-after" {
		t.Fatalf("acknowledged Next = %#v, %v", result, err)
	}
	if len(secondTools.Calls) != 0 || len(secondNoops.executed()) != 1 {
		t.Fatalf("acknowledged calls: tool=%d noop=%v", len(secondTools.Calls), secondNoops.executed())
	}
}

func TestCoordinatorDirectTraceDoesNotLeadAuthoritativeSessionState(t *testing.T) {
	root := t.TempDir()
	externalTracePath := filepath.Join(root, "external-trace.jsonl")
	firstTrace, err := internaltrace.NewJSONLWriter(externalTracePath)
	if err != nil {
		t.Fatalf("NewJSONLWriter first: %v", err)
	}
	baseSessions := sessionstore.NewDirStore(root + "/sessions")
	firstSessions := &projectionErrorStore{Store: baseSessions}
	firstRuns := runstore.NewDirRunStore(root + "/runs")
	firstTools := testutil.NewFakeToolRuntime()
	firstTools.RegisterResult("remote", "mutate", &tool.ToolResult{Stdout: "done"})
	firstRegistry := executor.NewMapRegistry()
	firstRegistry.Register("tool", executor.NewToolExecutor(firstTools, nil))
	firstRuntime := internalengine.New(engine.EngineConfig{
		Executors: firstRegistry, Dispatcher: eventbus.NewDispatcher(), TraceWriter: firstTrace,
		Platform: platform.NewFakePlatform(), Store: firstRuns,
	})
	firstCoordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
		Sessions: firstSessions, Runs: firstRuns, Engine: firstRuntime,
	})
	if err != nil {
		t.Fatalf("New first: %v", err)
	}
	plan := &engine.ExecutionPlan{
		RunbookPath: "trace-authority.runbook.yaml",
		Steps: []engine.ResolvedStep{{
			ID: "mutate", Kind: "tool", Spec: &schema.ToolCallSpec{
				Tool: schema.ToolInvocation{Name: "remote", Action: "mutate"},
			},
		}},
		Metadata: engine.PlanMetadata{RunbookID: "trace-authority", RunbookName: "Trace Authority"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	sessionID, segmentID, runID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	handle, err := firstCoordinator.Start(context.Background(), sessioncoordinator.StartRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), SegmentID: segmentID, RunID: runID,
		Plan: plan, Graph: targetGraphJSON(t, plan),
		RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	firstSessions.failMutationBeforeCommit = true
	if _, err := handle.Next(context.Background()); err == nil {
		t.Fatal("Next unexpectedly survived rejected intent")
	}
	if err := firstTrace.Close(); err != nil {
		t.Fatalf("close first trace: %v", err)
	}
	if err := firstRuns.Close(); err != nil {
		t.Fatalf("close first runs: %v", err)
	}
	if err := baseSessions.Close(); err != nil {
		t.Fatalf("close first sessions: %v", err)
	}

	secondTrace, err := internaltrace.NewJSONLWriter(externalTracePath)
	if err != nil {
		t.Fatalf("NewJSONLWriter second: %v", err)
	}
	defer secondTrace.Close()
	secondSessions := sessionstore.NewDirStore(root + "/sessions")
	secondRuns := runstore.NewDirRunStore(root + "/runs")
	t.Cleanup(func() {
		_ = secondRuns.Close()
		_ = secondSessions.Close()
	})
	secondTools := testutil.NewFakeToolRuntime()
	secondTools.RegisterResult("remote", "mutate", &tool.ToolResult{Stdout: "done"})
	secondRegistry := executor.NewMapRegistry()
	secondRegistry.Register("tool", executor.NewToolExecutor(secondTools, nil))
	secondRuntime := internalengine.New(engine.EngineConfig{
		Executors: secondRegistry, Dispatcher: eventbus.NewDispatcher(), TraceWriter: secondTrace,
		Platform: platform.NewFakePlatform(), Store: secondRuns,
	})
	secondCoordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
		Sessions: secondSessions, Runs: secondRuns, Engine: secondRuntime,
	})
	if err != nil {
		t.Fatalf("New second: %v", err)
	}
	resumed, err := secondCoordinator.Resume(context.Background(), sessioncoordinator.ResumeRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
	})
	if err != nil {
		t.Fatalf("Resume with external trace projection: %v", err)
	}
	if result, err := resumed.Next(context.Background()); err != nil || result.StepID != "mutate" {
		t.Fatalf("resumed Next = %#v, %v", result, err)
	}
	if len(secondTools.Calls) != 1 {
		t.Fatalf("resumed tool calls = %d, want one", len(secondTools.Calls))
	}
}

func TestCoordinatorTraceProjectionFailureRebuildsFromSessionJournal(t *testing.T) {
	for _, test := range []struct {
		name           string
		failRunTrace   bool
		failConfigured bool
	}{
		{name: "run trace", failRunTrace: true},
		{name: "configured trace", failConfigured: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			sessions := sessionstore.NewDirStore(root + "/sessions")
			runs := &failingRunTraceProjectionStore{DirRunStore: runstore.NewDirRunStore(root + "/runs")}
			writer := &failingConfiguredTraceWriter{}
			t.Cleanup(func() {
				_ = runs.Close()
				_ = sessions.Close()
			})
			registry := executor.NewMapRegistry()
			registry.Register("noop", noopExecutor{})
			runtime := internalengine.New(engine.EngineConfig{
				Executors: registry, Dispatcher: eventbus.NewDispatcher(), TraceWriter: writer,
				Platform: platform.NewFakePlatform(), Store: runs,
			})
			coordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
				Sessions: sessions, Runs: runs, Engine: runtime,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			plan := &engine.ExecutionPlan{
				RunbookPath: "trace-projection.runbook.yaml",
				Steps:       []engine.ResolvedStep{{ID: "work", Kind: "noop", Spec: &schema.NoopSpec{}}},
				Metadata:    engine.PlanMetadata{RunbookID: "trace-projection", RunbookName: "Trace Projection"},
			}
			if err := planner.ValidateExecutionPlan(plan); err != nil {
				t.Fatalf("ValidateExecutionPlan: %v", err)
			}
			sessionID, runID := uuid.NewString(), uuid.NewString()
			handle, err := coordinator.Start(context.Background(), sessioncoordinator.StartRequest{
				SessionID: sessionID, CommandID: uuid.NewString(), SegmentID: uuid.NewString(), RunID: runID,
				Plan: plan, Graph: targetGraphJSON(t, plan),
				RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
			})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			runs.fail = test.failRunTrace
			writer.fail = test.failConfigured
			if result, err := handle.Next(context.Background()); err != nil || result.StepID != "work" {
				t.Fatalf("Next after trace projection failure = %#v, %v", result, err)
			}
			events, err := sessions.ReadEvents(context.Background(), sessionID, 0)
			if err != nil {
				t.Fatalf("ReadEvents: %v", err)
			}
			foundTrace := false
			for _, event := range events {
				foundTrace = foundTrace || event.Kind == session.EventTraceCommitted
			}
			if !foundTrace {
				t.Fatal("direct trace was not committed to the session journal")
			}
			if err := runs.Close(); err != nil {
				t.Fatalf("close first runs: %v", err)
			}
			if err := sessions.Close(); err != nil {
				t.Fatalf("close first sessions: %v", err)
			}
			reopenedSessions := sessionstore.NewDirStore(root + "/sessions")
			reopenedRuns := runstore.NewDirRunStore(root + "/runs")
			defer reopenedSessions.Close()
			defer reopenedRuns.Close()
			reopenedExecutor := &recordingExecutor{}
			reopenedRegistry := executor.NewMapRegistry()
			reopenedRegistry.Register("noop", reopenedExecutor)
			var reopenedTrace trace.TraceWriter = discardTraceWriter{}
			if test.failConfigured {
				writer.mu.Lock()
				writer.fail = false
				writer.mu.Unlock()
				reopenedTrace = writer
			}
			reopenedRuntime := internalengine.New(engine.EngineConfig{
				Executors: reopenedRegistry, Dispatcher: eventbus.NewDispatcher(), TraceWriter: reopenedTrace,
				Platform: platform.NewFakePlatform(), Store: reopenedRuns,
			})
			reopenedCoordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
				Sessions: reopenedSessions, Runs: reopenedRuns, Engine: reopenedRuntime,
			})
			if err != nil {
				t.Fatalf("New reopened: %v", err)
			}
			resumed, err := reopenedCoordinator.Resume(context.Background(), sessioncoordinator.ResumeRequest{
				SessionID: sessionID, CommandID: uuid.NewString(), RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
			})
			if err != nil {
				t.Fatalf("Resume after projection failure: %v", err)
			}
			if _, err := resumed.Next(context.Background()); !errors.Is(err, io.EOF) {
				t.Fatalf("resumed Next = %v, want EOF", err)
			}
			if got := reopenedExecutor.executed(); len(got) != 0 {
				t.Fatalf("projection recovery re-executed %v", got)
			}
			if test.failConfigured {
				attempt := resumed.Manifest().Attempts[runID]
				lastSequence := attempt.CommittedTraceSequence
				if attempt.JournaledTraceSequence > lastSequence {
					lastSequence = attempt.JournaledTraceSequence
				}
				projected := writer.collected()
				if len(projected) != int(lastSequence) {
					t.Fatalf("configured trace projected %d events, want %d", len(projected), lastSequence)
				}
				for index, event := range projected {
					if event.Sequence != int64(index+1) {
						t.Fatalf("configured trace event %d has sequence %d", index, event.Sequence)
					}
				}
			}
		})
	}
}

func TestCoordinatorRecoversCommittedProjectionErrorsWithoutCancellingRun(t *testing.T) {
	for _, test := range []struct {
		name       string
		failCreate bool
		failUpdate bool
	}{
		{name: "creation projection", failCreate: true},
		{name: "attempt projection", failUpdate: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			baseSessions := sessionstore.NewDirStore(root + "/sessions")
			store := &projectionErrorStore{Store: baseSessions, failCreate: test.failCreate, failUpdate: test.failUpdate}
			runs := runstore.NewDirRunStore(root + "/runs")
			t.Cleanup(func() {
				_ = runs.Close()
				_ = baseSessions.Close()
			})
			registry := executor.NewMapRegistry()
			registry.Register("noop", noopExecutor{})
			runtime := internalengine.New(engine.EngineConfig{
				Executors: registry, Dispatcher: eventbus.NewDispatcher(), TraceWriter: discardTraceWriter{},
				Platform: platform.NewFakePlatform(), Store: runs,
			})
			coordinator, err := sessioncoordinator.New(sessioncoordinator.Config{Sessions: store, Runs: runs, Engine: runtime})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			plan := &engine.ExecutionPlan{
				RunbookPath: "projection.runbook.yaml",
				Steps:       []engine.ResolvedStep{{ID: "work", Kind: "noop", Spec: &schema.NoopSpec{}}},
				Metadata:    engine.PlanMetadata{RunbookID: "projection", RunbookName: "Projection"},
			}
			if err := planner.ValidateExecutionPlan(plan); err != nil {
				t.Fatalf("ValidateExecutionPlan: %v", err)
			}
			handle, err := coordinator.Start(context.Background(), sessioncoordinator.StartRequest{
				SessionID: uuid.NewString(), CommandID: uuid.NewString(), SegmentID: uuid.NewString(), RunID: uuid.NewString(),
				Plan: plan, Graph: targetGraphJSON(t, plan),
				RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
			})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if result, err := handle.Next(context.Background()); err != nil || result.StepID != "work" {
				t.Fatalf("Next after projection repair = %#v, %v", result, err)
			}
			if _, err := handle.Next(context.Background()); !errors.Is(err, io.EOF) {
				t.Fatalf("complete after projection repair: %v", err)
			}
			if err := handle.Release(); err != nil {
				t.Fatalf("Release: %v", err)
			}
		})
	}
}

func TestCoordinatorRecoversCommittedPauseAndResumeProjectionErrors(t *testing.T) {
	root := t.TempDir()
	baseSessions := sessionstore.NewDirStore(root + "/sessions")
	store := &projectionErrorStore{Store: baseSessions, failPause: true}
	runs := runstore.NewDirRunStore(root + "/runs")
	t.Cleanup(func() {
		_ = runs.Close()
		_ = baseSessions.Close()
	})
	registry := executor.NewMapRegistry()
	registry.Register("noop", noopExecutor{})
	runtime := internalengine.New(engine.EngineConfig{
		Executors: registry, Dispatcher: eventbus.NewDispatcher(), TraceWriter: discardTraceWriter{},
		Platform: platform.NewFakePlatform(), Store: runs,
	})
	coordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
		Sessions: store, Runs: runs, Engine: runtime,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	plan := &engine.ExecutionPlan{
		RunbookPath: "pause-resume-projection.runbook.yaml",
		Steps:       []engine.ResolvedStep{{ID: "work", Kind: "noop", Spec: &schema.NoopSpec{}}},
		Metadata:    engine.PlanMetadata{RunbookID: "pause-resume-projection", RunbookName: "Pause resume projection"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	sessionID, runID := uuid.NewString(), uuid.NewString()
	handle, err := coordinator.Start(context.Background(), sessioncoordinator.StartRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), SegmentID: uuid.NewString(), RunID: runID,
		Plan: plan, Graph: targetGraphJSON(t, plan), RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	pauseCommandID := uuid.NewString()
	paused, err := handle.Detach(context.Background(), pauseCommandID, "projection failure")
	if err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if paused.Session.Status != session.StatusPaused ||
		paused.AcceptedCommands[pauseCommandID].EventKind != session.EventSessionPaused {
		t.Fatalf("paused manifest = %#v", paused)
	}
	store.failResume = true
	resumeCommandID := uuid.NewString()
	resumed, err := coordinator.Resume(context.Background(), sessioncoordinator.ResumeRequest{
		SessionID: sessionID, CommandID: resumeCommandID,
		RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	resumedManifest := resumed.Manifest()
	if resumedManifest.Session.Status != session.StatusActive ||
		resumedManifest.AcceptedCommands[resumeCommandID].EventKind != session.EventSessionResumed {
		t.Fatalf("resumed manifest = %#v", resumedManifest)
	}
	if _, err := resumed.Detach(context.Background(), uuid.NewString(), "cleanup"); err != nil {
		t.Fatalf("cleanup Detach: %v", err)
	}
}

func TestCoordinatorOnEventCanInspectManifestWithoutDeadlock(t *testing.T) {
	root := t.TempDir()
	sessions := sessionstore.NewDirStore(root + "/sessions")
	runs := runstore.NewDirRunStore(root + "/runs")
	t.Cleanup(func() {
		_ = runs.Close()
		_ = sessions.Close()
	})
	registry := executor.NewMapRegistry()
	registry.Register("noop", noopExecutor{})
	runtime := internalengine.New(engine.EngineConfig{
		Executors: registry, Dispatcher: eventbus.NewDispatcher(), TraceWriter: discardTraceWriter{},
		Platform: platform.NewFakePlatform(), Store: runs,
	})
	coordinator, err := sessioncoordinator.New(sessioncoordinator.Config{Sessions: sessions, Runs: runs, Engine: runtime})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	plan := &engine.ExecutionPlan{
		RunbookPath: "callback.runbook.yaml",
		Steps:       []engine.ResolvedStep{{ID: "work", Kind: "noop", Spec: &schema.NoopSpec{}}},
		Metadata:    engine.PlanMetadata{RunbookID: "callback", RunbookName: "Callback"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	type lifecycleCheckpoint struct {
		kind       string
		checkpoint int64
	}
	callbackCheckpoint := make(chan lifecycleCheckpoint, 2)
	sessionID, segmentID, runID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	var handle *sessioncoordinator.Handle
	handle, err = coordinator.Start(context.Background(), sessioncoordinator.StartRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), SegmentID: segmentID, RunID: runID,
		Plan: plan, Graph: targetGraphJSON(t, plan),
		RunOptions: engine.RunOptions{Mode: engine.RunModeReal, OnEvent: func(event engine.Event) {
			checkpoint := handle.Manifest().Attempts[runID].CheckpointSequence
			if event.Kind == "step/started" || event.Kind == "step/completed" {
				callbackCheckpoint <- lifecycleCheckpoint{kind: event.Kind, checkpoint: checkpoint}
			}
		}},
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
	case observed := <-callbackCheckpoint:
		if observed.kind != "step/started" || observed.checkpoint != 0 {
			t.Fatalf("start must publish before execution commit: %#v", observed)
		}
	case <-time.After(time.Second):
		t.Fatal("OnEvent deadlocked while inspecting Manifest")
	}
	select {
	case observed := <-callbackCheckpoint:
		if observed.kind != "step/completed" || observed.checkpoint < 1 {
			t.Fatalf("terminal must publish after committed step checkpoint: %#v", observed)
		}
	case <-time.After(time.Second):
		t.Fatal("terminal callback missing")
	}
	select {
	case nextErr := <-nextDone:
		if nextErr != nil {
			t.Fatalf("Next: %v", nextErr)
		}
	case <-time.After(time.Second):
		t.Fatal("Next did not finish after callback")
	}
}

func TestCoordinatorStartRetryReusesCommittedCreationAndPreallocatedIDs(t *testing.T) {
	root := t.TempDir()
	sessions := sessionstore.NewDirStore(root + "/sessions")
	runs := runstore.NewDirRunStore(root + "/runs")
	t.Cleanup(func() {
		_ = runs.Close()
		_ = sessions.Close()
	})
	registry := executor.NewMapRegistry()
	registry.Register("noop", noopExecutor{})
	inner := internalengine.New(engine.EngineConfig{
		Executors: registry, Dispatcher: eventbus.NewDispatcher(), TraceWriter: discardTraceWriter{},
		Platform: platform.NewFakePlatform(), Store: runs,
	})
	runtime := &failFirstStartEngine{inner: inner}
	coordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
		Sessions: sessions, Runs: runs, Engine: runtime,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	plan := &engine.ExecutionPlan{
		RunbookPath: "retry.runbook.yaml",
		Steps:       []engine.ResolvedStep{{ID: "work", Kind: "noop", Spec: &schema.NoopSpec{}}},
		Metadata:    engine.PlanMetadata{RunbookID: "retry", RunbookName: "Retry"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	request := sessioncoordinator.StartRequest{
		SessionID: uuid.NewString(), CommandID: uuid.NewString(), SegmentID: uuid.NewString(), RunID: uuid.NewString(),
		Plan: plan, Graph: targetGraphJSON(t, plan),
		RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
	}
	if _, err := coordinator.Start(context.Background(), request); err == nil {
		t.Fatal("first Start unexpectedly succeeded")
	}
	manifest, err := sessions.LoadManifest(context.Background(), request.SessionID)
	if err != nil {
		t.Fatalf("LoadManifest after lost start: %v", err)
	}
	if manifest.Session.Sequence != 1 || manifest.Session.ActiveRunID != request.RunID ||
		manifest.Attempts[request.RunID].Status != session.AttemptStatusStarting {
		t.Fatalf("prepared creation = %#v", manifest)
	}
	projectionLease, err := runs.AcquireRunLease(context.Background(), request.RunID)
	if err != nil {
		t.Fatalf("acquire stray projection lease: %v", err)
	}
	strayPlan := *plan
	strayPlan.RunID = request.RunID
	if err := runs.SavePlan(context.Background(), request.RunID, &strayPlan); err != nil {
		t.Fatalf("save stray plan: %v", err)
	}
	if err := runs.SaveState(context.Background(), engine.RunState{
		RunID: request.RunID, RunbookPath: plan.RunbookPath, WriterEpoch: projectionLease.Epoch(),
		Status: engine.RunStatusRunning, CurrentStepIndex: -1,
	}); err != nil {
		t.Fatalf("save stray state: %v", err)
	}
	if err := projectionLease.Release(); err != nil {
		t.Fatalf("release stray projection lease: %v", err)
	}
	inspection, err := coordinator.Inspect(context.Background(), request.SessionID)
	if err != nil {
		t.Fatalf("Inspect starting attempt: %v", err)
	}
	if inspection.Run != nil {
		t.Fatalf("Inspect exposed uncommitted local state: %#v", inspection.Run)
	}
	changedVars := request
	changedVars.RunOptions.Vars = map[string]string{"region": "westus"}
	if _, err := coordinator.Start(context.Background(), changedVars); !errors.Is(err, session.ErrCreationConflict) {
		t.Fatalf("changed Vars retry error = %v, want ErrCreationConflict", err)
	}
	changedRuntimeVars := request
	changedRuntimeVars.RunOptions.RuntimeVars = map[string]any{"attempt": 2}
	if _, err := coordinator.Start(context.Background(), changedRuntimeVars); !errors.Is(err, session.ErrCreationConflict) {
		t.Fatalf("changed RuntimeVars retry error = %v, want ErrCreationConflict", err)
	}
	changedProvenance := request
	changedProvenance.DerivedFromSessionID = uuid.NewString()
	if _, err := coordinator.Start(context.Background(), changedProvenance); !errors.Is(err, session.ErrCreationConflict) {
		t.Fatalf("changed provenance retry error = %v, want ErrCreationConflict", err)
	}
	handle, err := coordinator.Start(context.Background(), request)
	if err != nil {
		t.Fatalf("retried Start: %v", err)
	}
	events, err := sessions.ReadEvents(context.Background(), request.SessionID, 0)
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	if len(events) != 2 || events[0].Kind != session.EventSessionCreated || events[1].Kind != session.EventExecutionCommitted {
		t.Fatalf("retried creation events = %#v", events)
	}
	if handle.Manifest().Session.ActiveRunID != request.RunID || handle.Manifest().Segments[request.SegmentID].SegmentID == "" {
		t.Fatalf("retried IDs changed: %#v", handle.Manifest())
	}
	if err := handle.Release(); !errors.Is(err, session.ErrAttemptActive) {
		t.Fatalf("active Release error = %v, want ErrAttemptActive", err)
	}
	if _, err := handle.CloseInvestigation(context.Background(), uuid.NewString(), session.StatusResolved); !errors.Is(err, session.ErrAttemptActive) {
		t.Fatalf("active CloseInvestigation error = %v, want ErrAttemptActive", err)
	}
	if _, err := handle.Next(context.Background()); err != nil {
		t.Fatalf("execute retried run: %v", err)
	}
	if _, err := handle.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("complete retried run: %v", err)
	}
	if err := handle.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}
