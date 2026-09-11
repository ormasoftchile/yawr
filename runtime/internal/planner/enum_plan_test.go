package planner_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	plannerPkg "github.com/ormasoftchile/yawr/runtime/pkg/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// TestValidatePlanEnum006DefaultNotMember covers ENUM-006 (S3): an input's
// default: value must be a declared enum member.
func TestValidatePlanEnum006DefaultNotMember(t *testing.T) {
	rb := &parser.ParsedRunbook{Source: "enum-default.yaml", Runbook: &schema.Runbook{
		ID: "enum-default", Name: "enum-default",
		Inputs: map[string]*schema.Input{
			"env_name": {Type: "string", Default: "prod", Enum: schema.EnumConstraint{"dev", "staging"}},
		},
		Flow: []schema.FlowNode{{Step: &schema.Step{ID: "s1", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "echo"}}}},
	}}
	pl := planner.New(plannerPkg.Config{Loader: &fakeLoader{runbooks: map[string]*parser.ParsedRunbook{}}, Tools: &fakeRegistry{tools: map[string]*schema.ToolDef{}}})
	_, err := pl.Plan(context.Background(), rb)
	if err == nil {
		t.Fatal("expected ENUM-006 validation error")
	}
	if !strings.Contains(err.Error(), "ENUM-006") {
		t.Fatalf("expected ENUM-006 in error, got: %v", err)
	}
}

// TestValidatePlanEnum006DefaultIsMember is the corresponding happy path:
// a default that IS a declared member must not fail plan validation.
func TestValidatePlanEnum006DefaultIsMember(t *testing.T) {
	rb := &parser.ParsedRunbook{Source: "enum-default-ok.yaml", Runbook: &schema.Runbook{
		ID: "enum-default-ok", Name: "enum-default-ok",
		Inputs: map[string]*schema.Input{
			"env_name": {Type: "string", Default: "dev", Enum: schema.EnumConstraint{"dev", "staging"}},
		},
		Flow: []schema.FlowNode{{Step: &schema.Step{ID: "s1", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "echo"}}}},
	}}
	pl := planner.New(plannerPkg.Config{Loader: &fakeLoader{runbooks: map[string]*parser.ParsedRunbook{}}, Tools: &fakeRegistry{tools: map[string]*schema.ToolDef{}}})
	plan, err := pl.Plan(context.Background(), rb)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	meta, ok := plan.Validation.EnumConstraints["inputs.env_name"]
	if !ok {
		t.Fatal("expected inputs.env_name enum metadata to be carried on ValidatedPlan")
	}
	if meta.Redacted || meta.MemberCount != 2 || len(meta.Members) != 2 {
		t.Fatalf("unexpected enum metadata: %+v", meta)
	}
}

// TestValidatePlanEnum007StaticLiteralNotMember covers ENUM-007 (S1): a
// statically-known (non-GIS-interpolated) tool.args literal bound to an
// enum-constrained arg must be a declared member.
func TestValidatePlanEnum007StaticLiteralNotMember(t *testing.T) {
	toolDef := &schema.ToolDef{Name: "kubectl", Actions: map[string]*schema.ToolAction{
		"drain-node": {Args: map[string]*schema.ArgDef{
			"mode": {Type: "string", Enum: schema.EnumConstraint{"graceful", "force"}},
		}},
	}}
	rb := &parser.ParsedRunbook{Source: "enum-static.yaml", Runbook: &schema.Runbook{
		ID: "enum-static", Name: "enum-static",
		Flow: []schema.FlowNode{{Step: &schema.Step{ID: "s1", Type: schema.StepTypeTool, ToolCall: &schema.ToolCallSpec{
			Tool: schema.ToolInvocation{Name: "kubectl", Action: "drain-node", Args: map[string]any{"mode": "immediate"}},
		}}}},
	}}
	pl := planner.New(plannerPkg.Config{Loader: &fakeLoader{runbooks: map[string]*parser.ParsedRunbook{}}, Tools: &fakeRegistry{tools: map[string]*schema.ToolDef{"kubectl/drain-node": toolDef}}})
	_, err := pl.Plan(context.Background(), rb)
	if err == nil {
		t.Fatal("expected ENUM-007 validation error")
	}
	if !strings.Contains(err.Error(), "ENUM-007") {
		t.Fatalf("expected ENUM-007 in error, got: %v", err)
	}
}

// TestValidatePlanEnum007SkipsInterpolatedLiterals confirms a GIS-templated
// tool arg value (not statically known) is never checked at plan time
// (AR-ENUM-7: interpolated values are runtime-only, ENUM-008).
func TestValidatePlanEnum007SkipsInterpolatedLiterals(t *testing.T) {
	toolDef := &schema.ToolDef{Name: "kubectl", Actions: map[string]*schema.ToolAction{
		"drain-node": {Args: map[string]*schema.ArgDef{
			"mode": {Type: "string", Enum: schema.EnumConstraint{"graceful", "force"}},
		}},
	}}
	rb := &parser.ParsedRunbook{Source: "enum-interp.yaml", Runbook: &schema.Runbook{
		ID: "enum-interp", Name: "enum-interp",
		Inputs: map[string]*schema.Input{"mode_in": {Type: "string"}},
		Flow: []schema.FlowNode{{Step: &schema.Step{ID: "s1", Type: schema.StepTypeTool, ToolCall: &schema.ToolCallSpec{
			Tool: schema.ToolInvocation{Name: "kubectl", Action: "drain-node", Args: map[string]any{"mode": "${mode_in}"}},
		}}}},
	}}
	pl := planner.New(plannerPkg.Config{Loader: &fakeLoader{runbooks: map[string]*parser.ParsedRunbook{}}, Tools: &fakeRegistry{tools: map[string]*schema.ToolDef{"kubectl/drain-node": toolDef}}})
	if _, err := pl.Plan(context.Background(), rb); err != nil {
		t.Fatalf("expected no plan-time error for interpolated arg, got: %v", err)
	}
}

// TestValidatePlanEnumMetadataRedactedByGovernance covers C1 redaction: a
// declaration name matched by a governance redact rule is carried with
// Redacted=true and Members=nil, never the plaintext member list.
func TestValidatePlanEnumMetadataRedactedByGovernance(t *testing.T) {
	rb := &parser.ParsedRunbook{Source: "enum-redact.yaml", Runbook: &schema.Runbook{
		ID: "enum-redact", Name: "enum-redact",
		Governance: &schema.GovernanceConfig{
			Redact: []schema.RedactRule{{Pattern: "token", Replace: "[REDACTED]"}},
		},
		Inputs: map[string]*schema.Input{
			"token": {Type: "string", Enum: schema.EnumConstraint{"alpha", "beta"}},
		},
		Flow: []schema.FlowNode{{Step: &schema.Step{ID: "s1", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "echo"}}}},
	}}
	pl := planner.New(plannerPkg.Config{Loader: &fakeLoader{runbooks: map[string]*parser.ParsedRunbook{}}, Tools: &fakeRegistry{tools: map[string]*schema.ToolDef{}}})
	plan, err := pl.Plan(context.Background(), rb)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	meta, ok := plan.Validation.EnumConstraints["inputs.token"]
	if !ok {
		t.Fatal("expected inputs.token enum metadata")
	}
	if !meta.Redacted || meta.Members != nil || meta.MemberCount != 2 {
		t.Fatalf("expected redacted metadata with member count only, got: %+v", meta)
	}
}
