package engine

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/internal/runstore"
	internaltrace "github.com/ormasoftchile/yawr/runtime/internal/trace"
	enginepkg "github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	tracepkg "github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

type protectedResultExecutor struct {
	secret string
}

func (executor protectedResultExecutor) Execute(
	_ context.Context,
	step enginepkg.ResolvedStep,
	_ map[string]any,
) (*enginepkg.StepResult, error) {
	return &enginepkg.StepResult{
		StepID: step.ID, Status: enginepkg.StepStatusCompleted, Outcome: enginepkg.StepOutcomeSuccess,
		Output: map[string]any{"token": executor.secret},
	}, nil
}

func (executor protectedResultExecutor) ResolveDebugProtection(
	context.Context,
	enginepkg.ResolvedStep,
	map[string]any,
) (enginepkg.DebugProtection, bool) {
	return enginepkg.DebugProtection{SecretValues: []string{executor.secret}}, true
}

func TestResumeProjectsAlreadyRedactedProtectedResult(t *testing.T) {
	base := t.TempDir()
	const runID = "run-protected-trace-recovery"
	const secret = "trace-recovery-secret"
	plan := &enginepkg.ExecutionPlan{
		RunID: runID, RunbookPath: "protected.runbook.yaml",
		Steps:    []enginepkg.ResolvedStep{{ID: "protected", Kind: "noop", Spec: &schema.NoopSpec{}}},
		Metadata: enginepkg.PlanMetadata{RunbookID: "protected-trace"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	firstStore := runstore.NewDirRunStore(base)
	firstConfig := makeTestConfig()
	firstRegistry := newFakeExecutorRegistry()
	firstRegistry.Register("noop", protectedResultExecutor{secret: secret})
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
	var pendingID string
	for _, event := range checkpoint.PendingTraceEvents {
		if event.Kind == string(tracepkg.EventKindStepCompleted) {
			pendingID = event.EventID
			if output, _ := event.Payload["output"].(map[string]any); output["token"] != "<redacted>" {
				t.Fatalf("checkpoint persisted unredacted output: %#v", output)
			}
		}
	}
	if pendingID == "" {
		t.Fatal("checkpoint has no pending step/completed event")
	}
	if err := firstStore.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}

	externalPath := filepath.Join(base, "protected-trace.jsonl")
	externalWriter, err := internaltrace.NewJSONLWriter(externalPath)
	if err != nil {
		t.Fatalf("NewJSONLWriter: %v", err)
	}
	resumeStore := runstore.NewDirRunStore(base)
	resumeConfig := makeTestConfig()
	resumeRegistry := newFakeExecutorRegistry()
	resumeRegistry.Register("noop", protectedResultExecutor{secret: secret})
	resumeConfig.Executors = resumeRegistry
	resumeConfig.TraceWriter = externalWriter
	resumeConfig.Store = resumeStore
	if _, err := New(resumeConfig).Resume(context.Background(), runID, enginepkg.RunOptions{
		Store: resumeStore, AcknowledgeIndeterminate: true,
	}); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if err := resumeStore.Close(); err != nil {
		t.Fatalf("close resume store: %v", err)
	}
	if err := externalWriter.Close(); err != nil {
		t.Fatalf("close external writer: %v", err)
	}
	events, err := internaltrace.NewJSONLReader(externalPath).ReadAll(context.Background())
	if err != nil {
		t.Fatalf("read recovered trace: %v", err)
	}
	for _, event := range events {
		if event.EventID != pendingID {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("decode recovered event: %v", err)
		}
		output, _ := payload["output"].(map[string]any)
		if output["token"] != "<redacted>" {
			t.Fatalf("recovered protected output = %#v", output["token"])
		}
		return
	}
	t.Fatalf("recovered trace omitted pending event %s", pendingID)
}

func TestForwardedSubEngineEventReachesConfiguredTraceWriter(t *testing.T) {
	writer := &fakeTraceWriter{}
	config := makeTestConfig()
	config.TraceWriter = writer
	plan := enginepkg.ValidatedForTest(&enginepkg.ExecutionPlan{
		RunID: "run-forwarded-trace", RunbookPath: "forwarded.runbook.yaml",
		Steps:    []enginepkg.ResolvedStep{{ID: "work", Kind: "noop", Spec: &schema.NoopSpec{}}},
		Metadata: enginepkg.PlanMetadata{RunbookID: "forwarded-trace"},
	})
	handle, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	internalHandle := handle.(*runHandle)
	priorSequence := internalHandle.run.Sequence
	internalHandle.forwardSubEngineEvent(enginepkg.Event{
		EventID: "nested-event", RunID: "child", RunbookID: "substeps",
		Kind: string(tracepkg.EventKindStepCompleted), Sequence: 1,
		Payload: map[string]any{"step_id": "nested"},
	})
	for _, event := range writer.collect() {
		if event.EventID == "nested-event" {
			if event.RunID != plan.RunID || event.Sequence != priorSequence+1 {
				t.Fatalf("forwarded identity = run %q sequence %d", event.RunID, event.Sequence)
			}
			return
		}
	}
	t.Fatal("configured trace writer omitted the parent-resequenced sub-engine event")
}
