package executor

import (
	"context"

	internalplanner "github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func (e *IncludeExecutor) loadScopedRunbook(ctx context.Context, spec *schema.IncludeSpec) (*LoadedRunbook, error) {
	scopes := engine.ToolScopesFromContext(ctx)
	if scopes == nil || !scopes.HasScope(spec.TargetScopeID) {
		return nil, errkit.New("SCOPE-002", "deferred include has no frozen target scope")
	}
	loader, ok := e.loader.(ScopedLazyRunbookLoader)
	if !ok {
		return nil, errkit.New("SCOPE-002", "deferred include requires a captured scoped loader")
	}
	loaded, err := loader.LoadScoped(ctx, spec.LazyRunbookPath, spec.TargetScopeID)
	if err != nil {
		return nil, err
	}
	if loaded == nil || loaded.RootScopeID != spec.TargetScopeID {
		return nil, errkit.New("SCOPE-002", "captured include returned another lexical owner")
	}
	snapshot := scopes.Export()
	document := snapshot.Documents[snapshot.Scopes[spec.TargetScopeID].DocumentID]
	if loaded.SourceDigest != document.SourceDigest ||
		(spec.LazyRunbookDigest != "" && loaded.SourceDigest != spec.LazyRunbookDigest) {
		return nil, errkit.New("SCOPE-002", "captured include source does not match its frozen identity")
	}
	if err := internalplanner.ValidateScopedFlow(loaded.Flow, scopes, spec.TargetScopeID); err != nil {
		return nil, err
	}
	return loaded, nil
}
