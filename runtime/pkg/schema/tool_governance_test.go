package schema

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// TestRequiresApprovalValues verifies the current YAML representation.
//
// Cases:
//
//	(a) governance block absent                       → nil
//	(b) governance present, other fields, no requires-approval → nil  ← the counterparty catch
//	(c) explicit requires-approval: true              → &true
func TestRequiresApprovalValues(t *testing.T) {
	t.Run("a_governance_absent", func(t *testing.T) {
		input := `
name: test-tool
transport:
  mode: mcp
  command: /usr/bin/test
actions: []
`
		var def ToolDef
		if err := yaml.Unmarshal([]byte(input), &def); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if def.Governance != nil {
			t.Fatalf("expected Governance == nil, got %+v", def.Governance)
		}
	})

	t.Run("b_governance_present_no_requires_approval", func(t *testing.T) {
		// governance block exists with requires-capabilities but NO requires-approval.
		// This is THE case the counterparty caught: before the *bool change,
		// absence and explicit-false were indistinguishable.
		input := `
name: test-tool
transport:
  mode: mcp
  command: /usr/bin/test
governance:
  requires-capabilities:
    - network
actions: []
`
		var def ToolDef
		if err := yaml.Unmarshal([]byte(input), &def); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if def.Governance == nil {
			t.Fatal("expected Governance != nil")
		}
		if def.Governance.RequiresApproval != nil {
			t.Fatalf("expected RequiresApproval == nil (unspecified), got %v", *def.Governance.RequiresApproval)
		}
	})

	t.Run("c_explicit_true", func(t *testing.T) {
		input := `
name: test-tool
transport:
  mode: mcp
  command: /usr/bin/test
governance:
  requires-approval: true
actions: []
`
		var def ToolDef
		if err := yaml.Unmarshal([]byte(input), &def); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if def.Governance == nil {
			t.Fatal("expected Governance != nil")
		}
		if def.Governance.RequiresApproval == nil {
			t.Fatal("expected RequiresApproval != nil, got nil")
		}
		if *def.Governance.RequiresApproval != true {
			t.Fatalf("expected *RequiresApproval == true, got %v", *def.Governance.RequiresApproval)
		}
	})
}
