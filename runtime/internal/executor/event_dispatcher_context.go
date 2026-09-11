package executor

import (
	"context"

	"github.com/ormasoftchile/yawr/runtime/pkg/eventbus"
)

type runEventDispatcherContextKey struct{}

func WithRunEventDispatcher(ctx context.Context, dispatcher eventbus.EventDispatcher) context.Context {
	if dispatcher == nil {
		return ctx
	}
	return context.WithValue(ctx, runEventDispatcherContextKey{}, dispatcher)
}

func RunEventDispatcherFromContext(ctx context.Context) eventbus.EventDispatcher {
	dispatcher, _ := ctx.Value(runEventDispatcherContextKey{}).(eventbus.EventDispatcher)
	return dispatcher
}
