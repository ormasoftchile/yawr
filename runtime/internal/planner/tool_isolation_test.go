package planner_test

import (
	"context"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestPlannerOwnsToolDefinitions(t *testing.T) {
	shared := &schema.ToolDef{Name: "shared", Actions: map[string]*schema.ToolAction{
		"run": {Args: map[string]*schema.ArgDef{"flag": {Type: "boolean", Default: false}}},
	}}
	registry := &fakeRegistry{tools: map[string]*schema.ToolDef{"shared/run": shared}}
	p := newPlanner(&fakeLoader{}, registry)
	runbook := &parser.ParsedRunbook{Source: "caller.runbook.yaml", Runbook: &schema.Runbook{
		ID: "caller", Name: "Caller", Flow: []schema.FlowNode{{Step: &schema.Step{ID: "call", Type: schema.StepTypeTool,
			ToolCall: &schema.ToolCallSpec{Tool: schema.ToolInvocation{Name: "shared", Action: "run"}}}}},
	}}
	first, err := p.Plan(context.Background(), runbook)
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.Plan(context.Background(), runbook)
	if err != nil {
		t.Fatal(err)
	}
	first.Tools["shared"].Actions["run"].Args["flag"].Default = true
	if shared.Actions["run"].Args["flag"].Default != false ||
		second.Tools["shared"].Actions["run"].Args["flag"].Default != false {
		t.Fatal("plans alias registry or each other")
	}
}
