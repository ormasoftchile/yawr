package tool

import (
	"strings"
	"testing"
)

// ─── validateActionOutputContracts tests ───────────────────────────────────
//
// Parse-gate for the OPT-IN output_contract.additional_outputs policy
// (validateActionOutputContracts in scan.go, called from ParseToolFile).
// Only "ignore" (or an absent field/block) is valid; anything else is a hard
// parse error, never a silent runtime fallback — a typo must not accidentally
// relax output enforcement.

const outputContractToolHeader = `apiVersion: yawr.tool/v1
meta:
  name: probe
  version: "1.0.0"
transport:
  mode: stdio
actions:
`

func parseOutputContractTool(t *testing.T, actionYAML string) error {
	t.Helper()
	_, err := parseToolFileFromString(t, outputContractToolHeader+actionYAML)
	return err
}

func TestOutputContract_Absent_Valid(t *testing.T) {
	yaml := "  - name: act\n    argv: [\"echo\"]\n    args: {}\n"
	if err := parseOutputContractTool(t, yaml); err != nil {
		t.Errorf("absent output_contract rejected unexpectedly: %v", err)
	}
}

func TestOutputContract_Ignore_Valid(t *testing.T) {
	yaml := "  - name: act\n    argv: [\"echo\"]\n    args: {}\n" +
		"    output_contract:\n      additional_outputs: ignore\n"
	if err := parseOutputContractTool(t, yaml); err != nil {
		t.Errorf("additional_outputs: ignore rejected unexpectedly: %v", err)
	}
}

func TestOutputContract_UnknownValue_Rejected(t *testing.T) {
	yaml := "  - name: act\n    argv: [\"echo\"]\n    args: {}\n" +
		"    output_contract:\n      additional_outputs: allow\n"
	err := parseOutputContractTool(t, yaml)
	if err == nil {
		t.Fatal("expected error for unknown additional_outputs value, got nil")
	}
	if !strings.Contains(err.Error(), "additional_outputs") {
		t.Errorf("error does not mention 'additional_outputs': %v", err)
	}
}

func TestOutputContract_EmptyString_Valid(t *testing.T) {
	// An explicit empty string is the strict default (same as absent) and is
	// accepted; it does not relax enforcement.
	yaml := "  - name: act\n    argv: [\"echo\"]\n    args: {}\n" +
		"    output_contract:\n      additional_outputs: \"\"\n"
	if err := parseOutputContractTool(t, yaml); err != nil {
		t.Errorf("explicit empty additional_outputs rejected unexpectedly: %v", err)
	}
}
