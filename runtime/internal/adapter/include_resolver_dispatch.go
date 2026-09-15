package adapter

import (
	"context"
	"errors"

	"github.com/ormasoftchile/yawr/runtime/internal/executor"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
)

type versionedIncludeResolver struct {
	legacy executor.DynamicIncludeResolver
}

func (resolver versionedIncludeResolver) Resolve(ctx context.Context, ref string) (*executor.DynamicIncludeResult, error) {
	if engine.ToolScopesFromContext(ctx) != nil {
		return NewFrozenScopedIncludeResolver().Resolve(ctx, ref)
	}
	if resolver.legacy == nil {
		return nil, errors.New("adapter: legacy dynamic include resolver is not configured")
	}
	return resolver.legacy.Resolve(ctx, ref)
}
