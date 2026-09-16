package replay

import (
	"context"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/tool"
	"github.com/ormasoftchile/yawr/runtime/pkg/toolscope"
)

func TestReplayFrozenDefinitionDoesNotUseSharedAliasRegistry(t *testing.T) {
	snapshot := toolscope.Snapshot{Version: toolscope.Version, CatalogDigest: "catalog",
		ProfileDigest: toolscope.ProfileDigest(nil), Documents: map[string]toolscope.Document{},
		Scopes: map[string]toolscope.Scope{}, Bindings: map[string]toolscope.Binding{},
		Definitions: map[string]tool.BoundDefinition{}}
	var steps []engine.ResolvedStep
	for _, marker := range []string{"left", "right"} {
		action := &schema.ToolAction{Description: marker}
		definition := tool.BoundDefinition{
			Runtime:     tool.ToolDef{Name: marker, Actions: map[string]*tool.ToolAction{"run": (&tool.ToolAction{}).WithSchemaAction(action)}},
			Declaration: &schema.ToolDef{Name: marker, Actions: map[string]*schema.ToolAction{"run": action}},
		}
		id, err := tool.DefinitionID(definition)
		if err != nil {
			t.Fatal(err)
		}
		document := toolscope.Document{SourceIdentity: marker + ".yaml", SourceDigest: marker, PackageIdentity: "workspace"}
		documentID := toolscope.DocumentID(document)
		scopeID := toolscope.ScopeID(documentID, snapshot.CatalogDigest, snapshot.ProfileDigest)
		bindingID := tool.BindingID(scopeID, "query", id)
		document.ScopeID = scopeID
		snapshot.Documents[documentID] = document
		snapshot.Scopes[scopeID] = toolscope.Scope{DocumentID: documentID, Bindings: map[string]string{"query": bindingID}}
		snapshot.Bindings[bindingID] = toolscope.Binding{ScopeID: scopeID, LogicalName: "query", DefinitionID: id}
		snapshot.Definitions[id] = definition
		steps = append(steps, engine.ResolvedStep{ID: marker, Kind: "tool", LexicalScopeID: scopeID, ToolBindingID: bindingID,
			Spec: &schema.ToolCallSpec{Tool: schema.ToolInvocation{Name: "query", Action: "run"}}})
	}
	scopes, err := toolscope.New(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	executor := &replayToolExecutor{registry: &ReplayExecutorRegistry{
		tools: map[string]*schema.ToolDef{"query": {Name: "wrong-registry-definition"}},
	}}
	ctx := engine.WithToolScopes(context.Background(), scopes)
	for _, step := range steps {
		definition, action, err := executor.frozenDefinition(ctx, step, nil)
		if err != nil || definition == nil || definition.Name != "query" || action.Description != step.ID {
			t.Fatalf("frozen replay definition = %+v, %+v, %v", definition, action, err)
		}
	}
	corrupt := steps[0]
	corrupt.ToolBindingID = steps[1].ToolBindingID
	if _, _, err := executor.frozenDefinition(ctx, corrupt, nil); err == nil {
		t.Fatal("replay accepted another scope's binding")
	}
}
