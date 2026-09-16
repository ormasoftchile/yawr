package engine

import (
	"testing"

	enginepkg "github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/tool"
	"github.com/ormasoftchile/yawr/runtime/pkg/toolscope"
)

func TestScopedClassificationUsesExactBinding(t *testing.T) {
	classification := "read-only"
	action := &schema.ToolAction{Classification: &classification}
	definition := tool.BoundDefinition{
		Runtime: tool.ToolDef{Name: "canonical", Actions: map[string]*tool.ToolAction{
			"run": (&tool.ToolAction{}).WithSchemaAction(action),
		}},
		Declaration: &schema.ToolDef{Name: "canonical", Actions: map[string]*schema.ToolAction{"run": action}},
	}
	definitionID, err := tool.DefinitionID(definition)
	if err != nil {
		t.Fatal(err)
	}
	document := toolscope.Document{SourceIdentity: "child.yaml", SourceDigest: "captured", PackageIdentity: "workspace"}
	documentID := toolscope.DocumentID(document)
	scopeID := toolscope.ScopeID(documentID, "catalog", toolscope.ProfileDigest(nil))
	document.ScopeID = scopeID
	bindingID := tool.BindingID(scopeID, "query", definitionID)
	scopes, err := toolscope.New(toolscope.Snapshot{
		Version: toolscope.Version, CatalogDigest: "catalog", ProfileDigest: toolscope.ProfileDigest(nil),
		Documents:   map[string]toolscope.Document{documentID: document},
		Scopes:      map[string]toolscope.Scope{scopeID: {DocumentID: documentID, Bindings: map[string]string{"query": bindingID}}},
		Bindings:    map[string]toolscope.Binding{bindingID: {ScopeID: scopeID, LogicalName: "query", DefinitionID: definitionID}},
		Definitions: map[string]tool.BoundDefinition{definitionID: definition},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan := &enginepkg.ExecutionPlan{ToolScopes: scopes, RootScopeID: scopeID}
	step := enginepkg.ResolvedStep{ID: "query", Kind: "tool", LexicalScopeID: scopeID, ToolBindingID: bindingID,
		Spec: &schema.ToolCallSpec{Tool: schema.ToolInvocation{Name: "query", Action: "run"}}}
	got, err := resolveStepClassification(step, plan)
	if err != nil || got == nil || *got != "read-only" {
		t.Fatalf("classification = %v, %v", got, err)
	}
	step.ToolBindingID = "wrong"
	if _, err := resolveStepClassification(step, plan); err == nil {
		t.Fatal("classification accepted a different binding")
	}
}
