package parser

import (
	"context"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
)

func mustParser(t *testing.T) *impl {
	t.Helper()
	p, err := New(platform.Real())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p.(*impl)
}

func parseEnumCase(t *testing.T, yamlSrc string) error {
	t.Helper()
	p := mustParser(t)
	_, err := p.ParseBytes(context.Background(), []byte(yamlSrc))
	return err
}

const enumFlowTail = `
flow:
  - step:
      id: end
      type: end
      outcome: {category: success, code: ok}
`

func TestEnumDecl_NonStringType_ENUM001(t *testing.T) {
	src := `apiVersion: yawr.runbook/v1
id: r
name: r
inputs:
  env_name:
    type: number
    required: false
    enum: ["1", "2"]
` + enumFlowTail
	err := parseEnumCase(t, src)
	if err == nil || !strings.Contains(err.Error(), "ENUM-001") {
		t.Fatalf("expected ENUM-001, got %v", err)
	}
}

func TestEnumDecl_Secret_ENUM001(t *testing.T) {
	src := `apiVersion: yawr.runbook/v1
id: r
name: r
inputs:
  api_key:
    type: secret
    required: false
    enum: ["tokenA", "tokenB"]
` + enumFlowTail
	err := parseEnumCase(t, src)
	if err == nil || !strings.Contains(err.Error(), "ENUM-001") {
		t.Fatalf("expected ENUM-001, got %v", err)
	}
}

func TestEnumDecl_MappingNotSequence_ENUM002(t *testing.T) {
	src := `apiVersion: yawr.runbook/v1
id: r
name: r
inputs:
  env_name:
    type: string
    required: false
    enum: {a: 1, b: 2}
` + enumFlowTail
	err := parseEnumCase(t, src)
	if err == nil || !strings.Contains(err.Error(), "ENUM-002") {
		t.Fatalf("expected ENUM-002, got %v", err)
	}
}

func TestEnumDecl_Empty_ENUM002(t *testing.T) {
	src := `apiVersion: yawr.runbook/v1
id: r
name: r
inputs:
  env_name:
    type: string
    required: false
    enum: []
` + enumFlowTail
	err := parseEnumCase(t, src)
	if err == nil || !strings.Contains(err.Error(), "ENUM-002") {
		t.Fatalf("expected ENUM-002, got %v", err)
	}
}

func TestEnumDecl_NestedSequence_ENUM002(t *testing.T) {
	src := `apiVersion: yawr.runbook/v1
id: r
name: r
inputs:
  env_name:
    type: string
    required: false
    enum: [["prod"], "stage"]
` + enumFlowTail
	err := parseEnumCase(t, src)
	if err == nil || !strings.Contains(err.Error(), "ENUM-002") {
		t.Fatalf("expected ENUM-002, got %v", err)
	}
}

func TestEnumDecl_ExplicitTag_ENUM002(t *testing.T) {
	src := `apiVersion: yawr.runbook/v1
id: r
name: r
inputs:
  env_name:
    type: string
    required: false
    enum: [!!int "1", "2"]
` + enumFlowTail
	err := parseEnumCase(t, src)
	if err == nil || !strings.Contains(err.Error(), "ENUM-002") {
		t.Fatalf("expected ENUM-002, got %v", err)
	}
}

func TestEnumDecl_EmptyMember_ENUM003(t *testing.T) {
	src := `apiVersion: yawr.runbook/v1
id: r
name: r
inputs:
  env_name:
    type: string
    required: false
    enum: ["prod", ""]
` + enumFlowTail
	err := parseEnumCase(t, src)
	if err == nil || !strings.Contains(err.Error(), "ENUM-003") {
		t.Fatalf("expected ENUM-003, got %v", err)
	}
}

func TestEnumDecl_WhitespaceMember_ENUM003(t *testing.T) {
	src := `apiVersion: yawr.runbook/v1
id: r
name: r
inputs:
  env_name:
    type: string
    required: false
    enum: ["prod", "   "]
` + enumFlowTail
	err := parseEnumCase(t, src)
	if err == nil || !strings.Contains(err.Error(), "ENUM-003") {
		t.Fatalf("expected ENUM-003, got %v", err)
	}
}

func TestEnumDecl_LeadingWhitespaceMember_ENUM003(t *testing.T) {
	src := `apiVersion: yawr.runbook/v1
id: r
name: r
inputs:
  env_name:
    type: string
    required: false
    enum: [" prod", "stage"]
` + enumFlowTail
	err := parseEnumCase(t, src)
	if err == nil || !strings.Contains(err.Error(), "ENUM-003") {
		t.Fatalf("expected ENUM-003, got %v", err)
	}
}

func TestEnumDecl_ExactDuplicate_ENUM004(t *testing.T) {
	src := `apiVersion: yawr.runbook/v1
id: r
name: r
inputs:
  env_name:
    type: string
    required: false
    enum: ["prod", "stage", "prod"]
` + enumFlowTail
	err := parseEnumCase(t, src)
	if err == nil || !strings.Contains(err.Error(), "ENUM-004") {
		t.Fatalf("expected ENUM-004, got %v", err)
	}
}

func TestEnumDecl_CaseOnlyDistinct_ENUMW001Warning(t *testing.T) {
	src := `apiVersion: yawr.runbook/v1
id: r
name: r
inputs:
  env_name:
    type: string
    required: false
    default: prod
    enum: ["Prod", "prod"]
` + enumFlowTail
	p := mustParser(t)
	pr, err := p.ParseBytes(context.Background(), []byte(src))
	if err != nil {
		t.Fatalf("expected parse success (warning only), got error: %v", err)
	}
	found := false
	for _, w := range pr.Warnings {
		if strings.Contains(w.Message, "ENUM-W001") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected ENUM-W001 warning, got %+v", pr.Warnings)
	}
}

func TestEnumDecl_OutputNonStringType_ENUM001(t *testing.T) {
	src := `apiVersion: yawr.runbook/v1
id: r
name: r
outputs:
  drain_state:
    type: integer
    enum: ["1", "2"]
flow:
  - step:
      id: drain
      type: cli
      command: bash
      args: ["-c", "echo drained"]
      capture:
        drain: stdout
  - step:
      id: end
      type: end
      outcome: {category: success, code: ok}
`
	err := parseEnumCase(t, src)
	if err == nil || !strings.Contains(err.Error(), "ENUM-001") {
		t.Fatalf("expected ENUM-001, got %v", err)
	}
}

func TestEnumDecl_NotAlreadyNFC_ENUM005(t *testing.T) {
	src := "apiVersion: yawr.runbook/v1\nid: r\nname: r\ninputs:\n  city:\n    type: string\n    required: false\n    enum: [\"cafe\\u0301\", \"stage\"]\n" + enumFlowTail
	err := parseEnumCase(t, src)
	if err == nil || !strings.Contains(err.Error(), "ENUM-005") {
		t.Fatalf("expected ENUM-005, got %v", err)
	}
}

func TestEnumDecl_BidiControl_ENUM005(t *testing.T) {
	src := "apiVersion: yawr.runbook/v1\nid: r\nname: r\ninputs:\n  env_name:\n    type: string\n    required: false\n    enum: [\"prod\\u202E\", \"stage\"]\n" + enumFlowTail
	err := parseEnumCase(t, src)
	if err == nil || !strings.Contains(err.Error(), "ENUM-005") {
		t.Fatalf("expected ENUM-005, got %v", err)
	}
}

func TestEnumDecl_ValidEnum_NoError(t *testing.T) {
	src := `apiVersion: yawr.runbook/v1
id: r
name: r
inputs:
  env_name:
    type: string
    required: false
    default: prod
    enum: ["prod", "stage"]
` + enumFlowTail
	err := parseEnumCase(t, src)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestEnumDecl_UnconstrainedString_Unaffected(t *testing.T) {
	src := `apiVersion: yawr.runbook/v1
id: r
name: r
inputs:
  env_name:
    type: string
    required: false
` + enumFlowTail
	err := parseEnumCase(t, src)
	if err != nil {
		t.Fatalf("expected no error for unconstrained string, got %v", err)
	}
}
