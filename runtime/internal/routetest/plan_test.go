package routetest

import (
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestValidateScenarioPlan_BindsQualifiedKinds(t *testing.T) {
	plan := &engine.ExecutionPlan{Steps: []engine.ResolvedStep{
		{ID: "inspect", Kind: "include", Depth: 0, Spec: &schema.IncludeSpec{}},
		{ID: "open", Kind: "host_action", Depth: 1, ParentID: "inspect", ParentKind: "include", Spec: &schema.HostActionSpec{HostAction: schema.HostActionConfig{Capability: "xts.open-view"}}},
		{ID: "branch", Kind: "branch", Depth: 1, ParentID: "inspect", ParentKind: "include"},
		{ID: "findings", Kind: "collector", Depth: 2, ParentID: "branch", ParentKind: "branch", Spec: &schema.CollectorSpec{Fields: []schema.CollectorField{{Name: "health", Type: schema.FieldTypeSelect, Ephemeral: true}}}},
		{ID: "route", Kind: "branch", Depth: 0},
		{ID: "target", Kind: "cli", Depth: 1, ParentID: "route", ParentKind: "branch"},
	}}
	scenario := Scenario{
		Target:              Selector{CallPath: []string{"route"}, Step: "target", Phase: "before", Invocation: 1, Attempt: 1},
		HostActionResponses: []HostActionBinding{{At: Selector{CallPath: []string{"inspect"}, Step: "open", Phase: "execute", Invocation: 1, Attempt: 1}, Capability: "xts.open-view"}},
		InteractionAnswers:  []InteractionBinding{{At: Selector{CallPath: []string{"inspect", "branch"}, Step: "findings", Phase: "execute", Invocation: 1, Attempt: 1}, Kind: "collector", Values: map[string]any{"health": "healthy"}}},
	}
	if err := ValidateScenarioPlan(scenario, plan); err == nil || !strings.Contains(err.Error(), "ephemeral") {
		t.Fatalf("error = %v, want ephemeral rejection", err)
	}

	scenario.InteractionAnswers[0].Values = nil
	if err := ValidateScenarioPlan(scenario, plan); err != nil {
		t.Fatalf("ValidateScenarioPlan: %v", err)
	}
	scenario.HostActionResponses[0].At.CallPath = nil
	if err := ValidateScenarioPlan(scenario, plan); err == nil || !strings.Contains(err.Error(), "does not resolve") {
		t.Fatalf("error = %v, want qualified selector rejection", err)
	}
}

func TestValidateScenarioPlan_RejectsBindingKindMismatch(t *testing.T) {
	plan := &engine.ExecutionPlan{Steps: []engine.ResolvedStep{{ID: "target", Kind: "noop"}, {ID: "answer", Kind: "choice", Spec: &schema.ChoiceSpec{}}}}
	scenario := Scenario{
		Target:             Selector{Step: "target", Phase: "before", Invocation: 1, Attempt: 1},
		InteractionAnswers: []InteractionBinding{{At: Selector{Step: "answer", Phase: "execute", Invocation: 1, Attempt: 1}, Kind: "collector"}},
	}
	if err := ValidateScenarioPlan(scenario, plan); err == nil || !strings.Contains(err.Error(), "kind") {
		t.Fatalf("error = %v, want kind mismatch", err)
	}
}

func TestValidateScenarioPlan_RejectsConcurrentIterate(t *testing.T) {
	plan := &engine.ExecutionPlan{Steps: []engine.ResolvedStep{
		{ID: "loop", Kind: "iterate", Spec: &schema.IterateNode{Concurrency: 2}},
		{ID: "target", Kind: "noop", Depth: 1, ParentID: "loop", ParentKind: "iterate"},
	}}
	scenario := Scenario{Target: Selector{
		CallPath: []string{"loop"}, Step: "target", Phase: "before", Invocation: 1, Attempt: 1,
	}}

	err := ValidateScenarioPlan(scenario, plan)
	if err == nil || !strings.Contains(err.Error(), "concurrency") {
		t.Fatalf("error = %v, want concurrent iterate rejection", err)
	}
}

func TestValidateScenarioPlan_RejectsTargetBeneathParallel(t *testing.T) {
	plan := &engine.ExecutionPlan{Steps: []engine.ResolvedStep{
		{ID: "fanout", Kind: "parallel", Depth: 0},
		{ID: "target", Kind: "noop", Depth: 1, ParentID: "fanout", ParentKind: "parallel"},
	}}
	scenario := Scenario{Target: Selector{
		CallPath: []string{"fanout"}, Step: "target", Phase: "before", Invocation: 1, Attempt: 1,
	}}
	err := ValidateScenarioPlan(scenario, plan)
	if err == nil || !strings.Contains(err.Error(), "multi-cursor") {
		t.Fatalf("error = %v, want parallel target rejection", err)
	}
}
