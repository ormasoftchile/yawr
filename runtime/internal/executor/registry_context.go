package executor

import (
	"context"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
)

type executorRegistryContextKey struct{}

func WithExecutorRegistry(ctx context.Context, registry engine.ExecutorRegistry) context.Context {
	if registry == nil {
		return ctx
	}
	return context.WithValue(ctx, executorRegistryContextKey{}, registry)
}

func ExecutorRegistryFromContext(ctx context.Context) engine.ExecutorRegistry {
	registry, _ := ctx.Value(executorRegistryContextKey{}).(engine.ExecutorRegistry)
	return registry
}
