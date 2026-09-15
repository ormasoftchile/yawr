package replay

import (
	"context"
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func scopedReplayToolDefinition(ctx context.Context, step engine.ResolvedStep, name, action string) (*schema.ToolDef, error) {
	scopes := engine.ToolScopesFromContext(ctx)
	if scopes == nil {
		return nil, fmt.Errorf("replay: scoped step %s has no immutable bindings", step.ID)
	}
	if action == "" {
		action = "run"
	}
	invocation, err := scopes.Resolve(step.LexicalScopeID, name, action)
	if err != nil {
		return nil, err
	}
	if step.ToolBindingID != invocation.BindingID {
		return nil, fmt.Errorf("replay: tool binding for step %s does not match its lexical scope", step.ID)
	}
	if invocation.Definition.Declaration == nil {
		return nil, fmt.Errorf("replay: tool binding for step %s has no frozen declaration", step.ID)
	}
	definition := schema.CloneToolDef(invocation.Definition.Declaration)
	definition.Name = name
	return definition, nil
}
