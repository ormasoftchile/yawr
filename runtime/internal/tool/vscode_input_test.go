package tool

import (
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

// makeTestAction builds a minimal ToolAction with VSCodeInput for testing.
func makeTestAction(argNames []string, input map[string]*schema.VSCodeInputMapping) *toolpkg.ToolAction {
	args := make(map[string]*toolpkg.ArgDef, len(argNames))
	for _, name := range argNames {
		args[name] = &toolpkg.ArgDef{Type: "string"}
	}
	return &toolpkg.ToolAction{
		Args:        args,
		VSCodeInput: input,
	}
}

func TestVSCodeInputAdaptation_UsesDeclaredNamesWithoutMapping(t *testing.T) {
	action := &toolpkg.ToolAction{VSCodeInput: nil}
	args := map[string]any{"incident_id": "42"}
	got, err := applyVSCodeInputAdaptation(action, args)
	if err != nil || got["incident_id"] != "42" {
		t.Fatalf("declared argument names were not retained: %v, %v", got, err)
	}
}

// TestVSCodeInputAdaptation_BasicMapping verifies a simple logical→provider rename.
func TestVSCodeInputAdaptation_BasicMapping(t *testing.T) {
	action := makeTestAction([]string{"incident_id"}, map[string]*schema.VSCodeInputMapping{
		"incidentId": {From: "incident_id"},
	})
	got, err := applyVSCodeInputAdaptation(action, map[string]any{"incident_id": "99"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["incidentId"] != "99" {
		t.Errorf("expected incidentId=99; got %v", got)
	}
	if _, ok := got["incident_id"]; ok {
		t.Error("logical key incident_id must not appear in adapted args")
	}
}

// TestVSCodeInputAdaptation_CoerceIntegerFromString verifies "7" → int64(7).
func TestVSCodeInputAdaptation_CoerceIntegerFromString(t *testing.T) {
	action := makeTestAction([]string{"incident_id"}, map[string]*schema.VSCodeInputMapping{
		"incidentId": {From: "incident_id", Coerce: "integer"},
	})
	got, err := applyVSCodeInputAdaptation(action, map[string]any{"incident_id": "7"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["incidentId"] != int64(7) {
		t.Errorf("expected int64(7), got %T %v", got["incidentId"], got["incidentId"])
	}
}

// TestVSCodeInputAdaptation_CoerceIntegerFromFloat64Lossless verifies
// float64(7.0) → int64(7) (lossless).
func TestVSCodeInputAdaptation_CoerceIntegerFromFloat64Lossless(t *testing.T) {
	action := makeTestAction([]string{"n"}, map[string]*schema.VSCodeInputMapping{
		"nParam": {From: "n", Coerce: "integer"},
	})
	got, err := applyVSCodeInputAdaptation(action, map[string]any{"n": float64(7.0)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["nParam"] != int64(7) {
		t.Errorf("expected int64(7), got %T %v", got["nParam"], got["nParam"])
	}
}

// TestVSCodeInputAdaptation_CoerceIntegerLossy verifies "12.5" → error.
func TestVSCodeInputAdaptation_CoerceIntegerLossy(t *testing.T) {
	action := makeTestAction([]string{"incident_id"}, map[string]*schema.VSCodeInputMapping{
		"incidentId": {From: "incident_id", Coerce: "integer"},
	})
	_, err := applyVSCodeInputAdaptation(action, map[string]any{"incident_id": "12.5"})
	if err == nil {
		t.Fatal("expected error for lossy coerce: '12.5' is not an integer")
	}
	if !strings.Contains(err.Error(), "cannot be converted to integer") {
		t.Errorf("error should mention 'cannot be converted to integer', got: %v", err)
	}
}

// TestVSCodeInputAdaptation_CoerceIntegerLossyFloat verifies 12.5 (float64) → error.
func TestVSCodeInputAdaptation_CoerceIntegerLossyFloat(t *testing.T) {
	action := makeTestAction([]string{"n"}, map[string]*schema.VSCodeInputMapping{
		"nParam": {From: "n", Coerce: "integer"},
	})
	_, err := applyVSCodeInputAdaptation(action, map[string]any{"n": float64(12.5)})
	if err == nil {
		t.Fatal("expected error for lossy float64→integer coerce")
	}
	if !strings.Contains(err.Error(), "not losslessly representable") {
		t.Errorf("error should mention lossless, got: %v", err)
	}
}

// TestVSCodeInputAdaptation_CoerceIntegerEmptyString verifies "" → error.
func TestVSCodeInputAdaptation_CoerceIntegerEmptyString(t *testing.T) {
	action := makeTestAction([]string{"n"}, map[string]*schema.VSCodeInputMapping{
		"nParam": {From: "n", Coerce: "integer"},
	})
	_, err := applyVSCodeInputAdaptation(action, map[string]any{"n": ""})
	if err == nil {
		t.Fatal("expected error for empty string → integer coerce")
	}
	if !strings.Contains(err.Error(), "empty string") {
		t.Errorf("error should mention empty string, got: %v", err)
	}
}

// TestVSCodeInputAdaptation_RequiredAbsent verifies required:true + absent → error.
func TestVSCodeInputAdaptation_RequiredAbsent(t *testing.T) {
	action := makeTestAction([]string{"incident_id"}, map[string]*schema.VSCodeInputMapping{
		"incidentId": {From: "incident_id", Required: true},
	})
	_, err := applyVSCodeInputAdaptation(action, map[string]any{})
	if err == nil {
		t.Fatal("expected error: required arg is absent")
	}
	if !strings.Contains(err.Error(), "required") {
		t.Errorf("error should mention 'required', got: %v", err)
	}
}

// TestVSCodeInputAdaptation_OptionalAbsent verifies optional + absent → no error, no key.
func TestVSCodeInputAdaptation_OptionalAbsent(t *testing.T) {
	action := makeTestAction([]string{"incident_id"}, map[string]*schema.VSCodeInputMapping{
		"incidentId": {From: "incident_id", Required: false},
	})
	got, err := applyVSCodeInputAdaptation(action, map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error for optional absent arg: %v", err)
	}
	if _, ok := got["incidentId"]; ok {
		t.Error("absent optional arg should not appear in adapted args")
	}
}

// TestVSCodeInputAdaptation_UnmappedArg verifies that a logical arg not
// referenced by any mapping → error (fail-closed: do not silently drop).
func TestVSCodeInputAdaptation_UnmappedArg(t *testing.T) {
	action := makeTestAction([]string{"incident_id", "region"}, map[string]*schema.VSCodeInputMapping{
		"incidentId": {From: "incident_id"},
		// "region" is declared but not in VSCodeInput mapping
	})
	_, err := applyVSCodeInputAdaptation(action, map[string]any{
		"incident_id": "42",
		"region":      "eastus", // supplied but unmapped
	})
	if err == nil {
		t.Fatal("expected error: 'region' is supplied but not mapped in vscode_input")
	}
	if !strings.Contains(err.Error(), "not mapped") {
		t.Errorf("error should mention 'not mapped', got: %v", err)
	}
}

// TestValidateVSCodeInputActions_FromNonExistentArg verifies that from: naming
// a non-declared arg fails at scan time.
func TestValidateVSCodeInputActions_FromNonExistentArg(t *testing.T) {
	transport := schema.TransportConfig{Type: schema.TransportVSCodeMCP}
	actions := map[string]*schema.ToolAction{
		"get-incident": {
			Args: map[string]*schema.ArgDef{
				"incident_id": {Type: "string"},
			},
			VSCodeInput: map[string]*schema.VSCodeInputMapping{
				"incidentId": {From: "nonexistent_arg", Coerce: "integer"},
			},
		},
	}
	err := ValidateVSCodeInputActions(transport, actions)
	if err == nil {
		t.Fatal("expected validation error for from: naming undeclared arg")
	}
	if !strings.Contains(err.Error(), "not a declared arg") {
		t.Errorf("error should mention 'not a declared arg', got: %v", err)
	}
}

// TestValidateVSCodeInputActions_WrongTransport verifies that vscode_input on a
// non-vscode-mcp transport fails at scan time (VSCODE-002).
func TestValidateVSCodeInputActions_WrongTransport(t *testing.T) {
	transport := schema.TransportConfig{Type: schema.TransportMCPHTTP}
	actions := map[string]*schema.ToolAction{
		"get-incident": {
			Args: map[string]*schema.ArgDef{
				"incident_id": {Type: "string"},
			},
			VSCodeInput: map[string]*schema.VSCodeInputMapping{
				"incidentId": {From: "incident_id"},
			},
		},
	}
	err := ValidateVSCodeInputActions(transport, actions)
	if err == nil {
		t.Fatal("expected VSCODE-002 error for vscode_input on non-vscode-mcp transport")
	}
	if !strings.Contains(err.Error(), "VSCODE-002") {
		t.Errorf("error should contain VSCODE-002, got: %v", err)
	}
}

// TestValidateVSCodeInputActions_BadCoerce verifies that an unknown coerce type
// fails at scan time.
func TestValidateVSCodeInputActions_BadCoerce(t *testing.T) {
	transport := schema.TransportConfig{Type: schema.TransportVSCodeMCP}
	actions := map[string]*schema.ToolAction{
		"get-incident": {
			Args: map[string]*schema.ArgDef{
				"incident_id": {Type: "string"},
			},
			VSCodeInput: map[string]*schema.VSCodeInputMapping{
				"incidentId": {From: "incident_id", Coerce: "hexadecimal"},
			},
		},
	}
	err := ValidateVSCodeInputActions(transport, actions)
	if err == nil {
		t.Fatal("expected error for unknown coerce type 'hexadecimal'")
	}
	if !strings.Contains(err.Error(), "not a supported coercion type") {
		t.Errorf("error should mention 'not a supported coercion type', got: %v", err)
	}
}

func TestVSCodeInputAdaptation_NilActionDoesNotInventMapping(t *testing.T) {
	args := map[string]any{"x": "y"}
	got, err := applyVSCodeInputAdaptation(nil, args)
	if err != nil || got["x"] != "y" {
		t.Fatalf("unexpected adaptation: %v, %v", got, err)
	}
}

func TestValidateToolArgumentsRejectsUnknownAndMissing(t *testing.T) {
	action := &toolpkg.ToolAction{Args: map[string]*toolpkg.ArgDef{
		"required": {Required: true},
	}}
	if err := validateToolArguments(action, map[string]any{"obsolete": true}); err == nil || !strings.Contains(err.Error(), "not declared") {
		t.Fatalf("unknown argument was not rejected: %v", err)
	}
	if err := validateToolArguments(action, map[string]any{}); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("missing required argument was not rejected: %v", err)
	}
}
