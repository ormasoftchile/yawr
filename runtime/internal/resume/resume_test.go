package resume

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ormasoftchile/yawr/runtime/internal/runstore"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	tracepkg "github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

func TestResume_SkipsCompletedSteps(t *testing.T) {
	store := runstore.NewDirRunStore(makeResumeDir(t))
	plan := makePlan()
	store.RegisterPlan("run-1", plan)
	state := engine.RunState{
		RunID:            "run-1",
		RunbookPath:      "runbook.yaml",
		Status:           engine.RunStatusRunning,
		CurrentStep:      "step-1",
		CurrentStepIndex: 0,
		Vars:             map[string]any{"foo": "bar"},
		StartedAt:        time.Now(),
		UpdatedAt:        time.Now(),
	}
	if err := store.SaveState(context.Background(), state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	writeTrace(store, "run-1", []engine.Event{
		makeEvent(1, tracepkg.EventKindStepCompleted, map[string]any{"step_id": "step-1"}),
		makeEvent(2, tracepkg.EventKind("checkpoint"), map[string]any{"snapshot_file": "step-0000.json"}),
	})

	run, _, err := ResumeFromTrace(context.Background(), store, "run-1", plan, engine.RunOptions{Mode: engine.RunModeReal})
	if err != nil {
		t.Fatalf("ResumeFromTrace: %v", err)
	}
	if _, ok := run.StepResults["step-1"]; !ok {
		t.Fatalf("expected step-1 marked completed")
	}
}

func TestResume_ContinuesFromCheckpoint(t *testing.T) {
	store := runstore.NewDirRunStore(makeResumeDir(t))
	plan := makePlan()
	store.RegisterPlan("run-1", plan)
	state := engine.RunState{
		RunID:            "run-1",
		RunbookPath:      "runbook.yaml",
		Status:           engine.RunStatusRunning,
		CurrentStep:      "step-2",
		CurrentStepIndex: 1,
		Vars:             map[string]any{},
		StartedAt:        time.Now(),
		UpdatedAt:        time.Now(),
	}
	if err := store.SaveState(context.Background(), state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	writeTrace(store, "run-1", []engine.Event{
		makeEvent(1, tracepkg.EventKindStepCompleted, map[string]any{"step_id": "step-1"}),
		makeEvent(2, tracepkg.EventKindStepCompleted, map[string]any{"step_id": "step-2"}),
		makeEvent(3, tracepkg.EventKind("checkpoint"), map[string]any{"snapshot_file": "step-0001.json"}),
	})

	run, _, err := ResumeFromTrace(context.Background(), store, "run-1", plan, engine.RunOptions{Mode: engine.RunModeReal})
	if err != nil {
		t.Fatalf("ResumeFromTrace: %v", err)
	}
	if run.CurrentStepIndex != 1 {
		t.Fatalf("expected current step index 1, got %d", run.CurrentStepIndex)
	}
}

func TestRebuildRunUsesCursorBeforeLegacyCurrentStep(t *testing.T) {
	plan := makePlan()
	state := engine.RunState{
		RunID:            "run-1",
		CurrentStep:      "step-3",
		CurrentStepIndex: 2,
		CursorSet: &engine.ExecutionCursorSet{
			SchemaVersion: engine.ExecutionCursorSchemaV1,
			Cursors: []engine.ExecutionCursor{{
				QualifiedNodeID: "step-2",
				StepID:          "step-2",
				StepIndex:       1,
				Phase:           engine.ExecutionPhaseBefore,
				Invocation:      1,
				RetryAttempt:    1,
			}},
		},
	}
	run, err := RebuildRun("run-1", plan, state, nil, engine.RunOptions{Mode: engine.RunModeReal})
	if err != nil {
		t.Fatalf("RebuildRun: %v", err)
	}
	if run.CurrentStepIndex != 0 {
		t.Fatalf("current step index = %d, want 0 so Next executes cursor step 1", run.CurrentStepIndex)
	}
}

func TestRebuildRunRestoresPersistedStepResults(t *testing.T) {
	plan := makePlan()
	state := engine.RunState{
		CurrentStep: "step-1",
		StepResults: map[string]*engine.StepResult{
			"step-1": {
				StepID: "step-1", Status: engine.StepStatusCompleted,
				Output: map[string]any{"answer": "saved"}, Vars: map[string]any{"captured": true},
			},
		},
	}
	run, err := RebuildRun("run-1", plan, state, nil, engine.RunOptions{})
	if err != nil {
		t.Fatalf("RebuildRun: %v", err)
	}
	result := run.StepResults["step-1"]
	if result == nil || result.Output["answer"] != "saved" || result.Vars["captured"] != true {
		t.Fatalf("restored result = %#v", result)
	}
}

func TestRebuildRunRejectsSequencedStateWithoutCursor(t *testing.T) {
	state := engine.RunState{CheckpointSequence: 1, CurrentStep: "step-1"}
	if _, err := RebuildRun("run-1", makePlan(), state, nil, engine.RunOptions{}); !errors.Is(err, ErrCursorUnsupported) {
		t.Fatalf("RebuildRun error = %v, want ErrCursorUnsupported", err)
	}
}

func TestRebuildRunDoesNotMergeTraceResultsIntoCursorCheckpoint(t *testing.T) {
	state := engine.RunState{
		CheckpointSequence: 1,
		CursorSet: &engine.ExecutionCursorSet{
			SchemaVersion: engine.ExecutionCursorSchemaV1,
			Cursors: []engine.ExecutionCursor{{
				QualifiedNodeID: "step-2", StepID: "step-2", StepIndex: 1,
				Phase: engine.ExecutionPhaseBefore, Invocation: 1, RetryAttempt: 1,
			}},
		},
		StepResults: map[string]*engine.StepResult{
			"step-1": {StepID: "step-1", Status: engine.StepStatusCompleted},
		},
	}
	run, err := RebuildRun("run-1", makePlan(), state, &ResumeContext{
		CompletedStepIDs: map[string]bool{"step-1": true, "step-2": true},
	}, engine.RunOptions{})
	if err != nil {
		t.Fatalf("RebuildRun: %v", err)
	}
	if _, exists := run.StepResults["step-2"]; exists {
		t.Fatal("trace-ahead step-2 was merged into cursor-authoritative results")
	}
}

func TestRebuildRunRejectsUnsupportedCursorSet(t *testing.T) {
	tests := []struct {
		name    string
		plan    *engine.ExecutionPlan
		cursors *engine.ExecutionCursorSet
	}{
		{
			name: "multiple cursors",
			plan: makePlan(),
			cursors: &engine.ExecutionCursorSet{SchemaVersion: engine.ExecutionCursorSchemaV1, Cursors: []engine.ExecutionCursor{
				{StepID: "step-1", StepIndex: 0, Phase: engine.ExecutionPhaseBefore},
				{StepID: "step-2", StepIndex: 1, Phase: engine.ExecutionPhaseBefore},
			}},
		},
		{
			name: "unknown schema",
			plan: makePlan(),
			cursors: &engine.ExecutionCursorSet{SchemaVersion: "execution-cursor/v2", Cursors: []engine.ExecutionCursor{{
				StepID: "step-1", StepIndex: 0, Phase: engine.ExecutionPhaseBefore,
			}}},
		},
		{
			name: "nonterminal end index",
			plan: makePlan(),
			cursors: &engine.ExecutionCursorSet{SchemaVersion: engine.ExecutionCursorSchemaV1, Cursors: []engine.ExecutionCursor{{
				StepIndex: 3, Phase: engine.ExecutionPhaseBefore, Invocation: 1, RetryAttempt: 1,
			}}},
		},
		{
			name: "non-empty call path",
			plan: makePlan(),
			cursors: &engine.ExecutionCursorSet{SchemaVersion: engine.ExecutionCursorSchemaV1, Cursors: []engine.ExecutionCursor{{
				QualifiedNodeID: "parent/step-1", CallPath: []engine.DebugCallFrame{{StepID: "parent"}},
				StepID: "step-1", StepIndex: 0, Phase: engine.ExecutionPhaseBefore, Invocation: 1, RetryAttempt: 1,
			}}},
		},
		{
			name: "nested plan node",
			plan: &engine.ExecutionPlan{Steps: []engine.ResolvedStep{{ID: "child", Kind: "cli", Depth: 1}}},
			cursors: &engine.ExecutionCursorSet{SchemaVersion: engine.ExecutionCursorSchemaV1, Cursors: []engine.ExecutionCursor{{
				QualifiedNodeID: "child", StepID: "child", StepIndex: 0,
				Phase: engine.ExecutionPhaseBefore, Invocation: 1, RetryAttempt: 1,
			}}},
		},
		{
			name: "malformed end cursor",
			plan: makePlan(),
			cursors: &engine.ExecutionCursorSet{SchemaVersion: engine.ExecutionCursorSchemaV1, Cursors: []engine.ExecutionCursor{{
				StepID: "step-3", StepIndex: 3, Phase: engine.ExecutionPhaseBefore,
				Invocation: 1, RetryAttempt: 1, AtEnd: true,
			}}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := engine.RunState{CursorSet: test.cursors}
			if _, err := RebuildRun("run-1", test.plan, state, nil, engine.RunOptions{}); !errors.Is(err, ErrCursorUnsupported) {
				t.Fatalf("RebuildRun error = %v, want ErrCursorUnsupported", err)
			}
		})
	}
}

func TestRebuildRunAcceptsFrameBackedParallelLeafCursors(t *testing.T) {
	plan := makePlan()
	frameA := "sha256:" + strings.Repeat("a", 64)
	frameB := "sha256:" + strings.Repeat("b", 64)
	callPath := []engine.DebugCallFrame{{StepID: "step-1"}}
	state := engine.RunState{
		CursorSet: &engine.ExecutionCursorSet{
			SchemaVersion: engine.ExecutionCursorSchemaV1,
			Cursors: []engine.ExecutionCursor{
				{QualifiedNodeID: "step-1/a", CallPath: callPath, StepID: "a", StepIndex: 0,
					FrameID: frameA, BranchLabel: "left", Phase: engine.ExecutionPhaseBefore, Invocation: 1, RetryAttempt: 1},
				{QualifiedNodeID: "step-1/b", CallPath: callPath, StepID: "b", StepIndex: 0,
					FrameID: frameB, BranchLabel: "right", Phase: engine.ExecutionPhaseBefore, Invocation: 1, RetryAttempt: 1},
			},
		},
		ExecutionFrames: map[string]*engine.ExecutionFrameState{
			frameA: {FrameID: frameA, Status: engine.ExecutionFrameStatusActive, CallPath: callPath,
				BranchLabel: "left", Invocation: 1, StepCount: 1, StepIDs: []string{"a"}},
			frameB: {FrameID: frameB, Status: engine.ExecutionFrameStatusActive, CallPath: callPath,
				BranchLabel: "right", Invocation: 1, StepCount: 1, StepIDs: []string{"b"}},
		},
	}
	run, err := RebuildRun("run-1", plan, state, nil, engine.RunOptions{})
	if err != nil {
		t.Fatalf("RebuildRun: %v", err)
	}
	if run.CurrentStepIndex != -1 || len(run.ExecutionFrames) != 2 {
		t.Fatalf("rebuilt run index=%d frames=%#v", run.CurrentStepIndex, run.ExecutionFrames)
	}
}

func TestResume_AlreadyComplete(t *testing.T) {
	store := runstore.NewDirRunStore(makeResumeDir(t))
	plan := makePlan()
	store.RegisterPlan("run-1", plan)
	state := engine.RunState{
		RunID:            "run-1",
		RunbookPath:      "runbook.yaml",
		Status:           engine.RunStatusCompleted,
		CurrentStep:      "step-3",
		CurrentStepIndex: 2,
		Vars:             map[string]any{},
		StartedAt:        time.Now(),
		UpdatedAt:        time.Now(),
	}
	if err := store.SaveState(context.Background(), state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	if _, _, err := ResumeFromTrace(context.Background(), store, "run-1", plan, engine.RunOptions{Mode: engine.RunModeReal}); err == nil {
		t.Fatalf("expected error for completed run")
	}
}

func makePlan() *engine.ExecutionPlan {
	return &engine.ExecutionPlan{
		RunID:       "run-1",
		RunbookPath: "runbook.yaml",
		Steps: []engine.ResolvedStep{
			{ID: "step-1", Kind: "cli"},
			{ID: "step-2", Kind: "cli"},
			{ID: "step-3", Kind: "cli"},
		},
		Metadata: engine.PlanMetadata{
			RunbookID:   "rb-1",
			RunbookName: "Runbook",
			PlannedAt:   time.Now(),
		},
	}
}

func writeTrace(store *runstore.DirRunStore, runID string, events []engine.Event) {
	for _, ev := range events {
		_ = store.WriteTrace(context.Background(), runID, ev)
	}
}

func makeEvent(seq int64, kind tracepkg.EventKind, payload map[string]any) engine.Event {
	return engine.Event{
		EventID:   "evt",
		RunID:     "run-1",
		RunbookID: "rb-1",
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Kind:      string(kind),
		Sequence:  seq,
		Payload:   payload,
	}
}

func makeResumeDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(".", "resume-test-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
