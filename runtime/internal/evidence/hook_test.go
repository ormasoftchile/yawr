package evidence

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	internalengine "github.com/ormasoftchile/yawr/runtime/internal/engine"
	internaleventbus "github.com/ormasoftchile/yawr/runtime/internal/eventbus"
	internalexecutor "github.com/ormasoftchile/yawr/runtime/internal/executor"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	evidencepkg "github.com/ormasoftchile/yawr/runtime/pkg/evidence"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	tracepkg "github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

type captureTraceWriter struct {
	events []tracepkg.TraceEvent
}

func (w *captureTraceWriter) Append(event tracepkg.TraceEvent) error {
	w.events = append(w.events, event)
	return nil
}

func (w *captureTraceWriter) Close() error {
	return nil
}

type stubExecutor struct{}

func (s *stubExecutor) Execute(_ context.Context, step engine.ResolvedStep, _ map[string]any) (*engine.StepResult, error) {
	return &engine.StepResult{
		StepID:  step.ID,
		Status:  engine.StepStatusCompleted,
		Outcome: engine.StepOutcomeSuccess,
		Output:  map[string]any{"stdout": "ok"},
	}, nil
}

func TestEvidenceHook_IntegrationWithEngine(t *testing.T) {
	collector := NewDefaultCollector()
	writer := &captureTraceWriter{}
	registry := internalexecutor.NewMapRegistry()
	registry.Register("cli", &stubExecutor{})

	cfg := engine.EngineConfig{
		Executors:    registry,
		Dispatcher:   internaleventbus.NewDispatcher(),
		TraceWriter:  writer,
		Platform:     platform.NewFakePlatform(),
		EvidenceHook: collector,
	}
	eng := internalengine.New(cfg)
	plan := &engine.ExecutionPlan{
		RunID:       "run-1",
		RunbookPath: "runbook.yaml",
		Steps: []engine.ResolvedStep{{
			ID:   "step-1",
			Kind: "cli",
		}},
		Metadata: engine.PlanMetadata{
			RunbookID:   "rb-1",
			RunbookName: "Runbook",
			PlannedAt:   time.Now(),
		},
	}

	handle, err := eng.Start(context.Background(), engine.ValidatedForTest(plan), engine.RunOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	result, err := handle.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if len(result.Evidence) != 1 || result.Evidence[0].Kind != evidencepkg.EvidenceKindText {
		t.Fatalf("expected evidence on step result, got %#v", result.Evidence)
	}

	found := false
	for _, ev := range writer.events {
		if ev.Kind != tracepkg.EventKindStepCompleted {
			continue
		}
		var payload map[string]any
		if err := jsonUnmarshal(ev.Payload, &payload); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if evidence, ok := payload["evidence"].([]any); ok && len(evidence) > 0 {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected evidence in trace payload")
	}
}

func jsonUnmarshal(data []byte, dest any) error {
	return json.Unmarshal(data, dest)
}
