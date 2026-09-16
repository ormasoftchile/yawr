package planner_test

import (
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestEnumDefaultAdmissionRetainsPlannerDiagnosticOrder(t *testing.T) {
	action := &schema.ToolAction{
		Args: map[string]*schema.ArgDef{
			"z": {Type: "string", Enum: schema.EnumConstraint{"ok"}, Default: "bad-z"},
			"a": {Type: "string", Enum: schema.EnumConstraint{"ok"}, Default: "bad-a"},
		},
		Outputs: map[string]*schema.ArgDef{
			"output": {Type: "string", Enum: schema.EnumConstraint{"ok"}, Default: "bad-output"},
		},
	}
	plan := &engine.ExecutionPlan{
		Metadata: engine.PlanMetadata{PlanHash: "enum-default-fixture"},
		Inputs:   map[string]*schema.Input{"input": {Type: "string", Enum: schema.EnumConstraint{"ok"}, Default: "bad-input"}},
		Tools:    map[string]*schema.ToolDef{"tool": {Name: "tool", Actions: map[string]*schema.ToolAction{"action": action}}},
	}
	err := planner.ValidateExecutionPlan(plan)
	if err == nil {
		t.Fatal("expected enum default admission failure")
	}
	expected := []error{schema.ValidateEnumDefault(plan.Inputs["input"].Enum, "bad-input", `input "input"`)}
	expected = append(expected, schema.ValidateToolActionEnumDefaults("tool", "action", action)...)
	previous := -1
	for _, issue := range expected {
		index := strings.Index(err.Error(), issue.Error())
		if index <= previous {
			t.Fatalf("changed planner default message/order: %v; missing or misplaced %v", err, issue)
		}
		previous = index
	}
}
