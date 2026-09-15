package planner

import (
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/tool"
	"github.com/ormasoftchile/yawr/runtime/pkg/toolscope"
)

func TestScopedTypedBoundFlowRetainsStructuralOwnership(t *testing.T) {
	document := toolscope.Document{SourceIdentity: "child.yaml", SourceDigest: "source-digest", PackageIdentity: "workspace"}
	documentID := toolscope.DocumentID(document)
	owner := toolscope.ScopeID(documentID, "catalog", toolscope.ProfileDigest(nil))
	document.ScopeID = owner
	definition := tool.BoundDefinition{
		Runtime:     tool.ToolDef{Name: "actual", Actions: map[string]*tool.ToolAction{"run": {}}},
		Declaration: &schema.ToolDef{Name: "actual", Actions: map[string]*schema.ToolAction{"run": {}}},
	}
	definitionID, err := tool.DefinitionID(definition)
	if err != nil {
		t.Fatal(err)
	}
	bindingID := tool.BindingID(owner, "query", definitionID)
	scopes, err := toolscope.New(toolscope.Snapshot{
		Version: toolscope.Version, CatalogDigest: "catalog", ProfileDigest: toolscope.ProfileDigest(nil),
		Documents:   map[string]toolscope.Document{documentID: document},
		Scopes:      map[string]toolscope.Scope{owner: {DocumentID: documentID, Bindings: map[string]string{"query": bindingID}}},
		Bindings:    map[string]toolscope.Binding{bindingID: {ScopeID: owner, LogicalName: "query", DefinitionID: definitionID}},
		Definitions: map[string]tool.BoundDefinition{definitionID: definition},
	})
	if err != nil {
		t.Fatal(err)
	}
	call := func(destination string) *schema.Step {
		return &schema.Step{ID: destination, Type: schema.StepTypeTool, LexicalScopeID: owner, ToolBindingID: bindingID,
			ToolCall: &schema.ToolCallSpec{Tool: schema.ToolInvocation{Name: "query", Action: "run"}},
			Capture:  map[string]string{destination: "stdout"}}
	}
	left, right := call("left"), call("right")
	flow := []schema.FlowNode{{Parallel: &schema.ParallelNode{ID: "parallel", Branches: []schema.ParallelBranch{
		{Steps: []schema.FlowNode{{Step: left}}}, {Steps: []schema.FlowNode{{Step: right}}},
	}}}}
	bindings := []schema.Binding{{Name: "left", Mutable: true}, {Name: "right", Mutable: true}}
	if err := ValidateScopedTypedBoundFlow(flow, bindings, scopes, owner); err != nil {
		t.Fatal(err)
	}
	right.Capture = map[string]string{"left": "stdout"}
	if err := ValidateScopedTypedBoundFlow(flow, bindings, scopes, owner); err == nil {
		t.Fatal("parallel write conflict accepted")
	}
	right.Capture = map[string]string{"right": "stdout"}
	right.LexicalScopeID = "unrelated-owner"
	if err := ValidateScopedTypedBoundFlow(flow, bindings, scopes, owner); err == nil {
		t.Fatal("structural body switched ownership")
	}
	right.LexicalScopeID = owner
	right.ToolBindingID = "unrelated-binding"
	if err := ValidateScopedTypedBoundFlow(flow, bindings, scopes, owner); err == nil {
		t.Fatal("binding mismatch accepted")
	}
	right.ToolBindingID = bindingID
	if err := ValidateTypedBoundFlow(flow, bindings, map[string]*schema.ToolDef{"query": definition.Declaration}); err == nil {
		t.Fatal("legacy validator accepted scoped meaning")
	}
	if err := ValidateScopedTypedBoundFlow(flow, bindings, nil, owner); err == nil {
		t.Fatal("missing immutable set accepted")
	}
}
