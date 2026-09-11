package runstore_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	"github.com/ormasoftchile/yawr/runtime/internal/runstore"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func newStore(t *testing.T) *runstore.DirRunStore {
	t.Helper()
	return runstore.NewDirRunStore(t.TempDir())
}

type blockingJSONValue struct {
	entered chan struct{}
	release chan struct{}
	once    *sync.Once
}

func (value blockingJSONValue) MarshalJSON() ([]byte, error) {
	value.once.Do(func() { close(value.entered) })
	<-value.release
	return []byte(`"serialized"`), nil
}

// TestDirRunStore_SaveLoadState tests round-trip state serialization.
func TestDirRunStore_SaveLoadState(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	state := engine.RunState{
		RunID:            "run-abc",
		RunbookPath:      "deploy.runbook.yaml",
		Status:           engine.RunStatusRunning,
		CurrentStep:      "step-1",
		CurrentStepIndex: 1,
		Vars:             map[string]any{"env": "prod"},
		StartedAt:        time.Now().Truncate(time.Second),
		UpdatedAt:        time.Now().Truncate(time.Second),
	}

	if err := s.SaveState(ctx, state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	loaded, err := s.LoadState(ctx, state.RunID)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if loaded.RunID != state.RunID {
		t.Errorf("RunID: got %q, want %q", loaded.RunID, state.RunID)
	}
	if loaded.Status != state.Status {
		t.Errorf("Status: got %q, want %q", loaded.Status, state.Status)
	}
	if loaded.CurrentStep != state.CurrentStep {
		t.Errorf("CurrentStep: got %q, want %q", loaded.CurrentStep, state.CurrentStep)
	}
	if loaded.CurrentStepIndex != state.CurrentStepIndex {
		t.Errorf("CurrentStepIndex: got %d, want %d", loaded.CurrentStepIndex, state.CurrentStepIndex)
	}
}

func TestDirRunStore_SaveLoadStateOmitsPlanAndPreservesLargeInteger(t *testing.T) {
	s := newStore(t)
	state := engine.RunState{
		RunID: "run-state-dto", RunbookPath: "state.runbook.yaml", Status: engine.RunStatusRunning,
		CurrentStep: "step-1", CurrentStepIndex: 0,
		Vars: map[string]any{"large_sequence": int64(9007199254740993)},
		Plan: &engine.ExecutionPlan{RunbookPath: "must-not-be-embedded"},
	}
	if err := s.SaveState(context.Background(), state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(s.RunDir(state.RunID), "snapshots", "step-0000.json"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if bytes.Contains(data, []byte(`"Plan"`)) || bytes.Contains(data, []byte("must-not-be-embedded")) {
		t.Fatal("state snapshot embedded the execution plan")
	}
	loaded, err := s.LoadState(context.Background(), state.RunID)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if loaded.Plan != nil {
		t.Fatal("loaded state unexpectedly contains a plan")
	}
	if got, ok := loaded.Vars["large_sequence"].(json.Number); !ok || got.String() != "9007199254740993" {
		t.Fatalf("large integer changed: %#v", loaded.Vars["large_sequence"])
	}
}

func TestDirRunStore_SaveLoadStatePreservesSettledResults(t *testing.T) {
	s := newStore(t)
	state := engine.RunState{
		RunID: "run-results", Status: engine.RunStatusRunning, CheckpointSequence: 1,
		StepResults: map[string]*engine.StepResult{
			"collect": {
				StepID: "collect", Status: engine.StepStatusFailed, Outcome: engine.StepOutcomeFailed,
				Output: map[string]any{"large_sequence": int64(9007199254740993)},
				Vars:   map[string]any{"region": "westus"}, Error: errors.New("saved failure"),
			},
		},
	}
	if err := s.SaveState(context.Background(), state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	loaded, err := s.LoadState(context.Background(), state.RunID)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	result := loaded.StepResults["collect"]
	if result == nil || result.Error == nil || result.Error.Error() != "saved failure" {
		t.Fatalf("settled result error changed: %#v", result)
	}
	if got, ok := result.Output["large_sequence"].(json.Number); !ok || got.String() != "9007199254740993" {
		t.Fatalf("settled result output changed: %#v", result.Output)
	}
	if result.Vars["region"] != "westus" {
		t.Fatalf("settled result vars changed: %#v", result.Vars)
	}
}

func TestDirRunStore_SaveLoadStatePreservesInteractions(t *testing.T) {
	store := newStore(t)
	pendingRequest := json.RawMessage(`{"kind":"choice","options":["one","two"]}`)
	approvalRequest := json.RawMessage(`{"kind":"approval"}`)
	approvalAnswer := json.RawMessage(`{"approved":true}`)
	invocations := engine.NewInteractionInvocationTracker()
	invocations.Next("include/choose", "choice")
	invocations.Next("include/approve", "approval")
	invocations.Next("include/approve", "approval")
	state := engine.RunState{
		RunID: "run-interactions", Status: engine.RunStatusWaiting, CheckpointSequence: 1,
		InteractionInvocationCounts: invocations.Snapshot(),
		Interactions: map[string]*engine.InteractionState{
			"turn-pending": {
				SchemaVersion: engine.InteractionStateSchemaV1,
				TurnID:        "turn-pending", OwnerStepID: "inspect", NodeID: "include/choose",
				StepID: "choose", Kind: "choice", Ordinal: 1,
				Status:        engine.InteractionStatusPending,
				RequestDigest: engine.InteractionPayloadDigest(pendingRequest), Request: pendingRequest,
			},
			"turn-answered": {
				SchemaVersion: engine.InteractionStateSchemaV1,
				TurnID:        "turn-answered", OwnerStepID: "inspect", NodeID: "include/approve",
				StepID: "approve", Kind: "approval", Ordinal: 2,
				Status:        engine.InteractionStatusAnswered,
				RequestDigest: engine.InteractionPayloadDigest(approvalRequest), Request: approvalRequest,
				AnswerDigest: engine.InteractionPayloadDigest(approvalAnswer), Answer: approvalAnswer,
				AcceptedAt: "2026-08-31T00:00:00Z", AuditToken: "turn-answered",
			},
		},
	}
	if err := store.SaveState(context.Background(), state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	loaded, err := store.LoadState(context.Background(), state.RunID)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(loaded.Interactions) != 2 {
		t.Fatalf("interactions = %#v, want 2", loaded.Interactions)
	}
	pending := loaded.Interactions["turn-pending"]
	if pending == nil || pending.Status != engine.InteractionStatusPending || pending.NodeID != "include/choose" || pending.Ordinal != 1 {
		t.Fatalf("pending interaction changed: %#v", pending)
	}
	answered := loaded.Interactions["turn-answered"]
	if answered == nil || answered.Status != engine.InteractionStatusAnswered || answered.AnswerDigest != engine.InteractionPayloadDigest(approvalAnswer) ||
		!bytes.Equal(answered.Answer, []byte(`{"approved":true}`)) {
		t.Fatalf("answered interaction changed: %#v", answered)
	}
}

func TestDirRunStorePreservesAndValidatesExecutionInvocationCounts(t *testing.T) {
	store := newStore(t)
	tracker := engine.NewDebugInvocationTracker()
	tracker.Next([]engine.DebugCallFrame{{StepID: "loop"}}, "target")
	tracker.Next([]engine.DebugCallFrame{{StepID: "loop"}}, "target")
	state := engine.RunState{
		RunID: "run-execution-invocations", Status: engine.RunStatusRunning, CheckpointSequence: 1,
		ExecutionInvocationCounts: tracker.Snapshot(),
	}
	if err := store.SaveState(context.Background(), state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	loaded, err := store.LoadState(context.Background(), state.RunID)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got := loaded.ExecutionInvocationCounts["loop/target"]; got != 2 {
		t.Fatalf("execution invocation count = %d, want 2", got)
	}
	state.RunID = "run-malformed-execution-invocations"
	state.ExecutionInvocationCounts = map[string]int{"loop/target": 0}
	if err := store.SaveState(context.Background(), state); err == nil || !strings.Contains(err.Error(), "execution invocation") {
		t.Fatalf("SaveState malformed count error = %v", err)
	}
}

func TestDirRunStore_SaveLoadStatePreservesDispatchJournal(t *testing.T) {
	store := newStore(t)
	const runID = "run-dispatch-journal"
	lease, err := store.AcquireRunLease(context.Background(), runID)
	if err != nil {
		t.Fatalf("AcquireRunLease: %v", err)
	}
	defer lease.Release()
	preparedAt := "2026-08-31T01:00:00Z"
	requestDigest := engine.InteractionPayloadDigest([]byte(`{"command":"inspect"}`))
	occurrenceID := engine.InteractionPayloadDigest([]byte("run-dispatch-journal/inspect/1/1"))
	idempotencyKey := engine.InteractionPayloadDigest([]byte("idempotency/" + occurrenceID))
	state := engine.RunState{
		RunID: runID, WriterEpoch: lease.Epoch(), Status: engine.RunStatusRunning, CheckpointSequence: 1,
		Dispatches: map[string]*engine.DispatchState{
			occurrenceID: {
				SchemaVersion: engine.DispatchStateSchemaV1, OccurrenceID: occurrenceID,
				WriterEpoch: lease.Epoch(), QualifiedNodeID: "inspect", StepID: "inspect",
				Phase: engine.ExecutionPhaseExecute, Invocation: 1, RetryAttempt: 1, OccurrenceSequence: 1,
				Classification: "read-only", EndpointIdentity: "local:pwsh",
				RequestDigest: requestDigest, IdempotencyKey: idempotencyKey,
				Status: engine.DispatchStatusPrepared, PreparedAt: preparedAt,
			},
		},
	}
	if err := store.SaveState(context.Background(), state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	loaded, err := store.LoadState(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	dispatch := loaded.Dispatches[occurrenceID]
	if dispatch == nil || dispatch.Status != engine.DispatchStatusPrepared || dispatch.WriterEpoch != lease.Epoch() ||
		dispatch.RequestDigest != requestDigest || dispatch.IdempotencyKey != idempotencyKey || dispatch.PreparedAt != preparedAt {
		t.Fatalf("dispatch changed during round trip: %#v", dispatch)
	}
}

func TestDirRunStore_SaveLoadStatePreservesExecutionFrames(t *testing.T) {
	store := newStore(t)
	const runID = "run-execution-frames"
	lease, err := store.AcquireRunLease(context.Background(), runID)
	if err != nil {
		t.Fatalf("AcquireRunLease: %v", err)
	}
	defer lease.Release()
	frameID := engine.InteractionPayloadDigest([]byte("branch frame"))
	definitionDigest := engine.InteractionPayloadDigest([]byte("child-a,child-b"))
	large := strings.Repeat("frame-value-", 10000)
	state := engine.RunState{
		RunID: runID, WriterEpoch: lease.Epoch(), Status: engine.RunStatusRunning, CheckpointSequence: 1,
		ExecutionFrames: map[string]*engine.ExecutionFrameState{
			frameID: {
				SchemaVersion: engine.ExecutionFrameStateSchemaV1, FrameID: frameID,
				WriterEpoch: lease.Epoch(), ParentQualifiedNodeID: "choose-path", ParentStepID: "choose-path",
				Kind: "branch", CallPath: []engine.DebugCallFrame{{StepID: "choose-path"}}, BranchLabel: "active",
				Invocation: 1, DefinitionDigest: definitionDigest, StepCount: 2,
				StepIDs: []string{"child-a", "child-b"}, NextStepIndex: 1,
				WorkingVars: map[string]any{"large": large},
				Results: map[string]*engine.StepResult{
					"0": {StepID: "child-a", Status: engine.StepStatusCompleted, Output: map[string]any{"large": large}},
				},
				Status: engine.ExecutionFrameStatusActive, StartedAt: "2026-08-31T02:00:00Z",
			},
		},
	}
	if err := store.SaveState(context.Background(), state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	loaded, err := store.LoadState(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	frame := loaded.ExecutionFrames[frameID]
	if frame == nil || frame.Status != engine.ExecutionFrameStatusActive || frame.NextStepIndex != 1 ||
		frame.WorkingVars["large"] != large || frame.Results["0"].Output["large"] != large {
		t.Fatalf("execution frame changed during round trip: %#v", frame)
	}
}

func TestDirRunStore_RejectsInconsistentExecutionStateRelationships(t *testing.T) {
	makeFrame := func(epoch uint64) (string, *engine.ExecutionFrameState) {
		frameID := engine.InteractionPayloadDigest([]byte("relationship frame"))
		return frameID, &engine.ExecutionFrameState{
			SchemaVersion: engine.ExecutionFrameStateSchemaV1, FrameID: frameID, WriterEpoch: epoch,
			ParentQualifiedNodeID: "parent", ParentStepID: "parent", Kind: "include",
			CallPath: []engine.DebugCallFrame{{StepID: "parent"}}, Invocation: 1,
			DefinitionDigest: engine.InteractionPayloadDigest([]byte("children")),
			StepCount:        1, StepIDs: []string{"child"}, Results: map[string]*engine.StepResult{},
			Status: engine.ExecutionFrameStatusActive, StartedAt: "2026-08-31T04:00:00Z",
		}
	}
	t.Run("dispatch references missing frame", func(t *testing.T) {
		store := newStore(t)
		const runID = "run-missing-dispatch-frame"
		lease, err := store.AcquireRunLease(context.Background(), runID)
		if err != nil {
			t.Fatalf("AcquireRunLease: %v", err)
		}
		defer lease.Release()
		frameID, _ := makeFrame(lease.Epoch())
		occurrenceID := engine.InteractionPayloadDigest([]byte("missing frame dispatch"))
		state := engine.RunState{
			RunID: runID, WriterEpoch: lease.Epoch(), CheckpointSequence: 1,
			Dispatches: map[string]*engine.DispatchState{
				occurrenceID: {
					SchemaVersion: engine.DispatchStateSchemaV1, OccurrenceID: occurrenceID,
					WriterEpoch: lease.Epoch(), QualifiedNodeID: "parent/child",
					CallPath: []engine.DebugCallFrame{{StepID: "parent"}}, StepID: "child",
					FrameID: frameID, Phase: engine.ExecutionPhaseExecute,
					Invocation: 1, RetryAttempt: 1, OccurrenceSequence: 1,
					Classification: "unspecified", EndpointIdentity: "provider",
					RequestDigest:  engine.InteractionPayloadDigest([]byte("request")),
					IdempotencyKey: engine.InteractionPayloadDigest([]byte("key")),
					Status:         engine.DispatchStatusPrepared, PreparedAt: "2026-08-31T04:00:00Z",
				},
			},
		}
		if err := store.SaveState(context.Background(), state); err == nil || !strings.Contains(err.Error(), "frame") {
			t.Fatalf("SaveState error = %v, want missing frame rejection", err)
		}
	})

	t.Run("cursor points at wrong frame child", func(t *testing.T) {
		store := newStore(t)
		const runID = "run-wrong-frame-cursor"
		lease, err := store.AcquireRunLease(context.Background(), runID)
		if err != nil {
			t.Fatalf("AcquireRunLease: %v", err)
		}
		defer lease.Release()
		frameID, frame := makeFrame(lease.Epoch())
		state := engine.RunState{
			RunID: runID, WriterEpoch: lease.Epoch(), CheckpointSequence: 1,
			ExecutionFrames: map[string]*engine.ExecutionFrameState{frameID: frame},
			CursorSet: &engine.ExecutionCursorSet{
				SchemaVersion: engine.ExecutionCursorSchemaV1,
				Cursors: []engine.ExecutionCursor{{
					QualifiedNodeID: "parent/not-child", CallPath: []engine.DebugCallFrame{{StepID: "parent"}},
					StepID: "not-child", StepIndex: 0, FrameID: frameID,
					Phase: engine.ExecutionPhaseBefore, Invocation: 1, RetryAttempt: 1,
				}},
			},
		}
		if err := store.SaveState(context.Background(), state); err == nil || !strings.Contains(err.Error(), "cursor") {
			t.Fatalf("SaveState error = %v, want cursor/frame rejection", err)
		}
	})
}

func TestDirRunStore_SaveLoadStatePreservesDynamicIncludeResolutions(t *testing.T) {
	store := newStore(t)
	const runID = "run-dynamic-resolution"
	lease, err := store.AcquireRunLease(context.Background(), runID)
	if err != nil {
		t.Fatalf("AcquireRunLease: %v", err)
	}
	defer lease.Release()
	resolutionID := engine.InteractionPayloadDigest([]byte("dynamic resolution"))
	closure, err := plansnapshot.EncodeFlowClosure([]schema.FlowNode{{Step: &schema.Step{
		ID: "captured-child", Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{},
	}}})
	if err != nil {
		t.Fatalf("EncodeFlowClosure: %v", err)
	}
	state := engine.RunState{
		RunID: runID, WriterEpoch: lease.Epoch(), Status: engine.RunStatusRunning, CheckpointSequence: 1,
		DynamicIncludes: map[string]*engine.DynamicIncludeResolutionState{
			resolutionID: {
				SchemaVersion: engine.DynamicIncludeResolutionStateSchemaV1, ResolutionID: resolutionID,
				WriterEpoch: lease.Epoch(), QualifiedNodeID: "include", StepID: "include", Invocation: 1, Revision: 1,
				Pin: schema.LockedDynamicInclude{
					StepID: "include", QualifiedNodeID: "include", Invocation: 1, Revision: 1,
					RenderedRef: "acme/child", QualifiedID: "acme/child",
					RunbookID: "child", RunbookName: "Child", RunbookContentHash: strings.Repeat("a", 64),
					AbsPath: `C:\workspace\child.runbook.yaml`, PackageName: "acme", PackageVersion: "1.0.0",
					FileDigest:        engine.InteractionPayloadDigest([]byte("file")),
					PackageDigest:     engine.InteractionPayloadDigest([]byte("package")),
					ExecutableClosure: closure,
					ResolvedInputs:    map[string]*schema.Input{"server": {Type: "string", Required: true}},
				},
				Status: engine.DynamicIncludeResolutionStatusActive, CommittedAt: "2026-08-31T03:00:00Z",
			},
		},
	}
	if err := store.SaveState(context.Background(), state); err == nil {
		t.Fatal("SaveState accepted a dynamic resolution without resolver dispatch provenance")
	}
	resolution := state.DynamicIncludes[resolutionID]
	encodedPin, err := json.Marshal(resolution.Pin)
	if err != nil {
		t.Fatalf("marshal pin: %v", err)
	}
	dispatchID := engine.InteractionPayloadDigest([]byte("resolver dispatch"))
	state.Dispatches = map[string]*engine.DispatchState{
		dispatchID: {
			SchemaVersion: engine.DispatchStateSchemaV1, OccurrenceID: dispatchID,
			WriterEpoch: lease.Epoch(), QualifiedNodeID: "include", StepID: "include",
			Phase: engine.ExecutionPhaseExecute, Invocation: 1, RetryAttempt: 1, OccurrenceSequence: 1,
			Classification: "read-only", EndpointIdentity: "dynamic-include-resolver",
			RequestDigest:  engine.InteractionPayloadDigest([]byte("request")),
			IdempotencyKey: engine.InteractionPayloadDigest([]byte("idempotency")),
			Status:         engine.DispatchStatusSettled, ResultDigest: engine.InteractionPayloadDigest(encodedPin),
			PreparedAt: "2026-08-31T02:59:59Z", SettledAt: "2026-08-31T03:00:00Z",
		},
	}
	if err := store.SaveState(context.Background(), state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	loaded, err := store.LoadState(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	resolution = loaded.DynamicIncludes[resolutionID]
	if resolution == nil || resolution.Status != engine.DynamicIncludeResolutionStatusActive ||
		resolution.Pin.QualifiedID != "acme/child" || resolution.WriterEpoch != lease.Epoch() ||
		len(resolution.Pin.ExecutableClosure) == 0 || resolution.Pin.ResolvedInputs["server"] == nil {
		t.Fatalf("dynamic include resolution changed: %#v", resolution)
	}
	restoredFlow, err := plansnapshot.RestoreFlowClosure(resolution.Pin.ExecutableClosure)
	if err != nil || len(restoredFlow) != 1 || restoredFlow[0].Step.ID != "captured-child" {
		t.Fatalf("restored dynamic closure = %#v, %v", restoredFlow, err)
	}
}

func TestDirRunStore_RequiresDigestBoundDynamicIncludeNotFoundResult(t *testing.T) {
	makeState := func(t *testing.T, runID string) (*runstore.DirRunStore, engine.RunState) {
		t.Helper()
		store := newStore(t)
		lease, err := store.AcquireRunLease(context.Background(), runID)
		if err != nil {
			t.Fatalf("AcquireRunLease: %v", err)
		}
		t.Cleanup(func() { _ = lease.Release() })
		result := &engine.StepResult{
			StepID: "include", Status: engine.StepStatusSkipped, Outcome: engine.StepOutcomeSkipped,
			Output: map[string]any{"skip_reason": "include_not_found"},
			Vars:   map[string]any{"runbook_found": false},
		}
		digest, ok := engine.DynamicIncludeNotFoundResultDigest(result)
		if !ok {
			t.Fatal("valid not-found result did not produce a digest")
		}
		dispatchID := engine.InteractionPayloadDigest([]byte(runID + " resolver dispatch"))
		return store, engine.RunState{
			RunID: runID, WriterEpoch: lease.Epoch(), Status: engine.RunStatusRunning, CheckpointSequence: 1,
			StepResults: map[string]*engine.StepResult{"include": result},
			Dispatches: map[string]*engine.DispatchState{dispatchID: {
				SchemaVersion: engine.DispatchStateSchemaV1, OccurrenceID: dispatchID,
				WriterEpoch: lease.Epoch(), QualifiedNodeID: "include", StepID: "include",
				Phase: engine.ExecutionPhaseExecute, Invocation: 1, RetryAttempt: 1, OccurrenceSequence: 1,
				Classification: "read-only", EndpointIdentity: "dynamic-include-resolver",
				RequestDigest:  engine.InteractionPayloadDigest([]byte("request")),
				IdempotencyKey: engine.InteractionPayloadDigest([]byte("idempotency")),
				Status:         engine.DispatchStatusSettled, ResultDigest: digest,
				PreparedAt: "2026-08-31T02:59:59Z", SettledAt: "2026-08-31T03:00:00Z",
			}},
		}
	}

	t.Run("valid", func(t *testing.T) {
		store, state := makeState(t, "run-dynamic-not-found")
		if err := store.SaveState(context.Background(), state); err != nil {
			t.Fatalf("SaveState: %v", err)
		}
		if _, err := store.LoadState(context.Background(), state.RunID); err != nil {
			t.Fatalf("LoadState: %v", err)
		}
	})

	t.Run("tampered digest", func(t *testing.T) {
		store, state := makeState(t, "run-dynamic-not-found-digest")
		for _, dispatch := range state.Dispatches {
			dispatch.ResultDigest = engine.InteractionPayloadDigest([]byte("tampered"))
		}
		if err := store.SaveState(context.Background(), state); err == nil || !strings.Contains(err.Error(), "no dynamic include resolution") {
			t.Fatalf("SaveState error = %v, want unpaired resolver rejection", err)
		}
	})

	t.Run("forged marker", func(t *testing.T) {
		store, state := makeState(t, "run-dynamic-not-found-marker")
		state.StepResults["include"].Output["skip_reason"] = "condition_false"
		if err := store.SaveState(context.Background(), state); err == nil || !strings.Contains(err.Error(), "no dynamic include resolution") {
			t.Fatalf("SaveState error = %v, want unpaired resolver rejection", err)
		}
	})

	t.Run("duplicate dispatch", func(t *testing.T) {
		store, state := makeState(t, "run-dynamic-not-found-duplicate")
		var duplicate engine.DispatchState
		for _, dispatch := range state.Dispatches {
			duplicate = *dispatch
		}
		duplicate.OccurrenceID = engine.InteractionPayloadDigest([]byte("duplicate resolver dispatch"))
		duplicate.OccurrenceSequence++
		duplicate.IdempotencyKey = engine.InteractionPayloadDigest([]byte("duplicate idempotency"))
		state.Dispatches[duplicate.OccurrenceID] = &duplicate
		if err := store.SaveState(context.Background(), state); err == nil || !strings.Contains(err.Error(), "ambiguous") {
			t.Fatalf("SaveState error = %v, want ambiguous not-found dispatch rejection", err)
		}
	})

	t.Run("repeated invocation", func(t *testing.T) {
		store, state := makeState(t, "run-dynamic-not-found-repeated")
		var repeated engine.DispatchState
		for _, dispatch := range state.Dispatches {
			repeated = *dispatch
		}
		repeated.OccurrenceID = engine.InteractionPayloadDigest([]byte("repeated resolver dispatch"))
		repeated.OccurrenceSequence++
		repeated.Invocation++
		repeated.IdempotencyKey = engine.InteractionPayloadDigest([]byte("repeated idempotency"))
		state.Dispatches[repeated.OccurrenceID] = &repeated
		if err := store.SaveState(context.Background(), state); err != nil {
			t.Fatalf("SaveState: %v", err)
		}
	})

	t.Run("historical negative chain", func(t *testing.T) {
		store, state := makeState(t, "run-dynamic-not-found-chain")
		var first engine.DispatchState
		for _, dispatch := range state.Dispatches {
			first = *dispatch
		}
		second := first
		second.OccurrenceID = engine.InteractionPayloadDigest([]byte("second negative resolver dispatch"))
		second.OccurrenceSequence, second.Invocation = 2, 2
		second.IdempotencyKey = engine.InteractionPayloadDigest([]byte("second negative idempotency"))
		state.Dispatches[second.OccurrenceID] = &second
		closure, err := plansnapshot.EncodeFlowClosure([]schema.FlowNode{{Step: &schema.Step{
			ID: "child", Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{},
		}}})
		if err != nil {
			t.Fatalf("EncodeFlowClosure: %v", err)
		}
		pin := schema.LockedDynamicInclude{
			StepID: "include", QualifiedNodeID: "include", Invocation: 3, Revision: 1,
			RenderedRef: "pkg/child", QualifiedID: "pkg/child", RunbookID: "child", RunbookName: "Child",
			RunbookContentHash: strings.Repeat("a", 64), AbsPath: `C:\workspace\child.runbook.yaml`,
			PackageName: "pkg", PackageVersion: "1.0.0",
			FileDigest:    engine.InteractionPayloadDigest([]byte("file")),
			PackageDigest: engine.InteractionPayloadDigest([]byte("package")), ExecutableClosure: closure,
		}
		encodedPin, err := json.Marshal(pin)
		if err != nil {
			t.Fatalf("Marshal pin: %v", err)
		}
		success := first
		success.OccurrenceID = engine.InteractionPayloadDigest([]byte("successful resolver dispatch"))
		success.OccurrenceSequence, success.Invocation = 3, 3
		success.IdempotencyKey = engine.InteractionPayloadDigest([]byte("successful idempotency"))
		success.ResultDigest = engine.InteractionPayloadDigest(encodedPin)
		state.Dispatches[success.OccurrenceID] = &success
		resolutionID := engine.InteractionPayloadDigest([]byte("successful resolution"))
		state.DynamicIncludes = map[string]*engine.DynamicIncludeResolutionState{resolutionID: {
			SchemaVersion: engine.DynamicIncludeResolutionStateSchemaV1, ResolutionID: resolutionID,
			WriterEpoch: state.WriterEpoch, QualifiedNodeID: "include", StepID: "include",
			Invocation: 3, Revision: 1, Pin: pin, Status: engine.DynamicIncludeResolutionStatusCompleted,
			CommittedAt: "2026-08-31T03:00:00Z", CompletedAt: "2026-08-31T03:00:01Z",
		}}
		state.StepResults["include"] = &engine.StepResult{
			StepID: "include", Status: engine.StepStatusCompleted, Outcome: engine.StepOutcomeSuccess,
			Output: map[string]any{}, Vars: map[string]any{},
		}
		if err := store.SaveState(context.Background(), state); err != nil {
			t.Fatalf("SaveState: %v", err)
		}
	})

	t.Run("nested call path without frame", func(t *testing.T) {
		store, state := makeState(t, "run-dynamic-not-found-root-spoof")
		for _, dispatch := range state.Dispatches {
			dispatch.CallPath = []engine.DebugCallFrame{{StepID: "parent"}}
			dispatch.QualifiedNodeID = engine.DebugNodeID(dispatch.CallPath, dispatch.StepID)
		}
		if err := store.SaveState(context.Background(), state); err == nil || !strings.Contains(err.Error(), "frame") {
			t.Fatalf("SaveState error = %v, want nested dispatch frame rejection", err)
		}
	})
}

func TestDirRunStore_RejectsInteractionOrdinalsNotCoveredByInvocationCounts(t *testing.T) {
	request := json.RawMessage(`{"kind":"choice"}`)
	makeState := func(runID string) engine.RunState {
		invocations := engine.NewInteractionInvocationTracker()
		invocations.Next("choose", "choice")
		invocations.Next("choose", "choice")
		return engine.RunState{
			RunID: runID, Status: engine.RunStatusWaiting, CheckpointSequence: 1,
			InteractionInvocationCounts: invocations.Snapshot(),
			Interactions: map[string]*engine.InteractionState{
				"turn-two": {
					SchemaVersion: engine.InteractionStateSchemaV1,
					TurnID:        "turn-two", OwnerStepID: "choose", NodeID: "choose", StepID: "choose",
					Kind: "choice", Ordinal: 2, Status: engine.InteractionStatusPending,
					RequestDigest: engine.InteractionPayloadDigest(request), Request: request,
				},
			},
		}
	}

	t.Run("save", func(t *testing.T) {
		store := newStore(t)
		state := makeState("run-stale-interaction-count-save")
		state.InteractionInvocationCounts = nil
		if err := store.SaveState(context.Background(), state); err == nil || !strings.Contains(err.Error(), "invocation") {
			t.Fatalf("SaveState error = %v, want invocation-count rejection", err)
		}
	})

	t.Run("load", func(t *testing.T) {
		store := newStore(t)
		state := makeState("run-stale-interaction-count-load")
		if err := store.SaveState(context.Background(), state); err != nil {
			t.Fatalf("SaveState: %v", err)
		}
		checkpointPath := filepath.Join(store.RunDir(state.RunID), "snapshots", "checkpoint-00000000000000000001.json")
		checkpointData, err := os.ReadFile(checkpointPath)
		if err != nil {
			t.Fatalf("ReadFile checkpoint: %v", err)
		}
		var checkpoint map[string]any
		if err := json.Unmarshal(checkpointData, &checkpoint); err != nil {
			t.Fatalf("decode checkpoint: %v", err)
		}
		checkpoint["InteractionInvocationCounts"] = map[string]any{}
		checkpointData, err = json.Marshal(checkpoint)
		if err != nil {
			t.Fatalf("encode checkpoint: %v", err)
		}
		if err := os.WriteFile(checkpointPath, checkpointData, 0o600); err != nil {
			t.Fatalf("write checkpoint: %v", err)
		}
		if _, err := store.LoadState(context.Background(), state.RunID); err == nil || !strings.Contains(err.Error(), "invocation") {
			t.Fatalf("LoadState error = %v, want invocation-count rejection", err)
		}
	})
}

func TestDirRunStore_LargeValuesUseVerifiedDeduplicatedBlobs(t *testing.T) {
	store := newStore(t)
	large := strings.Repeat("large-checkpoint-value-", 50000)
	state := engine.RunState{
		RunID: "run-large-blob", Status: engine.RunStatusRunning, CheckpointSequence: 1,
		Vars: map[string]any{"saved_rows": large},
		StepResults: map[string]*engine.StepResult{
			"query": {StepID: "query", Status: engine.StepStatusCompleted, Output: map[string]any{"saved_rows": large}},
		},
	}
	if err := store.SaveState(context.Background(), state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	checkpointPath := filepath.Join(store.RunDir(state.RunID), "snapshots", "checkpoint-00000000000000000001.json")
	checkpoint, err := os.ReadFile(checkpointPath)
	if err != nil {
		t.Fatalf("ReadFile checkpoint: %v", err)
	}
	if len(checkpoint) > 128*1024 || bytes.Contains(checkpoint, []byte(large[:256])) {
		t.Fatalf("checkpoint inlined large value: %d bytes", len(checkpoint))
	}
	blobDir := filepath.Join(store.RunDir(state.RunID), "blobs")
	blobs, err := os.ReadDir(blobDir)
	if err != nil {
		t.Fatalf("ReadDir blobs: %v", err)
	}
	if len(blobs) != 1 {
		t.Fatalf("blob count = %d, want one deduplicated value", len(blobs))
	}
	loaded, err := store.LoadState(context.Background(), state.RunID)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if loaded.Vars["saved_rows"] != large || loaded.StepResults["query"].Output["saved_rows"] != large {
		t.Fatal("large checkpoint value changed during blob round trip")
	}
	blobPath := filepath.Join(blobDir, blobs[0].Name())
	if err := os.WriteFile(blobPath, []byte("tampered"), 0o600); err != nil {
		t.Fatalf("tamper blob: %v", err)
	}
	if _, err := store.LoadState(context.Background(), state.RunID); err == nil {
		t.Fatal("LoadState accepted a tampered state blob")
	}
}

func TestDirRunStore_RejectsAggregateExpansionFromDuplicateBlobReferences(t *testing.T) {
	store := newStore(t)
	state := engine.RunState{
		RunID: "run-duplicate-blob-expansion", Status: engine.RunStatusRunning, CheckpointSequence: 1,
		Vars: map[string]any{"seed": strings.Repeat("x", 70<<10)},
	}
	if err := store.SaveState(context.Background(), state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	checkpointPath := filepath.Join(store.RunDir(state.RunID), "snapshots", "checkpoint-00000000000000000001.json")
	checkpointData, err := os.ReadFile(checkpointPath)
	if err != nil {
		t.Fatalf("ReadFile checkpoint: %v", err)
	}
	var checkpoint map[string]any
	if err := json.Unmarshal(checkpointData, &checkpoint); err != nil {
		t.Fatalf("decode checkpoint: %v", err)
	}
	durableVars := checkpoint["DurableVars"].(map[string]any)
	seedReference := durableVars["seed"]
	duplicateReferences := make(map[string]any, 4096)
	for index := 0; index < 4096; index++ {
		duplicateReferences[fmt.Sprintf("copy-%04d", index)] = seedReference
	}
	checkpoint["DurableVars"] = duplicateReferences
	checkpointData, err = json.Marshal(checkpoint)
	if err != nil {
		t.Fatalf("encode checkpoint: %v", err)
	}
	if err := os.WriteFile(checkpointPath, checkpointData, 0o600); err != nil {
		t.Fatalf("write checkpoint: %v", err)
	}

	if _, err := store.LoadState(context.Background(), state.RunID); err == nil || !strings.Contains(err.Error(), "expanded data") {
		t.Fatalf("LoadState error = %v, want aggregate expanded-data rejection", err)
	}
}

func TestDirRunStore_RejectsTooManyValuesBeforeWritingBlobs(t *testing.T) {
	store := newStore(t)
	values := make(map[string]any, 4097)
	values["first-large-value"] = strings.Repeat("x", 70<<10)
	for index := 1; index < 4097; index++ {
		values[fmt.Sprintf("value-%04d", index)] = index
	}
	state := engine.RunState{
		RunID: "run-too-many-values", Status: engine.RunStatusRunning,
		CheckpointSequence: 1, Vars: values,
	}

	err := store.SaveState(context.Background(), state)
	if err == nil || !strings.Contains(err.Error(), "value count") {
		t.Fatalf("SaveState error = %v, want checkpoint value-count rejection", err)
	}
	if _, statErr := os.Stat(filepath.Join(store.RunDir(state.RunID), "blobs")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("blob directory exists after rejected checkpoint: %v", statErr)
	}
}

func TestDirRunStore_DeduplicatedBlobsRestoreIndependentMutableValues(t *testing.T) {
	store := newStore(t)
	largeValue := map[string]any{"payload": strings.Repeat("independent-value-", 5000)}
	state := engine.RunState{
		RunID: "run-independent-blob-values", Status: engine.RunStatusRunning, CheckpointSequence: 1,
		Vars: map[string]any{"saved": largeValue},
		StepResults: map[string]*engine.StepResult{
			"query": {StepID: "query", Status: engine.StepStatusCompleted, Output: map[string]any{"saved": largeValue}},
		},
	}
	if err := store.SaveState(context.Background(), state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	loaded, err := store.LoadState(context.Background(), state.RunID)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	loadedVar := loaded.Vars["saved"].(map[string]any)
	loadedOutput := loaded.StepResults["query"].Output["saved"].(map[string]any)
	loadedVar["payload"] = "changed"
	if loadedOutput["payload"] == "changed" {
		t.Fatal("deduplicated blob cache aliased independently restored values")
	}
}

func TestDirRunStore_RejectsMalformedInteractionState(t *testing.T) {
	store := newStore(t)
	request := json.RawMessage(`{"kind":"choice"}`)
	state := engine.RunState{
		RunID: "run-bad-interaction", CheckpointSequence: 1,
		Interactions: map[string]*engine.InteractionState{
			"wrong-map-key": {
				SchemaVersion: engine.InteractionStateSchemaV1, TurnID: "turn-one",
				OwnerStepID: "choose", NodeID: "choose", StepID: "choose", Kind: "choice", Ordinal: 1,
				Status:        engine.InteractionStatusPending,
				RequestDigest: engine.InteractionPayloadDigest(request), Request: request,
			},
		},
	}
	if err := store.SaveState(context.Background(), state); err == nil {
		t.Fatal("SaveState accepted an interaction whose map key differs from turn ID")
	}
}

// TestDirRunStore_LoadState_Latest verifies that LoadState selects the highest-index legacy snapshot.
func TestDirRunStore_LoadState_Latest(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	const runID = "run-latest"

	for i := 0; i < 5; i++ {
		st := engine.RunState{
			RunID:            runID,
			Status:           engine.RunStatusRunning,
			CurrentStepIndex: i,
			CurrentStep:      fmt.Sprintf("step-%d", i),
		}
		if err := s.SaveState(ctx, st); err != nil {
			t.Fatalf("SaveState i=%d: %v", i, err)
		}
	}

	loaded, err := s.LoadState(ctx, runID)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if loaded.CurrentStepIndex != 4 {
		t.Errorf("expected latest index 4, got %d", loaded.CurrentStepIndex)
	}
	if loaded.CurrentStep != "step-4" {
		t.Errorf("expected current step 'step-4', got %q", loaded.CurrentStep)
	}
}

func TestDirRunStore_LoadStateUsesCheckpointSequenceAcrossBackwardJump(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	const runID = "run-backward-checkpoint"

	for _, state := range []engine.RunState{
		{RunID: runID, Status: engine.RunStatusRunning, CheckpointSequence: 1, CurrentStepIndex: 4, CurrentStep: "later"},
		{RunID: runID, Status: engine.RunStatusRunning, CheckpointSequence: 2, CurrentStepIndex: 1, CurrentStep: "handler"},
	} {
		if err := s.SaveState(ctx, state); err != nil {
			t.Fatalf("SaveState sequence %d: %v", state.CheckpointSequence, err)
		}
	}

	loaded, err := s.LoadState(ctx, runID)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if loaded.CheckpointSequence != 2 || loaded.CurrentStepIndex != 1 || loaded.CurrentStep != "handler" {
		t.Fatalf("latest state = %#v, want sequence 2 at handler", loaded)
	}
}

func TestDirRunStore_LoadStateRejectsCheckpointFilenameSequenceMismatch(t *testing.T) {
	store := newStore(t)
	state := engine.RunState{
		RunID: "run-renamed-checkpoint", Status: engine.RunStatusRunning,
		CheckpointSequence: 1, CurrentStep: "first",
	}
	if err := store.SaveState(context.Background(), state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	snapshotDir := filepath.Join(store.RunDir(state.RunID), "snapshots")
	oldPath := filepath.Join(snapshotDir, "checkpoint-00000000000000000001.json")
	newPath := filepath.Join(snapshotDir, "checkpoint-00000000000000000002.json")
	if err := os.Rename(oldPath, newPath); err != nil {
		t.Fatalf("rename checkpoint: %v", err)
	}

	if _, err := store.LoadState(context.Background(), state.RunID); err == nil || !strings.Contains(err.Error(), "sequence") {
		t.Fatalf("LoadState error = %v, want filename/body sequence rejection", err)
	}
}

func TestDirRunStore_SaveStateRejectsCheckpointSequenceReuse(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	first := engine.RunState{RunID: "run-sequence-reuse", CheckpointSequence: 1, CurrentStep: "first"}
	if err := s.SaveState(ctx, first); err != nil {
		t.Fatalf("SaveState first: %v", err)
	}
	conflict := first
	conflict.CurrentStep = "different"
	if err := s.SaveState(ctx, conflict); !errors.Is(err, runstore.ErrCheckpointConflict) {
		t.Fatalf("SaveState reused sequence error = %v, want ErrCheckpointConflict", err)
	}
}

func TestDirRunStore_SaveStateRejectsStaleIdempotentSequence(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	first := engine.RunState{RunID: "run-stale-sequence", CheckpointSequence: 1, CurrentStep: "first"}
	second := engine.RunState{RunID: first.RunID, CheckpointSequence: 2, CurrentStep: "second"}
	if err := s.SaveState(ctx, first); err != nil {
		t.Fatalf("SaveState first: %v", err)
	}
	if err := s.SaveState(ctx, second); err != nil {
		t.Fatalf("SaveState second: %v", err)
	}
	if err := s.SaveState(ctx, first); !errors.Is(err, runstore.ErrCheckpointConflict) {
		t.Fatalf("SaveState stale idempotent error = %v, want ErrCheckpointConflict", err)
	}
}

func TestDirRunStore_SaveStateSequenceRaceIsProcessSafe(t *testing.T) {
	base := t.TempDir()
	stores := []*runstore.DirRunStore{runstore.NewDirRunStore(base), runstore.NewDirRunStore(base)}
	states := []engine.RunState{
		{RunID: "run-sequence-race", CheckpointSequence: 1, CurrentStep: "first"},
		{RunID: "run-sequence-race", CheckpointSequence: 1, CurrentStep: "different"},
	}
	start := make(chan struct{})
	errorsByWriter := make(chan error, len(stores))
	for index := range stores {
		go func(index int) {
			<-start
			errorsByWriter <- stores[index].SaveState(context.Background(), states[index])
		}(index)
	}
	close(start)
	var successes, conflicts int
	for range stores {
		err := <-errorsByWriter
		switch {
		case err == nil:
			successes++
		case errors.Is(err, runstore.ErrCheckpointConflict):
			conflicts++
		default:
			t.Fatalf("SaveState race error = %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("race outcomes: successes=%d conflicts=%d", successes, conflicts)
	}
	loaded, err := runstore.NewDirRunStore(base).LoadState(context.Background(), states[0].RunID)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if loaded.CurrentStep != "first" && loaded.CurrentStep != "different" {
		t.Fatalf("raced checkpoint is corrupt: %#v", loaded)
	}
}

func TestDirRunStore_LoadStateRejectsCursorWithoutStepIndex(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	state := engine.RunState{
		RunID: "run-missing-index", CheckpointSequence: 1,
		CursorSet: &engine.ExecutionCursorSet{
			SchemaVersion: engine.ExecutionCursorSchemaV1,
			Cursors: []engine.ExecutionCursor{{
				QualifiedNodeID: "first", StepID: "first", StepIndex: 0,
				Phase: engine.ExecutionPhaseBefore, Invocation: 1, RetryAttempt: 1,
			}},
		},
	}
	if err := s.SaveState(ctx, state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	path := filepath.Join(s.RunDir(state.RunID), "snapshots", "checkpoint-00000000000000000001.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	data = bytes.Replace(data, []byte(`"step_index":0,`), nil, 1)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := s.LoadState(ctx, state.RunID); err == nil {
		t.Fatal("LoadState accepted a durable cursor without step_index")
	}
}

// TestDirRunStore_LoadState_SkipsTmpFiles verifies that .tmp files are ignored during load.
func TestDirRunStore_LoadState_SkipsTmpFiles(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	const runID = "run-skiptmp"

	// Write a valid snapshot at index 1.
	st := engine.RunState{
		RunID:            runID,
		Status:           engine.RunStatusRunning,
		CurrentStepIndex: 1,
		CurrentStep:      "step-1",
	}
	if err := s.SaveState(ctx, st); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	// Manually plant a .tmp file with a higher index — must be ignored.
	snapshotDir := filepath.Join(s.RunDir(runID), "snapshots")
	tmpFile := filepath.Join(snapshotDir, "step-0099.json.tmp")
	if err := os.WriteFile(tmpFile, []byte(`{"bad":"data"}`), 0o644); err != nil {
		t.Fatalf("write tmp file: %v", err)
	}

	loaded, err := s.LoadState(ctx, runID)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if loaded.CurrentStepIndex != 1 {
		t.Errorf("expected index 1 (tmp ignored), got %d", loaded.CurrentStepIndex)
	}
}

// TestDirRunStore_LoadState_Empty verifies that LoadState returns os.ErrNotExist on an empty dir.
func TestDirRunStore_LoadState_Empty(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	_, err := s.LoadState(ctx, "no-such-run")
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected os.ErrNotExist, got %v", err)
	}
}

// TestDirRunStore_WriteTrace verifies that WriteTrace appends a JSONL event to the trace file.
func TestDirRunStore_WriteTrace(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	const runID = "run-trace"
	ev := engine.Event{
		EventID:  "evt-1",
		RunID:    runID,
		Kind:     "step/started",
		Sequence: 1,
		Payload:  map[string]any{"step_id": "step-1"},
	}
	if err := s.WriteTrace(ctx, runID, ev); err != nil {
		t.Fatalf("WriteTrace: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	tracePath := s.TracePath(runID)
	f, err := os.Open(tracePath)
	if err != nil {
		t.Fatalf("open trace file: %v", err)
	}
	defer f.Close()

	var lines []map[string]any
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var m map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &m); err != nil {
			t.Fatalf("unmarshal line: %v", err)
		}
		lines = append(lines, m)
	}

	if len(lines) != 1 {
		t.Fatalf("expected 1 trace line, got %d", len(lines))
	}
	if got := lines[0]["run_id"]; got != runID {
		t.Errorf("run_id: got %v, want %q", got, runID)
	}
	if got := lines[0]["kind"]; got != "step/started" {
		t.Errorf("kind: got %v, want %q", got, "step/started")
	}
}

// TestDirRunStore_RegisterPlan verifies that RegisterPlan caches a plan by run ID.
func TestDirRunStore_RegisterPlan(t *testing.T) {
	s := newStore(t)

	plan := &engine.ExecutionPlan{
		RunbookPath: "my.runbook.yaml",
		Metadata: engine.PlanMetadata{
			RunbookID: "my-runbook",
		},
	}
	s.RegisterPlan("run-plan", plan)

	got := s.Plan("run-plan")
	if got == nil {
		t.Fatal("Plan: expected non-nil plan after RegisterPlan")
	}
	if got.RunbookPath != plan.RunbookPath {
		t.Errorf("RunbookPath: got %q, want %q", got.RunbookPath, plan.RunbookPath)
	}

	// Unknown ID returns nil.
	if s.Plan("unknown") != nil {
		t.Error("Plan(unknown): expected nil")
	}
}

func TestDirRunStore_SaveLoadPlanAcrossInstances(t *testing.T) {
	base := t.TempDir()
	plan := &engine.ExecutionPlan{
		RunID:       "run-persisted-plan",
		RunbookPath: "persisted.runbook.yaml",
		Steps: []engine.ResolvedStep{{
			ID: "ready", Name: "Ready", Kind: "noop", Spec: &schema.NoopSpec{},
		}},
		Metadata: engine.PlanMetadata{RunbookID: "persisted", RunbookName: "Persisted plan"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("validate plan: %v", err)
	}

	first := runstore.NewDirRunStore(base)
	if err := first.SavePlan(context.Background(), plan.RunID, plan); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}

	second := runstore.NewDirRunStore(base)
	restored, err := second.LoadPlan(context.Background(), plan.RunID)
	if err != nil {
		t.Fatalf("LoadPlan: %v", err)
	}
	if restored.RunID != plan.RunID || restored.RunbookPath != plan.RunbookPath {
		t.Fatalf("restored wrong plan: %#v", restored)
	}
	if len(restored.Steps) != 1 || restored.Steps[0].ID != "ready" || restored.Validation == nil {
		t.Fatalf("restored incomplete plan: %#v", restored)
	}
}

func TestDirRunStoreExportRestoreRunProjection(t *testing.T) {
	base := t.TempDir()
	const runID = "run-projection-backup"
	plan := &engine.ExecutionPlan{
		RunID: runID, RunbookPath: "projection.runbook.yaml",
		Steps:    []engine.ResolvedStep{{ID: "ready", Kind: "noop", Spec: &schema.NoopSpec{}}},
		Metadata: engine.PlanMetadata{RunbookID: "projection", RunbookName: "Projection"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	first := runstore.NewDirRunStore(base)
	lease, err := first.AcquireRunLease(context.Background(), runID)
	if err != nil {
		t.Fatalf("AcquireRunLease: %v", err)
	}
	if err := first.SavePlan(context.Background(), runID, plan); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	state := engine.RunState{
		RunID: runID, RunbookPath: plan.RunbookPath, WriterEpoch: lease.Epoch(),
		CheckpointSequence: 1, Status: engine.RunStatusRunning, CurrentStepIndex: -1,
		CursorSet: &engine.ExecutionCursorSet{SchemaVersion: engine.ExecutionCursorSchemaV1, Cursors: []engine.ExecutionCursor{{
			QualifiedNodeID: "ready", StepID: "ready", StepIndex: 0,
			Phase: engine.ExecutionPhaseBefore, Invocation: 1, RetryAttempt: 1,
		}}},
		Vars:      map[string]any{"nested": map[string]any{"value": "saved"}},
		StartedAt: time.Now().UTC(),
	}
	if err := first.SaveState(context.Background(), state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	archive, err := first.ExportRunProjection(context.Background(), runID)
	if err != nil {
		t.Fatalf("ExportRunProjection: %v", err)
	}
	if err := lease.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close first: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(base, runID)); err != nil {
		t.Fatalf("remove projection: %v", err)
	}
	restoredStore := runstore.NewDirRunStore(base)
	t.Cleanup(func() { _ = restoredStore.Close() })
	if err := restoredStore.RestoreRunProjection(context.Background(), runID, archive); err != nil {
		t.Fatalf("RestoreRunProjection: %v", err)
	}
	restoredPlan, err := restoredStore.LoadPlan(context.Background(), runID)
	if err != nil || restoredPlan.Metadata.PlanHash != plan.Metadata.PlanHash {
		t.Fatalf("restored plan = %#v, %v", restoredPlan, err)
	}
	restoredState, err := restoredStore.LoadState(context.Background(), runID)
	if err != nil || restoredState.CheckpointSequence != 1 ||
		restoredState.Vars["nested"].(map[string]any)["value"] != "saved" {
		t.Fatalf("restored state = %#v, %v", restoredState, err)
	}
}

func TestDirRunStoreExportProjectionIncludesOnlyCurrentCheckpointState(t *testing.T) {
	base := t.TempDir()
	const runID = "run-projection-current-only"
	plan := &engine.ExecutionPlan{
		RunID: runID, RunbookPath: "projection-current.runbook.yaml",
		Steps:    []engine.ResolvedStep{{ID: "ready", Kind: "noop", Spec: &schema.NoopSpec{}}},
		Metadata: engine.PlanMetadata{RunbookID: "projection-current", RunbookName: "Projection Current"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	store := runstore.NewDirRunStore(base)
	t.Cleanup(func() { _ = store.Close() })
	lease, err := store.AcquireRunLease(context.Background(), runID)
	if err != nil {
		t.Fatalf("AcquireRunLease: %v", err)
	}
	defer lease.Release()
	if err := store.SavePlan(context.Background(), runID, plan); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	state := engine.RunState{
		RunID: runID, RunbookPath: plan.RunbookPath, WriterEpoch: lease.Epoch(),
		CheckpointSequence: 1, Status: engine.RunStatusRunning, CurrentStepIndex: -1,
		CursorSet: &engine.ExecutionCursorSet{SchemaVersion: engine.ExecutionCursorSchemaV1, Cursors: []engine.ExecutionCursor{{
			QualifiedNodeID: "ready", StepID: "ready", StepIndex: 0,
			Phase: engine.ExecutionPhaseBefore, Invocation: 1, RetryAttempt: 1,
		}}},
		Vars: map[string]any{"value": strings.Repeat("old", 30_000)}, StartedAt: time.Now().UTC(),
	}
	if err := store.SaveState(context.Background(), state); err != nil {
		t.Fatalf("SaveState first: %v", err)
	}
	state.CheckpointSequence = 2
	state.Vars = map[string]any{"value": strings.Repeat("new", 30_000)}
	if err := store.SaveState(context.Background(), state); err != nil {
		t.Fatalf("SaveState second: %v", err)
	}
	archiveData, err := store.ExportRunProjection(context.Background(), runID)
	if err != nil {
		t.Fatalf("ExportRunProjection: %v", err)
	}
	var archive struct {
		Files []struct {
			Path string `json:"path"`
		} `json:"files"`
	}
	if err := json.Unmarshal(archiveData, &archive); err != nil {
		t.Fatalf("decode archive: %v", err)
	}
	snapshots, blobs := 0, 0
	for _, file := range archive.Files {
		if strings.HasPrefix(file.Path, "snapshots/") {
			snapshots++
			if file.Path != "snapshots/checkpoint-00000000000000000002.json" {
				t.Fatalf("obsolete snapshot exported: %s", file.Path)
			}
		}
		if strings.HasPrefix(file.Path, "blobs/") {
			blobs++
		}
	}
	if snapshots != 1 || blobs != 1 {
		t.Fatalf("exported %d snapshots and %d state blobs, want one each", snapshots, blobs)
	}
}

func TestDirRunStoreRejectsDifferentPlanForExistingRun(t *testing.T) {
	base := t.TempDir()
	makePlan := func(name string) *engine.ExecutionPlan {
		plan := &engine.ExecutionPlan{
			RunID: "run-immutable-plan", RunbookPath: "immutable.runbook.yaml",
			Steps:    []engine.ResolvedStep{{ID: "ready", Name: name, Kind: "noop", Spec: &schema.NoopSpec{}}},
			Metadata: engine.PlanMetadata{RunbookID: "immutable", RunbookName: name},
		}
		if err := planner.ValidateExecutionPlan(plan); err != nil {
			t.Fatalf("validate plan: %v", err)
		}
		return plan
	}
	first := runstore.NewDirRunStore(base)
	if err := first.SavePlan(context.Background(), "run-immutable-plan", makePlan("First")); err != nil {
		t.Fatalf("SavePlan first: %v", err)
	}
	if err := first.SavePlan(context.Background(), "run-immutable-plan", makePlan("First")); err != nil {
		t.Fatalf("SavePlan idempotent: %v", err)
	}
	second := runstore.NewDirRunStore(base)
	if err := second.SavePlan(context.Background(), "run-immutable-plan", makePlan("Different")); !errors.Is(err, runstore.ErrPlanConflict) {
		t.Fatalf("SavePlan different error = %v, want ErrPlanConflict", err)
	}
}

func TestDirRunStore_SavePlanRaceIsProcessSafe(t *testing.T) {
	base := t.TempDir()
	makePlan := func(name string) *engine.ExecutionPlan {
		plan := &engine.ExecutionPlan{
			RunID: "run-plan-race", RunbookPath: "race.runbook.yaml",
			Steps:    []engine.ResolvedStep{{ID: "ready", Name: name, Kind: "noop", Spec: &schema.NoopSpec{}}},
			Metadata: engine.PlanMetadata{RunbookID: "race", RunbookName: name},
		}
		if err := planner.ValidateExecutionPlan(plan); err != nil {
			t.Fatalf("validate plan: %v", err)
		}
		return plan
	}
	stores := []*runstore.DirRunStore{runstore.NewDirRunStore(base), runstore.NewDirRunStore(base)}
	plans := []*engine.ExecutionPlan{makePlan("First"), makePlan("Different")}
	start := make(chan struct{})
	results := make(chan error, 2)
	for index := range stores {
		go func(index int) {
			<-start
			results <- stores[index].SavePlan(context.Background(), plans[index].RunID, plans[index])
		}(index)
	}
	close(start)
	var successes, conflicts int
	for range stores {
		switch err := <-results; {
		case err == nil:
			successes++
		case errors.Is(err, runstore.ErrPlanConflict):
			conflicts++
		default:
			t.Fatalf("SavePlan race error = %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("plan race outcomes: successes=%d conflicts=%d", successes, conflicts)
	}
	if _, err := runstore.NewDirRunStore(base).LoadPlan(context.Background(), "run-plan-race"); err != nil {
		t.Fatalf("LoadPlan winner: %v", err)
	}
}

func TestDirRunStore_RunLeaseIsExclusiveAcrossStoreInstances(t *testing.T) {
	base := t.TempDir()
	firstStore := runstore.NewDirRunStore(base)
	secondStore := runstore.NewDirRunStore(base)
	firstLease, err := firstStore.AcquireRunLease(context.Background(), "run-exclusive-lease")
	if err != nil {
		t.Fatalf("AcquireRunLease first: %v", err)
	}
	if firstLease.Epoch() != 1 {
		t.Fatalf("first lease epoch = %d, want 1", firstLease.Epoch())
	}
	if _, err := secondStore.AcquireRunLease(context.Background(), "run-exclusive-lease"); !errors.Is(err, engine.ErrRunLeaseHeld) {
		t.Fatalf("AcquireRunLease competing error = %v, want ErrRunLeaseHeld", err)
	}
	if err := firstLease.Release(); err != nil {
		t.Fatalf("Release first lease: %v", err)
	}
	secondLease, err := secondStore.AcquireRunLease(context.Background(), "run-exclusive-lease")
	if err != nil {
		t.Fatalf("AcquireRunLease after release: %v", err)
	}
	if secondLease.Epoch() != 2 {
		t.Fatalf("second lease epoch = %d, want 2", secondLease.Epoch())
	}
	if err := secondLease.Release(); err != nil {
		t.Fatalf("Release second lease: %v", err)
	}
}

func TestDirRunStore_RejectsCheckpointFromStaleWriterEpoch(t *testing.T) {
	store := newStore(t)
	const runID = "run-stale-writer"
	firstLease, err := store.AcquireRunLease(context.Background(), runID)
	if err != nil {
		t.Fatalf("AcquireRunLease first: %v", err)
	}
	firstState := engine.RunState{RunID: runID, WriterEpoch: firstLease.Epoch(), CheckpointSequence: 1}
	if err := store.SaveState(context.Background(), firstState); err != nil {
		t.Fatalf("SaveState first writer: %v", err)
	}
	if err := firstLease.Release(); err != nil {
		t.Fatalf("Release first lease: %v", err)
	}
	secondLease, err := store.AcquireRunLease(context.Background(), runID)
	if err != nil {
		t.Fatalf("AcquireRunLease second: %v", err)
	}
	defer secondLease.Release()
	staleState := firstState
	staleState.CheckpointSequence = 2
	if err := store.SaveState(context.Background(), staleState); !errors.Is(err, engine.ErrRunLeaseStale) {
		t.Fatalf("SaveState stale writer error = %v, want ErrRunLeaseStale", err)
	}
	currentState := staleState
	currentState.WriterEpoch = secondLease.Epoch()
	if err := store.SaveState(context.Background(), currentState); err != nil {
		t.Fatalf("SaveState current writer: %v", err)
	}
}

func TestDirRunStore_CloseImmediatelyFencesReleasedWriter(t *testing.T) {
	store := newStore(t)
	const runID = "run-closed-writer"
	lease, err := store.AcquireRunLease(context.Background(), runID)
	if err != nil {
		t.Fatalf("AcquireRunLease: %v", err)
	}
	state := engine.RunState{RunID: runID, WriterEpoch: lease.Epoch(), CheckpointSequence: 1}
	if err := store.SaveState(context.Background(), state); err != nil {
		t.Fatalf("SaveState while leased: %v", err)
	}
	traceCtx := engine.WithRunWriterEpoch(context.Background(), lease.Epoch())
	if err := store.WriteTrace(traceCtx, runID, engine.Event{
		EventID: "before-close", RunID: runID, Kind: "test", Sequence: 1,
	}); err != nil {
		t.Fatalf("WriteTrace while leased: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	state.CheckpointSequence = 2
	if err := store.SaveState(context.Background(), state); !errors.Is(err, engine.ErrRunLeaseStale) {
		t.Fatalf("SaveState after Close error = %v, want ErrRunLeaseStale", err)
	}
	if err := store.WriteTrace(context.Background(), runID, engine.Event{
		EventID: "after-close", RunID: runID, Kind: "test", Sequence: 2,
	}); !errors.Is(err, engine.ErrRunLeaseStale) {
		t.Fatalf("WriteTrace after Close error = %v, want ErrRunLeaseStale", err)
	}
}

func TestDirRunStore_WriteTraceRejectsStaleWriterEpoch(t *testing.T) {
	store := newStore(t)
	t.Cleanup(func() { _ = store.Close() })
	const runID = "run-stale-trace-writer"
	firstLease, err := store.AcquireRunLease(context.Background(), runID)
	if err != nil {
		t.Fatalf("AcquireRunLease first: %v", err)
	}
	firstCtx := engine.WithRunWriterEpoch(context.Background(), firstLease.Epoch())
	if err := store.WriteTrace(firstCtx, runID, engine.Event{
		EventID: "first", RunID: runID, Kind: "test", Sequence: 1,
	}); err != nil {
		t.Fatalf("WriteTrace first: %v", err)
	}
	if err := firstLease.Release(); err != nil {
		t.Fatalf("Release first: %v", err)
	}
	secondLease, err := store.AcquireRunLease(context.Background(), runID)
	if err != nil {
		t.Fatalf("AcquireRunLease second: %v", err)
	}
	defer secondLease.Release()
	if err := store.WriteTrace(firstCtx, runID, engine.Event{
		EventID: "stale", RunID: runID, Kind: "test", Sequence: 2,
	}); !errors.Is(err, engine.ErrRunLeaseStale) {
		t.Fatalf("stale WriteTrace error = %v, want ErrRunLeaseStale", err)
	}
	secondCtx := engine.WithRunWriterEpoch(context.Background(), secondLease.Epoch())
	if err := store.WriteTrace(secondCtx, runID, engine.Event{
		EventID: "second", RunID: runID, Kind: "test", Sequence: 2,
	}); err != nil {
		t.Fatalf("WriteTrace second: %v", err)
	}
}

func TestDirRunStore_CloseWaitsForInFlightCheckpointMutation(t *testing.T) {
	store := newStore(t)
	const runID = "run-close-checkpoint-race"
	lease, err := store.AcquireRunLease(context.Background(), runID)
	if err != nil {
		t.Fatalf("AcquireRunLease: %v", err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce sync.Once
	saveDone := make(chan error, 1)
	go func() {
		saveDone <- store.SaveState(context.Background(), engine.RunState{
			RunID: runID, WriterEpoch: lease.Epoch(), CheckpointSequence: 1,
			Vars: map[string]any{"blocked": blockingJSONValue{entered: entered, release: release, once: &enteredOnce}},
		})
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("checkpoint did not enter mutation hook")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- store.Close() }()
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned during in-flight checkpoint: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if err := <-saveDone; err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestDirRunStoreBindsStateToPlanSnapshot(t *testing.T) {
	base := t.TempDir()
	plan := &engine.ExecutionPlan{
		RunID: "run-bound-state", RunbookPath: "bound.runbook.yaml",
		Steps:    []engine.ResolvedStep{{ID: "ready", Name: "Ready", Kind: "noop", Spec: &schema.NoopSpec{}}},
		Metadata: engine.PlanMetadata{RunbookID: "bound"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("validate plan: %v", err)
	}
	store := runstore.NewDirRunStore(base)
	if err := store.SavePlan(context.Background(), plan.RunID, plan); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	state := engine.RunState{RunID: plan.RunID, Status: engine.RunStatusRunning, CurrentStep: "ready"}
	if err := store.SaveState(context.Background(), state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	loaded, err := runstore.NewDirRunStore(base).LoadState(context.Background(), plan.RunID)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if loaded.PlanSnapshotDigest == "" {
		t.Fatal("loaded state has no plan snapshot digest")
	}
	state.PlanSnapshotDigest = "sha256:different"
	if err := store.SaveState(context.Background(), state); !errors.Is(err, runstore.ErrPlanConflict) {
		t.Fatalf("SaveState mismatched digest error = %v, want ErrPlanConflict", err)
	}
}

func TestDirRunStore_LoadPlanRejectsTrailingJSON(t *testing.T) {
	base := t.TempDir()
	plan := &engine.ExecutionPlan{
		RunID: "run-trailing", RunbookPath: "trailing.runbook.yaml",
		Steps:    []engine.ResolvedStep{{ID: "ready", Name: "Ready", Kind: "noop", Spec: &schema.NoopSpec{}}},
		Metadata: engine.PlanMetadata{RunbookID: "trailing"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("validate plan: %v", err)
	}
	s := runstore.NewDirRunStore(base)
	if err := s.SavePlan(context.Background(), plan.RunID, plan); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	file, err := os.OpenFile(s.PlanPath(plan.RunID), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := file.WriteString(`{}`); err != nil {
		_ = file.Close()
		t.Fatalf("append trailing JSON: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := runstore.NewDirRunStore(base).LoadPlan(context.Background(), plan.RunID); err == nil {
		t.Fatal("LoadPlan accepted trailing JSON")
	}
}

func TestDirRunStoreRejectsTraversalRunID(t *testing.T) {
	s := newStore(t)
	if err := s.SaveState(context.Background(), engine.RunState{RunID: "../escape"}); err == nil {
		t.Fatal("SaveState accepted a path-traversal run ID")
	}
	if _, err := s.LoadPlan(context.Background(), "../escape"); err == nil {
		t.Fatal("LoadPlan accepted a path-traversal run ID")
	}
	if err := s.DeleteRun(context.Background(), "../escape"); err == nil {
		t.Fatal("DeleteRun accepted a path-traversal run ID")
	}
}

func TestDirRunStoreRejectsWindowsAliasRunIDs(t *testing.T) {
	s := newStore(t)
	for _, runID := range []string{"CON", "nul.json", "run.", "Run-Mixed-Case"} {
		if err := s.SaveState(context.Background(), engine.RunState{RunID: runID}); err == nil {
			t.Errorf("SaveState accepted non-canonical run ID %q", runID)
		}
	}
}

func TestDirRunStoreRejectsOversizedPlanWithoutUnboundedRead(t *testing.T) {
	s := newStore(t)
	const runID = "run-oversized"
	if err := os.MkdirAll(s.RunDir(runID), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	file, err := os.OpenFile(s.PlanPath(runID), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if err := file.Truncate((64 << 20) + 1); err != nil {
		_ = file.Close()
		t.Fatalf("Truncate: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := s.LoadPlan(context.Background(), runID); err == nil {
		t.Fatal("LoadPlan accepted an oversized snapshot")
	}
}

// TestDirRunStore_TracePath verifies that TracePath returns the correct path.
func TestDirRunStore_TracePath(t *testing.T) {
	base := t.TempDir()
	s := runstore.NewDirRunStore(base)

	got := s.TracePath("run-123")
	want := filepath.Join(base, "run-123", "trace.jsonl")
	if got != want {
		t.Errorf("TracePath: got %q, want %q", got, want)
	}
}

// TestDirRunStore_Concurrent verifies race-safe access from multiple goroutines.
func TestDirRunStore_Concurrent(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	const goroutines = 10
	const eventsEach = 100
	const runID = "run-concurrent"

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		g := g
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < eventsEach; i++ {
				ev := engine.Event{
					EventID:  fmt.Sprintf("evt-%d-%d", g, i),
					RunID:    runID,
					Kind:     "step/started",
					Sequence: int64(g*eventsEach + i),
					Payload:  map[string]any{"g": g, "i": i},
				}
				if err := s.WriteTrace(ctx, runID, ev); err != nil {
					t.Errorf("goroutine %d WriteTrace %d: %v", g, i, err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Count lines written.
	f, err := os.Open(s.TracePath(runID))
	if err != nil {
		t.Fatalf("open trace: %v", err)
	}
	defer f.Close()

	var count int
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		count++
	}
	want := goroutines * eventsEach
	if count != want {
		t.Errorf("expected %d trace lines, got %d", want, count)
	}
}

// TestWriteFileAtomic_PartialWrite verifies temp file cleanup on write errors.
// We test this indirectly via SaveState by checking no .tmp files remain.
func TestWriteFileAtomic_PartialWrite(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	const runID = "run-atomic"
	state := engine.RunState{
		RunID:            runID,
		Status:           engine.RunStatusRunning,
		CurrentStepIndex: 0,
	}

	if err := s.SaveState(ctx, state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	// Verify no .tmp files remain in snapshots directory.
	snapshotDir := filepath.Join(s.RunDir(runID), "snapshots")
	entries, err := os.ReadDir(snapshotDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("unexpected .tmp file left behind: %s", e.Name())
		}
	}

	// Verify the final file exists and is valid JSON.
	finalPath := filepath.Join(snapshotDir, "step-0000.json")
	data, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var loaded engine.RunState
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if loaded.RunID != runID {
		t.Errorf("RunID: got %q, want %q", loaded.RunID, runID)
	}
}

// TestDirRunStore_ListRuns verifies listing all runs from the store directory.
func TestDirRunStore_ListRuns(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	// Seed two runs.
	runs := []engine.RunState{
		{RunID: "run-list-a", Status: engine.RunStatusCompleted, StartedAt: time.Now().Truncate(time.Second)},
		{RunID: "run-list-b", Status: engine.RunStatusFailed, StartedAt: time.Now().Truncate(time.Second)},
	}
	for _, r := range runs {
		if err := s.SaveState(ctx, r); err != nil {
			t.Fatalf("SaveState %s: %v", r.RunID, err)
		}
	}

	listed, err := s.ListRuns(ctx)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("expected 2 runs, got %d", len(listed))
	}
	ids := make(map[string]bool)
	for _, r := range listed {
		ids[r.RunID] = true
	}
	for _, want := range []string{"run-list-a", "run-list-b"} {
		if !ids[want] {
			t.Errorf("expected run %s in listing", want)
		}
	}
}

// TestDirRunStore_ListRuns_Empty verifies ListRuns returns nil for empty store.
func TestDirRunStore_ListRuns_Empty(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	listed, err := s.ListRuns(ctx)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(listed) != 0 {
		t.Errorf("expected empty listing, got %d runs", len(listed))
	}
}

// TestDirRunStore_ListRuns_MissingBaseDir verifies ListRuns returns nil for a non-existent base dir.
func TestDirRunStore_ListRuns_MissingBaseDir(t *testing.T) {
	s := runstore.NewDirRunStore(filepath.Join(t.TempDir(), "no-such-dir"))
	ctx := context.Background()

	listed, err := s.ListRuns(ctx)
	if err != nil {
		t.Fatalf("expected nil error for missing dir, got: %v", err)
	}
	if len(listed) != 0 {
		t.Errorf("expected empty listing, got %d runs", len(listed))
	}
}

// TestDirRunStore_DeleteRun verifies that a run directory is removed.
func TestDirRunStore_DeleteRun(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	const runID = "run-delete"
	if err := s.SaveState(ctx, engine.RunState{
		RunID:  runID,
		Status: engine.RunStatusCompleted,
	}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	if err := s.DeleteRun(ctx, runID); err != nil {
		t.Fatalf("DeleteRun: %v", err)
	}

	// LoadState should fail after deletion.
	if _, err := s.LoadState(ctx, runID); err == nil {
		t.Error("expected error loading deleted run, got nil")
	}
}

func TestDirRunStore_DeleteRunRejectsActiveLeaseAndPreservesWriterEpoch(t *testing.T) {
	base := t.TempDir()
	firstStore := runstore.NewDirRunStore(base)
	secondStore := runstore.NewDirRunStore(base)
	t.Cleanup(func() {
		_ = firstStore.Close()
		_ = secondStore.Close()
	})
	const runID = "run-delete-leased"
	firstLease, err := firstStore.AcquireRunLease(context.Background(), runID)
	if err != nil {
		t.Fatalf("AcquireRunLease first: %v", err)
	}
	marker := filepath.Join(firstStore.RunDir(runID), "marker")
	if err := os.WriteFile(marker, []byte("active"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	if err := secondStore.DeleteRun(context.Background(), runID); !errors.Is(err, engine.ErrRunLeaseHeld) {
		t.Fatalf("DeleteRun active error = %v, want ErrRunLeaseHeld", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("active run was modified by deletion: %v", err)
	}
	firstEpoch := firstLease.Epoch()
	if err := firstLease.Release(); err != nil {
		t.Fatalf("release first lease: %v", err)
	}
	if err := secondStore.DeleteRun(context.Background(), runID); err != nil {
		t.Fatalf("DeleteRun after release: %v", err)
	}
	secondLease, err := secondStore.AcquireRunLease(context.Background(), runID)
	if err != nil {
		t.Fatalf("AcquireRunLease after deletion: %v", err)
	}
	defer secondLease.Release()
	if secondLease.Epoch() <= firstEpoch {
		t.Fatalf("writer epoch reset across deletion: first=%d second=%d", firstEpoch, secondLease.Epoch())
	}
}

// TestDirRunStore_DeleteRun_NotFound verifies graceful handling of missing run.
func TestDirRunStore_DeleteRun_NotFound(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	if err := s.DeleteRun(ctx, "no-such-run"); err != nil {
		t.Fatalf("expected nil error for missing run, got: %v", err)
	}
}
