package graphdoc_test

// Tests for AR-CE-2 (T-CLIENT-ENUM-DTO): Document.Inputs carries declared
// top-level input metadata, with enum in declared order and C1-safe
// enumRedacted/enumMemberCount, never folded into a generic "options"
// shape. Covers CE-D-01..03 from barbara-client-enum-compatibility-
// ruling.md's acceptance matrix (§9).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
)

func buildDocFromYAML(t *testing.T, yamlSrc string) *graphdoc.Document {
	t.Helper()
	p, err := parser.New(platform.Real())
	if err != nil {
		t.Fatalf("parser.New: %v", err)
	}
	rb, err := p.ParseBytes(context.Background(), []byte(yamlSrc))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	rb.Source = "test.runbook.yaml"
	doc, err := (&graphdoc.Builder{}).Build(context.Background(), rb)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return doc
}

const enumRunbookTemplate = `
apiVersion: yawr.runbook/v1
id: test-enum-decl
name: Test Enum Decl
inputs:
  env_name:
    type: string
    required: true
    description: Target environment
    enum: [%s]
  free_text:
    type: string
    required: false
flow:
  - step:
      id: show
      type: display
      title: Show
      content: "env=${env_name}"
`

// CE-D-01: preview document carries inputs[] with enum in declared order.
func TestDocument_InputDecl_EnumDeclaredOrder(t *testing.T) {
	doc := buildDocFromYAML(t, fmtEnumRunbook(`"prod", "staging"`))

	var envDecl *graphdoc.InputDecl
	for i := range doc.Inputs {
		if doc.Inputs[i].Name == "env_name" {
			envDecl = &doc.Inputs[i]
		}
	}
	if envDecl == nil {
		t.Fatal("expected env_name in Document.Inputs")
	}
	if got, want := envDecl.Enum, []string{"prod", "staging"}; !equalStrings(got, want) {
		t.Fatalf("Enum = %v, want %v (declared order)", got, want)
	}
	if !envDecl.Required {
		t.Fatal("expected Required=true")
	}
	if envDecl.Type != "string" {
		t.Fatalf("Type = %q, want string", envDecl.Type)
	}
	if envDecl.Description != "Target environment" {
		t.Fatalf("Description = %q", envDecl.Description)
	}
}

// CE-D-02: reordered members produce a DTO whose order matches the file,
// never re-sorted.
func TestDocument_InputDecl_EnumOrderMatchesFile_NotSorted(t *testing.T) {
	doc := buildDocFromYAML(t, fmtEnumRunbook(`"staging", "prod"`))

	var envDecl *graphdoc.InputDecl
	for i := range doc.Inputs {
		if doc.Inputs[i].Name == "env_name" {
			envDecl = &doc.Inputs[i]
		}
	}
	if envDecl == nil {
		t.Fatal("expected env_name in Document.Inputs")
	}
	if got, want := envDecl.Enum, []string{"staging", "prod"}; !equalStrings(got, want) {
		t.Fatalf("Enum = %v, want %v (file order, unsorted)", got, want)
	}
}

// CE-D-03: an input with no enum carries no enum key at all -- absent,
// never [] and never null (AR-ENUM-11 restated for the wire).
func TestDocument_InputDecl_NoEnum_KeyAbsent(t *testing.T) {
	doc := buildDocFromYAML(t, fmtEnumRunbook(`"prod", "staging"`))

	var freeDecl *graphdoc.InputDecl
	for i := range doc.Inputs {
		if doc.Inputs[i].Name == "free_text" {
			freeDecl = &doc.Inputs[i]
		}
	}
	if freeDecl == nil {
		t.Fatal("expected free_text in Document.Inputs")
	}
	if freeDecl.Enum != nil {
		t.Fatalf("Enum = %v, want nil (absent)", freeDecl.Enum)
	}
	if freeDecl.EnumRedacted {
		t.Fatal("EnumRedacted must be false for an unconstrained input")
	}

	raw := marshalToJSON(t, freeDecl)
	if containsKey(raw, `"enum"`) {
		t.Fatalf("JSON must omit \"enum\" entirely for an unconstrained input, got: %s", raw)
	}
}

// Redaction (C1): a redact-governed enum input never carries its member
// list, but does carry the raw count, and Default is also omitted.
func TestDocument_InputDecl_Redacted_NoMembersNoDefault(t *testing.T) {
	src := `
apiVersion: yawr.runbook/v1
id: test-enum-redacted
name: Test Enum Redacted
governance:
  redact:
    - pattern: "inputs\\.env_name"
      replace: "<redacted>"
inputs:
  env_name:
    type: string
    required: true
    default: prod
    enum: ["prod", "staging", "canary"]
flow:
  - step:
      id: show
      type: display
      title: Show
      content: "env=${env_name}"
`
	doc := buildDocFromYAML(t, src)

	var envDecl *graphdoc.InputDecl
	for i := range doc.Inputs {
		if doc.Inputs[i].Name == "env_name" {
			envDecl = &doc.Inputs[i]
		}
	}
	if envDecl == nil {
		t.Fatal("expected env_name in Document.Inputs")
	}
	if !envDecl.EnumRedacted {
		t.Fatal("expected EnumRedacted=true")
	}
	if envDecl.EnumMemberCount != 3 {
		t.Fatalf("EnumMemberCount = %d, want 3", envDecl.EnumMemberCount)
	}
	if envDecl.Enum != nil {
		t.Fatalf("Enum must be nil when redacted, got %v", envDecl.Enum)
	}
	if envDecl.Default != nil {
		t.Fatalf("Default must be omitted when redacted, got %v", envDecl.Default)
	}

	raw := marshalToJSON(t, envDecl)
	for _, member := range []string{"prod", "staging", "canary"} {
		if containsKey(raw, member) {
			t.Fatalf("redacted JSON must never leak member %q, got: %s", member, raw)
		}
	}
	if containsKey(raw, `"default"`) {
		t.Fatalf("redacted JSON must omit default entirely, got: %s", raw)
	}
}

func fmtEnumRunbook(members string) string {
	return fmt.Sprintf(enumRunbookTemplate, members)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func marshalToJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return string(b)
}

func containsKey(raw, needle string) bool {
	return strings.Contains(raw, needle)
}
