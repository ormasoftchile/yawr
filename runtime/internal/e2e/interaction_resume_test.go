package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ormasoftchile/yawr/runtime/internal/adapter"
	internalengine "github.com/ormasoftchile/yawr/runtime/internal/engine"
	internalplanner "github.com/ormasoftchile/yawr/runtime/internal/planner"
	internalserve "github.com/ormasoftchile/yawr/runtime/internal/serve"
	internaltrace "github.com/ormasoftchile/yawr/runtime/internal/trace"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/input"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	tracepkg "github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

func TestE2E_ResumeRepublishesDurablePendingInteraction(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const runID = "run-durable-interaction-resume"

	firstRoot := t.TempDir()
	firstBroker := internalserve.NewPromptBroker(8)
	firstBroker.Register(runID)
	firstConfig, firstShutdown, err := adapter.BuildEngineConfig(ctx, adapter.WireOptions{
		Mode: "real", RunDir: filepath.Join(firstRoot, "runs"),
		TraceFile: filepath.Join(firstRoot, "trace.jsonl"), TTYOutput: false,
		ToolScanDir: firstRoot, PromptProviderOverride: firstBroker,
	})
	if err != nil {
		t.Fatalf("BuildEngineConfig first: %v", err)
	}
	t.Cleanup(firstShutdown)
	plan := engine.ValidatedForTest(&engine.ExecutionPlan{
		RunID: runID, RunbookPath: "interaction-resume.runbook.yaml",
		Steps: []engine.ResolvedStep{
			{ID: "choose", Name: "Choose", Kind: "choice", Spec: &schema.ChoiceSpec{
				Prompt: "Choose", Variable: "selected",
				Options: []schema.ChoiceOption{{Label: "One", Value: "one"}},
			}},
			{ID: "done", Name: "Done", Kind: "noop", Spec: &schema.NoopSpec{}},
		},
		Metadata: engine.PlanMetadata{
			RunbookID: "interaction-resume", RunbookName: "Interaction resume", PlanHash: "sha256:interaction-resume",
		},
	})
	firstHandle, err := internalengine.New(firstConfig).Start(ctx, plan, engine.RunOptions{Store: firstConfig.Store})
	if err != nil {
		t.Fatalf("Start first: %v", err)
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
	var firstPending internalserve.PendingInteraction
	select {
	case frame := <-firstFrames:
		var ok bool
		firstPending, ok = frame.(internalserve.PendingInteraction)
		if !ok {
			t.Fatalf("first nested interaction frame = %T", frame)
		}
	case nextErr := <-firstNext:
		t.Fatalf("first nested execution ended before prompt: %v", nextErr)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first nested interaction")
	}
	if firstPending.StepID != "choose" {
		t.Fatalf("first pending = %#v", firstPending)
	}
	waitingState := firstHandle.State()
	if waitingState.CursorSet == nil || len(waitingState.CursorSet.Cursors) != 1 {
		t.Fatalf("waiting cursor = %#v", waitingState.CursorSet)
	}
	waitingInvocation := waitingState.CursorSet.Cursors[0].Invocation

	secondRoot := t.TempDir()
	copyTree(t, filepath.Join(firstRoot, "runs"), filepath.Join(secondRoot, "runs"))
	firstBroker.Unregister(runID)
	select {
	case <-firstNext:
	case <-time.After(time.Second):
		t.Fatal("first process did not stop after broker shutdown")
	}

	secondBroker := internalserve.NewPromptBroker(8)
	secondBroker.Register(runID)
	t.Cleanup(func() { secondBroker.Unregister(runID) })
	secondConfig, secondShutdown, err := adapter.BuildEngineConfig(ctx, adapter.WireOptions{
		Mode: "real", RunDir: filepath.Join(secondRoot, "runs"),
		TraceFile: filepath.Join(secondRoot, "trace.jsonl"), TTYOutput: false,
		ToolScanDir: secondRoot, PromptProviderOverride: secondBroker,
	})
	if err != nil {
		t.Fatalf("BuildEngineConfig second: %v", err)
	}
	t.Cleanup(secondShutdown)
	secondHandle, err := internalengine.New(secondConfig).Resume(ctx, runID, engine.RunOptions{Store: secondConfig.Store})
	if err != nil {
		t.Fatalf("Resume second: %v", err)
	}
	secondFrames, err := secondBroker.Subscribe(runID, 0)
	if err != nil {
		t.Fatalf("Subscribe second: %v", err)
	}
	secondResult := make(chan *engine.StepResult, 1)
	secondNext := make(chan error, 1)
	go func() {
		result, nextErr := secondHandle.Next(ctx)
		secondResult <- result
		secondNext <- nextErr
	}()
	var secondPending internalserve.PendingInteraction
	select {
	case frame := <-secondFrames:
		var ok bool
		secondPending, ok = frame.(internalserve.PendingInteraction)
		if !ok {
			t.Fatalf("resumed nested interaction frame = %T", frame)
		}
	case nextErr := <-secondNext:
		t.Fatalf("resumed nested execution ended before prompt: %v", nextErr)
	case <-time.After(time.Second):
		state := secondHandle.State()
		t.Fatalf("timed out waiting for resumed nested interaction: status=%s frames=%#v interactions=%#v",
			state.Status, state.ExecutionFrames, state.Interactions)
	}
	if secondPending.TurnID != firstPending.TurnID {
		t.Fatalf("resumed turn ID = %q, want %q", secondPending.TurnID, firstPending.TurnID)
	}
	resumedWaitingState := secondHandle.State()
	if resumedWaitingState.Status != engine.RunStatusWaiting ||
		resumedWaitingState.CheckpointSequence != waitingState.CheckpointSequence+1 ||
		resumedWaitingState.Interactions[firstPending.TurnID] == nil {
		t.Fatalf("resumed waiting state = sequence %d status %s interactions %#v",
			resumedWaitingState.CheckpointSequence, resumedWaitingState.Status, resumedWaitingState.Interactions)
	}
	if err := secondBroker.Answer(runID, secondPending.TurnID, internalserve.AnswerEnvelope{
		Kind: "choice", Selected: []string{"o:0"},
	}); err != nil {
		t.Fatalf("Answer resumed choice: %v", err)
	}
	if err := <-secondNext; err != nil {
		t.Fatalf("resumed Next: %v", err)
	}
	result := <-secondResult
	if result == nil || result.Status != engine.StepStatusCompleted || result.Vars["selected"] != "one" {
		t.Fatalf("resumed choice result = %#v", result)
	}
	state := secondHandle.State()
	if len(state.Interactions) != 0 || state.CheckpointSequence != resumedWaitingState.CheckpointSequence+2 {
		t.Fatalf("settled state = sequence %d interactions %#v", state.CheckpointSequence, state.Interactions)
	}
	if count := state.ExecutionInvocationCounts["choose"]; count != waitingInvocation {
		t.Fatalf("resumed execution invocation count = %d, want waiting invocation %d", count, waitingInvocation)
	}
	resumedEvents, err := internaltrace.NewJSONLReader(filepath.Join(secondRoot, "trace.jsonl")).ReadAll(ctx)
	if err != nil {
		t.Fatalf("ReadAll resumed trace: %v", err)
	}
	completedInvocation := 0
	for _, event := range resumedEvents {
		if event.Kind != tracepkg.EventKindStepCompleted {
			continue
		}
		var payload struct {
			StepID     string `json:"step_id"`
			Invocation int    `json:"invocation"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("decode resumed completion: %v", err)
		}
		if payload.StepID == "choose" {
			completedInvocation = payload.Invocation
		}
	}
	if completedInvocation != waitingInvocation {
		t.Fatalf("resumed completion invocation = %d, want waiting invocation %d", completedInvocation, waitingInvocation)
	}
	if _, err := secondHandle.Next(ctx); err != nil {
		t.Fatalf("execute step after resumed choice: %v", err)
	}
	if _, err := secondHandle.Next(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("finish resumed run: %v", err)
	}
}

func TestE2E_ResumeRepublishesPendingInteractionInsideIncludeFrame(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const runID = "run-nested-interaction-resume"
	plan := &engine.ExecutionPlan{
		RunID: runID, RunbookPath: "nested-interaction.runbook.yaml",
		Steps: []engine.ResolvedStep{{
			ID: "include", Kind: "include", Spec: &schema.IncludeSpec{
				Include: schema.IncludeConfig{Runbook: "child.runbook.yaml"},
				ResolvedSteps: []schema.FlowNode{
					{Step: &schema.Step{ID: "choose", Type: schema.StepTypeChoice, ChoiceSpec: &schema.ChoiceSpec{
						Prompt: "Choose", Variable: "selected",
						Options: []schema.ChoiceOption{{Label: "One", Value: "one"}},
					}}},
					{Step: &schema.Step{ID: "done", Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{}}},
				},
			},
		}},
		Metadata: engine.PlanMetadata{RunbookID: "nested-interaction", RunbookName: "Nested interaction"},
	}
	if err := internalplanner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}

	firstRoot := t.TempDir()
	firstBroker := internalserve.NewPromptBroker(8)
	firstBroker.Register(runID)
	firstConfig, firstShutdown, err := adapter.BuildEngineConfig(ctx, adapter.WireOptions{
		Mode: "real", RunDir: filepath.Join(firstRoot, "runs"), TraceFile: filepath.Join(firstRoot, "trace.jsonl"),
		TTYOutput: false, ToolScanDir: firstRoot, PromptProviderOverride: firstBroker,
	})
	if err != nil {
		t.Fatalf("BuildEngineConfig first: %v", err)
	}
	t.Cleanup(firstShutdown)
	firstHandle, err := internalengine.New(firstConfig).Start(ctx, plan, engine.RunOptions{Store: firstConfig.Store})
	if err != nil {
		t.Fatalf("Start first: %v", err)
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
	var firstPending internalserve.PendingInteraction
	select {
	case frame := <-firstFrames:
		var ok bool
		firstPending, ok = frame.(internalserve.PendingInteraction)
		if !ok {
			t.Fatalf("first nested interaction frame = %T", frame)
		}
	case nextErr := <-firstNext:
		t.Fatalf("first nested execution ended before prompt: %v", nextErr)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first nested interaction")
	}
	if firstPending.NodeID != "include/choose" {
		t.Fatalf("first nested pending node = %q", firstPending.NodeID)
	}
	nestedWaitingState := firstHandle.State()
	if nestedWaitingState.CursorSet == nil || len(nestedWaitingState.CursorSet.Cursors) != 1 {
		t.Fatalf("nested waiting cursor = %#v", nestedWaitingState.CursorSet)
	}
	nestedWaitingCursor := nestedWaitingState.CursorSet.Cursors[0]
	if nestedWaitingCursor.QualifiedNodeID != "include/choose" || nestedWaitingCursor.Invocation < 1 {
		t.Fatalf("nested waiting cursor = %#v", nestedWaitingCursor)
	}

	secondRoot := t.TempDir()
	copyTree(t, filepath.Join(firstRoot, "runs"), filepath.Join(secondRoot, "runs"))
	firstBroker.Unregister(runID)
	select {
	case <-firstNext:
	case <-time.After(time.Second):
		t.Fatal("first process did not stop after broker shutdown")
	}

	secondBroker := internalserve.NewPromptBroker(8)
	secondBroker.Register(runID)
	t.Cleanup(func() { secondBroker.Unregister(runID) })
	secondConfig, secondShutdown, err := adapter.BuildEngineConfig(ctx, adapter.WireOptions{
		Mode: "real", RunDir: filepath.Join(secondRoot, "runs"), TraceFile: filepath.Join(secondRoot, "trace.jsonl"),
		TTYOutput: false, ToolScanDir: secondRoot, PromptProviderOverride: secondBroker,
	})
	if err != nil {
		t.Fatalf("BuildEngineConfig second: %v", err)
	}
	t.Cleanup(secondShutdown)
	secondHandle, err := internalengine.New(secondConfig).Resume(ctx, runID, engine.RunOptions{Store: secondConfig.Store})
	if err != nil {
		t.Fatalf("Resume second: %v", err)
	}
	secondFrames, err := secondBroker.Subscribe(runID, 0)
	if err != nil {
		t.Fatalf("Subscribe second: %v", err)
	}
	secondResult := make(chan *engine.StepResult, 1)
	secondNext := make(chan error, 1)
	go func() {
		result, nextErr := secondHandle.Next(ctx)
		secondResult <- result
		secondNext <- nextErr
	}()
	var secondPending internalserve.PendingInteraction
	select {
	case frame := <-secondFrames:
		var ok bool
		secondPending, ok = frame.(internalserve.PendingInteraction)
		if !ok {
			t.Fatalf("resumed nested interaction frame = %T", frame)
		}
	case nextErr := <-secondNext:
		t.Fatalf("resumed nested execution ended before prompt: %v", nextErr)
	case <-time.After(time.Second):
		state := secondHandle.State()
		t.Fatalf("timed out waiting for resumed nested interaction: status=%s frames=%#v interactions=%#v",
			state.Status, state.ExecutionFrames, state.Interactions)
	}
	if secondPending.TurnID != firstPending.TurnID || secondPending.NodeID != "include/choose" {
		t.Fatalf("resumed nested pending = %#v, want turn %s", secondPending, firstPending.TurnID)
	}
	if err := secondBroker.Answer(runID, secondPending.TurnID, internalserve.AnswerEnvelope{
		Kind: "choice", Selected: []string{"o:0"},
	}); err != nil {
		t.Fatalf("Answer resumed nested choice: %v", err)
	}
	if err := <-secondNext; err != nil {
		t.Fatalf("resumed nested Next: %v", err)
	}
	result := <-secondResult
	if result == nil || result.Status != engine.StepStatusCompleted || result.Vars["selected"] != "one" {
		t.Fatalf("resumed include result = %#v", result)
	}
	state := secondHandle.State()
	if len(state.Interactions) != 0 || len(state.ExecutionFrames) != 1 {
		t.Fatalf("settled nested state interactions=%#v frames=%#v", state.Interactions, state.ExecutionFrames)
	}
	for nodeID, waitingCount := range nestedWaitingState.ExecutionInvocationCounts {
		if settledCount := state.ExecutionInvocationCounts[nodeID]; settledCount != waitingCount {
			t.Fatalf("nested resumed invocation %s = %d, want %d", nodeID, settledCount, waitingCount)
		}
	}
	nestedEvents, err := internaltrace.NewJSONLReader(filepath.Join(secondRoot, "trace.jsonl")).ReadAll(ctx)
	if err != nil {
		t.Fatalf("ReadAll nested resumed trace: %v", err)
	}
	nestedCompletedInvocation := 0
	for _, event := range nestedEvents {
		if event.Kind != tracepkg.EventKindStepCompleted {
			continue
		}
		var payload struct {
			StepID     string `json:"step_id"`
			Invocation int    `json:"invocation"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("decode nested resumed completion: %v", err)
		}
		if payload.StepID == "choose" {
			nestedCompletedInvocation = payload.Invocation
		}
	}
	if nestedCompletedInvocation != nestedWaitingCursor.Invocation {
		t.Fatalf("nested resumed completion invocation = %d, want %d", nestedCompletedInvocation, nestedWaitingCursor.Invocation)
	}
	for _, frame := range state.ExecutionFrames {
		if frame.Status != engine.ExecutionFrameStatusCompleted || frame.NextStepIndex != 2 {
			t.Fatalf("settled nested frame = %#v", frame)
		}
	}
	if _, err := secondHandle.Next(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("finish resumed nested run: %v", err)
	}
}

func waitPendingInteraction(t *testing.T, frames <-chan any) internalserve.PendingInteraction {
	t.Helper()
	select {
	case frame := <-frames:
		pending, ok := frame.(internalserve.PendingInteraction)
		if !ok {
			t.Fatalf("interaction frame = %T, want PendingInteraction", frame)
		}
		return pending
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for pending interaction")
		return internalserve.PendingInteraction{}
	}
}

func copyTree(t *testing.T, source, destination string) {
	t.Helper()
	err := filepath.Walk(source, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if info.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if strings.HasSuffix(info.Name(), ".writer.lock") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	})
	if err != nil {
		t.Fatalf("copy run store: %v", err)
	}
}

var _ input.PromptProvider = (*internalserve.PromptBroker)(nil)
