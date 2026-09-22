package planner_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	plannerPkg "github.com/ormasoftchile/yawr/runtime/pkg/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestValidatePlanHappyPathRecordsGrammars(t *testing.T) {
	rb := validationRunbook([]schema.FlowNode{{Step: &schema.Step{ID: "s1", Type: schema.StepTypeCLI, Title: "hello ${name}", When: `name != ""`, CLI: &schema.CLISpec{Command: "echo", Args: []string{"${name}"}}, Capture: map[string]string{"out": "stdout"}}}})
	pl := planner.New(plannerPkg.Config{Loader: &fakeLoader{runbooks: map[string]*parser.ParsedRunbook{}}, Tools: &fakeRegistry{tools: map[string]*schema.ToolDef{}}})
	plan, err := pl.Plan(context.Background(), rb)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Validation == nil {
		t.Fatal("expected validation metadata")
	}
	if plan.Validation.GrammarVersions.GXL != engine.GrammarVersionGXL || plan.Validation.GrammarVersions.GIS != engine.GrammarVersionGIS || plan.Validation.GrammarVersions.GCP != engine.GrammarVersionGCP {
		t.Fatalf("grammar versions not recorded: %+v", plan.Validation.GrammarVersions)
	}
	if plan.Validation.ExpressionCount.GXL != 1 || plan.Validation.ExpressionCount.GIS == 0 || plan.Validation.ExpressionCount.GCP != 1 {
		t.Fatalf("unexpected expression counts: %+v", plan.Validation.ExpressionCount)
	}
}

func TestValidatePlanInvalidSingleExpression(t *testing.T) {
	rb := validationRunbook([]schema.FlowNode{{Step: &schema.Step{ID: "bad", Type: schema.StepTypeBranch, BranchSpec: &schema.BranchSpec{Branches: []schema.BranchArm{{Condition: `!ready`}}}}}})
	pl := planner.New(plannerPkg.Config{Loader: &fakeLoader{runbooks: map[string]*parser.ParsedRunbook{}}, Tools: &fakeRegistry{tools: map[string]*schema.ToolDef{}}})
	_, err := pl.Plan(context.Background(), rb)
	if err == nil {
		t.Fatal("expected validation error")
	}
	msg := err.Error()
	for _, want := range []string{"bad", "branches[0].condition", "GXL-PARSE-007", "!ready"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q missing %q", msg, want)
		}
	}
}

func TestValidatePlanAllowsDeclaredMCPOutputCapture(t *testing.T) {
	rb := validationRunbook([]schema.FlowNode{{Step: &schema.Step{
		ID:       "get_incident",
		Type:     schema.StepTypeTool,
		ToolCall: &schema.ToolCallSpec{Tool: schema.ToolInvocation{Name: "icm", Action: "get-incident"}},
		Capture:  map[string]string{"title": "outputs.title"},
	}}})
	tools := map[string]*schema.ToolDef{
		"icm/get-incident": {
			Name: "icm",
			Actions: map[string]*schema.ToolAction{
				"get-incident": {Outputs: map[string]*schema.ArgDef{"title": {Type: "string"}}},
			},
		},
	}
	pl := planner.New(plannerPkg.Config{
		Loader: &fakeLoader{runbooks: map[string]*parser.ParsedRunbook{}},
		Tools:  &fakeRegistry{tools: tools},
	})
	if _, err := pl.Plan(context.Background(), rb); err != nil {
		t.Fatalf("declared MCP output capture must plan successfully: %v", err)
	}
}

func TestValidatePlanAllowsHostActionOutputsAndValidatesTemplates(t *testing.T) {
	rb := validationRunbook([]schema.FlowNode{{Step: &schema.Step{
		ID:   "open_external_view",
		Type: schema.StepTypeHostAction,
		Capture: map[string]string{
			"host_status":     "outputs.status",
			"resource_status": "outputs.result.status",
		},
		HostActionSpec: &schema.HostActionSpec{HostAction: schema.HostActionConfig{
			Capability: "product.open-resource",
			Request: map[string]any{
				"resource": "${incident.id}",
				"options":  map[string]any{"region": "${target.region}", "labels": []any{"primary", "${target.label}"}},
			},
		}},
	}}})
	pl := planner.New(plannerPkg.Config{Loader: &fakeLoader{runbooks: map[string]*parser.ParsedRunbook{}}, Tools: &fakeRegistry{tools: map[string]*schema.ToolDef{}}})
	plan, err := pl.Plan(context.Background(), rb)
	if err != nil {
		t.Fatalf("host action status capture must plan successfully: %v", err)
	}
	for _, field := range []string{
		"host_action.request.resource",
		"host_action.request.options.region",
		"host_action.request.options.labels[1]",
	} {
		if _, ok := plan.Validation.GISTemplates[engine.StepRef{StepID: "open_external_view", FieldPath: field}]; !ok {
			t.Fatalf("host action template %q was not validated", field)
		}
	}
}

func TestValidatePlanRejectsUndeclaredHostActionOutputCapture(t *testing.T) {
	rb := validationRunbook([]schema.FlowNode{{Step: &schema.Step{
		ID:      "open_external_view",
		Type:    schema.StepTypeHostAction,
		Capture: map[string]string{"view": "outputs.view"},
		HostActionSpec: &schema.HostActionSpec{HostAction: schema.HostActionConfig{
			Capability: "product.open-resource",
			Request: map[string]any{
				"resource": "incident-42",
			},
		}},
	}}})
	pl := planner.New(plannerPkg.Config{Loader: &fakeLoader{runbooks: map[string]*parser.ParsedRunbook{}}, Tools: &fakeRegistry{tools: map[string]*schema.ToolDef{}}})
	_, err := pl.Plan(context.Background(), rb)
	if err == nil || !strings.Contains(err.Error(), "GCP-PARSE-001") {
		t.Fatalf("undeclared host-action output was accepted: %v", err)
	}
}

// TestValidatePlanRejectsUndeclaredOutputCapture is the negative case:
// capturing outputs.<name> when the action does NOT declare that output must
// still produce GCP-PARSE-001. This keeps stepBindsDeclaredOutput from
// becoming a blanket "always allow".
//
// Mutation control: if stepBindsDeclaredOutput is stubbed to return true
// unconditionally, this test fails — proving the check is not vacuous.
func TestValidatePlanAllowsQueryResultSynthesizedOutputCapture(t *testing.T) {
	rb := validationRunbook([]schema.FlowNode{{Step: &schema.Step{
		ID:       "query",
		Type:     schema.StepTypeTool,
		ToolCall: &schema.ToolCallSpec{Tool: schema.ToolInvocation{Name: "queryer", Action: "query"}},
		Capture:  map[string]string{"count": "outputs.row_count", "rows": "outputs.rows"},
	}}})
	tools := map[string]*schema.ToolDef{
		"queryer/query": {
			Name: "queryer",
			Actions: map[string]*schema.ToolAction{
				"query": {Result: &schema.ActionResultContract{
					Format:   schema.ActionResultFormatQueryResultV1,
					Source:   schema.ActionResultSourceStdoutJSON,
					RowCount: "rowCount",
					Columns:  "columns",
					Rows:     "data",
				}},
			},
		},
	}
	pl := planner.New(plannerPkg.Config{
		Loader: &fakeLoader{runbooks: map[string]*parser.ParsedRunbook{}},
		Tools:  &fakeRegistry{tools: tools},
	})
	if _, err := pl.Plan(context.Background(), rb); err != nil {
		t.Fatalf("synthesized query-result output captures must plan successfully: %v", err)
	}
}

func TestValidatePlanRejectsUndeclaredOutputCapture(t *testing.T) {
	rb := validationRunbook([]schema.FlowNode{{Step: &schema.Step{
		ID:       "get_incident",
		Type:     schema.StepTypeTool,
		ToolCall: &schema.ToolCallSpec{Tool: schema.ToolInvocation{Name: "icm", Action: "get-incident"}},
		Capture:  map[string]string{"title": "outputs.title"},
	}}})
	// Action exists but declares NO outputs — "title" is undeclared.
	tools := map[string]*schema.ToolDef{
		"icm/get-incident": {
			Name: "icm",
			Actions: map[string]*schema.ToolAction{
				"get-incident": {Outputs: map[string]*schema.ArgDef{}},
			},
		},
	}
	pl := planner.New(plannerPkg.Config{
		Loader: &fakeLoader{runbooks: map[string]*parser.ParsedRunbook{}},
		Tools:  &fakeRegistry{tools: tools},
	})
	_, err := pl.Plan(context.Background(), rb)
	if err == nil {
		t.Fatal("MUTATION CONTROL FAILED: expected GCP-PARSE-001 for undeclared output capture, got nil error")
	}
	if !strings.Contains(err.Error(), "GCP-PARSE-001") {
		t.Fatalf("expected GCP-PARSE-001 in error, got: %v", err)
	}
}

func TestValidatePlanMultipleErrorsStableOrder(t *testing.T) {
	rb := validationRunbook([]schema.FlowNode{
		{Step: &schema.Step{ID: "a", Type: schema.StepTypeCLI, When: `!bad`, CLI: &schema.CLISpec{Command: "echo"}}},
		{Step: &schema.Step{ID: "b", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "echo"}, Capture: map[string]string{"z": "exitCode"}}},
	})
	pl := planner.New(plannerPkg.Config{Loader: &fakeLoader{runbooks: map[string]*parser.ParsedRunbook{}}, Tools: &fakeRegistry{tools: map[string]*schema.ToolDef{}}})
	_, err := pl.Plan(context.Background(), rb)
	if err == nil {
		t.Fatal("expected validation error")
	}
	msg := err.Error()
	first := strings.Index(msg, "step a when")
	second := strings.Index(msg, "step b capture.z")
	if first < 0 || second < 0 || first > second {
		t.Fatalf("errors not stable/in plan order: %s", msg)
	}
}

func validationRunbook(flow []schema.FlowNode) *parser.ParsedRunbook {
	return &parser.ParsedRunbook{Source: "validation-test.yaml", Runbook: &schema.Runbook{ID: "validation-test", Name: "validation-test", Flow: flow}}
}
