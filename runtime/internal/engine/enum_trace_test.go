package engine

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

// TestEngine_PlanValidated_CarriesEnumConstraintsOnce covers R3
// (barbara-enum-mvp-implementation-gate.md, AR-ENUM-10): approved enum
// metadata must be emitted exactly once per run, in the existing
// plan/validated trace event, in declared order with C1-safe redaction,
// and must never be repeated in any per-step (tool/invoked, tool/completed,
// step/started, step/completed) event payload.
func TestEngine_PlanValidated_CarriesEnumConstraintsOnce(t *testing.T) {
	tw := &fakeTraceWriter{}
	cfg := makeTestConfig()
	cfg.TraceWriter = tw
	reg := newFakeExecutorRegistry()
	reg.Register("cli", &passThroughExecutor{})
	cfg.Executors = reg
	eng := New(cfg)

	plan := makeTestPlan(engine.ResolvedStep{ID: "step-1", Kind: "cli", Spec: &cliStepSpec{}})
	plan = engine.ValidatedForTest(plan)
	plan.Validation.EnumConstraints = map[string]engine.EnumMeta{
		"inputs.env_name": {Members: []string{"prod", "staging"}, MemberCount: 2},
		"inputs.api_key":  {Redacted: true, MemberCount: 3},
	}

	handle, err := eng.Start(context.Background(), plan, engine.RunOptions{})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	for {
		if _, nerr := handle.Next(context.Background()); nerr != nil {
			break
		}
	}

	evs := tw.collect()
	var planValidatedCount int
	for _, ev := range evs {
		var payload map[string]any
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			t.Fatalf("unmarshal %s payload: %v", ev.Kind, err)
		}
		_, hasEnum := payload["enum_constraints"]
		switch ev.Kind {
		case trace.EventKindPlanValidated:
			planValidatedCount++
			if !hasEnum {
				t.Fatalf("plan.validated missing enum_constraints")
			}
			ec, ok := payload["enum_constraints"].(map[string]any)
			if !ok {
				t.Fatalf("enum_constraints has unexpected shape: %#v", payload["enum_constraints"])
			}
			envEntry, ok := ec["inputs.env_name"].(map[string]any)
			if !ok {
				t.Fatalf("missing inputs.env_name entry: %#v", ec)
			}
			members, ok := envEntry["members"].([]any)
			if !ok || len(members) != 2 || members[0] != "prod" || members[1] != "staging" {
				t.Fatalf("expected declared-order members [prod staging], got %#v", envEntry["members"])
			}
			if envEntry["member_count"].(float64) != 2 {
				t.Fatalf("expected member_count 2, got %v", envEntry["member_count"])
			}
			redactedEntry, ok := ec["inputs.api_key"].(map[string]any)
			if !ok {
				t.Fatalf("missing inputs.api_key entry: %#v", ec)
			}
			if redactedEntry["members"] != "<redacted>" {
				t.Fatalf("expected redacted members to be \"<redacted>\", got %#v", redactedEntry["members"])
			}
			if redactedEntry["member_count"].(float64) != 3 {
				t.Fatalf("expected redacted member_count 3, got %v", redactedEntry["member_count"])
			}
		case trace.EventKindToolInvoked, trace.EventKindToolCompleted,
			trace.EventKindStepStarted, trace.EventKindStepCompleted:
			if hasEnum {
				t.Fatalf("%s payload must never carry enum_constraints", ev.Kind)
			}
		}
	}
	if planValidatedCount != 1 {
		t.Fatalf("expected exactly one plan.validated event, got %d", planValidatedCount)
	}
}
