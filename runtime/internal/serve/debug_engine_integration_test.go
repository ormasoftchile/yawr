package serve

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	internalengine "github.com/ormasoftchile/yawr/runtime/internal/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/eventbus"
	gcpparser "github.com/ormasoftchile/yawr/runtime/pkg/gcp/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

type debugIntegrationExecutorFunc func(context.Context, engine.ResolvedStep, map[string]any) (*engine.StepResult, error)

func (f debugIntegrationExecutorFunc) Execute(ctx context.Context, step engine.ResolvedStep, vars map[string]any) (*engine.StepResult, error) {
	return f(ctx, step, vars)
}

type debugIntegrationRegistry map[string]engine.StepExecutor

func (r debugIntegrationRegistry) Register(kind string, executor engine.StepExecutor) {
	r[kind] = executor
}
func (r debugIntegrationRegistry) Lookup(kind string) engine.StepExecutor { return r[kind] }

type debugIntegrationTrace struct{}

func (debugIntegrationTrace) Append(trace.TraceEvent) error { return nil }
func (debugIntegrationTrace) Close() error                  { return nil }

type debugIntegrationDispatcher struct{}

func (debugIntegrationDispatcher) Dispatch(eventbus.InboundEvent) error { return nil }
func (debugIntegrationDispatcher) Wait(context.Context, string, eventbus.EventFilter, time.Duration) (*eventbus.InboundEvent, error) {
	return nil, errors.New("not used")
}
func (debugIntegrationDispatcher) Cancel(string, string) {}

type debugIntegrationStepSpec struct{ kind string }

func (s debugIntegrationStepSpec) StepKind() string { return s.kind }

func TestDebugE2E_MitigatedActualBecomesActiveEffectiveForNextStep(t *testing.T) {
	const runID = "debug-e2e"
	broker := NewPromptBroker(16)
	queue := broker.Register(runID)
	if err := broker.ConfigureDebug(runID, DebugRunConfig{
		Enabled:     true,
		Breakpoints: []DebugBreakpoint{{Step: "get_incident", Phase: engine.DebugPhaseAfter}},
	}); err != nil {
		t.Fatalf("ConfigureDebug: %v", err)
	}

	registry := debugIntegrationRegistry{}
	registry.Register("icm", debugIntegrationExecutorFunc(func(_ context.Context, step engine.ResolvedStep, _ map[string]any) (*engine.StepResult, error) {
		return &engine.StepResult{
			StepID: step.ID, Status: engine.StepStatusCompleted, Outcome: engine.StepOutcomeSuccess,
			Output: map[string]any{"incident": map[string]any{"status": "Mitigated"}},
			Vars:   map[string]any{},
		}, nil
	}))
	registry.Register("observe", debugIntegrationExecutorFunc(func(_ context.Context, step engine.ResolvedStep, vars map[string]any) (*engine.StepResult, error) {
		return &engine.StepResult{
			StepID: step.ID, Status: engine.StepStatusCompleted, Outcome: engine.StepOutcomeSuccess,
			Output: map[string]any{"observed_status": vars["incident_status"]}, Vars: map[string]any{},
		}, nil
	}))

	capturePath, err := gcpparser.Parse("outputs.incident.status")
	if err != nil {
		t.Fatalf("parse capture: %v", err)
	}
	getIncident := engine.ResolvedStep{
		ID: "get_incident", Kind: "icm", Spec: debugIntegrationStepSpec{kind: "icm"},
		Capture: map[string]string{"incident_status": "outputs.incident.status"},
	}
	observe := engine.ResolvedStep{ID: "route_active", Kind: "observe", Spec: debugIntegrationStepSpec{kind: "observe"}}
	plan := engine.ValidatedForTest(&engine.ExecutionPlan{
		RunID: runID, RunbookPath: "triage.runbook.yaml", Steps: []engine.ResolvedStep{getIncident, observe},
		Metadata: engine.PlanMetadata{RunbookID: "triage", RunbookName: "Triage"},
	})
	plan.Validation.GCPPaths[engine.StepRef{StepID: "get_incident", FieldPath: "capture.incident_status"}] = capturePath

	eng := internalengine.New(engine.EngineConfig{
		Executors: registry, Dispatcher: debugIntegrationDispatcher{}, TraceWriter: debugIntegrationTrace{},
		Platform: platform.NewFakePlatform(),
	})
	handle, err := eng.Start(context.Background(), plan, engine.RunOptions{Debugger: broker})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	firstResult := make(chan *engine.StepResult, 1)
	firstErr := make(chan error, 1)
	go func() {
		result, err := handle.Next(context.Background())
		if err != nil {
			firstErr <- err
			return
		}
		firstResult <- result
	}()
	turnID := waitForPendingDebugTurn(t, queue, "get_incident")
	if err := broker.Answer(runID, turnID, AnswerEnvelope{
		Kind: "debug_break", Action: engine.DebugActionContinue,
		Set: &DebugSet{
			Status:      engine.StepStatusCompleted,
			OutputPatch: map[string]any{"incident": map[string]any{"status": "Active"}},
		},
	}); err != nil {
		t.Fatalf("Answer: %v", err)
	}

	select {
	case err := <-firstErr:
		t.Fatalf("first Next: %v", err)
	case result := <-firstResult:
		if result.Vars["incident_status"] != "Active" {
			t.Fatalf("effective capture = %#v, want Active", result.Vars)
		}
	case <-time.After(time.Second):
		t.Fatal("first step did not resume")
	}

	second, err := handle.Next(context.Background())
	if err != nil {
		t.Fatalf("second Next: %v", err)
	}
	if second.Output["observed_status"] != "Active" {
		t.Fatalf("next step observed %v, want Active", second.Output["observed_status"])
	}
	if _, err := handle.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("final Next = %v, want EOF", err)
	}
}
