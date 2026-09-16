package replay

import (
	"context"
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/internal/executor"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
)

func (loader *PinBasedIncludeLoader) LoadScoped(ctx context.Context, path, scopeID string) (*executor.LoadedRunbook, error) {
	pin, ok := loader.pinsByPath[path]
	if !ok || pin.TargetScopeID != scopeID || scopeID == "" || engine.ToolScopesFromContext(ctx) == nil {
		return nil, fmt.Errorf("replay: source %s has no captured pin for requested scope", path)
	}
	return loader.Load(ctx, path)
}
