package serve

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func debugSnapshot(callSite string) engine.DebugSnapshot {
	var callPath []engine.DebugCallFrame
	if callSite != "" {
		callPath = []engine.DebugCallFrame{{StepID: callSite}}
	}
	return engine.DebugSnapshot{
		Phase: engine.DebugPhaseAfter,
		Location: engine.DebugLocation{
			RunID:       "run-debug",
			RunbookPath: "triage.runbook.yaml",
			CallPath:    callPath,
			StepID:      "get_incident",
			Invocation:  1,
			Attempt:     1,
		},
		Vars: map[string]any{"incident_id": "123", "incident_status": "Mitigated"},
		Actual: &engine.StepResult{
			StepID: "get_incident",
			Status: engine.StepStatusCompleted,
			Output: map[string]any{
				"incident": map[string]any{"status": "Mitigated"},
			},
		},
	}
}

func TestDebugInteraction_UnconfiguredRunContinuesWithoutPendingTurn(t *testing.T) {
	broker := NewPromptBroker(8)
	queue := broker.Register("run-debug")
	ctx := engine.WithRunID(context.Background(), "run-debug")

	decision, err := broker.Pause(ctx, debugSnapshot("inspect_primary_icm"))
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if decision.Action != engine.DebugActionContinue {
		t.Fatalf("action = %q, want continue", decision.Action)
	}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if len(queue.pending) != 0 || len(queue.history) != 0 {
		t.Fatalf("unconfigured debug run published interaction: pending=%d history=%d", len(queue.pending), len(queue.history))
	}
}

func TestDebugInteraction_NestedBreakpointRoundTrip(t *testing.T) {
	broker := NewPromptBroker(8)
	broker.Register("run-debug")
	if err := broker.ConfigureDebug("run-debug", DebugRunConfig{
		Enabled: true,
		Breakpoints: []DebugBreakpoint{{
			Step:     "get_incident",
			Phase:    engine.DebugPhaseAfter,
			CallPath: []engine.DebugCallFrame{{StepID: "inspect_primary_icm"}},
		}},
	}); err != nil {
		t.Fatalf("ConfigureDebug: %v", err)
	}

	ctx := engine.WithRunID(context.Background(), "run-debug")
	resultCh := make(chan engine.DebugDecision, 1)
	errCh := make(chan error, 1)
	go func() {
		decision, err := broker.Pause(ctx, debugSnapshot("inspect_primary_icm"))
		if err != nil {
			errCh <- err
			return
		}
		resultCh <- decision
	}()

	frames, err := broker.Subscribe("run-debug", 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer broker.Unsubscribe("run-debug", frames)

	var pending PendingInteraction
	select {
	case frame := <-frames:
		var ok bool
		pending, ok = frame.(PendingInteraction)
		if !ok {
			t.Fatalf("frame = %T, want PendingInteraction", frame)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for debug breakpoint")
	}
	if pending.Kind != "debug_break" || pending.Debug == nil {
		t.Fatalf("pending frame = %#v, want debug_break payload", pending)
	}
	if pending.Debug.Actual.Output["incident"].(map[string]any)["status"] != "Mitigated" {
		t.Fatalf("actual output = %#v", pending.Debug.Actual.Output)
	}

	err = broker.Answer("run-debug", pending.TurnID, AnswerEnvelope{
		Kind:   "debug_break",
		Action: engine.DebugActionContinue,
		Set: &DebugSet{
			Status:      engine.StepStatusCompleted,
			OutputPatch: map[string]any{"incident": map[string]any{"status": "Active"}},
			Vars:        map[string]any{"forced_path": true},
		},
	})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}

	select {
	case err := <-errCh:
		t.Fatalf("Pause returned error: %v", err)
	case decision := <-resultCh:
		if decision.Result == nil || decision.Result.OutputPatch["incident"].(map[string]any)["status"] != "Active" {
			t.Fatalf("decision result = %#v", decision.Result)
		}
		if decision.Vars["forced_path"] != true {
			t.Fatalf("decision vars = %#v", decision.Vars)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for debug decision")
	}
}

func TestDebugInteraction_CallPathMismatchDoesNotPause(t *testing.T) {
	broker := NewPromptBroker(8)
	queue := broker.Register("run-debug")
	if err := broker.ConfigureDebug("run-debug", DebugRunConfig{
		Enabled: true,
		Breakpoints: []DebugBreakpoint{{
			Step:     "get_incident",
			Phase:    engine.DebugPhaseAfter,
			CallPath: []engine.DebugCallFrame{{StepID: "inspect_primary_icm"}},
		}},
	}); err != nil {
		t.Fatalf("ConfigureDebug: %v", err)
	}

	ctx := engine.WithRunID(context.Background(), "run-debug")
	decision, err := broker.Pause(ctx, debugSnapshot("inspect_secondary_icm"))
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if decision.Action != engine.DebugActionContinue {
		t.Fatalf("action = %q, want continue", decision.Action)
	}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if len(queue.pending) != 0 {
		t.Fatalf("mismatched call path created %d pending turns", len(queue.pending))
	}
}

func TestDebugProfile_AppliesOnlyToExactNestedCallPath(t *testing.T) {
	broker := NewPromptBroker(8)
	queue := broker.Register("run-debug")
	if err := broker.ConfigureDebug("run-debug", DebugRunConfig{
		Enabled: true,
		Profile: &DebugProfile{
			Version:        "yawr.debug-profile/v1",
			Name:           "Mitigated ICM as active",
			Root:           DebugProfileRoot{Ref: "triage.runbook.yaml", ID: "triage"},
			CreatedAgainst: "test-hash",
			Overrides: []DebugProfileOverride{{
				Target: DebugProfileTarget{
					Step:     "get_incident",
					Phase:    engine.DebugPhaseAfter,
					CallPath: []engine.DebugCallFrame{{StepID: "inspect_primary_icm"}},
				},
				Set: DebugSet{
					Status:      engine.StepStatusCompleted,
					OutputPatch: map[string]any{"incident": map[string]any{"status": "Active"}},
				},
			}},
		},
	}); err != nil {
		t.Fatalf("ConfigureDebug: %v", err)
	}

	ctx := engine.WithRunID(context.Background(), "run-debug")
	primary, err := broker.Pause(ctx, debugSnapshot("inspect_primary_icm"))
	if err != nil {
		t.Fatalf("primary Pause: %v", err)
	}
	if primary.Result == nil || primary.Result.OutputPatch["incident"].(map[string]any)["status"] != "Active" {
		t.Fatalf("primary decision = %#v, want automatic Active override", primary)
	}

	secondary, err := broker.Pause(ctx, debugSnapshot("inspect_secondary_icm"))
	if err != nil {
		t.Fatalf("secondary Pause: %v", err)
	}
	if secondary.Result != nil || secondary.Action != engine.DebugActionContinue {
		t.Fatalf("secondary decision = %#v, want unchanged continue", secondary)
	}

	queue.mu.Lock()
	defer queue.mu.Unlock()
	if len(queue.pending) != 0 {
		t.Fatalf("automatic profile created %d pending interactions", len(queue.pending))
	}
}

func TestDebugInteraction_StepIntoTargetsOnlyCurrentIncludeInvocation(t *testing.T) {
	broker := NewPromptBroker(16)
	broker.Register("run-debug")
	if err := broker.ConfigureDebug("run-debug", DebugRunConfig{
		Enabled: true,
		Breakpoints: []DebugBreakpoint{{
			Step:  "inspect_primary_icm",
			Phase: engine.DebugPhaseBefore,
		}},
	}); err != nil {
		t.Fatalf("ConfigureDebug: %v", err)
	}
	ctx := engine.WithRunID(context.Background(), "run-debug")

	includeDone := make(chan engine.DebugDecision, 1)
	go func() {
		decision, _ := broker.Pause(ctx, engine.DebugSnapshot{
			Phase:       engine.DebugPhaseBefore,
			CanStepInto: true,
			Location: engine.DebugLocation{
				RunID: "run-debug", StepID: "inspect_primary_icm", Invocation: 1, Attempt: 1,
			},
		})
		includeDone <- decision
	}()
	queue := broker.queueFor("run-debug")
	includeTurn := waitForPendingDebugTurn(t, queue, "inspect_primary_icm")
	if err := broker.Answer("run-debug", includeTurn, AnswerEnvelope{
		Kind: "debug_break", Action: engine.DebugActionStepInto,
	}); err != nil {
		t.Fatalf("step-into answer: %v", err)
	}
	if decision := <-includeDone; decision.Action != engine.DebugActionContinue {
		t.Fatalf("engine decision = %q, want continue", decision.Action)
	}

	secondary, err := broker.Pause(ctx, engine.DebugSnapshot{
		Phase: engine.DebugPhaseBefore,
		Location: engine.DebugLocation{
			RunID: "run-debug", StepID: "get_incident", Invocation: 1, Attempt: 1,
			CallPath: []engine.DebugCallFrame{{StepID: "inspect_secondary_icm"}},
		},
	})
	if err != nil || secondary.Action != engine.DebugActionContinue {
		t.Fatalf("secondary child decision = %#v, err=%v", secondary, err)
	}

	childDone := make(chan engine.DebugDecision, 1)
	go func() {
		decision, _ := broker.Pause(ctx, engine.DebugSnapshot{
			Phase: engine.DebugPhaseBefore,
			Location: engine.DebugLocation{
				RunID: "run-debug", StepID: "get_incident", Invocation: 1, Attempt: 1,
				CallPath: []engine.DebugCallFrame{{StepID: "inspect_primary_icm"}},
			},
		})
		childDone <- decision
	}()
	childTurn := waitForPendingDebugTurn(t, queue, "get_incident")
	if err := broker.Answer("run-debug", childTurn, AnswerEnvelope{
		Kind: "debug_break", Action: engine.DebugActionContinue,
	}); err != nil {
		t.Fatalf("child continue answer: %v", err)
	}
	if decision := <-childDone; decision.Action != engine.DebugActionContinue {
		t.Fatalf("child decision = %q, want continue", decision.Action)
	}
}

func TestDebugInteraction_RejectsStepIntoWithoutChildPath(t *testing.T) {
	broker := NewPromptBroker(8)
	queue := broker.Register("run-debug")
	if err := broker.ConfigureDebug("run-debug", DebugRunConfig{
		Enabled:     true,
		Breakpoints: []DebugBreakpoint{{Step: "dynamic_include", Phase: engine.DebugPhaseBefore}},
	}); err != nil {
		t.Fatalf("ConfigureDebug: %v", err)
	}
	ctx, cancel := context.WithCancel(engine.WithRunID(context.Background(), "run-debug"))
	defer cancel()
	go func() {
		_, _ = broker.Pause(ctx, engine.DebugSnapshot{
			Phase:       engine.DebugPhaseBefore,
			Location:    engine.DebugLocation{RunID: "run-debug", StepID: "dynamic_include", Invocation: 1, Attempt: 1},
			CanStepInto: false,
		})
	}()
	turnID := waitForPendingDebugTurn(t, queue, "dynamic_include")
	err := broker.Answer("run-debug", turnID, AnswerEnvelope{Kind: "debug_break", Action: engine.DebugActionStepInto})
	if !errors.Is(err, errInvalidDebugAnswer) {
		t.Fatalf("Step Into error = %v, want errInvalidDebugAnswer", err)
	}
}

func TestDebugInteraction_StepOverLastChildPausesAtParentReturn(t *testing.T) {
	broker := NewPromptBroker(16)
	queue := broker.Register("run-debug")
	if err := broker.ConfigureDebug("run-debug", DebugRunConfig{
		Enabled: true,
		Breakpoints: []DebugBreakpoint{{
			Step: "get_incident", Phase: engine.DebugPhaseAfter,
			CallPath: []engine.DebugCallFrame{{StepID: "inspect_primary_icm"}},
		}},
	}); err != nil {
		t.Fatalf("ConfigureDebug: %v", err)
	}
	ctx := engine.WithRunID(context.Background(), "run-debug")
	childDone := make(chan struct{})
	go func() {
		_, _ = broker.Pause(ctx, debugSnapshot("inspect_primary_icm"))
		close(childDone)
	}()
	childTurn := waitForPendingDebugTurn(t, queue, "get_incident")
	if err := broker.Answer("run-debug", childTurn, AnswerEnvelope{Kind: "debug_break", Action: engine.DebugActionStepOver}); err != nil {
		t.Fatalf("Step Over: %v", err)
	}
	<-childDone

	parentDone := make(chan struct{})
	go func() {
		_, _ = broker.Pause(ctx, engine.DebugSnapshot{
			Phase: engine.DebugPhaseAfter,
			Location: engine.DebugLocation{
				RunID: "run-debug", StepID: "inspect_primary_icm", Invocation: 1, Attempt: 1,
			},
			Actual: &engine.StepResult{StepID: "inspect_primary_icm", Status: engine.StepStatusCompleted},
		})
		close(parentDone)
	}()
	parentTurn := waitForPendingDebugTurn(t, queue, "inspect_primary_icm")
	if err := broker.Answer("run-debug", parentTurn, AnswerEnvelope{Kind: "debug_break", Action: engine.DebugActionContinue}); err != nil {
		t.Fatalf("parent continue: %v", err)
	}
	select {
	case <-parentDone:
	case <-time.After(time.Second):
		t.Fatal("parent return did not resume")
	}
}

func TestDebugInteraction_WatchesEvaluateAgainstPausedVariables(t *testing.T) {
	broker := NewPromptBroker(8)
	queue := broker.Register("run-debug")
	if err := broker.ConfigureDebug("run-debug", DebugRunConfig{
		Enabled: true,
		Breakpoints: []DebugBreakpoint{{
			Step: "get_incident", Phase: engine.DebugPhaseAfter,
			CallPath: []engine.DebugCallFrame{{StepID: "inspect_primary_icm"}},
		}},
		Watches: []string{"incident_status", "missing_value"},
	}); err != nil {
		t.Fatalf("ConfigureDebug: %v", err)
	}
	ctx := engine.WithRunID(context.Background(), "run-debug")
	done := make(chan struct{})
	go func() {
		_, _ = broker.Pause(ctx, debugSnapshot("inspect_primary_icm"))
		close(done)
	}()
	turnID := waitForPendingDebugTurn(t, queue, "get_incident")
	queue.mu.Lock()
	pending := queue.pending[turnID].frame
	queue.mu.Unlock()
	if pending.Debug == nil || len(pending.Debug.Watches) != 2 {
		t.Fatalf("watch results = %#v", pending.Debug)
	}
	if pending.Debug.Watches[0].Expression != "incident_status" || pending.Debug.Watches[0].Value != "Mitigated" {
		t.Fatalf("first watch = %#v", pending.Debug.Watches[0])
	}
	if pending.Debug.Watches[1].Expression != "missing_value" || pending.Debug.Watches[1].Error == "" {
		t.Fatalf("missing-value watch = %#v", pending.Debug.Watches[1])
	}
	if err := broker.Answer("run-debug", turnID, AnswerEnvelope{Kind: "debug_break", Action: engine.DebugActionContinue}); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watch pause did not resume")
	}
}

func TestDebugInteraction_NonFiniteValuesRemainJSONSerializable(t *testing.T) {
	payload := debugBreakPayload(engine.DebugSnapshot{
		Phase:    engine.DebugPhaseAfter,
		Location: engine.DebugLocation{RunID: "run-debug", StepID: "get_incident", Invocation: 1, Attempt: 1},
		Vars:     map[string]any{"nan": math.NaN(), "inf": math.Inf(1)},
		Actual: &engine.StepResult{
			StepID: "get_incident", Status: engine.StepStatusCompleted,
			Output: map[string]any{"negative_inf": math.Inf(-1)},
		},
	}, nil)
	frame := PendingInteraction{Type: "pending", RunID: "run-debug", TurnID: "turn", StepID: "get_incident", Kind: "debug_break", Debug: payload}
	projected := previewInteractionFrame(frame)
	projectedMap, ok := projected.(map[string]any)
	if !ok {
		t.Fatalf("projected frame = %T, want map", projected)
	}
	debugMap, ok := projectedMap["debug"].(map[string]any)
	if !ok {
		t.Fatalf("debug payload was not preserved as an object: %#v", projectedMap["debug"])
	}
	variables, _ := debugMap["variables"].(map[string]any)
	if variables["nan"] != "NaN" || variables["inf"] != "+Inf" {
		t.Fatalf("non-finite variables were not normalized: %#v", variables)
	}
	if _, err := json.Marshal(projected); err != nil {
		t.Fatalf("debug SSE frame is not JSON serializable: %v; frame=%#v", err, projected)
	}
}

func TestDebugInteraction_LargeActualPreservesControlMetadata(t *testing.T) {
	frame := PendingInteraction{
		Type: "pending", RunID: "run-debug", TurnID: "turn", StepID: "get_incident", Kind: "debug_break",
		Debug: &DebugBreakPayload{
			Phase: engine.DebugPhaseAfter, Invocation: 7, Attempt: 2, CanStepInto: true,
			CallPath: []engine.DebugCallFrame{{StepID: "inspect_primary_icm"}},
			Actual: &DebugActualResult{
				Status: engine.StepStatusFailed,
				Output: map[string]any{"huge": strings.Repeat("x", 100_000)},
			},
		},
	}
	projected := previewInteractionFrame(frame).(map[string]any)
	debug := projected["debug"].(map[string]any)
	actual := debug["actual"].(map[string]any)
	if debug["phase"] != engine.DebugPhaseAfter || debug["invocation"] != 7 || debug["attempt"] != 2 {
		t.Fatalf("control metadata was evicted: %#v", debug)
	}
	if actual["status"] != engine.StepStatusFailed {
		t.Fatalf("actual status was evicted or changed: %#v", actual)
	}
	if projected["debug_preview_truncated"] != true {
		t.Fatalf("large output was not marked truncated: %#v", projected)
	}
}

func TestDebugInteraction_BeforeVariableEditDoesNotCreateResultOverride(t *testing.T) {
	broker := NewPromptBroker(8)
	queue := broker.Register("run-debug")
	if err := broker.ConfigureDebug("run-debug", DebugRunConfig{
		Enabled:     true,
		Breakpoints: []DebugBreakpoint{{Step: "get_incident", Phase: engine.DebugPhaseBefore}},
	}); err != nil {
		t.Fatalf("ConfigureDebug: %v", err)
	}
	ctx := engine.WithRunID(context.Background(), "run-debug")
	decisionCh := make(chan engine.DebugDecision, 1)
	go func() {
		decision, _ := broker.Pause(ctx, engine.DebugSnapshot{
			Phase:    engine.DebugPhaseBefore,
			Location: engine.DebugLocation{RunID: "run-debug", StepID: "get_incident", Invocation: 1, Attempt: 1},
		})
		decisionCh <- decision
	}()
	turnID := waitForPendingDebugTurn(t, queue, "get_incident")
	if err := broker.Answer("run-debug", turnID, AnswerEnvelope{
		Kind: "debug_break", Action: engine.DebugActionContinue,
		Set: &DebugSet{Vars: map[string]any{"incident_status": "Active"}},
	}); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	decision := <-decisionCh
	if decision.Result != nil || decision.Vars["incident_status"] != "Active" {
		t.Fatalf("decision = %#v, want vars-only edit", decision)
	}
}

func TestDebugInteraction_ReconnectAlwaysReplaysUnresolvedPause(t *testing.T) {
	broker := NewPromptBroker(2)
	queue := broker.Register("run-debug")
	if err := broker.ConfigureDebug("run-debug", DebugRunConfig{
		Enabled:     true,
		Breakpoints: []DebugBreakpoint{{Step: "get_incident", Phase: engine.DebugPhaseAfter}},
	}); err != nil {
		t.Fatalf("ConfigureDebug: %v", err)
	}
	ctx := engine.WithRunID(context.Background(), "run-debug")
	done := make(chan struct{})
	go func() {
		_, _ = broker.Pause(ctx, engine.DebugSnapshot{
			Phase:    engine.DebugPhaseAfter,
			Location: engine.DebugLocation{RunID: "run-debug", StepID: "get_incident", Invocation: 1, Attempt: 1},
			Actual:   &engine.StepResult{StepID: "get_incident", Status: engine.StepStatusCompleted},
		})
		close(done)
	}()
	turnID := waitForPendingDebugTurn(t, queue, "get_incident")

	frames, err := broker.Subscribe("run-debug", 1)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer broker.Unsubscribe("run-debug", frames)
	select {
	case frame := <-frames:
		pending, ok := frame.(PendingInteraction)
		if !ok || pending.TurnID != turnID || pending.Kind != "debug_break" {
			t.Fatalf("replayed frame = %#v", frame)
		}
	case <-time.After(time.Second):
		t.Fatal("unresolved debug pause was not replayed")
	}
	if err := broker.Answer("run-debug", turnID, AnswerEnvelope{Kind: "debug_break", Action: engine.DebugActionContinue}); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("replayed pause did not resume")
	}
}

func TestDebugInteraction_RejectsResultEditAtBeforePhase(t *testing.T) {
	broker := NewPromptBroker(8)
	queue := broker.Register("run-debug")
	if err := broker.ConfigureDebug("run-debug", DebugRunConfig{
		Enabled:     true,
		Breakpoints: []DebugBreakpoint{{Step: "get_incident", Phase: engine.DebugPhaseBefore}},
	}); err != nil {
		t.Fatalf("ConfigureDebug: %v", err)
	}
	ctx, cancel := context.WithCancel(engine.WithRunID(context.Background(), "run-debug"))
	defer cancel()
	go func() {
		_, _ = broker.Pause(ctx, engine.DebugSnapshot{
			Phase:    engine.DebugPhaseBefore,
			Location: engine.DebugLocation{RunID: "run-debug", StepID: "get_incident", Invocation: 1, Attempt: 1},
		})
	}()
	turnID := waitForPendingDebugTurn(t, queue, "get_incident")
	err := broker.Answer("run-debug", turnID, AnswerEnvelope{
		Kind: "debug_break", Action: engine.DebugActionContinue,
		Set: &DebugSet{Status: engine.StepStatusCompleted},
	})
	if !errors.Is(err, errInvalidDebugAnswer) {
		t.Fatalf("Answer error = %v, want errInvalidDebugAnswer", err)
	}
}

func TestDebugInteraction_ClosedQueueRejectsLatePauseWithoutPanic(t *testing.T) {
	broker := NewPromptBroker(8)
	queue := broker.Register("run-debug")
	if err := broker.ConfigureDebug("run-debug", DebugRunConfig{
		Enabled:     true,
		Breakpoints: []DebugBreakpoint{{Step: "get_incident", Phase: engine.DebugPhaseAfter}},
	}); err != nil {
		t.Fatalf("ConfigureDebug: %v", err)
	}
	queue.mu.Lock()
	queue.closed = true
	queue.pending = nil
	queue.mu.Unlock()
	ctx := engine.WithRunID(context.Background(), "run-debug")
	_, err := broker.Pause(ctx, engine.DebugSnapshot{
		Phase:    engine.DebugPhaseAfter,
		Location: engine.DebugLocation{RunID: "run-debug", StepID: "get_incident", Invocation: 1, Attempt: 1},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Pause error = %v, want context.Canceled", err)
	}
}

func TestDebugHTTP_RejectsUnknownDebugField(t *testing.T) {
	harness := newInteractionHarness(t)
	server := newHTTPTestServer(t, harness.server)
	defer server.Close()
	body := `{"runbookPath":"runbook.yaml","debug":{"enabled":true,"mystery":true,"breakpoints":[{"step":"get_incident","phase":"after"}]}}`
	response, err := http.Post(server.URL+"/runs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /runs: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		raw, _ := io.ReadAll(response.Body)
		t.Fatalf("status = %d, want 400; body=%s", response.StatusCode, raw)
	}
}

func TestDebugHTTP_DisabledDebugIgnoresWrongRootProfile(t *testing.T) {
	harness := newInteractionHarness(t)
	setDebugHTTPRunbook(harness, "runbook.yaml")
	server := newHTTPTestServer(t, harness.server)
	defer server.Close()
	body := `{"runbookPath":"runbook.yaml","debug":{"enabled":false,"profile":{"version":"yawr.debug-profile/v1","name":"ignored","root":{"ref":"wrong.runbook.yaml","id":"wrong"},"created_against":"wrong","overrides":[{"target":{"step":"get_incident","phase":"after"},"set":{"status":"completed"}}]}}}`
	response, err := http.Post(server.URL+"/runs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /runs: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(response.Body)
		t.Fatalf("status = %d, want 201; body=%s", response.StatusCode, raw)
	}
}

func TestDebugHTTP_RejectsUnsupportedBreakpointTargets(t *testing.T) {
	harness := newInteractionHarness(t)
	harness.parser.result = &parser.ParsedRunbook{Source: "runbook.yaml", Runbook: &schema.Runbook{
		ID: "debug-targets", Name: "Debug targets",
		Flow: []schema.FlowNode{
			{Parallel: &schema.ParallelNode{ID: "fanout", Branches: []schema.ParallelBranch{{
				Label: "one", Steps: []schema.FlowNode{{Step: &schema.Step{ID: "parallel_child", Type: schema.StepTypeNoop}}},
			}}}},
			{Step: &schema.Step{ID: "wait", Type: schema.StepTypeWaitForEvent}},
			{Iterate: &schema.IterateNode{
				ID: "concurrent_loop", Over: "items", Concurrency: 2,
				Steps: []schema.FlowNode{{Step: &schema.Step{ID: "concurrent_child", Type: schema.StepTypeNoop}}},
			}},
		},
	}}
	harness.planner.plan = &engine.ExecutionPlan{
		RunID: "run-1", RunbookPath: "runbook.yaml",
		Metadata: engine.PlanMetadata{RunbookID: "debug-targets"},
	}
	server := newHTTPTestServer(t, harness.server)
	defer server.Close()

	for _, breakpoint := range []string{
		`{"step":"fanout","phase":"before"}`,
		`{"step":"parallel_child","phase":"before"}`,
		`{"step":"wait","phase":"after"}`,
		`{"step":"concurrent_child","phase":"before","callPath":[{"step_id":"concurrent_loop"}]}`,
	} {
		body := `{"runbookPath":"runbook.yaml","debug":{"enabled":true,"breakpoints":[` + breakpoint + `]}}`
		response, err := http.Post(server.URL+"/runs", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST /runs: %v", err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("breakpoint %s status = %d, want 400", breakpoint, response.StatusCode)
		}
	}
}

func waitForPendingDebugTurn(t *testing.T, queue *runQueue, stepID string) string {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		queue.mu.Lock()
		for turnID, turn := range queue.pending {
			if turn.frame.Kind == "debug_break" && turn.frame.StepID == stepID {
				queue.mu.Unlock()
				return turnID
			}
		}
		queue.mu.Unlock()
		runtime.Gosched()
	}
	t.Fatalf("timed out waiting for debug turn at %s", stepID)
	return ""
}

func TestDebugHTTP_ConfiguresRunAndAcceptsBreakpointAnswer(t *testing.T) {
	harness := newInteractionHarness(t)
	setDebugHTTPRunbook(harness, "runbook.yaml")
	server := newHTTPTestServer(t, harness.server)
	defer server.Close()

	createBody := `{
		"runbookPath":"runbook.yaml",
		"debug":{
			"enabled":true,
			"breakpoints":[{
				"step":"get_incident",
				"phase":"after",
				"callPath":[]
			}]
		}
	}`
	response, err := http.Post(server.URL+"/runs", "application/json", strings.NewReader(createBody))
	if err != nil {
		t.Fatalf("POST /runs: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("POST /runs status = %d, want 201; body=%s", response.StatusCode, body)
	}
	var created struct{ RunID string }
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	decisionCh := make(chan engine.DebugDecision, 1)
	errCh := make(chan error, 1)
	go func() {
		ctx := engine.WithRunID(context.Background(), created.RunID)
		decision, err := harness.server.broker.Pause(ctx, debugSnapshot(""))
		if err != nil {
			errCh <- err
			return
		}
		decisionCh <- decision
	}()

	stream, closeStream := openInteractionStream(t, server.URL, created.RunID)
	defer closeStream()
	frame, err := readInteractionFrame(stream)
	if err != nil {
		t.Fatalf("read debug frame: %v", err)
	}
	if frame["kind"] != "debug_break" {
		t.Fatalf("kind = %v, want debug_break", frame["kind"])
	}
	turnID, _ := frame["turnID"].(string)
	postInteractionAnswer(t, server.URL, created.RunID, turnID,
		`{"kind":"debug_break","action":"continue","mystery":true}`,
		http.StatusBadRequest,
	)
	postInteractionAnswer(t, server.URL, created.RunID, turnID,
		`{"kind":"debug_break","action":"continue","set":{"status":"completed","output_patch":{"incident":{"status":"Active"}}}}`,
		http.StatusNoContent,
	)

	select {
	case err := <-errCh:
		t.Fatalf("Pause: %v", err)
	case decision := <-decisionCh:
		if decision.Result == nil || decision.Result.OutputPatch["incident"].(map[string]any)["status"] != "Active" {
			t.Fatalf("decision = %#v", decision)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for HTTP debug answer")
	}
}

func TestDebugHTTP_RejectsInvalidBreakpointBeforeStartingRun(t *testing.T) {
	harness := newInteractionHarness(t)
	server := newHTTPTestServer(t, harness.server)
	defer server.Close()

	body := `{"runbookPath":"runbook.yaml","debug":{"enabled":true,"breakpoints":[{"step":"get_incident","phase":"later"}]}}`
	response, err := http.Post(server.URL+"/runs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /runs: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		raw, _ := io.ReadAll(response.Body)
		t.Fatalf("status = %d, want 400; body=%s", response.StatusCode, raw)
	}
}

func TestDebugHTTP_RejectsProfileBoundToAnotherRoot(t *testing.T) {
	workspace := t.TempDir()
	runbookPath := filepath.Join(workspace, "triage.runbook.yaml")
	writeDebugRunbookFile(t, runbookPath)
	harness := newInteractionHarness(t)
	harness.server.cfg.WorkspaceRoot = workspace
	harness.planner.plan.RunbookPath = runbookPath
	harness.planner.plan.Metadata.RunbookID = "triage"
	harness.planner.plan.Metadata.PlanHash = "current-hash"
	server := newHTTPTestServer(t, harness.server)
	defer server.Close()

	profile := `{"version":"yawr.debug-profile/v1","name":"Wrong root","root":{"ref":"other.runbook.yaml","id":"other"},"created_against":"current-hash","overrides":[{"target":{"step":"get_incident","phase":"after"},"set":{"status":"completed"}}]}`
	body := `{"runbookPath":` + mustJSON(t, runbookPath) + `,"debug":{"enabled":true,"profile":` + profile + `}}`
	response, err := http.Post(server.URL+"/runs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /runs: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		raw, _ := io.ReadAll(response.Body)
		t.Fatalf("status = %d, want 400; body=%s", response.StatusCode, raw)
	}
}

func TestDebugHTTP_StaleProfileRequiresExplicitConfirmation(t *testing.T) {
	workspace := t.TempDir()
	runbookPath := filepath.Join(workspace, "triage.runbook.yaml")
	writeDebugRunbookFile(t, runbookPath)
	harness := newInteractionHarness(t)
	setDebugHTTPRunbook(harness, runbookPath)
	harness.server.cfg.WorkspaceRoot = workspace
	harness.planner.plan.RunbookPath = runbookPath
	harness.planner.plan.Metadata.RunbookID = "triage"
	harness.planner.plan.Metadata.PlanHash = "current-hash"
	server := newHTTPTestServer(t, harness.server)
	defer server.Close()

	profile := `{"version":"yawr.debug-profile/v1","name":"Old","root":{"ref":"triage.runbook.yaml","id":"triage"},"created_against":"old-hash","overrides":[{"target":{"step":"get_incident","phase":"after"},"set":{"status":"completed"}}]}`
	post := func(allow bool) int {
		body := `{"runbookPath":` + mustJSON(t, runbookPath) + `,"debug":{"enabled":true,"allowStaleProfile":` + strconv.FormatBool(allow) + `,"profile":` + profile + `}}`
		response, err := http.Post(server.URL+"/runs", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST /runs: %v", err)
		}
		defer response.Body.Close()
		return response.StatusCode
	}
	if got := post(false); got != http.StatusBadRequest {
		t.Fatalf("unconfirmed stale profile status = %d, want 400", got)
	}
	if got := post(true); got != http.StatusCreated {
		t.Fatalf("confirmed stale profile status = %d, want 201", got)
	}
}

func setDebugHTTPRunbook(harness *interactionHarness, runbookPath string) {
	harness.parser.result = &parser.ParsedRunbook{Source: runbookPath, Runbook: &schema.Runbook{
		ID: "triage", Name: "Triage",
		Flow: []schema.FlowNode{{Step: &schema.Step{ID: "get_incident", Type: schema.StepTypeNoop}}},
	}}
	harness.planner.plan.RunbookPath = runbookPath
	if harness.planner.plan.Metadata.RunbookID == "" || harness.planner.plan.Metadata.RunbookID == "rb-1" {
		harness.planner.plan.Metadata.RunbookID = "triage"
	}
}
