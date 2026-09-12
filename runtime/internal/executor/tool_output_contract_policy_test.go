package executor

// tool_output_contract_policy_test.go — tests for the OPT-IN
// output_contract.additional_outputs: ignore policy. These prove the policy
// drops undeclared provider-owned keys WITHOUT relaxing any other part of the
// contract, and that the default (no policy) remains strict.

import (
	"context"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/testutil"
	"github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

// ─── pure-function tests ──────────────────────────────────────────────────────

// (1) Default (ignoreAdditionalOutputs=false) still rejects an undeclared key.
// Negative control: identical inputs to test (2), only the policy flag differs.
func TestEnforceOutputContractPolicy_DefaultStrict_RejectsUndeclared(t *testing.T) {
	decl := map[string]*schema.ArgDef{"status": {Type: "string"}}
	actual := map[string]any{
		"stdout": "", "stderr": "", "exit_code": 0,
		"status":      "ok",
		"description": "provider-owned extra", // undeclared
	}
	_, err := enforceOutputContractPolicy("icm", "get-incident", decl, actual, false)
	if err == nil {
		t.Fatal("NEGATIVE CONTROL FAILED: expected strict default to reject undeclared key")
	}
	if !strings.Contains(err.Error(), "undeclared output") {
		t.Errorf("expected 'undeclared output' in error, got: %v", err)
	}
}

// (2) ignore accepts the undeclared key AND omits it from the validated output.
func TestEnforceOutputContractPolicy_Ignore_DropsUndeclared(t *testing.T) {
	decl := map[string]*schema.ArgDef{"status": {Type: "string"}}
	actual := map[string]any{
		"stdout": "", "stderr": "", "exit_code": 0,
		"status":      "ok",
		"description": "provider-owned extra", // undeclared string
		"isOutage":    true,                   // undeclared boolean
	}
	got, err := enforceOutputContractPolicy("icm", "get-incident", decl, actual, true)
	if err != nil {
		t.Fatalf("ignore policy should accept undeclared keys, got: %v", err)
	}
	if got["status"] != "ok" {
		t.Errorf("declared output must survive, got %v", got["status"])
	}
	if _, present := got["description"]; present {
		t.Errorf("undeclared 'description' must be DROPPED, not present in validated output: %v", got)
	}
	if _, present := got["isOutage"]; present {
		t.Errorf("undeclared 'isOutage' must be DROPPED, not present in validated output: %v", got)
	}
}

// (3) ignore does NOT excuse a missing required declared key.
func TestEnforceOutputContractPolicy_Ignore_StillRequiresDeclared(t *testing.T) {
	decl := map[string]*schema.ArgDef{
		"title": {Type: "string"}, // required (not Optional)
	}
	actual := map[string]any{
		"stdout": "", "stderr": "", "exit_code": 0,
		"description": "provider-owned extra", // undeclared; title absent
	}
	_, err := enforceOutputContractPolicy("icm", "get-incident", decl, actual, true)
	if err == nil {
		t.Fatal("ignore must NOT excuse a missing required declared output")
	}
	if !strings.Contains(err.Error(), "missing declared output") {
		t.Errorf("expected 'missing declared output' in error, got: %v", err)
	}
}

// (4) ignore does NOT excuse a declared type mismatch.
func TestEnforceOutputContractPolicy_Ignore_StillEnforcesDeclaredType(t *testing.T) {
	decl := map[string]*schema.ArgDef{"severity": {Type: "int"}}
	actual := map[string]any{
		"stdout": "", "stderr": "", "exit_code": 0,
		"severity":    "not-a-number", // declared type mismatch
		"description": "provider-owned extra",
	}
	_, err := enforceOutputContractPolicy("icm", "get-incident", decl, actual, true)
	if err == nil {
		t.Fatal("ignore must NOT excuse a declared type mismatch")
	}
	if !strings.Contains(err.Error(), "output contract violation") {
		t.Errorf("expected 'output contract violation' in error, got: %v", err)
	}
}

// (6) Process channels are unchanged under the ignore policy.
func TestEnforceOutputContractPolicy_Ignore_ProcessChannelsUnchanged(t *testing.T) {
	decl := map[string]*schema.ArgDef{"status": {Type: "string"}}
	actual := map[string]any{
		"stdout": "out", "stderr": "err", "exit_code": 3,
		"status":      "ok",
		"description": "provider-owned extra", // undeclared, dropped
	}
	got, err := enforceOutputContractPolicy("icm", "get-incident", decl, actual, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["stdout"] != "out" || got["stderr"] != "err" || got["exit_code"] != 3 {
		t.Errorf("process channels must pass through unchanged, got %v", got)
	}
	if _, present := got["description"]; present {
		t.Errorf("undeclared key must still be dropped alongside intact process channels: %v", got)
	}
}

// ─── Execute-path integration (policy wired through ToolExecutor) ─────────────

func contractDefWithPolicy(toolName string, outputs map[string]*schema.ArgDef, policy *schema.OutputContract) *tool.ToolDef {
	return &tool.ToolDef{
		Name: toolName,
		Actions: map[string]*tool.ToolAction{
			"run": {Outputs: outputs, OutputContract: policy},
		},
	}
}

// Execute-path proof: with additional_outputs: ignore, a runtime result that
// carries undeclared provider keys completes AND those keys are absent from
// the step's validated Output (so nothing downstream can capture them).
func TestToolExecutor_OutputContract_IgnorePolicy_DropsUndeclared(t *testing.T) {
	runtime := testutil.NewFakeToolRuntime()
	runtime.RegisterDef("icm", contractDefWithPolicy("icm",
		map[string]*schema.ArgDef{"title": {Type: "string"}},
		&schema.OutputContract{AdditionalOutputs: schema.AdditionalOutputsIgnore},
	))
	runtime.RegisterResult("icm", "run", &tool.ToolResult{
		Output: map[string]any{
			"title":       "incident",
			"description": "provider-owned extra",
			"isOutage":    true,
		},
	})

	exec := NewToolExecutor(runtime, nil)
	res, err := exec.Execute(context.Background(), makeContractStep("icm"), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != engine.StepStatusCompleted {
		t.Fatalf("expected completed with ignore policy, got %s: %v", res.Status, res.Error)
	}
	if res.Output["title"] != "incident" {
		t.Errorf("declared output must survive, got %v", res.Output["title"])
	}
	if _, present := res.Output["description"]; present {
		t.Errorf("undeclared 'description' must be dropped from step Output: %v", res.Output)
	}
	if _, present := res.Output["isOutage"]; present {
		t.Errorf("undeclared 'isOutage' must be dropped from step Output: %v", res.Output)
	}
}

// Execute-path negative control: the SAME undeclared payload without the
// policy still fails the step, proving the policy is what relaxes acceptance.
func TestToolExecutor_OutputContract_NoPolicy_RejectsUndeclared(t *testing.T) {
	runtime := testutil.NewFakeToolRuntime()
	runtime.RegisterDef("icm", contractDefWithPolicy("icm",
		map[string]*schema.ArgDef{"title": {Type: "string"}},
		nil, // no policy → strict
	))
	runtime.RegisterResult("icm", "run", &tool.ToolResult{
		Output: map[string]any{"title": "incident", "description": "provider-owned extra"},
	})

	exec := NewToolExecutor(runtime, nil)
	res, err := exec.Execute(context.Background(), makeContractStep("icm"), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != engine.StepStatusFailed {
		t.Fatalf("NEGATIVE CONTROL FAILED: expected strict rejection without policy, got %s", res.Status)
	}
	if !strings.Contains(res.Error.Error(), "undeclared output") {
		t.Errorf("expected 'undeclared output' in error, got: %v", res.Error)
	}
}
