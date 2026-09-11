package tool

import (
	"strings"
	"testing"
)

// ─── validateActionClassifications tests ───────────────────────────────────
//
// These tests verify the parse-gate that rejects explicit, unknown
// classification values on tool actions (see validateActionClassifications in
// scan.go, called from ParseToolFile). The four valid values are:
// "read-only", "mutating", "destructive", "unspecified".
//
// Absent (nil) is always valid — nil means unspecified and is ORTHOGONAL to
// requires-approval (GOV-009/GOV-010 invariant: requires-approval:false does
// NOT imply classification:read-only and vice versa).
//
// An explicit empty string is also invalid — the pointer type exists
// precisely so that nil (absent) is distinguishable from an authored empty
// value, and an authored empty value is a mistake.

const classificationToolHeader = `apiVersion: yawr.tool/v1
meta:
  name: probe
  version: "1.0.0"
transport:
  mode: stdio
actions:
`

func parseClassificationTool(t *testing.T, actionYAML string) error {
	t.Helper()
	_, err := parseToolFileFromString(t, classificationToolHeader+actionYAML)
	return err
}

func TestClassification_ValidValues_AllAccepted(t *testing.T) {
	valid := []string{"read-only", "mutating", "destructive", "unspecified"}
	for _, v := range valid {
		t.Run(v, func(t *testing.T) {
			yaml := "  - name: act\n    classification: " + `"` + v + `"` + "\n    argv: [\"echo\"]\n    args: {}\n"
			if err := parseClassificationTool(t, yaml); err != nil {
				t.Errorf("classification %q rejected unexpectedly: %v", v, err)
			}
		})
	}
}

func TestClassification_Absent_Valid(t *testing.T) {
	// Absent classification (nil) must be accepted — nil means unspecified.
	yaml := "  - name: act\n    argv: [\"echo\"]\n    args: {}\n"
	if err := parseClassificationTool(t, yaml); err != nil {
		t.Errorf("absent classification rejected unexpectedly: %v", err)
	}
}

func TestClassification_UnknownValue_Rejected(t *testing.T) {
	// An unknown string value must be rejected with a clear error.
	yaml := "  - name: act\n    classification: \"invalid-class\"\n    argv: [\"echo\"]\n    args: {}\n"
	err := parseClassificationTool(t, yaml)
	if err == nil {
		t.Fatal("expected error for unknown classification value, got nil")
	}
	if !strings.Contains(err.Error(), "classification") {
		t.Errorf("error does not mention 'classification': %v", err)
	}
}

func TestClassification_EmptyString_Rejected(t *testing.T) {
	// An explicit empty string must be rejected — the pointer exists so nil
	// (absent) is distinguishable from an authored empty value.
	yaml := "  - name: act\n    classification: \"\"\n    argv: [\"echo\"]\n    args: {}\n"
	err := parseClassificationTool(t, yaml)
	if err == nil {
		t.Fatal("expected error for explicit empty classification, got nil")
	}
}

func TestClassification_Orthogonal_RequiresApprovalFalse_Destructive(t *testing.T) {
	// ORTHOGONALITY INVARIANT (GOV-009): requires-approval: false must never
	// imply or coerce classification to read-only. A tool with explicit
	// requires-approval: false and classification: destructive must be valid.
	yaml := `apiVersion: yawr.tool/v1
meta:
  name: probe
  version: "1.0.0"
transport:
  mode: stdio
governance:
  requires-approval: false
actions:
  - name: act
    classification: "destructive"
    argv: ["echo"]
    args: {}
`
	_, err := parseToolFileFromString(t, yaml)
	if err != nil {
		t.Errorf("requires-approval:false + destructive incorrectly rejected: %v", err)
	}
}

func TestClassification_Orthogonal_RequiresApprovalFalse_Mutating(t *testing.T) {
	// ORTHOGONALITY INVARIANT (GOV-010): same as above for mutating.
	yaml := `apiVersion: yawr.tool/v1
meta:
  name: probe
  version: "1.0.0"
transport:
  mode: stdio
governance:
  requires-approval: false
actions:
  - name: act
    classification: "mutating"
    argv: ["echo"]
    args: {}
`
	_, err := parseToolFileFromString(t, yaml)
	if err != nil {
		t.Errorf("requires-approval:false + mutating incorrectly rejected: %v", err)
	}
}
