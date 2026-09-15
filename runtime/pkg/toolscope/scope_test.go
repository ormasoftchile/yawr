package toolscope

import (
	"encoding/json"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

func TestSetDigestCoversGeneratedBodiesWithoutRecursiveDefinitionIDs(t *testing.T) {
	document := Document{SourceIdentity: "substitute.yaml", SourceDigest: "sha256:source", PackageIdentity: "workspace"}
	docID := DocumentID(document)
	owner := ScopeID(docID, "sha256:catalog", ProfileDigest(nil))
	document.ScopeID = owner
	action := &schema.ToolAction{Execute: &schema.ExecuteSpec{Kind: "runbook", Path: "substitute.yaml"},
		FrozenSubstitution: &schema.FrozenToolSubstitution{ExecutableClosure: json.RawMessage(`{"body":"original"}`)}}
	definition := tool.BoundDefinition{Runtime: tool.ToolDef{Name: "real", Actions: map[string]*tool.ToolAction{"run": new(tool.ToolAction).WithSchemaAction(action)}}}
	id, err := tool.DefinitionID(definition)
	if err != nil {
		t.Fatal(err)
	}
	bindingID := tool.BindingID(owner, "local", id)
	input := Snapshot{Version: Version, CatalogDigest: "sha256:catalog", ProfileDigest: ProfileDigest(nil),
		Documents: map[string]Document{docID: document}, Scopes: map[string]Scope{owner: {DocumentID: docID, Bindings: map[string]string{"local": bindingID}}},
		Bindings: map[string]Binding{bindingID: {ScopeID: owner, LogicalName: "local", DefinitionID: id}}, Definitions: map[string]tool.BoundDefinition{id: definition}}
	set, err := New(input)
	if err != nil {
		t.Fatal(err)
	}
	action.FrozenSubstitution.ExecutableClosure = json.RawMessage(`{"body":"caller-mutation"}`)
	call, err := set.Resolve(owner, "local", "run")
	if err != nil {
		t.Fatal(err)
	}
	if string(call.Definition.Runtime.Actions["run"].SchemaAction().FrozenSubstitution.ExecutableClosure) != `{"body":"original"}` {
		t.Fatal("constructor retained mutable schema")
	}
	exported := set.Export()
	d := exported.Definitions[id]
	d.Runtime.Actions["run"].SchemaAction().FrozenSubstitution.ExecutableClosure = json.RawMessage(`{"body":"tampered"}`)
	exported.Definitions[id] = d
	again, err := tool.DefinitionID(d)
	if err != nil || again != id {
		t.Fatal("generated body made definition identity recursive")
	}
	if _, err := Restore(exported); err == nil {
		t.Fatal("generated body escaped containing digest")
	}
	body, err := json.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	var restored Set
	if err := json.Unmarshal(body, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Export().Digest != set.Export().Digest {
		t.Fatal("set codec changed digest")
	}
}
