package executor

import (
	"context"

	"github.com/ormasoftchile/yawr/runtime/pkg/pkgsubst"
)

// substState carries the active substitution call-graph state across
// nested tool-action invocations (a substituted action's body is run
// through the same SubStepRunner -> nested engine -> ToolExecutor path as
// include/branch/iterate, so a second-level substitution reaches
// ToolExecutor.Execute again with no other way to see the enclosing
// frame). This is what lets pkgsubst.Plan correctly detect cycles (PKG-015)
// and depth (PKG-028) across nested substitution calls, and lets governance
// composition chain frame-to-frame rather than resetting at each level.
type substState struct {
	frames     []pkgsubst.Frame
	governance *pkgsubst.EffectiveGovernance
}

type substCtxKey struct{}

// withSubstState returns a child context carrying the given substitution
// call-graph state, for the nested engine run of a substitute's body.
func withSubstState(ctx context.Context, st substState) context.Context {
	return context.WithValue(ctx, substCtxKey{}, st)
}

// substStateFromContext returns the substitution call-graph state carried
// on ctx, or the zero value with ok=false at the top level (a directly
// invoked, non-nested substituted action).
func substStateFromContext(ctx context.Context) (substState, bool) {
	if ctx == nil {
		return substState{}, false
	}
	st, ok := ctx.Value(substCtxKey{}).(substState)
	return st, ok
}
