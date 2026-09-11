package executor

import "context"

type runResolverContextKey struct{}
type runPinRecorderContextKey struct{}

// WithRunResolver returns a derived context carrying the dynamic include
// resolver for one run. It lets long-lived engines keep singleton executors
// while served runs supply isolated per-run catalogs.
func WithRunResolver(ctx context.Context, resolver DynamicIncludeResolver) context.Context {
	if resolver == nil {
		return ctx
	}
	return context.WithValue(ctx, runResolverContextKey{}, resolver)
}

func resolverFromCtx(ctx context.Context) DynamicIncludeResolver {
	if ctx == nil {
		return nil
	}
	resolver, _ := ctx.Value(runResolverContextKey{}).(DynamicIncludeResolver)
	return resolver
}

// WithRunPinRecorder returns a derived context carrying the dynamic include
// pin recorder for one run.
func WithRunPinRecorder(ctx context.Context, recorder PinRecorder) context.Context {
	if recorder == nil {
		return ctx
	}
	return context.WithValue(ctx, runPinRecorderContextKey{}, recorder)
}

func pinRecorderFromCtx(ctx context.Context) PinRecorder {
	if ctx == nil {
		return nil
	}
	recorder, _ := ctx.Value(runPinRecorderContextKey{}).(PinRecorder)
	return recorder
}
