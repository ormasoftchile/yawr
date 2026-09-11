package planner

import (
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestTypedResultsDefaultAndExplicitTitles(t *testing.T) {
	if displayName(&schema.Step{ID: "publish", Type: schema.StepTypeResults}) != "Results" {
		t.Fatal("missing default Results title")
	}
	if displayName(&schema.Step{ID: "publish", Title: "publish", Type: schema.StepTypeResults}) != "publish" {
		t.Fatal("explicit title rewritten")
	}
	if displayName(&schema.Step{ID: "legacy", Type: schema.StepTypeNoop}) != "legacy" {
		t.Fatal("legacy default changed")
	}
}

func TestTypedRuntimeAdmitsValidScopeWithoutCapabilityAdvertisement(t *testing.T) {
	for _, runbook := range []*schema.Runbook{
		{Bindings: []schema.Binding{{Name: "x", Type: "any", ValuePresent: true}}},
		{Outputs: map[string]*schema.Output{"result": {Type: "any", ValueTreePresent: true}}},
		{Flow: []schema.FlowNode{{Step: &schema.Step{Type: schema.StepTypeResults}}}},
	} {
		if err := typedRuntimeAvailability(runbook); err != nil {
			t.Fatalf("valid typed execution rejected: %v", err)
		}
	}
	if err := typedRuntimeAvailability(&schema.Runbook{Flow: []schema.FlowNode{{Step: &schema.Step{Type: schema.StepTypeNoop}}}}); err != nil {
		t.Fatalf("legacy execution blocked: %v", err)
	}
}
