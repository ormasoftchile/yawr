package executor

import (
	"context"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
)

type runEvidenceHookContextKey struct{}

type runEvidenceHookBinding struct {
	hook engine.EvidenceHook
}

func WithRunEvidenceHook(ctx context.Context, hook engine.EvidenceHook) context.Context {
	return context.WithValue(ctx, runEvidenceHookContextKey{}, runEvidenceHookBinding{hook: hook})
}

func RunEvidenceHookFromContext(ctx context.Context) (engine.EvidenceHook, bool) {
	if ctx == nil {
		return nil, false
	}
	binding, found := ctx.Value(runEvidenceHookContextKey{}).(runEvidenceHookBinding)
	return binding.hook, found
}
