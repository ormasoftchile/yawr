package sessionstdio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	internalengine "github.com/ormasoftchile/yawr/runtime/internal/engine"
	"github.com/ormasoftchile/yawr/runtime/internal/eventbus"
	"github.com/ormasoftchile/yawr/runtime/internal/executor"
	internalinput "github.com/ormasoftchile/yawr/runtime/internal/input"
	"github.com/ormasoftchile/yawr/runtime/internal/runstore"
	"github.com/ormasoftchile/yawr/runtime/internal/serve"
	"github.com/ormasoftchile/yawr/runtime/internal/sessioncoordinator"
	"github.com/ormasoftchile/yawr/runtime/internal/sessionstore"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/input"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/session"
	tracepkg "github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

type runtimeDiscardTrace struct{}

func (runtimeDiscardTrace) Append(tracepkg.TraceEvent) error { return nil }
func (runtimeDiscardTrace) Close() error                     { return nil }

type toggleFrameWriter struct {
	mu   sync.Mutex
	fail bool
}

type matchingFailFrameWriter struct {
	mu      sync.Mutex
	match   []byte
	failed  bool
	matched chan struct{}
	once    sync.Once
}

func (writer *matchingFailFrameWriter) Write(data []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.failed || bytes.Contains(data, writer.match) {
		writer.failed = true
		writer.once.Do(func() { close(writer.matched) })
		return 0, io.ErrClosedPipe
	}
	return len(data), nil
}

type blockingPreparedDispatchExecutor struct {
	started chan struct{}
}

type countingExternalExecutor struct {
	mu          sync.Mutex
	invocations int
}

type startupVarsExecutor struct {
	started chan map[string]any
}

func (executor *startupVarsExecutor) Execute(
	_ context.Context,
	step engine.ResolvedStep,
	vars map[string]any,
) (*engine.StepResult, error) {
	captured := make(map[string]any, len(vars))
	for name, value := range vars {
		captured[name] = value
	}
	executor.started <- captured
	return &engine.StepResult{
		StepID: step.ID, Status: engine.StepStatusCompleted, Outcome: engine.StepOutcomeSuccess,
	}, nil
}

func (executor *countingExternalExecutor) Execute(
	ctx context.Context,
	step engine.ResolvedStep,
	_ map[string]any,
) (*engine.StepResult, error) {
	if _, err := engine.PrepareExternalDispatch(ctx, engine.DispatchRequest{
		Classification: "mutating", EndpointIdentity: "session-stdio-approval",
		RenderedRequest: map[string]any{"step": step.ID},
	}); err != nil {
		return nil, err
	}
	executor.mu.Lock()
	executor.invocations++
	executor.mu.Unlock()
	return &engine.StepResult{
		StepID: step.ID, Status: engine.StepStatusCompleted, Outcome: engine.StepOutcomeSuccess,
	}, nil
}

func (executor *countingExternalExecutor) invocationCount() int {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return executor.invocations
}

func (executor *blockingPreparedDispatchExecutor) Execute(
	ctx context.Context,
	step engine.ResolvedStep,
	_ map[string]any,
) (*engine.StepResult, error) {
	if _, err := engine.PrepareExternalDispatch(ctx, engine.DispatchRequest{
		Classification: "mutating", EndpointIdentity: "session-stdio-output-loss",
		RenderedRequest: map[string]any{"step": step.ID},
	}); err != nil {
		return nil, err
	}
	close(executor.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

func (writer *toggleFrameWriter) Write(data []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.fail {
		return 0, io.ErrClosedPipe
	}
	return len(data), nil
}

func (writer *toggleFrameWriter) Fail() {
	writer.mu.Lock()
	writer.fail = true
	writer.mu.Unlock()
}

func TestCoordinatorRuntimeConfiguresPrivateInputsBeforeStarting(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	root := t.TempDir()
	sessions := sessionstore.NewDirStore(filepath.Join(root, "sessions"))
	runs := runstore.NewDirRunStore(filepath.Join(root, "runs"))
	t.Cleanup(func() {
		_ = runs.Close()
		_ = sessions.Close()
	})

	sessionID, runID := uuid.NewString(), uuid.NewString()
	captureExecutor := &startupVarsExecutor{started: make(chan map[string]any, 1)}
	registry := executor.NewMapRegistry()
	registry.Register("noop", captureExecutor)
	engineRuntime := internalengine.New(engine.EngineConfig{
		Executors: registry, Dispatcher: eventbus.NewDispatcher(), TraceWriter: runtimeDiscardTrace{},
		Platform: platform.NewFakePlatform(), Store: runs,
	})
	coordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
		Sessions: sessions, Runs: runs, Engine: engineRuntime,
	})
	if err != nil {
		t.Fatalf("New coordinator: %v", err)
	}
	plan := engine.ValidatedForTest(&engine.ExecutionPlan{
		RunID: runID, RunbookPath: "configured.runbook.yaml",
		Inputs: map[string]*schema.Input{
			"access_token": {Type: "secret"},
		},
		Steps:    []engine.ResolvedStep{{ID: "capture", Kind: "noop", Spec: &schema.NoopSpec{}}},
		Metadata: engine.PlanMetadata{RunbookID: "configured", RunbookName: "Configured"},
	})
	handle, err := coordinator.Start(ctx, sessioncoordinator.StartRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), SegmentID: uuid.NewString(), RunID: runID,
		Plan: plan, Graph: runtimeGraph(t, plan),
		RunOptions: engine.RunOptions{Mode: engine.RunModeReal, Vars: map[string]string{"region": "westus"}},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var output bytes.Buffer
	attachment, err := Adopt(ctx, sessions, sessionID, 0, &output, handle)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	runtime, err := NewCoordinatorRuntime(coordinator, serve.NewPromptBroker(16), engine.RunOptions{Mode: engine.RunModeReal})
	if err != nil {
		t.Fatalf("NewCoordinatorRuntime: %v", err)
	}
	runtime.RequireStartupConfiguration()
	attachment.WithCommandHandler(runtime)
	if err := runtime.StartAttached(ctx, attachment, handle); err != nil {
		t.Fatalf("StartAttached: %v", err)
	}
	select {
	case vars := <-captureExecutor.started:
		t.Fatalf("execution started before session.configure with vars %#v", vars)
	case <-time.After(100 * time.Millisecond):
	}

	manifest, err := sessions.LoadManifest(ctx, sessionID)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	invalidPayload, err := json.Marshal(map[string]any{"inputs": map[string]string{
		"access_token": "must-not-survive-rejection",
		"region":       "override",
	}})
	if err != nil {
		t.Fatalf("marshal invalid configure payload: %v", err)
	}
	invalidCommand := session.StdioCommand{
		Version: session.StdioProtocolV1, Type: session.CommandSessionConfigure,
		CommandID: uuid.NewString(), SessionID: sessionID, WriterEpoch: attachment.WriterEpoch(),
		ExpectedSequence: manifest.Session.Sequence, RunID: runID, Payload: invalidPayload,
	}
	if detached, err := attachment.HandleCommand(ctx, invalidCommand); err == nil || detached {
		t.Fatalf("invalid configure detached/error = %v/%v", detached, err)
	}
	select {
	case vars := <-captureExecutor.started:
		t.Fatalf("execution started after rejected session.configure with vars %#v", vars)
	case <-time.After(100 * time.Millisecond):
	}

	const privateValue = "private-value-never-journal"
	payload, err := json.Marshal(map[string]any{"inputs": map[string]string{"access_token": privateValue}})
	if err != nil {
		t.Fatalf("marshal configure payload: %v", err)
	}
	command := session.StdioCommand{
		Version: session.StdioProtocolV1, Type: session.CommandSessionConfigure,
		CommandID: uuid.NewString(), SessionID: sessionID, WriterEpoch: attachment.WriterEpoch(),
		ExpectedSequence: manifest.Session.Sequence, RunID: runID, Payload: payload,
	}
	wantDigest, err := session.StdioCommandDigest(command)
	if err != nil {
		t.Fatalf("StdioCommandDigest: %v", err)
	}
	if detached, err := attachment.HandleCommand(ctx, command); err != nil || detached {
		t.Fatalf("configure detached/error = %v/%v", detached, err)
	}
	select {
	case vars := <-captureExecutor.started:
		if vars["region"] != "westus" || vars["access_token"] != privateValue {
			t.Fatalf("configured execution vars = %#v", vars)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("execution did not start after session.configure")
	}
	select {
	case driveErr := <-runtime.Done():
		if driveErr != nil {
			t.Fatalf("runtime drive: %v", driveErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("configured run did not finish")
	}

	configured, err := sessions.LoadManifest(ctx, sessionID)
	if err != nil {
		t.Fatalf("LoadManifest configured: %v", err)
	}
	receipt := configured.AcceptedCommands[command.CommandID]
	if receipt.ClientCommandDigest != wantDigest || receipt.EventKind != session.EventExecutionCommitted {
		t.Fatalf("configure receipt = %#v", receipt)
	}
	events, err := sessions.ReadEvents(ctx, sessionID, 0)
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	encodedEvents, err := json.Marshal(events)
	if err != nil {
		t.Fatalf("marshal events: %v", err)
	}
	if bytes.Contains(encodedEvents, []byte(privateValue)) {
		t.Fatal("private startup input was written to the session journal")
	}
	if detached, err := attachment.HandleCommand(ctx, command); err != nil || detached {
		t.Fatalf("configure retry detached/error = %v/%v", detached, err)
	}
	eventsAfterRetry, err := sessions.ReadEvents(ctx, sessionID, 0)
	if err != nil || len(eventsAfterRetry) != len(events) {
		t.Fatalf("configure retry events = %d/%d, %v", len(eventsAfterRetry), len(events), err)
	}
	_ = attachment.Release()
}

func TestCoordinatorRuntimeApprovalAnswerDoesNotOwnLaterDispatchCheckpoint(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	root := t.TempDir()
	sessions := sessionstore.NewDirStore(filepath.Join(root, "sessions"))
	runs := runstore.NewDirRunStore(filepath.Join(root, "runs"))
	t.Cleanup(func() {
		_ = runs.Close()
		_ = sessions.Close()
	})

	sessionID, runID := uuid.NewString(), uuid.NewString()
	broker := serve.NewPromptBroker(16)
	broker.Register(runID)
	defer broker.Unregister(runID)
	external := &countingExternalExecutor{}
	registry := executor.NewMapRegistry()
	registry.Register("cli", external)
	runtime := internalengine.New(engine.EngineConfig{
		Executors: registry, Dispatcher: eventbus.NewDispatcher(), TraceWriter: runtimeDiscardTrace{},
		Platform: platform.NewFakePlatform(), Store: runs, ApprovalGate: broker,
	})
	coordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
		Sessions: sessions, Runs: runs, Engine: runtime,
	})
	if err != nil {
		t.Fatalf("New coordinator: %v", err)
	}
	plan := engine.ValidatedForTest(&engine.ExecutionPlan{
		RunID: runID, RunbookPath: "approval-external.runbook.yaml",
		Steps: []engine.ResolvedStep{{
			ID: "invoke", Kind: "cli", Spec: &schema.CLISpec{Command: "provider"},
		}},
		GovernanceSource: &schema.GovernanceConfig{RequireApproval: true},
		Metadata:         engine.PlanMetadata{RunbookID: "approval-external", RunbookName: "Approval external"},
	})
	handle, err := coordinator.Start(ctx, sessioncoordinator.StartRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), SegmentID: uuid.NewString(), RunID: runID,
		Plan: plan, Graph: runtimeGraph(t, plan), RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	frames, err := broker.Subscribe(runID, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	type nextOutcome struct {
		result *engine.StepResult
		err    error
	}
	next := make(chan nextOutcome, 1)
	go func() {
		result, nextErr := handle.Next(ctx)
		next <- nextOutcome{result: result, err: nextErr}
	}()
	pending := waitRuntimePending(t, frames)
	if pending.Kind != "approval" {
		t.Fatalf("pending kind = %q, want approval", pending.Kind)
	}
	approved := true
	answer := serve.AnswerEnvelope{Kind: "approval", Approved: &approved, Approver: "operator"}
	answerPayload, err := json.Marshal(answer)
	if err != nil {
		t.Fatalf("marshal answer: %v", err)
	}
	answerCommand := session.StdioCommand{
		Version: session.StdioProtocolV1, Type: session.CommandInteractionAnswer,
		CommandID: uuid.NewString(), SessionID: sessionID, RunID: runID, TurnID: pending.TurnID,
		Payload: answerPayload,
	}
	answerDigest, err := session.StdioCommandDigest(answerCommand)
	if err != nil {
		t.Fatalf("answer digest: %v", err)
	}
	if err := broker.AnswerCommand(
		runID, pending.TurnID, answerCommand.CommandID, answerDigest,
		answer,
	); err != nil {
		t.Fatalf("AnswerCommand: %v", err)
	}
	select {
	case outcome := <-next:
		if outcome.err != nil || outcome.result == nil || outcome.result.StepID != "invoke" {
			t.Fatalf("approved Next = %#v, %v", outcome.result, outcome.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("approved external step did not return")
	}
	if _, err := handle.Next(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("complete approved run = %v, want EOF", err)
	}
	if got := external.invocationCount(); got != 1 {
		t.Fatalf("provider invocations = %d, want 1", got)
	}
	events, err := sessions.ReadEvents(ctx, sessionID, 0)
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	answerEvents := 0
	executionEvents := 0
	for _, event := range events {
		if event.Kind == session.EventExecutionCommitted {
			executionEvents++
			var payload struct {
				FrameProjectionHash string `json:"frame_projection_hash"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil || payload.FrameProjectionHash == "" {
				t.Fatalf("execution event has no frame projection: %#v, %v", event, err)
			}
			if _, err := sessions.ReadBlob(ctx, sessionID, payload.FrameProjectionHash); err != nil {
				t.Fatalf("read execution frame projection: %v", err)
			}
		}
		if event.CommandID == answerCommand.CommandID {
			answerEvents++
		}
	}
	if answerEvents != 1 || executionEvents == 0 {
		t.Fatalf("answer/execution events = %d/%d", answerEvents, executionEvents)
	}
}

func TestCoordinatorRuntimeReconnectsPendingInteractionAndAnswersOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	root := t.TempDir()
	sessionPath := filepath.Join(root, "sessions")
	runPath := filepath.Join(root, "runs")
	sessions := sessionstore.NewDirStore(sessionPath)
	runs := runstore.NewDirRunStore(runPath)
	t.Cleanup(func() {
		_ = runs.Close()
		_ = sessions.Close()
	})

	const runbookPath = "interaction.runbook.yaml"
	sessionID := uuid.NewString()
	runID := uuid.NewString()
	plan := engine.ValidatedForTest(&engine.ExecutionPlan{
		RunID: runID, RunbookPath: runbookPath,
		Steps: []engine.ResolvedStep{
			{ID: "choose", Kind: "choice", Spec: &schema.ChoiceSpec{
				Prompt: "Choose", Variable: "selected",
				Options: []schema.ChoiceOption{{Label: "One", Value: "one"}},
			}},
			{ID: "done", Kind: "noop", Spec: &schema.NoopSpec{}},
		},
		Metadata: engine.PlanMetadata{RunbookID: "interaction", RunbookName: "Interaction"},
	})

	firstBroker := serve.NewPromptBroker(16)
	firstBroker.Register(runID)
	firstCoordinator := newRuntimeCoordinator(t, sessions, runs, firstBroker)
	firstHandle, err := firstCoordinator.Start(ctx, sessioncoordinator.StartRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), SegmentID: uuid.NewString(), RunID: runID,
		Plan: plan, Graph: runtimeGraph(t, plan), RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	firstFrames, err := firstBroker.Subscribe(runID, 0)
	if err != nil {
		t.Fatalf("Subscribe first: %v", err)
	}
	firstNext := make(chan error, 1)
	go func() {
		_, nextErr := firstHandle.Next(ctx)
		firstNext <- nextErr
	}()
	firstPending := waitRuntimePending(t, firstFrames)
	firstManifest, err := firstHandle.Detach(ctx, uuid.NewString(), "stdio transport lost")
	if err != nil {
		t.Fatalf("first Detach: %v", err)
	}
	select {
	case nextErr := <-firstNext:
		if !errors.Is(nextErr, context.Canceled) {
			t.Fatalf("first Next: %v", nextErr)
		}
	case <-time.After(time.Second):
		t.Fatal("first Next did not stop on detach")
	}
	firstBroker.Unregister(runID)
	if firstManifest.Session.Status != session.StatusPaused {
		t.Fatalf("first manifest status = %s", firstManifest.Session.Status)
	}

	secondBroker := serve.NewPromptBroker(16)
	secondCoordinator := newRuntimeCoordinator(t, sessions, runs, secondBroker)
	var output bytes.Buffer
	attachment, err := Attach(ctx, sessions, sessionID, 0, &output)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	runtime, err := NewCoordinatorRuntime(
		secondCoordinator, secondBroker, engine.RunOptions{Mode: engine.RunModeReal},
	)
	if err != nil {
		t.Fatalf("NewCoordinatorRuntime: %v", err)
	}
	attachment.WithCommandHandler(runtime)
	resumeCommand := session.StdioCommand{
		Version: session.StdioProtocolV1, Type: session.CommandSessionResume,
		CommandID: uuid.NewString(), SessionID: sessionID, WriterEpoch: attachment.WriterEpoch(),
		ExpectedSequence: firstManifest.Session.Sequence,
	}
	if detached, err := attachment.HandleCommand(ctx, resumeCommand); err != nil || detached {
		t.Fatalf("resume detached/error = %v/%v", detached, err)
	}
	secondFrames, err := secondBroker.Subscribe(runID, 0)
	if err != nil {
		t.Fatalf("Subscribe second: %v", err)
	}
	secondPending := waitRuntimePending(t, secondFrames)
	if secondPending.TurnID != firstPending.TurnID {
		t.Fatalf("resumed turn id = %s, want %s", secondPending.TurnID, firstPending.TurnID)
	}
	manifest, err := sessions.LoadManifest(ctx, sessionID)
	if err != nil {
		t.Fatalf("LoadManifest before answer: %v", err)
	}
	answerPayload, _ := json.Marshal(serve.AnswerEnvelope{Kind: "choice", Selected: []string{"o:0"}})
	answerCommand := session.StdioCommand{
		Version: session.StdioProtocolV1, Type: session.CommandInteractionAnswer,
		CommandID: uuid.NewString(), SessionID: sessionID, WriterEpoch: attachment.WriterEpoch(),
		ExpectedSequence: manifest.Session.Sequence, RunID: runID, TurnID: secondPending.TurnID,
		Payload: answerPayload,
	}
	if detached, err := attachment.HandleCommand(ctx, answerCommand); err != nil || detached {
		t.Fatalf("answer detached/error = %v/%v", detached, err)
	}
	select {
	case driveErr := <-runtime.Done():
		if driveErr != nil {
			t.Fatalf("runtime drive: %v", driveErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("resumed run did not finish")
	}
	events, err := sessions.ReadEvents(ctx, sessionID, 0)
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	answerEvents := 0
	for _, event := range events {
		if event.CommandID == answerCommand.CommandID {
			answerEvents++
			if event.Kind != session.EventExecutionCommitted || event.ClientCommandDigest == "" {
				t.Fatalf("answer event = %#v", event)
			}
		}
	}
	if answerEvents != 1 {
		t.Fatalf("answer event count = %d", answerEvents)
	}
	if detached, err := attachment.HandleCommand(ctx, answerCommand); err != nil || detached {
		t.Fatalf("answer retry detached/error = %v/%v", detached, err)
	}
	eventsAfterRetry, err := sessions.ReadEvents(ctx, sessionID, 0)
	if err != nil || len(eventsAfterRetry) != len(events) {
		t.Fatalf("answer retry events = %d/%d, %v", len(eventsAfterRetry), len(events), err)
	}
	_ = attachment.Release()
}

func TestCoordinatorRuntimeReconcilesOrphanedWaitingAttemptBeforeResume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	root := t.TempDir()
	sessionPath := filepath.Join(root, "sessions")
	runPath := filepath.Join(root, "runs")
	sessionID, runID := uuid.NewString(), uuid.NewString()
	plan := engine.ValidatedForTest(&engine.ExecutionPlan{
		RunID: runID, RunbookPath: "orphaned-interaction.runbook.yaml",
		Steps: []engine.ResolvedStep{
			{ID: "choose", Kind: "choice", Spec: &schema.ChoiceSpec{
				Prompt: "Choose", Variable: "selected",
				Options: []schema.ChoiceOption{{Label: "One", Value: "one"}},
			}},
			{ID: "done", Kind: "noop", Spec: &schema.NoopSpec{}},
		},
		Metadata: engine.PlanMetadata{RunbookID: "orphaned-interaction", RunbookName: "Orphaned interaction"},
	})

	firstSessions := sessionstore.NewDirStore(sessionPath)
	firstRuns := runstore.NewDirRunStore(runPath)
	firstClosed := false
	t.Cleanup(func() {
		if !firstClosed {
			_ = firstRuns.Close()
			_ = firstSessions.Close()
		}
	})
	firstBroker := serve.NewPromptBroker(16)
	firstBroker.Register(runID)
	firstCoordinator := newRuntimeCoordinator(t, firstSessions, firstRuns, firstBroker)
	firstHandle, err := firstCoordinator.Start(ctx, sessioncoordinator.StartRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), SegmentID: uuid.NewString(), RunID: runID,
		Plan: plan, Graph: runtimeGraph(t, plan), RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	firstFrames, err := firstBroker.Subscribe(runID, 0)
	if err != nil {
		t.Fatalf("Subscribe first: %v", err)
	}
	nextCtx, cancelNext := context.WithCancel(ctx)
	firstNext := make(chan error, 1)
	go func() {
		_, nextErr := firstHandle.Next(nextCtx)
		firstNext <- nextErr
	}()
	firstPending := waitRuntimePending(t, firstFrames)
	orphaned, err := firstSessions.LoadManifest(ctx, sessionID)
	if err != nil {
		t.Fatalf("LoadManifest orphaned: %v", err)
	}
	if orphaned.Session.Status != session.StatusActive ||
		orphaned.Attempts[runID].Status != session.AttemptStatusWaiting {
		t.Fatalf("orphaned manifest = %#v", orphaned)
	}
	if err := firstRuns.Close(); err != nil {
		t.Fatalf("close first runs: %v", err)
	}
	if err := firstSessions.Close(); err != nil {
		t.Fatalf("close first sessions: %v", err)
	}
	firstClosed = true

	secondSessions := sessionstore.NewDirStore(sessionPath)
	secondRuns := runstore.NewDirRunStore(runPath)
	t.Cleanup(func() {
		_ = secondRuns.Close()
		_ = secondSessions.Close()
	})
	secondBroker := serve.NewPromptBroker(16)
	secondCoordinator := newRuntimeCoordinator(t, secondSessions, secondRuns, secondBroker)
	runtime, err := NewCoordinatorRuntime(
		secondCoordinator, secondBroker, engine.RunOptions{Mode: engine.RunModeReal},
	)
	if err != nil {
		t.Fatalf("NewCoordinatorRuntime: %v", err)
	}
	var output bytes.Buffer
	attachment, err := AttachReconciled(
		ctx, secondSessions, sessionID, 0, &output,
		func(attachment *Attachment) error {
			attachment.WithCommandHandler(runtime)
			return runtime.ReconcileAttached(ctx, attachment)
		},
	)
	if err != nil {
		t.Fatalf("Attach orphaned: %v", err)
	}
	defer attachment.Release()
	var firstFrame session.StdioFrame
	if err := json.NewDecoder(bytes.NewReader(output.Bytes())).Decode(&firstFrame); err != nil {
		t.Fatalf("decode first reconciled frame: %v", err)
	}
	if firstFrame.WriterEpoch != attachment.WriterEpoch() {
		t.Fatalf("first advertised/final writer epoch = %d/%d",
			firstFrame.WriterEpoch, attachment.WriterEpoch())
	}
	paused, err := secondSessions.LoadManifest(ctx, sessionID)
	if err != nil {
		t.Fatalf("LoadManifest paused: %v", err)
	}
	if paused.Session.Status != session.StatusPaused ||
		paused.Attempts[runID].Status != session.AttemptStatusPausedAtBoundary {
		t.Fatalf("reconciled manifest = %#v", paused)
	}
	if _, err := secondSessions.AcquireSessionLease(ctx, sessionID); !errors.Is(err, session.ErrSessionLeaseHeld) {
		t.Fatalf("reconciliation did not retain attachment lease: %v", err)
	}
	resumeCommand := session.StdioCommand{
		Version: session.StdioProtocolV1, Type: session.CommandSessionResume,
		CommandID: uuid.NewString(), SessionID: sessionID, WriterEpoch: firstFrame.WriterEpoch,
		ExpectedSequence: paused.Session.Sequence,
	}
	if detached, err := attachment.HandleCommand(ctx, resumeCommand); err != nil || detached {
		t.Fatalf("resume detached/error = %v/%v", detached, err)
	}
	secondFrames, err := secondBroker.Subscribe(runID, 0)
	if err != nil {
		t.Fatalf("Subscribe second: %v", err)
	}
	secondPending := waitRuntimePending(t, secondFrames)
	if secondPending.TurnID != firstPending.TurnID {
		t.Fatalf("reconciled turn = %s, want %s", secondPending.TurnID, firstPending.TurnID)
	}
	manifest, err := secondSessions.LoadManifest(ctx, sessionID)
	if err != nil {
		t.Fatalf("LoadManifest waiting: %v", err)
	}
	answerPayload, _ := json.Marshal(serve.AnswerEnvelope{Kind: "choice", Selected: []string{"o:0"}})
	answerCommand := session.StdioCommand{
		Version: session.StdioProtocolV1, Type: session.CommandInteractionAnswer,
		CommandID: uuid.NewString(), SessionID: sessionID, WriterEpoch: attachment.WriterEpoch(),
		ExpectedSequence: manifest.Session.Sequence, RunID: runID, TurnID: secondPending.TurnID,
		Payload: answerPayload,
	}
	if detached, err := attachment.HandleCommand(ctx, answerCommand); err != nil || detached {
		t.Fatalf("answer detached/error = %v/%v", detached, err)
	}
	select {
	case driveErr := <-runtime.Done():
		if driveErr != nil {
			t.Fatalf("runtime drive: %v", driveErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reconciled run did not finish")
	}
	cancelNext()
	firstBroker.Unregister(runID)
	select {
	case <-firstNext:
	case <-time.After(2 * time.Second):
		t.Fatal("stale process did not stop after recovery")
	}
}

func TestCoordinatorRuntimeAcceptedResumeRetryRebuildsRuntimeAfterCrash(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	root := t.TempDir()
	sessionPath := filepath.Join(root, "sessions")
	runPath := filepath.Join(root, "runs")
	sessionID, runID := uuid.NewString(), uuid.NewString()
	plan := engine.ValidatedForTest(&engine.ExecutionPlan{
		RunID: runID, RunbookPath: "resume-retry.runbook.yaml",
		Steps: []engine.ResolvedStep{
			{ID: "choose", Kind: "choice", Spec: &schema.ChoiceSpec{
				Prompt: "Choose", Variable: "selected",
				Options: []schema.ChoiceOption{{Label: "One", Value: "one"}},
			}},
			{ID: "done", Kind: "noop", Spec: &schema.NoopSpec{}},
		},
		Metadata: engine.PlanMetadata{RunbookID: "resume-retry", RunbookName: "Resume retry"},
	})

	firstSessions := sessionstore.NewDirStore(sessionPath)
	firstRuns := runstore.NewDirRunStore(runPath)
	firstBroker := serve.NewPromptBroker(16)
	firstBroker.Register(runID)
	firstCoordinator := newRuntimeCoordinator(t, firstSessions, firstRuns, firstBroker)
	firstHandle, err := firstCoordinator.Start(ctx, sessioncoordinator.StartRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), SegmentID: uuid.NewString(), RunID: runID,
		Plan: plan, Graph: runtimeGraph(t, plan), RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	firstFrames, err := firstBroker.Subscribe(runID, 0)
	if err != nil {
		t.Fatalf("Subscribe first: %v", err)
	}
	firstNext := make(chan error, 1)
	go func() {
		_, nextErr := firstHandle.Next(ctx)
		firstNext <- nextErr
	}()
	firstPending := waitRuntimePending(t, firstFrames)
	paused, err := firstHandle.Detach(ctx, uuid.NewString(), "first process detached")
	if err != nil {
		t.Fatalf("Detach first: %v", err)
	}
	select {
	case <-firstNext:
	case <-time.After(2 * time.Second):
		t.Fatal("first process did not stop")
	}
	firstBroker.Unregister(runID)
	if err := firstRuns.Close(); err != nil {
		t.Fatalf("close first runs: %v", err)
	}
	if err := firstSessions.Close(); err != nil {
		t.Fatalf("close first sessions: %v", err)
	}

	secondSessions := sessionstore.NewDirStore(sessionPath)
	secondRuns := runstore.NewDirRunStore(runPath)
	secondBroker := serve.NewPromptBroker(16)
	secondCoordinator := newRuntimeCoordinator(t, secondSessions, secondRuns, secondBroker)
	var secondOutput bytes.Buffer
	secondAttachment, err := Attach(ctx, secondSessions, sessionID, 0, &secondOutput)
	if err != nil {
		t.Fatalf("Attach second: %v", err)
	}
	secondRuntime, err := NewCoordinatorRuntime(
		secondCoordinator, secondBroker, engine.RunOptions{Mode: engine.RunModeReal},
	)
	if err != nil {
		t.Fatalf("NewCoordinatorRuntime second: %v", err)
	}
	secondAttachment.WithCommandHandler(secondRuntime)
	resumeCommand := session.StdioCommand{
		Version: session.StdioProtocolV1, Type: session.CommandSessionResume,
		CommandID: uuid.NewString(), SessionID: sessionID, WriterEpoch: secondAttachment.WriterEpoch(),
		ExpectedSequence: paused.Session.Sequence,
	}
	if detached, err := secondAttachment.HandleCommand(ctx, resumeCommand); err != nil || detached {
		t.Fatalf("second resume detached/error = %v/%v", detached, err)
	}
	secondFrames, err := secondBroker.Subscribe(runID, 0)
	if err != nil {
		t.Fatalf("Subscribe second: %v", err)
	}
	secondPending := waitRuntimePending(t, secondFrames)
	if secondPending.TurnID != firstPending.TurnID {
		t.Fatalf("second turn = %s, want %s", secondPending.TurnID, firstPending.TurnID)
	}
	accepted, err := secondSessions.LoadManifest(ctx, sessionID)
	if err != nil {
		t.Fatalf("LoadManifest accepted resume: %v", err)
	}
	resumeReceipt := accepted.AcceptedCommands[resumeCommand.CommandID]
	if resumeReceipt.ClientCommandDigest == "" || resumeReceipt.EventKind != session.EventSessionResumed {
		t.Fatalf("resume receipt = %#v", resumeReceipt)
	}
	if err := secondRuns.Close(); err != nil {
		t.Fatalf("close second runs: %v", err)
	}
	if err := secondSessions.Close(); err != nil {
		t.Fatalf("close second sessions: %v", err)
	}

	thirdSessions := sessionstore.NewDirStore(sessionPath)
	thirdRuns := runstore.NewDirRunStore(runPath)
	t.Cleanup(func() {
		_ = thirdRuns.Close()
		_ = thirdSessions.Close()
	})
	thirdBroker := serve.NewPromptBroker(16)
	thirdCoordinator := newRuntimeCoordinator(t, thirdSessions, thirdRuns, thirdBroker)
	var thirdOutput bytes.Buffer
	thirdAttachment, err := Attach(ctx, thirdSessions, sessionID, 0, &thirdOutput)
	if err != nil {
		t.Fatalf("Attach third: %v", err)
	}
	defer thirdAttachment.Release()
	thirdRuntime, err := NewCoordinatorRuntime(
		thirdCoordinator, thirdBroker, engine.RunOptions{Mode: engine.RunModeReal},
	)
	if err != nil {
		t.Fatalf("NewCoordinatorRuntime third: %v", err)
	}
	thirdAttachment.WithCommandHandler(thirdRuntime)
	if err := thirdRuntime.ReconcileAttached(ctx, thirdAttachment); err != nil {
		t.Fatalf("ReconcileAttached third: %v", err)
	}
	eventsBeforeRetry, err := thirdSessions.ReadEvents(ctx, sessionID, 0)
	if err != nil {
		t.Fatalf("ReadEvents before retry: %v", err)
	}
	resumeCommand.WriterEpoch = thirdAttachment.WriterEpoch()
	if detached, err := thirdAttachment.HandleCommand(ctx, resumeCommand); err != nil || detached {
		t.Fatalf("retried resume detached/error = %v/%v", detached, err)
	}
	thirdFrames, err := thirdBroker.Subscribe(runID, 0)
	if err != nil {
		t.Fatalf("Subscribe third: %v", err)
	}
	thirdPending := waitRuntimePending(t, thirdFrames)
	if thirdPending.TurnID != firstPending.TurnID {
		t.Fatalf("retried turn = %s, want %s", thirdPending.TurnID, firstPending.TurnID)
	}
	eventsAfterRetry, err := thirdSessions.ReadEvents(ctx, sessionID, 0)
	if err != nil {
		t.Fatalf("ReadEvents after retry: %v", err)
	}
	resumeEvents := 0
	for _, event := range eventsAfterRetry {
		if event.CommandID == resumeCommand.CommandID {
			resumeEvents++
		}
	}
	if resumeEvents != 1 || len(eventsAfterRetry) <= len(eventsBeforeRetry) {
		t.Fatalf("resume retry events/total = %d/%d->%d", resumeEvents, len(eventsBeforeRetry), len(eventsAfterRetry))
	}
	manifest, err := thirdSessions.LoadManifest(ctx, sessionID)
	if err != nil {
		t.Fatalf("LoadManifest third waiting: %v", err)
	}
	answerPayload, _ := json.Marshal(serve.AnswerEnvelope{Kind: "choice", Selected: []string{"o:0"}})
	answerCommand := session.StdioCommand{
		Version: session.StdioProtocolV1, Type: session.CommandInteractionAnswer,
		CommandID: uuid.NewString(), SessionID: sessionID, WriterEpoch: thirdAttachment.WriterEpoch(),
		ExpectedSequence: manifest.Session.Sequence, RunID: runID, TurnID: thirdPending.TurnID,
		Payload: answerPayload,
	}
	if detached, err := thirdAttachment.HandleCommand(ctx, answerCommand); err != nil || detached {
		t.Fatalf("answer detached/error = %v/%v", detached, err)
	}
	select {
	case driveErr := <-thirdRuntime.Done():
		if driveErr != nil {
			t.Fatalf("third runtime drive: %v", driveErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("retried resume run did not finish")
	}
	secondRuntime.mu.Lock()
	if secondRuntime.cancel != nil {
		secondRuntime.cancel()
	}
	secondRuntime.mu.Unlock()
	secondBroker.Unregister(runID)
	select {
	case <-secondRuntime.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("crashed runtime did not stop after stale cancellation")
	}
}

func TestCoordinatorRuntimeCancelsActiveSessionWithClientCommandReceipt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	root := t.TempDir()
	sessions := sessionstore.NewDirStore(filepath.Join(root, "sessions"))
	runs := runstore.NewDirRunStore(filepath.Join(root, "runs"))
	t.Cleanup(func() {
		_ = runs.Close()
		_ = sessions.Close()
	})
	sessionID, runID := uuid.NewString(), uuid.NewString()
	plan := engine.ValidatedForTest(&engine.ExecutionPlan{
		RunID: runID, RunbookPath: "cancel.runbook.yaml",
		Steps: []engine.ResolvedStep{{ID: "choose", Kind: "choice", Spec: &schema.ChoiceSpec{
			Prompt: "Choose", Variable: "selected",
			Options: []schema.ChoiceOption{{Label: "One", Value: "one"}},
		}}},
		Metadata: engine.PlanMetadata{RunbookID: "cancel", RunbookName: "Cancel"},
	})
	broker := serve.NewPromptBroker(16)
	broker.Register(runID)
	coordinator := newRuntimeCoordinator(t, sessions, runs, broker)
	handle, err := coordinator.Start(ctx, sessioncoordinator.StartRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), SegmentID: uuid.NewString(), RunID: runID,
		Plan: plan, Graph: runtimeGraph(t, plan), RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var output bytes.Buffer
	attachment, err := Adopt(ctx, sessions, sessionID, 0, &output, handle)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	runtime, err := NewCoordinatorRuntime(coordinator, broker, engine.RunOptions{Mode: engine.RunModeReal})
	if err != nil {
		t.Fatalf("NewCoordinatorRuntime: %v", err)
	}
	attachment.WithCommandHandler(runtime)
	frames, err := broker.Subscribe(runID, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := runtime.StartAttached(ctx, attachment, handle); err != nil {
		t.Fatalf("StartAttached: %v", err)
	}
	_ = waitRuntimePending(t, frames)
	manifest, err := sessions.LoadManifest(ctx, sessionID)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	payload, _ := json.Marshal(map[string]any{"reason": "operator cancelled"})
	command := session.StdioCommand{
		Version: session.StdioProtocolV1, Type: session.CommandSessionCancel,
		CommandID: uuid.NewString(), SessionID: sessionID, WriterEpoch: attachment.WriterEpoch(),
		ExpectedSequence: manifest.Session.Sequence, RunID: runID, Payload: payload,
	}
	wantDigest, err := session.StdioCommandDigest(command)
	if err != nil {
		t.Fatalf("StdioCommandDigest: %v", err)
	}
	if detached, err := attachment.HandleCommand(ctx, command); err != nil || !detached {
		t.Fatalf("cancel detached/error = %v/%v", detached, err)
	}
	cancelled, err := sessions.LoadManifest(ctx, sessionID)
	if err != nil {
		t.Fatalf("LoadManifest cancelled: %v", err)
	}
	receipt := cancelled.AcceptedCommands[command.CommandID]
	if cancelled.Session.Status != session.StatusCancelled || cancelled.Session.ActiveRunID != "" ||
		cancelled.Attempts[runID].Status != session.AttemptStatusCancelled ||
		receipt.ClientCommandDigest != wantDigest || receipt.EventKind != session.EventExecutionCommitted {
		t.Fatalf("cancelled manifest/receipt = %#v/%#v", cancelled.Session, receipt)
	}
	eventsBeforeRetry, err := sessions.ReadEvents(ctx, sessionID, 0)
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	if detached, err := attachment.HandleCommand(ctx, command); err != nil || !detached {
		t.Fatalf("cancel retry detached/error = %v/%v", detached, err)
	}
	eventsAfterRetry, err := sessions.ReadEvents(ctx, sessionID, 0)
	if err != nil || len(eventsAfterRetry) != len(eventsBeforeRetry) {
		t.Fatalf("cancel retry events = %d/%d, %v", len(eventsAfterRetry), len(eventsBeforeRetry), err)
	}
	_ = attachment.Release()
}

func TestCoordinatorRuntimeDetachReplaysFinalPauseBeforeLeaseRelease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	root := t.TempDir()
	sessions := sessionstore.NewDirStore(filepath.Join(root, "sessions"))
	runs := runstore.NewDirRunStore(filepath.Join(root, "runs"))
	t.Cleanup(func() {
		_ = runs.Close()
		_ = sessions.Close()
	})
	sessionID, runID := uuid.NewString(), uuid.NewString()
	plan := engine.ValidatedForTest(&engine.ExecutionPlan{
		RunID: runID, RunbookPath: "detach-final-frame.runbook.yaml",
		Steps: []engine.ResolvedStep{{ID: "choose", Kind: "choice", Spec: &schema.ChoiceSpec{
			Prompt: "Choose", Variable: "selected",
			Options: []schema.ChoiceOption{{Label: "One", Value: "one"}},
		}}},
		Metadata: engine.PlanMetadata{RunbookID: "detach-final-frame", RunbookName: "Detach final frame"},
	})
	broker := serve.NewPromptBroker(16)
	broker.Register(runID)
	coordinator := newRuntimeCoordinator(t, sessions, runs, broker)
	handle, err := coordinator.Start(ctx, sessioncoordinator.StartRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), SegmentID: uuid.NewString(), RunID: runID,
		Plan: plan, Graph: runtimeGraph(t, plan), RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	frames, err := broker.Subscribe(runID, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	nextDone := make(chan error, 1)
	go func() {
		_, nextErr := handle.Next(ctx)
		nextDone <- nextErr
	}()
	_ = waitRuntimePending(t, frames)
	var output bytes.Buffer
	attachment, err := Adopt(ctx, sessions, sessionID, 0, &output, handle)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	runtime, err := NewCoordinatorRuntime(coordinator, broker, engine.RunOptions{Mode: engine.RunModeReal})
	if err != nil {
		t.Fatalf("NewCoordinatorRuntime: %v", err)
	}
	runtime.mu.Lock()
	runtime.handle = handle
	runtime.runID = runID
	runtime.started = true
	runtime.cancel = cancel
	runtime.mu.Unlock()
	attachment.WithCommandHandler(runtime)
	manifest, err := sessions.LoadManifest(ctx, sessionID)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	command := session.StdioCommand{
		Version: session.StdioProtocolV1, Type: session.CommandSessionDetach,
		CommandID: uuid.NewString(), SessionID: sessionID, WriterEpoch: attachment.WriterEpoch(),
		ExpectedSequence: manifest.Session.Sequence,
	}
	if detached, err := attachment.HandleCommand(ctx, command); err != nil || !detached {
		t.Fatalf("detach detached/error = %v/%v", detached, err)
	}
	select {
	case <-nextDone:
	case <-time.After(2 * time.Second):
		t.Fatal("detached Next did not stop")
	}
	paused, err := sessions.LoadManifest(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("LoadManifest paused: %v", err)
	}
	decoder := json.NewDecoder(&output)
	foundFinalPause := false
	for {
		var frame session.StdioFrame
		if err := decoder.Decode(&frame); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("decode frame: %v", err)
		}
		foundFinalPause = foundFinalPause || frame.Type == session.FrameAttemptPaused &&
			frame.SessionSequence == paused.Session.Sequence
	}
	if !foundFinalPause {
		t.Fatalf("final paused frame missing at sequence %d: %s", paused.Session.Sequence, output.String())
	}
	lease, err := sessions.AcquireSessionLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("lease remained held after final replay: %v", err)
	}
	_ = lease.Release()
}

func TestCoordinatorRuntimeExplicitDetachPreparedDispatchUsesClientReceipt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	root := t.TempDir()
	sessions := sessionstore.NewDirStore(filepath.Join(root, "sessions"))
	runs := runstore.NewDirRunStore(filepath.Join(root, "runs"))
	t.Cleanup(func() {
		_ = runs.Close()
		_ = sessions.Close()
	})
	sessionID, runID := uuid.NewString(), uuid.NewString()
	plan := engine.ValidatedForTest(&engine.ExecutionPlan{
		RunID: runID, RunbookPath: "explicit-dispatch-detach.runbook.yaml",
		Steps: []engine.ResolvedStep{{
			ID: "mutate", Kind: "cli", Spec: &schema.CLISpec{Command: "blocked"},
		}},
		Metadata: engine.PlanMetadata{RunbookID: "explicit-dispatch-detach", RunbookName: "Explicit dispatch detach"},
	})
	dispatchExecutor := &blockingPreparedDispatchExecutor{started: make(chan struct{})}
	registry := executor.NewMapRegistry()
	registry.Register("cli", dispatchExecutor)
	broker := serve.NewPromptBroker(16)
	broker.Register(runID)
	runtimeEngine := internalengine.New(engine.EngineConfig{
		Executors: registry, Dispatcher: eventbus.NewDispatcher(), TraceWriter: runtimeDiscardTrace{},
		Platform: platform.NewFakePlatform(), Store: runs,
	})
	coordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
		Sessions: sessions, Runs: runs, Engine: runtimeEngine,
	})
	if err != nil {
		t.Fatalf("New coordinator: %v", err)
	}
	handle, err := coordinator.Start(ctx, sessioncoordinator.StartRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), SegmentID: uuid.NewString(), RunID: runID,
		Plan: plan, Graph: runtimeGraph(t, plan), RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var output bytes.Buffer
	attachment, err := Adopt(ctx, sessions, sessionID, 0, &output, handle)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	runtime, err := NewCoordinatorRuntime(coordinator, broker, engine.RunOptions{Mode: engine.RunModeReal})
	if err != nil {
		t.Fatalf("NewCoordinatorRuntime: %v", err)
	}
	attachment.WithCommandHandler(runtime)
	if err := runtime.StartAttached(ctx, attachment, handle); err != nil {
		t.Fatalf("StartAttached: %v", err)
	}
	select {
	case <-dispatchExecutor.started:
	case <-time.After(5 * time.Second):
		t.Fatal("prepared dispatch did not start")
	}
	manifest, err := sessions.LoadManifest(ctx, sessionID)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	command := session.StdioCommand{
		Version: session.StdioProtocolV1, Type: session.CommandSessionDetach,
		CommandID: uuid.NewString(), SessionID: sessionID, WriterEpoch: attachment.WriterEpoch(),
		ExpectedSequence: manifest.Session.Sequence,
	}
	wantDigest, err := session.StdioCommandDigest(command)
	if err != nil {
		t.Fatalf("StdioCommandDigest: %v", err)
	}
	if detached, err := attachment.HandleCommand(ctx, command); err != nil || !detached {
		t.Fatalf("detach detached/error = %v/%v", detached, err)
	}
	indeterminate, err := sessions.LoadManifest(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("LoadManifest indeterminate: %v", err)
	}
	receipt := indeterminate.AcceptedCommands[command.CommandID]
	if indeterminate.Session.Status != session.StatusIndeterminate ||
		indeterminate.Attempts[runID].Status != session.AttemptStatusIndeterminate ||
		receipt.ClientCommandDigest != wantDigest || receipt.EventKind != session.EventSessionDetached {
		t.Fatalf("indeterminate manifest/receipt = %#v/%#v", indeterminate, receipt)
	}
	lease, err := sessions.AcquireSessionLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("lease remained held after explicit indeterminate detach: %v", err)
	}
	_ = lease.Release()
}

func TestCoordinatorRuntimeClosesCompletedSessionWithClientCommandReceipt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	root := t.TempDir()
	sessions := sessionstore.NewDirStore(filepath.Join(root, "sessions"))
	runs := runstore.NewDirRunStore(filepath.Join(root, "runs"))
	t.Cleanup(func() {
		_ = runs.Close()
		_ = sessions.Close()
	})
	sessionID, runID := uuid.NewString(), uuid.NewString()
	plan := engine.ValidatedForTest(&engine.ExecutionPlan{
		RunID: runID, RunbookPath: "close.runbook.yaml",
		Steps:    []engine.ResolvedStep{{ID: "done", Kind: "noop", Spec: &schema.NoopSpec{}}},
		Metadata: engine.PlanMetadata{RunbookID: "close", RunbookName: "Close"},
	})
	broker := serve.NewPromptBroker(16)
	broker.Register(runID)
	coordinator := newRuntimeCoordinator(t, sessions, runs, broker)
	handle, err := coordinator.Start(ctx, sessioncoordinator.StartRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), SegmentID: uuid.NewString(), RunID: runID,
		Plan: plan, Graph: runtimeGraph(t, plan), RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var output bytes.Buffer
	attachment, err := Adopt(ctx, sessions, sessionID, 0, &output, handle)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	runtime, err := NewCoordinatorRuntime(coordinator, broker, engine.RunOptions{Mode: engine.RunModeReal})
	if err != nil {
		t.Fatalf("NewCoordinatorRuntime: %v", err)
	}
	attachment.WithCommandHandler(runtime)
	if err := runtime.StartAttached(ctx, attachment, handle); err != nil {
		t.Fatalf("StartAttached: %v", err)
	}
	select {
	case driveErr := <-runtime.Done():
		if driveErr != nil {
			t.Fatalf("runtime drive: %v", driveErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not finish")
	}
	manifest, err := sessions.LoadManifest(ctx, sessionID)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	payload, _ := json.Marshal(map[string]any{"status": session.StatusResolved})
	command := session.StdioCommand{
		Version: session.StdioProtocolV1, Type: session.CommandSessionClose,
		CommandID: uuid.NewString(), SessionID: sessionID, WriterEpoch: attachment.WriterEpoch(),
		ExpectedSequence: manifest.Session.Sequence, Payload: payload,
	}
	wantDigest, err := session.StdioCommandDigest(command)
	if err != nil {
		t.Fatalf("StdioCommandDigest: %v", err)
	}
	if detached, err := attachment.HandleCommand(ctx, command); err != nil || !detached {
		t.Fatalf("close detached/error = %v/%v", detached, err)
	}
	closed, err := sessions.LoadManifest(ctx, sessionID)
	if err != nil {
		t.Fatalf("LoadManifest closed: %v", err)
	}
	receipt := closed.AcceptedCommands[command.CommandID]
	if closed.Session.Status != session.StatusResolved || receipt.ClientCommandDigest != wantDigest ||
		receipt.EventKind != session.EventSessionClosed {
		t.Fatalf("closed manifest/receipt = %#v/%#v", closed.Session, receipt)
	}
	eventsBeforeRetry, err := sessions.ReadEvents(ctx, sessionID, 0)
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	if detached, err := attachment.HandleCommand(ctx, command); err != nil || !detached {
		t.Fatalf("close retry detached/error = %v/%v", detached, err)
	}
	eventsAfterRetry, err := sessions.ReadEvents(ctx, sessionID, 0)
	if err != nil || len(eventsAfterRetry) != len(eventsBeforeRetry) {
		t.Fatalf("close retry events = %d/%d, %v", len(eventsAfterRetry), len(eventsBeforeRetry), err)
	}
	_ = attachment.Release()
}

func TestCoordinatorRuntimeClosesCompletedSessionAfterFreshAttachment(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	root := t.TempDir()
	sessionPath := filepath.Join(root, "sessions")
	runPath := filepath.Join(root, "runs")
	sessionID, runID := uuid.NewString(), uuid.NewString()
	plan := engine.ValidatedForTest(&engine.ExecutionPlan{
		RunID: runID, RunbookPath: "close-after-restart.runbook.yaml",
		Steps:    []engine.ResolvedStep{{ID: "done", Kind: "noop", Spec: &schema.NoopSpec{}}},
		Metadata: engine.PlanMetadata{RunbookID: "close-after-restart", RunbookName: "Close after restart"},
	})
	firstSessions := sessionstore.NewDirStore(sessionPath)
	firstRuns := runstore.NewDirRunStore(runPath)
	firstBroker := serve.NewPromptBroker(16)
	firstCoordinator := newRuntimeCoordinator(t, firstSessions, firstRuns, firstBroker)
	handle, err := firstCoordinator.Start(ctx, sessioncoordinator.StartRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), SegmentID: uuid.NewString(), RunID: runID,
		Plan: plan, Graph: runtimeGraph(t, plan), RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := handle.Next(ctx); err != nil {
		t.Fatalf("step Next: %v", err)
	}
	if _, err := handle.Next(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("completion Next: %v", err)
	}
	completed := handle.Manifest()
	if completed.Session.Status != session.StatusPaused || completed.Session.ActiveRunID != "" ||
		completed.Attempts[runID].Status != session.AttemptStatusCompleted {
		t.Fatalf("completed manifest = %#v", completed)
	}
	if err := handle.Release(); err != nil {
		t.Fatalf("Release completed handle: %v", err)
	}
	if err := firstRuns.Close(); err != nil {
		t.Fatalf("close first runs: %v", err)
	}
	if err := firstSessions.Close(); err != nil {
		t.Fatalf("close first sessions: %v", err)
	}

	secondSessions := sessionstore.NewDirStore(sessionPath)
	secondRuns := runstore.NewDirRunStore(runPath)
	t.Cleanup(func() {
		_ = secondRuns.Close()
		_ = secondSessions.Close()
	})
	secondBroker := serve.NewPromptBroker(16)
	secondCoordinator := newRuntimeCoordinator(t, secondSessions, secondRuns, secondBroker)
	var output bytes.Buffer
	attachment, err := Attach(ctx, secondSessions, sessionID, 0, &output)
	if err != nil {
		t.Fatalf("Attach completed: %v", err)
	}
	defer attachment.Release()
	runtime, err := NewCoordinatorRuntime(
		secondCoordinator, secondBroker, engine.RunOptions{Mode: engine.RunModeReal},
	)
	if err != nil {
		t.Fatalf("NewCoordinatorRuntime: %v", err)
	}
	attachment.WithCommandHandler(runtime)
	if err := runtime.ReconcileAttached(ctx, attachment); err != nil {
		t.Fatalf("ReconcileAttached: %v", err)
	}
	manifest, err := secondSessions.LoadManifest(ctx, sessionID)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	payload, _ := json.Marshal(map[string]any{"status": session.StatusResolved})
	command := session.StdioCommand{
		Version: session.StdioProtocolV1, Type: session.CommandSessionClose,
		CommandID: uuid.NewString(), SessionID: sessionID, WriterEpoch: attachment.WriterEpoch(),
		ExpectedSequence: manifest.Session.Sequence, Payload: payload,
	}
	if detached, err := attachment.HandleCommand(ctx, command); err != nil || !detached {
		t.Fatalf("close detached/error = %v/%v", detached, err)
	}
	closed, err := secondSessions.LoadManifest(ctx, sessionID)
	if err != nil || closed.Session.Status != session.StatusResolved {
		t.Fatalf("closed session = %#v, %v", closed.Session, err)
	}
}

func TestCoordinatorRuntimeDriveOutputFailureDetachesBeforeLeaseRelease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	root := t.TempDir()
	sessions := sessionstore.NewDirStore(filepath.Join(root, "sessions"))
	runs := runstore.NewDirRunStore(filepath.Join(root, "runs"))
	t.Cleanup(func() {
		_ = runs.Close()
		_ = sessions.Close()
	})
	sessionID, runID := uuid.NewString(), uuid.NewString()
	plan := engine.ValidatedForTest(&engine.ExecutionPlan{
		RunID: runID, RunbookPath: "output-loss.runbook.yaml",
		Steps: []engine.ResolvedStep{
			{ID: "first", Kind: "noop", Spec: &schema.NoopSpec{}},
			{ID: "second", Kind: "noop", Spec: &schema.NoopSpec{}},
		},
		Metadata: engine.PlanMetadata{RunbookID: "output-loss", RunbookName: "Output loss"},
	})
	broker := serve.NewPromptBroker(16)
	broker.Register(runID)
	coordinator := newRuntimeCoordinator(t, sessions, runs, broker)
	handle, err := coordinator.Start(ctx, sessioncoordinator.StartRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), SegmentID: uuid.NewString(), RunID: runID,
		Plan: plan, Graph: runtimeGraph(t, plan), RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	writer := &toggleFrameWriter{}
	attachment, err := Adopt(ctx, sessions, sessionID, 0, writer, handle)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	t.Cleanup(func() {
		manifest, _ := sessions.LoadManifest(context.Background(), sessionID)
		if manifest.Session.ActiveRunID != "" && manifest.Session.Status != session.StatusPaused {
			_, _ = handle.Detach(context.Background(), uuid.NewString(), "test cleanup")
		}
		_ = attachment.Release()
	})
	runtime, err := NewCoordinatorRuntime(coordinator, broker, engine.RunOptions{Mode: engine.RunModeReal})
	if err != nil {
		t.Fatalf("NewCoordinatorRuntime: %v", err)
	}
	runCtx, stop := context.WithCancel(ctx)
	runtime.mu.Lock()
	runtime.handle = handle
	runtime.runID = runID
	runtime.started = true
	runtime.cancel = stop
	runtime.mu.Unlock()
	writer.Fail()
	go runtime.drive(runCtx, attachment, handle)
	select {
	case driveErr := <-runtime.Done():
		if !errors.Is(driveErr, io.ErrClosedPipe) {
			t.Fatalf("runtime drive error = %v", driveErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runtime did not report output failure")
	}
	manifest, err := sessions.LoadManifest(ctx, sessionID)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	attempt := manifest.Attempts[runID]
	if manifest.Session.Status != session.StatusPaused || attempt.Status != session.AttemptStatusPausedAtBoundary {
		t.Fatalf("output failure manifest = %#v/%#v", manifest.Session, attempt)
	}
	lease, err := sessions.AcquireSessionLease(ctx, sessionID)
	if err != nil {
		t.Fatalf("session lease remained held after output failure: %v", err)
	}
	_ = lease.Release()
}

func TestCoordinatorRuntimeOutputFailureMarksPreparedDispatchIndeterminateBeforeLeaseRelease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	root := t.TempDir()
	sessions := sessionstore.NewDirStore(filepath.Join(root, "sessions"))
	runs := runstore.NewDirRunStore(filepath.Join(root, "runs"))
	t.Cleanup(func() {
		_ = runs.Close()
		_ = sessions.Close()
	})
	sessionID, runID := uuid.NewString(), uuid.NewString()
	plan := engine.ValidatedForTest(&engine.ExecutionPlan{
		RunID: runID, RunbookPath: "dispatch-output-loss.runbook.yaml",
		Steps: []engine.ResolvedStep{{
			ID: "mutate", Kind: "cli", Spec: &schema.CLISpec{Command: "blocked"},
		}},
		Metadata: engine.PlanMetadata{RunbookID: "dispatch-output-loss", RunbookName: "Dispatch output loss"},
	})
	dispatchExecutor := &blockingPreparedDispatchExecutor{started: make(chan struct{})}
	registry := executor.NewMapRegistry()
	registry.Register("cli", dispatchExecutor)
	broker := serve.NewPromptBroker(16)
	broker.Register(runID)
	runtimeEngine := internalengine.New(engine.EngineConfig{
		Executors: registry, Dispatcher: eventbus.NewDispatcher(), TraceWriter: runtimeDiscardTrace{},
		Platform: platform.NewFakePlatform(), Store: runs,
	})
	coordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
		Sessions: sessions, Runs: runs, Engine: runtimeEngine,
	})
	if err != nil {
		t.Fatalf("New coordinator: %v", err)
	}
	handle, err := coordinator.Start(ctx, sessioncoordinator.StartRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), SegmentID: uuid.NewString(), RunID: runID,
		Plan: plan, Graph: runtimeGraph(t, plan), RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	writer := &matchingFailFrameWriter{
		match: []byte("dispatch/prepared"), matched: make(chan struct{}),
	}
	attachment, err := Adopt(ctx, sessions, sessionID, 0, writer, handle)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	runtime, err := NewCoordinatorRuntime(coordinator, broker, engine.RunOptions{Mode: engine.RunModeReal})
	if err != nil {
		t.Fatalf("NewCoordinatorRuntime: %v", err)
	}
	attachment.WithCommandHandler(runtime)
	if err := runtime.StartAttached(ctx, attachment, handle); err != nil {
		t.Fatalf("StartAttached: %v", err)
	}
	select {
	case <-dispatchExecutor.started:
	case <-time.After(5 * time.Second):
		t.Fatal("prepared dispatch did not start")
	}
	select {
	case <-writer.matched:
	case <-time.After(5 * time.Second):
		t.Fatal("dispatch frame did not reach failing writer")
	}
	select {
	case runtimeErr := <-runtime.Done():
		if !errors.Is(runtimeErr, io.ErrClosedPipe) {
			t.Fatalf("runtime error = %v", runtimeErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runtime did not finish transport detach")
	}
	manifest, err := sessions.LoadManifest(ctx, sessionID)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	attempt := manifest.Attempts[runID]
	if manifest.Session.Status != session.StatusIndeterminate || attempt.Status != session.AttemptStatusIndeterminate {
		t.Fatalf("dispatch output failure manifest = %#v/%#v", manifest.Session, attempt)
	}
	lease, err := sessions.AcquireSessionLease(ctx, sessionID)
	if err != nil {
		t.Fatalf("session lease remained held after indeterminate halt: %v", err)
	}
	_ = lease.Release()
}

func TestCoordinatorRuntimeReconcilesOrphanedPreparedDispatchAsIndeterminate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	root := t.TempDir()
	sessionPath := filepath.Join(root, "sessions")
	runPath := filepath.Join(root, "runs")
	sessionID, runID := uuid.NewString(), uuid.NewString()
	plan := engine.ValidatedForTest(&engine.ExecutionPlan{
		RunID: runID, RunbookPath: "orphaned-dispatch.runbook.yaml",
		Steps: []engine.ResolvedStep{{
			ID: "mutate", Kind: "cli", Spec: &schema.CLISpec{Command: "blocked"},
		}},
		Metadata: engine.PlanMetadata{RunbookID: "orphaned-dispatch", RunbookName: "Orphaned dispatch"},
	})
	dispatchExecutor := &blockingPreparedDispatchExecutor{started: make(chan struct{})}
	firstRegistry := executor.NewMapRegistry()
	firstRegistry.Register("cli", dispatchExecutor)
	firstSessions := sessionstore.NewDirStore(sessionPath)
	firstRuns := runstore.NewDirRunStore(runPath)
	firstRuntimeEngine := internalengine.New(engine.EngineConfig{
		Executors: firstRegistry, Dispatcher: eventbus.NewDispatcher(), TraceWriter: runtimeDiscardTrace{},
		Platform: platform.NewFakePlatform(), Store: firstRuns,
	})
	firstCoordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
		Sessions: firstSessions, Runs: firstRuns, Engine: firstRuntimeEngine,
	})
	if err != nil {
		t.Fatalf("New first coordinator: %v", err)
	}
	firstHandle, err := firstCoordinator.Start(ctx, sessioncoordinator.StartRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), SegmentID: uuid.NewString(), RunID: runID,
		Plan: plan, Graph: runtimeGraph(t, plan), RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	firstCtx, cancelFirst := context.WithCancel(ctx)
	firstNext := make(chan error, 1)
	go func() {
		_, nextErr := firstHandle.Next(firstCtx)
		firstNext <- nextErr
	}()
	select {
	case <-dispatchExecutor.started:
	case <-time.After(5 * time.Second):
		t.Fatal("prepared dispatch did not start")
	}
	if err := firstRuns.Close(); err != nil {
		t.Fatalf("close first runs: %v", err)
	}
	if err := firstSessions.Close(); err != nil {
		t.Fatalf("close first sessions: %v", err)
	}

	secondSessions := sessionstore.NewDirStore(sessionPath)
	secondRuns := runstore.NewDirRunStore(runPath)
	t.Cleanup(func() {
		_ = secondRuns.Close()
		_ = secondSessions.Close()
	})
	secondRegistry := executor.NewMapRegistry()
	secondRegistry.Register("cli", executor.NewCLIExecutor(platform.NewFakePlatform(), nil))
	secondBroker := serve.NewPromptBroker(16)
	secondRuntimeEngine := internalengine.New(engine.EngineConfig{
		Executors: secondRegistry, Dispatcher: eventbus.NewDispatcher(), TraceWriter: runtimeDiscardTrace{},
		Platform: platform.NewFakePlatform(), Store: secondRuns,
	})
	secondCoordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
		Sessions: secondSessions, Runs: secondRuns, Engine: secondRuntimeEngine,
	})
	if err != nil {
		t.Fatalf("New second coordinator: %v", err)
	}
	var output bytes.Buffer
	attachment, err := Attach(ctx, secondSessions, sessionID, 0, &output)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer attachment.Release()
	runtime, err := NewCoordinatorRuntime(
		secondCoordinator, secondBroker, engine.RunOptions{Mode: engine.RunModeReal},
	)
	if err != nil {
		t.Fatalf("NewCoordinatorRuntime: %v", err)
	}
	attachment.WithCommandHandler(runtime)
	if err := runtime.ReconcileAttached(ctx, attachment); err != nil {
		t.Fatalf("ReconcileAttached: %v", err)
	}
	manifest, err := secondSessions.LoadManifest(ctx, sessionID)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if manifest.Session.Status != session.StatusIndeterminate ||
		manifest.Attempts[runID].Status != session.AttemptStatusIndeterminate {
		t.Fatalf("reconciled dispatch manifest = %#v", manifest)
	}
	if _, err := secondSessions.AcquireSessionLease(ctx, sessionID); !errors.Is(err, session.ErrSessionLeaseHeld) {
		t.Fatalf("indeterminate attachment did not retain lease: %v", err)
	}
	cancelFirst()
	select {
	case <-firstNext:
	case <-time.After(2 * time.Second):
		t.Fatal("stale dispatch process did not stop")
	}
}

type blockingHandoffResolver struct {
	started chan struct{}
	release chan struct{}
	target  sessioncoordinator.HandoffTarget
	once    sync.Once
}

func (resolver *blockingHandoffResolver) ResolveHandoff(
	ctx context.Context,
	_ sessioncoordinator.HandoffResolveRequest,
) (sessioncoordinator.HandoffTarget, error) {
	resolver.once.Do(func() { close(resolver.started) })
	select {
	case <-resolver.release:
		return resolver.target, nil
	case <-ctx.Done():
		return sessioncoordinator.HandoffTarget{}, ctx.Err()
	}
}

func TestCoordinatorRuntimeSwitchesBrokerAndAnswerRoutingAtHandoff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	root := t.TempDir()
	sessions := sessionstore.NewDirStore(filepath.Join(root, "sessions"))
	runs := runstore.NewDirRunStore(filepath.Join(root, "runs"))
	t.Cleanup(func() {
		_ = runs.Close()
		_ = sessions.Close()
	})
	targetPlan := engine.ValidatedForTest(&engine.ExecutionPlan{
		RunID: uuid.NewString(), RunbookPath: filepath.Join(root, "interactive-target.runbook.yaml"),
		Steps: []engine.ResolvedStep{
			{ID: "target-choice", Kind: "choice", Spec: &schema.ChoiceSpec{
				Prompt: "Choose", Variable: "selected",
				Options: []schema.ChoiceOption{{Label: "One", Value: "one"}},
			}},
			{ID: "target-done", Kind: "noop", Spec: &schema.NoopSpec{}},
		},
		Metadata: engine.PlanMetadata{RunbookID: "interactive-target", RunbookName: "Interactive target"},
	})
	resolver := &blockingHandoffResolver{
		started: make(chan struct{}), release: make(chan struct{}),
		target: sessioncoordinator.HandoffTarget{Plan: targetPlan, Graph: runtimeGraph(t, targetPlan)},
	}
	close(resolver.release)
	broker := serve.NewPromptBroker(16)
	registry := executor.NewMapRegistry()
	registry.Register("handoff", executor.NewHandoffExecutor(nil))
	registry.Register("choice", executor.NewChoiceExecutor(internalinput.NewDurablePromptProvider(broker), nil))
	registry.Register("noop", executor.NewNoopExecutor(nil))
	runtimeEngine := internalengine.New(engine.EngineConfig{
		Executors: registry, Dispatcher: eventbus.NewDispatcher(), TraceWriter: runtimeDiscardTrace{},
		Platform: platform.NewFakePlatform(), Store: runs,
	})
	coordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
		Sessions: sessions, Runs: runs, Engine: runtimeEngine, HandoffResolver: resolver,
	})
	if err != nil {
		t.Fatalf("New coordinator: %v", err)
	}
	sessionID := uuid.NewString()
	sourcePlan := engine.ValidatedForTest(&engine.ExecutionPlan{
		RunID: uuid.NewString(), RunbookPath: filepath.Join(root, "source.runbook.yaml"),
		Steps: []engine.ResolvedStep{{
			ID: "route", Kind: "handoff", Spec: &schema.HandoffSpec{Handoff: schema.HandoffConfig{
				Runbook: "interactive-target.runbook.yaml",
				Reason:  schema.HandoffReason{Code: "continue", Summary: "Continue"},
			}},
		}},
		Metadata: engine.PlanMetadata{RunbookID: "source", RunbookName: "Source"},
	})
	broker.Register(sourcePlan.RunID)
	handle, err := coordinator.Start(ctx, sessioncoordinator.StartRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), SegmentID: uuid.NewString(), RunID: sourcePlan.RunID,
		Plan: sourcePlan, Graph: runtimeGraph(t, sourcePlan), RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var output bytes.Buffer
	attachment, err := Adopt(ctx, sessions, sessionID, 0, &output, handle)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	defer attachment.Release()
	runtime, err := NewCoordinatorRuntime(coordinator, broker, engine.RunOptions{Mode: engine.RunModeReal})
	if err != nil {
		t.Fatalf("NewCoordinatorRuntime: %v", err)
	}
	attachment.WithCommandHandler(runtime)
	if err := runtime.StartAttached(ctx, attachment, handle); err != nil {
		t.Fatalf("StartAttached: %v", err)
	}

	var targetRunID string
	var targetFrames <-chan any
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for targetFrames == nil {
		select {
		case runtimeErr := <-runtime.Done():
			t.Fatalf("runtime stopped before target interaction: %v", runtimeErr)
		case <-deadline:
			t.Fatal("target broker was not registered")
		case <-ticker.C:
			manifest, loadErr := sessions.LoadManifest(ctx, sessionID)
			if loadErr != nil || manifest.Session.ActiveRunID == "" ||
				manifest.Session.ActiveRunID == sourcePlan.RunID {
				continue
			}
			targetRunID = manifest.Session.ActiveRunID
			frames, subscribeErr := broker.Subscribe(targetRunID, 0)
			if subscribeErr == nil {
				targetFrames = frames
			}
		}
	}
	pending := waitRuntimePending(t, targetFrames)
	if pending.RunID != targetRunID || pending.StepID != "target-choice" {
		t.Fatalf("target pending = %#v", pending)
	}
	manifest, err := sessions.LoadManifest(ctx, sessionID)
	if err != nil {
		t.Fatalf("LoadManifest target waiting: %v", err)
	}
	answerPayload, _ := json.Marshal(serve.AnswerEnvelope{Kind: "choice", Selected: []string{"o:0"}})
	answerCommand := session.StdioCommand{
		Version: session.StdioProtocolV1, Type: session.CommandInteractionAnswer,
		CommandID: uuid.NewString(), SessionID: sessionID, WriterEpoch: attachment.WriterEpoch(),
		ExpectedSequence: manifest.Session.Sequence, RunID: targetRunID, TurnID: pending.TurnID,
		Payload: answerPayload,
	}
	if detached, err := attachment.HandleCommand(ctx, answerCommand); err != nil || detached {
		t.Fatalf("target answer detached/error = %v/%v", detached, err)
	}
	select {
	case runtimeErr := <-runtime.Done():
		if runtimeErr != nil {
			t.Fatalf("target runtime: %v", runtimeErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("target run did not finish")
	}
}

func TestCoordinatorDetachDuringHandoffReplaysOneTargetSegment(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	root := t.TempDir()
	sessions := sessionstore.NewDirStore(filepath.Join(root, "sessions"))
	runs := runstore.NewDirRunStore(filepath.Join(root, "runs"))
	t.Cleanup(func() {
		_ = runs.Close()
		_ = sessions.Close()
	})
	targetPlan := engine.ValidatedForTest(&engine.ExecutionPlan{
		RunID: uuid.NewString(), RunbookPath: filepath.Join(root, "target.runbook.yaml"),
		Steps:    []engine.ResolvedStep{{ID: "target-work", Kind: "noop", Spec: &schema.NoopSpec{}}},
		Metadata: engine.PlanMetadata{RunbookID: "target", RunbookName: "Target"},
	})
	resolver := &blockingHandoffResolver{
		started: make(chan struct{}), release: make(chan struct{}),
		target: sessioncoordinator.HandoffTarget{Plan: targetPlan, Graph: runtimeGraph(t, targetPlan)},
	}
	registry := executor.NewMapRegistry()
	registry.Register("handoff", executor.NewHandoffExecutor(nil))
	registry.Register("noop", executor.NewNoopExecutor(nil))
	runtime := internalengine.New(engine.EngineConfig{
		Executors: registry, Dispatcher: eventbus.NewDispatcher(), TraceWriter: runtimeDiscardTrace{},
		Platform: platform.NewFakePlatform(), Store: runs,
	})
	coordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
		Sessions: sessions, Runs: runs, Engine: runtime, HandoffResolver: resolver,
	})
	if err != nil {
		t.Fatalf("New coordinator: %v", err)
	}
	sessionID := uuid.NewString()
	sourcePlan := engine.ValidatedForTest(&engine.ExecutionPlan{
		RunID: uuid.NewString(), RunbookPath: filepath.Join(root, "source.runbook.yaml"),
		Steps: []engine.ResolvedStep{{ID: "route", Kind: "handoff", Spec: &schema.HandoffSpec{Handoff: schema.HandoffConfig{
			Runbook: "target.runbook.yaml", Reason: schema.HandoffReason{Code: "continue", Summary: "Continue"},
		}}}},
		Metadata: engine.PlanMetadata{RunbookID: "source", RunbookName: "Source"},
	})
	handle, err := coordinator.Start(ctx, sessioncoordinator.StartRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), SegmentID: uuid.NewString(), RunID: sourcePlan.RunID,
		Plan: sourcePlan, Graph: runtimeGraph(t, sourcePlan), RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	afterSequence := handle.Manifest().Session.Sequence
	nextDone := make(chan error, 1)
	go func() {
		_, nextErr := handle.Next(ctx)
		nextDone <- nextErr
	}()
	select {
	case <-resolver.started:
	case <-time.After(2 * time.Second):
		t.Fatal("handoff resolver did not start")
	}
	detachCommandID := uuid.NewString()
	detachDone := make(chan error, 1)
	go func() {
		_, detachErr := handle.Detach(ctx, detachCommandID, "stdio transport lost")
		detachDone <- detachErr
	}()
	close(resolver.release)
	if err := <-nextDone; err != nil {
		t.Fatalf("handoff Next: %v", err)
	}
	if err := <-detachDone; err != nil {
		t.Fatalf("concurrent Detach: %v", err)
	}
	manifest, err := sessions.LoadManifest(ctx, sessionID)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if manifest.Session.Status != session.StatusPaused || len(manifest.Segments) != 2 || len(manifest.Transitions) != 1 {
		t.Fatalf("handoff detach manifest = %#v", manifest)
	}
	activeAttempt := manifest.Attempts[manifest.Session.ActiveRunID]
	if activeAttempt.SegmentID == "" || activeAttempt.Status != session.AttemptStatusPausedAtBoundary {
		t.Fatalf("target attempt = %#v", activeAttempt)
	}

	firstFrames, firstIDs := attachFrameSuffix(t, sessions, sessionID, afterSequence)
	_, secondIDs := attachFrameSuffix(t, sessions, sessionID, afterSequence)
	if !reflect.DeepEqual(firstIDs, secondIDs) {
		t.Fatalf("reconnect frame ids changed: %#v != %#v", firstIDs, secondIDs)
	}
	foundTransition := false
	foundTargetGraph := false
	for _, frame := range firstFrames {
		foundTransition = foundTransition || frame.Type == session.FrameTransitionCommitted
		foundTargetGraph = foundTargetGraph || frame.Type == session.FrameSegmentGraph &&
			frame.SegmentID == activeAttempt.SegmentID
	}
	if !foundTransition || !foundTargetGraph {
		t.Fatalf("handoff reconnect frames = %#v", firstFrames)
	}
	finalManifest, err := sessions.LoadManifest(ctx, sessionID)
	if err != nil || len(finalManifest.Segments) != 2 || len(finalManifest.Transitions) != 1 {
		t.Fatalf("reconnect duplicated topology = %#v, %v", finalManifest, err)
	}
}

func attachFrameSuffix(
	t *testing.T,
	store *sessionstore.DirStore,
	sessionID string,
	afterSequence int64,
) ([]session.StdioFrame, []string) {
	t.Helper()
	var output bytes.Buffer
	attachment, err := Attach(context.Background(), store, sessionID, afterSequence, &output)
	if err != nil {
		t.Fatalf("Attach suffix: %v", err)
	}
	if err := attachment.Release(); err != nil {
		t.Fatalf("Release suffix: %v", err)
	}
	decoder := json.NewDecoder(&output)
	var frames []session.StdioFrame
	var ids []string
	for {
		var frame session.StdioFrame
		if err := decoder.Decode(&frame); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("decode suffix frame: %v", err)
		}
		frames = append(frames, frame)
		ids = append(ids, frame.FrameID)
	}
	return frames, ids
}

func newRuntimeCoordinator(
	t *testing.T,
	sessions session.Store,
	runs *runstore.DirRunStore,
	broker *serve.PromptBroker,
) *sessioncoordinator.Coordinator {
	t.Helper()
	registry := executor.NewMapRegistry()
	registry.Register("choice", executor.NewChoiceExecutor(internalinput.NewDurablePromptProvider(broker), nil))
	registry.Register("noop", executor.NewNoopExecutor(nil))
	runtime := internalengine.New(engine.EngineConfig{
		Executors: registry, Dispatcher: eventbus.NewDispatcher(), TraceWriter: runtimeDiscardTrace{},
		Platform: platform.NewFakePlatform(), Store: runs,
	})
	coordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
		Sessions: sessions, Runs: runs, Engine: runtime,
	})
	if err != nil {
		t.Fatalf("New coordinator: %v", err)
	}
	return coordinator
}

func runtimeGraph(t *testing.T, plan *engine.ExecutionPlan) json.RawMessage {
	t.Helper()
	graph, err := sessioncoordinator.BuildExecutionPlanGraph(plan)
	if err != nil {
		t.Fatalf("BuildExecutionPlanGraph: %v", err)
	}
	return graph
}

func waitRuntimePending(t *testing.T, frames <-chan any) serve.PendingInteraction {
	t.Helper()
	select {
	case frame := <-frames:
		pending, ok := frame.(serve.PendingInteraction)
		if !ok {
			t.Fatalf("frame = %T", frame)
		}
		return pending
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for pending interaction")
		return serve.PendingInteraction{}
	}
}

var _ input.PromptProvider = (*serve.PromptBroker)(nil)
var _ io.Writer = (*bytes.Buffer)(nil)
