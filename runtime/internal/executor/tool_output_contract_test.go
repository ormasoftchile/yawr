package executor

// tool_output_contract_test.go — non-vacuous tests for enforceOutputContract
// and its wiring inside ToolExecutor.Execute.
//
// Coverage:
//  1. Pure-function tests for enforceOutputContract directly (happy/negative paths).
//  2. Execute-path integration tests that prove the enforcement call is wired
//     (mutation controls): removing the enforceOutputContract block from Execute
//     causes each "Fail" test to pass instead of fail — empirically verified.
//
// Mutation-control contract: every test named *_Fail fails if the
// enforceOutputContract call is removed from Execute. The *_Pass tests
// continue to pass in both states (they guard against over-rejection).

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

func TestEnforceOutputContract_NilDeclarations_PassThrough(t *testing.T) {
	actual := map[string]any{"stdout": "hi", "exit_code": 0, "result": "ok"}
	got, err := enforceOutputContract("tool", "action", nil, actual)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["result"] != "ok" {
		t.Errorf("expected result pass-through, got %v", got)
	}
}

func TestEnforceOutputContract_EmptyDeclarations_PassThrough(t *testing.T) {
	actual := map[string]any{"stdout": "hi", "extra": "ok"}
	got, err := enforceOutputContract("tool", "action", map[string]*schema.ArgDef{}, actual)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["extra"] != "ok" {
		t.Errorf("expected extra pass-through, got %v", got)
	}
}

func TestEnforceOutputContract_AllSatisfied_Pass(t *testing.T) {
	decl := map[string]*schema.ArgDef{
		"status": {Type: "string"},
		"count":  {Type: "int"},
	}
	actual := map[string]any{
		"stdout":    "out",
		"stderr":    "",
		"exit_code": 0,
		"status":    "ok",
		"count":     42,
	}
	got, err := enforceOutputContract("my-tool", "run", decl, actual)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["status"] != "ok" {
		t.Errorf("expected status=ok, got %v", got["status"])
	}
	if got["count"] != 42 {
		t.Errorf("expected count=42, got %v", got["count"])
	}
	// Process channels must always be present.
	if got["stdout"] != "out" {
		t.Errorf("stdout must pass through, got %v", got["stdout"])
	}
	if got["exit_code"] != 0 {
		t.Errorf("exit_code must pass through, got %v", got["exit_code"])
	}
}

func TestEnforceOutputContract_MissingDeclaredOutput_Fail(t *testing.T) {
	decl := map[string]*schema.ArgDef{
		"status": {Type: "string"},
	}
	actual := map[string]any{
		"stdout":    "",
		"stderr":    "",
		"exit_code": 0,
		// "status" is absent
	}
	_, err := enforceOutputContract("my-tool", "run", decl, actual)
	if err == nil {
		t.Fatal("NEGATIVE CONTROL FAILED: expected error for missing declared output, got nil")
	}
	if !strings.Contains(err.Error(), "missing declared output") {
		t.Errorf("expected 'missing declared output' in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "my-tool#run") {
		t.Errorf("expected tool#action in error, got: %v", err)
	}
}

func TestEnforceOutputContract_UndeclaredOutput_Fail(t *testing.T) {
	decl := map[string]*schema.ArgDef{
		"status": {Type: "string"},
	}
	actual := map[string]any{
		"stdout":    "",
		"stderr":    "",
		"exit_code": 0,
		"status":    "ok",
		"extra":     "surprise", // undeclared
	}
	_, err := enforceOutputContract("my-tool", "run", decl, actual)
	if err == nil {
		t.Fatal("NEGATIVE CONTROL FAILED: expected error for undeclared output, got nil")
	}
	if !strings.Contains(err.Error(), "undeclared output") {
		t.Errorf("expected 'undeclared output' in error, got: %v", err)
	}
}

func TestEnforceOutputContract_TypeMismatch_Fail(t *testing.T) {
	decl := map[string]*schema.ArgDef{
		"count": {Type: "int"},
	}
	actual := map[string]any{
		"stdout":    "",
		"stderr":    "",
		"exit_code": 0,
		"count":     "not-a-number", // string, not int
	}
	_, err := enforceOutputContract("my-tool", "run", decl, actual)
	if err == nil {
		t.Fatal("NEGATIVE CONTROL FAILED: expected error for type mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "output contract violation") {
		t.Errorf("expected 'output contract violation' in error, got: %v", err)
	}
}

func TestEnforceOutputContract_ProcessChannels_NeverFlagged(t *testing.T) {
	// stdout, stderr, and exit_code must never cause "undeclared output" even
	// when they are not in the declared outputs: block.
	decl := map[string]*schema.ArgDef{
		"result": {Type: "string"},
	}
	actual := map[string]any{
		"stdout":    "some output",
		"stderr":    "some errors",
		"exit_code": 0,
		"result":    "ok",
	}
	got, err := enforceOutputContract("my-tool", "run", decl, actual)
	if err != nil {
		t.Fatalf("process channels must not cause enforcement error: %v", err)
	}
	for _, ch := range []string{"stdout", "stderr", "exit_code"} {
		if _, ok := got[ch]; !ok {
			t.Errorf("process channel %q must be present in validated output", ch)
		}
	}
}

func TestEnforceOutputContract_MultipleErrors_AllReported(t *testing.T) {
	// Both a missing output and an undeclared output should be reported together.
	decl := map[string]*schema.ArgDef{
		"required_field": {Type: "string"},
	}
	actual := map[string]any{
		"stdout":    "",
		"stderr":    "",
		"exit_code": 0,
		"wrong_key": "value", // undeclared; required_field is missing
	}
	_, err := enforceOutputContract("my-tool", "run", decl, actual)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "missing declared output") {
		t.Errorf("missing declared output not reported: %v", err)
	}
	if !strings.Contains(err.Error(), "undeclared output") {
		t.Errorf("undeclared output not reported: %v", err)
	}
}

func TestEnforceOutputContract_ProjectNestedOutput_Pass(t *testing.T) {
	decl := map[string]*schema.ArgDef{
		"environment": {Type: "string", From: "occuringLocation.environment"},
		"service":     {Type: "string", From: "owningTenantName"},
	}
	actual := map[string]any{
		"occuringLocation": map[string]any{"environment": "PROD"},
		"owningTenantName": "Azure SQL DB",
	}
	got, err := enforceOutputContract("icm", "get-incident", decl, actual)
	if err != nil {
		t.Fatalf("unexpected projection error: %v", err)
	}
	if got["environment"] != "PROD" || got["service"] != "Azure SQL DB" {
		t.Fatalf("projected outputs = %#v", got)
	}
}

func TestEnforceOutputContract_OptionalMissingDeclaredOutput_Pass(t *testing.T) {
	decl := map[string]*schema.ArgDef{
		"title":          {Type: "string"},
		"database":       {Type: "string", Optional: true},
		"logical_server": {Type: "string", Optional: true},
	}
	actual := map[string]any{"title": "incident"}
	got, err := enforceOutputContract("icm", "get-incident", decl, actual)
	if err != nil {
		t.Fatalf("optional missing outputs should not fail: %v", err)
	}
	if _, ok := got["database"]; ok {
		t.Fatalf("optional missing output database should not be invented: %#v", got)
	}
}

func TestEnforceOutputContract_ProjectKeyedArrayOutput_Pass(t *testing.T) {
	decl := map[string]*schema.ArgDef{
		"database":       {Type: "string", From: "customFields[Name=DatabaseName|CustomerDatabaseName].StringValue"},
		"logical_server": {Type: "string", From: "customFields[Name=ServerName|CustomerServerName].StringValue"},
	}
	actual := map[string]any{
		"customFields": []any{
			map[string]any{"Name": "CustomerServerName", "StringValue": "fallback-server"},
			map[string]any{"Name": "CustomerDatabaseName", "StringValue": "fallback-db"},
			map[string]any{"Name": "ServerName", "StringValue": "server-a"},
			map[string]any{"Name": "DatabaseName", "StringValue": "db-a"},
		},
	}
	got, err := enforceOutputContract("icm", "get-incident", decl, actual)
	if err != nil {
		t.Fatalf("unexpected keyed-array projection error: %v", err)
	}
	if got["database"] != "db-a" || got["logical_server"] != "server-a" {
		t.Fatalf("projected outputs = %#v", got)
	}
}

func TestEnforceOutputContract_ProjectKeyedArrayFallback_Pass(t *testing.T) {
	decl := map[string]*schema.ArgDef{
		"database": {Type: "string", From: "customFields[Name=DatabaseName|CustomerDatabaseName].StringValue"},
	}
	actual := map[string]any{
		"customFields": []any{
			map[string]any{"Name": "CustomerDatabaseName", "StringValue": "fallback-db"},
		},
	}
	got, err := enforceOutputContract("icm", "get-incident", decl, actual)
	if err != nil {
		t.Fatalf("unexpected keyed-array fallback error: %v", err)
	}
	if got["database"] != "fallback-db" {
		t.Fatalf("database = %#v, want fallback-db", got["database"])
	}
}

func TestEnforceOutputContract_RequiredKeyedArrayOutputMissing_Fail(t *testing.T) {
	decl := map[string]*schema.ArgDef{
		"database": {Type: "string", From: "customFields[Name=DatabaseName|CustomerDatabaseName].StringValue"},
	}
	actual := map[string]any{
		"customFields": []any{map[string]any{"Name": "ASC-Link", "StringValue": "NA"}},
	}
	_, err := enforceOutputContract("icm", "get-incident", decl, actual)
	if err == nil {
		t.Fatal("expected required SQL projection to fail when no matching custom field exists")
	}
	if !strings.Contains(err.Error(), "missing declared output \"database\"") {
		t.Fatalf("expected missing database error, got: %v", err)
	}
}

func TestEnforceOutputContract_ICMSQLShapePassesWithoutSummary(t *testing.T) {
	decl := icmRoutingOutputContractForTest()
	actual := map[string]any{
		"occuringLocation": map[string]any{"environment": "STAGING", "datacenter": "Region", "instance": "Stage"},
		"customFields": []any{
			map[string]any{"Name": "CustomerServerName", "StringValue": "fallback-server"},
			map[string]any{"Name": "CustomerDatabaseName", "StringValue": "fallback-db"},
			map[string]any{"Name": "ServerName", "StringValue": "sql-server-sanitized"},
			map[string]any{"Name": "DatabaseName", "StringValue": "sql-db-sanitized"},
		},
		"owningTenantName": "Azure SQL DB",
		"title":            "[STAGE][TSG: SOC036] sanitized SQL livesite incident",
		"monitorId":        "MonitorSanitized",
		"tsgLink":          "https://example.invalid/tsg",
		// summary intentionally omitted: ICM omits incident-class-dependent fields.
	}
	got, err := enforceOutputContract("icm", "get-incident", decl, actual)
	if err != nil {
		t.Fatalf("SQL ICM shape should pass without summary: %v", err)
	}
	if got["database"] != "sql-db-sanitized" || got["logical_server"] != "sql-server-sanitized" || got["environment"] != "STAGING" || got["service"] != "Azure SQL DB" {
		t.Fatalf("projected routing outputs = %#v", got)
	}
}

func TestEnforceOutputContract_ICMSparseNonSQLShapeFailsLoudly(t *testing.T) {
	decl := icmRoutingOutputContractForTest()
	actual := map[string]any{
		"title":   "sanitized non-SQL support ticket",
		"summary": "support-ticket summary",
		"customFields": []any{
			map[string]any{"Name": "ASC-Link", "StringValue": "NA"},
		},
	}
	_, err := enforceOutputContract("icm", "get-incident", decl, actual)
	if err == nil {
		t.Fatal("expected sparse non-SQL ICM shape to fail required routing outputs")
	}
	msg := err.Error()
	for _, want := range []string{"missing declared output \"environment\"", "missing declared output \"service\"", "missing declared output \"logical_server\"", "missing declared output \"database\""} {
		if !strings.Contains(msg, want) {
			t.Fatalf("expected %s in error, got: %v", want, err)
		}
	}
}

func icmRoutingOutputContractForTest() map[string]*schema.ArgDef {
	return map[string]*schema.ArgDef{
		"title":          {Type: "string"},
		"environment":    {Type: "string", From: "occuringLocation.environment"},
		"service":        {Type: "string", From: "owningTenantName"},
		"logical_server": {Type: "string", From: "customFields[Name=ServerName|CustomerServerName].StringValue"},
		"database":       {Type: "string", From: "customFields[Name=DatabaseName|CustomerDatabaseName].StringValue"},
		"summary":        {Type: "string", Optional: true},
		"monitorId":      {Type: "string", Optional: true},
		"tsgLink":        {Type: "string", Optional: true},
		"customFields":   {Type: "array", Optional: true},
	}
}

func TestEnforceOutputContract_ObjectAndArrayOutputs_Pass(t *testing.T) {
	decl := map[string]*schema.ArgDef{
		"occuringLocation": {Type: "object"},
		"tags":             {Type: "array"},
		"customFields":     {Type: "any"},
	}
	actual := map[string]any{
		"occuringLocation": map[string]any{"environment": "PROD"},
		"tags":             []any{"SAW Support"},
		"customFields":     []any{map[string]any{"Name": "ASC-Link"}},
	}
	got, err := enforceOutputContract("icm", "get-incident", decl, actual)
	if err != nil {
		t.Fatalf("object/array outputs should pass: %v", err)
	}
	if _, ok := got["occuringLocation"].(map[string]any); !ok {
		t.Fatalf("occuringLocation lost object shape: %#v", got["occuringLocation"])
	}
	if _, ok := got["tags"].([]any); !ok {
		t.Fatalf("tags lost array shape: %#v", got["tags"])
	}
}

func TestEnforceOutputContract_TypeCoercionFloat_Pass(t *testing.T) {
	// json.Unmarshal decodes integers as float64; coerceOutputAny should handle
	// float64→int coercion so JSON-returning tools with int outputs work.
	decl := map[string]*schema.ArgDef{
		"count": {Type: "int"},
	}
	actual := map[string]any{
		"stdout":    "",
		"stderr":    "",
		"exit_code": 0,
		"count":     float64(7), // json.Unmarshal produces float64
	}
	got, err := enforceOutputContract("my-tool", "run", decl, actual)
	if err != nil {
		t.Fatalf("float64→int coercion failed: %v", err)
	}
	if got["count"] != 7 {
		t.Errorf("expected count=7 (int), got %v (%T)", got["count"], got["count"])
	}
}

// ─── Execute-path mutation controls ──────────────────────────────────────────
//
// These tests exercise the enforcement via ToolExecutor.Execute with a
// FakeToolRuntime that has a RegisterDef (ToolDefLookup) entry. Each failure
// test is a mutation control: if the "if resolvedActionDef != nil { ... }"
// block is removed from Execute, the step succeeds instead of failing and
// the test breaks.

func contractDef(toolName string, outputs map[string]*schema.ArgDef) *tool.ToolDef {
	return &tool.ToolDef{
		Name: toolName,
		Actions: map[string]*tool.ToolAction{
			"run": {Outputs: outputs},
		},
	}
}

func makeContractStep(toolName string) engine.ResolvedStep {
	return engine.ResolvedStep{
		ID:   "step-1",
		Kind: "tool",
		Spec: &schema.ToolCallSpec{Tool: schema.ToolInvocation{Name: toolName, Action: "run"}},
	}
}

// TestToolExecutor_OutputContract_Satisfied_Pass: declared outputs all
// present with correct types → step completes.
// MUTATION CONTROL: removing enforcement does NOT break this test.
func TestToolExecutor_OutputContract_Satisfied_Pass(t *testing.T) {
	runtime := testutil.NewFakeToolRuntime()
	runtime.RegisterDef("my-tool", contractDef("my-tool", map[string]*schema.ArgDef{
		"status": {Type: "string"},
		"score":  {Type: "int"},
	}))
	runtime.RegisterResult("my-tool", "run", &tool.ToolResult{
		Output: map[string]any{"status": "ok", "score": 42},
	})

	exec := NewToolExecutor(runtime, nil)
	res, err := exec.Execute(context.Background(), makeContractStep("my-tool"), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != engine.StepStatusCompleted {
		t.Fatalf("expected completed, got %s: %v", res.Status, res.Error)
	}
	if res.Output["status"] != "ok" {
		t.Errorf("expected status=ok, got %v", res.Output["status"])
	}
}

// TestToolExecutor_OutputContract_Missing_Fail: declared output not returned
// → step fails. MUTATION CONTROL: fails if enforcement block is removed.
func TestToolExecutor_OutputContract_Missing_Fail(t *testing.T) {
	runtime := testutil.NewFakeToolRuntime()
	runtime.RegisterDef("my-tool", contractDef("my-tool", map[string]*schema.ArgDef{
		"status": {Type: "string"},
	}))
	// Runtime returns nothing in Output — "status" is missing.
	runtime.RegisterResult("my-tool", "run", &tool.ToolResult{Stdout: "done"})

	exec := NewToolExecutor(runtime, nil)
	res, err := exec.Execute(context.Background(), makeContractStep("my-tool"), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != engine.StepStatusFailed {
		t.Fatalf("MUTATION CONTROL FAILED: expected failed status for missing declared output, got %s", res.Status)
	}
	if !strings.Contains(res.Error.Error(), "missing declared output") {
		t.Errorf("expected 'missing declared output' in error, got: %v", res.Error)
	}
}

// TestToolExecutor_OutputContract_Undeclared_Fail: runtime returns a key
// not in the declared contract → step fails.
// MUTATION CONTROL: fails if enforcement block is removed.
func TestToolExecutor_OutputContract_Undeclared_Fail(t *testing.T) {
	runtime := testutil.NewFakeToolRuntime()
	runtime.RegisterDef("my-tool", contractDef("my-tool", map[string]*schema.ArgDef{
		"status": {Type: "string"},
	}))
	runtime.RegisterResult("my-tool", "run", &tool.ToolResult{
		Output: map[string]any{"status": "ok", "extra_key": "surprise"},
	})

	exec := NewToolExecutor(runtime, nil)
	res, err := exec.Execute(context.Background(), makeContractStep("my-tool"), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != engine.StepStatusFailed {
		t.Fatalf("MUTATION CONTROL FAILED: expected failed status for undeclared output, got %s", res.Status)
	}
	if !strings.Contains(res.Error.Error(), "undeclared output") {
		t.Errorf("expected 'undeclared output' in error, got: %v", res.Error)
	}
}

// TestToolExecutor_OutputContract_TypeMismatch_Fail: runtime returns a value
// with incompatible type for declared field → step fails.
// MUTATION CONTROL: fails if enforcement block is removed.
func TestToolExecutor_OutputContract_TypeMismatch_Fail(t *testing.T) {
	runtime := testutil.NewFakeToolRuntime()
	runtime.RegisterDef("my-tool", contractDef("my-tool", map[string]*schema.ArgDef{
		"count": {Type: "int"},
	}))
	runtime.RegisterResult("my-tool", "run", &tool.ToolResult{
		Output: map[string]any{"count": "not-a-number"},
	})

	exec := NewToolExecutor(runtime, nil)
	res, err := exec.Execute(context.Background(), makeContractStep("my-tool"), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != engine.StepStatusFailed {
		t.Fatalf("MUTATION CONTROL FAILED: expected failed status for type mismatch, got %s", res.Status)
	}
	if !strings.Contains(res.Error.Error(), "output contract violation") {
		t.Errorf("expected 'output contract violation' in error, got: %v", res.Error)
	}
}

// TestToolExecutor_OutputContract_NoDeclaration_PassThrough: action has no
// outputs: block → any outputs pass through, step completes.
// MUTATION CONTROL: removing enforcement does NOT break this test.
func TestToolExecutor_OutputContract_NoDeclaration_PassThrough(t *testing.T) {
	runtime := testutil.NewFakeToolRuntime()
	// Registered def with no outputs on the action.
	runtime.RegisterDef("my-tool", contractDef("my-tool", nil))
	runtime.RegisterResult("my-tool", "run", &tool.ToolResult{
		Output: map[string]any{"anything": "goes", "other": "value"},
	})

	exec := NewToolExecutor(runtime, nil)
	res, err := exec.Execute(context.Background(), makeContractStep("my-tool"), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != engine.StepStatusCompleted {
		t.Fatalf("expected completed for unconstrained action, got %s: %v", res.Status, res.Error)
	}
	if res.Output["anything"] != "goes" {
		t.Errorf("expected unconstrained output pass-through, got %v", res.Output)
	}
}

// TestToolExecutor_OutputContract_ProcessChannels_AlwaysPresent: stdout,
// stderr, and exit_code must always be in Output after enforcement, even when
// the action declares outputs: containing only other fields.
// MUTATION CONTROL: removing enforcement does NOT break this test, but
// confirms the carve-out rule is correctly implemented.
func TestToolExecutor_OutputContract_ProcessChannels_AlwaysPresent(t *testing.T) {
	runtime := testutil.NewFakeToolRuntime()
	runtime.RegisterDef("my-tool", contractDef("my-tool", map[string]*schema.ArgDef{
		"result": {Type: "string"},
	}))
	runtime.RegisterResult("my-tool", "run", &tool.ToolResult{
		Stdout:   "raw output",
		Stderr:   "some error",
		ExitCode: 0,
		Output:   map[string]any{"result": "ok"},
	})

	exec := NewToolExecutor(runtime, nil)
	res, err := exec.Execute(context.Background(), makeContractStep("my-tool"), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != engine.StepStatusCompleted {
		t.Fatalf("expected completed, got %s: %v", res.Status, res.Error)
	}
	for _, ch := range []string{"stdout", "stderr", "exit_code"} {
		if _, ok := res.Output[ch]; !ok {
			t.Errorf("process channel %q missing from Output after enforcement", ch)
		}
	}
	if res.Output["stdout"] != "raw output" {
		t.Errorf("stdout mismatch: got %v", res.Output["stdout"])
	}
	if res.Output["result"] != "ok" {
		t.Errorf("declared output result mismatch: got %v", res.Output["result"])
	}
}

// TestToolExecutor_OutputContract_NoDefRegistered_PassThrough: when the
// runtime has no def for the tool (LookupDef returns false), enforcement is
// skipped and all outputs flow through. This covers the majority of existing
// tests that use FakeToolRuntime without RegisterDef.
func TestToolExecutor_OutputContract_NoDefRegistered_PassThrough(t *testing.T) {
	runtime := testutil.NewFakeToolRuntime()
	// No RegisterDef call — LookupDef returns (nil, false).
	runtime.RegisterResult("my-tool", "run", &tool.ToolResult{
		Output: map[string]any{"anything": "value"},
	})

	exec := NewToolExecutor(runtime, nil)
	step := engine.ResolvedStep{
		ID:   "step-1",
		Kind: "tool",
		Spec: &schema.ToolCallSpec{Tool: schema.ToolInvocation{Name: "my-tool", Action: "run"}},
	}
	res, err := exec.Execute(context.Background(), step, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != engine.StepStatusCompleted {
		t.Fatalf("expected completed without def, got %s", res.Status)
	}
	if res.Output["anything"] != "value" {
		t.Errorf("expected pass-through without def, got %v", res.Output)
	}
}
