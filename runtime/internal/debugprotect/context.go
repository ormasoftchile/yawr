package debugprotect

import (
	"context"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/governance"
)

type protectionContextKey struct{}
type protectionSinkContextKey struct{}
type protectAllContextKey struct{}

// WithProtection carries only names and redaction policy into a nested engine.
// Secret values are rehydrated there from its private variable scope.
func WithProtection(ctx context.Context, protection engine.DebugProtection) context.Context {
	transport := cloneProtection(protection)
	transport.SecretValues = nil
	return context.WithValue(ctx, protectionContextKey{}, transport)
}

func ProtectionFromContext(ctx context.Context) engine.DebugProtection {
	protection, _ := ctx.Value(protectionContextKey{}).(engine.DebugProtection)
	return cloneProtection(protection)
}

func WithProtectAll(ctx context.Context, protectAll bool) context.Context {
	if !protectAll {
		return ctx
	}
	return context.WithValue(ctx, protectAllContextKey{}, true)
}

func ProtectAllFromContext(ctx context.Context) bool {
	protectAll, _ := ctx.Value(protectAllContextKey{}).(bool)
	return protectAll
}

func WithSink(ctx context.Context, sink func(engine.DebugProtection)) context.Context {
	if sink == nil {
		return ctx
	}
	parent, _ := ctx.Value(protectionSinkContextKey{}).(func(engine.DebugProtection))
	if parent != nil {
		local := sink
		sink = func(protection engine.DebugProtection) {
			local(cloneProtection(protection))
			parent(cloneProtection(protection))
		}
	}
	return context.WithValue(ctx, protectionSinkContextKey{}, sink)
}

// HasSink reports whether runtime protection can be published to an owning
// output boundary without exposing the accumulated secret state.
func HasSink(ctx context.Context) bool {
	sink, _ := ctx.Value(protectionSinkContextKey{}).(func(engine.DebugProtection))
	return sink != nil
}

// PublishProtection is write-only: callers can extend owning-engine
// protection but cannot retrieve its accumulated secret state.
func PublishProtection(ctx context.Context, protection engine.DebugProtection) {
	sink, _ := ctx.Value(protectionSinkContextKey{}).(func(engine.DebugProtection))
	if sink != nil {
		sink(cloneProtection(protection))
	}
}

func cloneProtection(protection engine.DebugProtection) engine.DebugProtection {
	patterns := make([]*governance.RedactionPattern, 0, len(protection.RedactionPatterns))
	for _, pattern := range protection.RedactionPatterns {
		if pattern == nil {
			continue
		}
		copied := *pattern
		patterns = append(patterns, &copied)
	}
	return engine.DebugProtection{
		ProtectedVars:     append([]string(nil), protection.ProtectedVars...),
		SecretValues:      append([]string(nil), protection.SecretValues...),
		RedactionPatterns: patterns,
	}
}
