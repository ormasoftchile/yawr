package executor

import (
	"context"
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

func resolveBoundInvocation(ctx context.Context, step engine.ResolvedStep, name, action string) (*tool.BoundInvocation, error) {
	scopes := engine.ToolScopesFromContext(ctx)
	if scopes == nil {
		if step.LexicalScopeID != "" || step.ToolBindingID != "" {
			return nil, errkit.New("SCOPE-002", "scoped tool step has no immutable scope set")
		}
		return nil, nil
	}
	if step.LexicalScopeID == "" || step.ToolBindingID == "" {
		return nil, errkit.New("SCOPE-002", "scoped tool step is missing its owner or binding")
	}
	invocation, err := scopes.Resolve(step.LexicalScopeID, name, action)
	if err != nil {
		return nil, errkit.Wrap("SCOPE-002", "cannot resolve frozen tool invocation", err)
	}
	if invocation.BindingID != step.ToolBindingID {
		return nil, errkit.New("SCOPE-002", "tool step binding does not match its lexical owner")
	}
	if invocation.Definition.Declaration == nil || invocation.Definition.Declaration.Actions[action] == nil {
		return nil, errkit.New("SCOPE-002", "bound tool invocation is missing its frozen declaration")
	}
	return &invocation, nil
}

type boundDefinitionLookup struct{ invocation *tool.BoundInvocation }

func (lookup boundDefinitionLookup) LookupDef(name string) (*tool.ToolDef, bool) {
	if name != lookup.invocation.LogicalName {
		return nil, false
	}
	return &lookup.invocation.Definition.Runtime, true
}

func (e *ToolExecutor) invocationLookup(bound *tool.BoundInvocation) tool.ToolDefLookup {
	if bound != nil {
		return boundDefinitionLookup{invocation: bound}
	}
	lookup, _ := e.runtime.(tool.ToolDefLookup)
	return lookup
}

func recordInvocationPresentation(ctx context.Context, name, action string, bound *tool.BoundInvocation) (context.Context, error) {
	if bound == nil {
		engine.RecordToolPresentation(ctx, name, action, engine.PlanToolsFromContext(ctx)[name])
		return ctx, nil
	}
	declarations, err := engine.ToolScopesFromContext(ctx).Declarations(bound.ScopeID)
	if err != nil {
		return ctx, err
	}
	ctx = engine.WithPlanTools(ctx, declarations)
	if presentation := engine.ToolPresentationFromContext(ctx); presentation != nil {
		presentation.FrozenTools = declarations
		presentation.ScopeID = bound.ScopeID
		presentation.BindingID = bound.BindingID
		presentation.DefinitionID = bound.DefinitionID
	}
	engine.RecordToolPresentation(ctx, name, action, declarations[name])
	return ctx, nil
}

func frozenInvocationDefinition(ctx context.Context, name, action string, bound *tool.BoundInvocation) (*schema.ToolDef, error) {
	if bound == nil {
		return frozenSubstitutionDefinition(ctx, name, action), nil
	}
	declaration := bound.Definition.Declaration
	authored := declaration.Actions[action]
	isSubstitution := authored.Execute.IsSubstitution()
	if isSubstitution != bound.Definition.Runtime.Actions[action].Execute.IsSubstitution() {
		return nil, errkit.New("SCOPE-002", "bound action declaration disagrees with its runtime definition")
	}
	if !isSubstitution {
		return nil, nil
	}
	if authored.FrozenSubstitution == nil {
		return nil, errkit.New("SCOPE-002", fmt.Sprintf("bound substitution %s#%s is not frozen", name, action))
	}
	owned := schema.CloneToolDef(declaration)
	owned.Name = name
	return owned, nil
}
