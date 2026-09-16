package tool

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func boundFixture(t *testing.T) BoundInvocation {
	t.Helper()
	action := &schema.ToolAction{Args: map[string]*schema.ArgDef{
		"count": {Type: "integer", Default: json.Number("9007199254740993")},
	}}
	definition := BoundDefinition{
		Runtime: ToolDef{Name: "query", Source: "tool://query", Command: "fixture", Transport: TransportNative,
			Actions: map[string]*ToolAction{"run": (&ToolAction{Args: map[string]*ArgDef{
				"count": {Type: "integer", Default: json.Number("9007199254740993")},
			}}).WithSchemaAction(action)}},
		Declaration:  &schema.ToolDef{Name: "query", Actions: map[string]*schema.ToolAction{"run": action}},
		SourceDigest: "sha256:" + strings.Repeat("a", 64), PackageVersion: "1.0.0",
	}
	id, err := DefinitionID(definition)
	if err != nil {
		t.Fatal(err)
	}
	return BoundInvocation{ScopeID: "scope-fixture", BindingID: BindingID("scope-fixture", "query", id),
		DefinitionID: id, LogicalName: "query", Action: "run", Definition: definition}
}

func TestBoundDefinitionRoundTripPreservesRetainedSchemasAndNumbers(t *testing.T) {
	before := boundFixture(t)
	body, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	var after BoundInvocation
	if err := json.Unmarshal(body, &after); err != nil {
		t.Fatal(err)
	}
	if err := ValidateBoundInvocation(after); err != nil {
		t.Fatal(err)
	}
	action := after.Definition.Runtime.Actions["run"]
	if action.SchemaAction() == nil || action.SchemaAction().Args["count"].Default != json.Number("9007199254740993") ||
		action.Args["count"].Default != json.Number("9007199254740993") {
		t.Fatalf("lost retained schema or integer precision: %+v", action)
	}
}

func TestBoundDefinitionCloneDoesNotAliasInputs(t *testing.T) {
	original := boundFixture(t).Definition
	cloned, err := CloneBoundDefinition(original)
	if err != nil {
		t.Fatal(err)
	}
	cloned.Runtime.Actions["run"].Args["count"].Type = "string"
	cloned.Runtime.Actions["run"].SchemaAction().Args["count"].Default = "mutated"
	cloned.Declaration.Name = "other"
	if original.Runtime.Actions["run"].Args["count"].Type != "integer" ||
		original.Runtime.Actions["run"].SchemaAction().Args["count"].Default != json.Number("9007199254740993") ||
		original.Declaration.Name != "query" {
		t.Fatal("cloning leaked mutable state")
	}
}

func TestBoundInvocationRejectsIdentityMutation(t *testing.T) {
	for _, field := range []string{"scope", "binding", "definition", "logical-name", "command", "schema", "source", "version", "action"} {
		t.Run(field, func(t *testing.T) {
			invocation := boundFixture(t)
			switch field {
			case "scope":
				invocation.ScopeID = "other"
			case "binding":
				invocation.BindingID = "other"
			case "definition":
				invocation.DefinitionID = "other"
			case "logical-name":
				invocation.LogicalName = "other"
			case "command":
				invocation.Definition.Runtime.Command = "other"
			case "schema":
				invocation.Definition.Runtime.Actions["run"].SchemaAction().Args["count"].Default = "other"
			case "source":
				invocation.Definition.SourceDigest = "other"
			case "version":
				invocation.Definition.PackageVersion = "2.0.0"
			case "action":
				invocation.Action = "undeclared"
			}
			if err := ValidateBoundInvocation(invocation); !errors.Is(err, errkit.ErrSCOPE002) {
				t.Fatalf("%s mutation was not rejected: %v", field, err)
			}
		})
	}
}

func TestBoundDefinitionIDDoesNotRecursivelyHashGeneratedSubstitution(t *testing.T) {
	definition := boundFixture(t).Definition
	before, err := DefinitionID(definition)
	if err != nil {
		t.Fatal(err)
	}
	frozen := &schema.FrozenToolSubstitution{RunbookID: "child"}
	definition.Declaration.Actions["run"].FrozenSubstitution = frozen
	definition.Runtime.Actions["run"].SchemaAction().FrozenSubstitution = frozen
	after, err := DefinitionID(definition)
	if err != nil || after != before {
		t.Fatalf("recursive generated state changed definition identity: %s %s %v", before, after, err)
	}
	if definition.Declaration.Actions["run"].FrozenSubstitution != frozen ||
		definition.Runtime.Actions["run"].SchemaAction().FrozenSubstitution != frozen {
		t.Fatal("identity computation changed caller-owned generated state")
	}
	body, err := json.Marshal(definition)
	if err != nil || !strings.Contains(string(body), "child") {
		t.Fatalf("full persistence lost generated executable state: %s %v", body, err)
	}
}

func TestBoundDefinitionRejectsUnknownVersionAndFields(t *testing.T) {
	for _, body := range []string{
		`{"version":"old","definition":{}}`,
		`{"version":"yawr.bound-tool-definition/v1","definition":{},"unknown":true}`,
		`{"version":"yawr.bound-tool-definition/v1","definition":{},"schema_actions":{"orphan":{}}}`,
		`{"version":"yawr.bound-tool-definition/v1","definition":{}} {}`,
	} {
		var definition BoundDefinition
		if err := definition.UnmarshalJSON([]byte(body)); err == nil {
			t.Fatalf("invalid envelope accepted: %s", body)
		}
	}
}
