package adapter

import (
	"context"
	"fmt"
	"maps"

	"github.com/ormasoftchile/yawr/runtime/internal/executor"
	internalplanner "github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/expand"
	plannerpkg "github.com/ormasoftchile/yawr/runtime/pkg/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// Plan builds a fresh validated plan exclusively from captured sources and
// materializes every static include before returning. Its durable result needs
// no run-specific loader at execution or resume time.
func (prepared *PreparedScopedRun) Plan(ctx context.Context, policy expand.Policy) (*engine.ExecutionPlan, error) {
	ctx = engine.WithToolScopes(ctx, prepared.Scopes)
	root, err := prepared.Loader.LoadScope(ctx, prepared.Root.Source, prepared.RootScopeID)
	if err != nil {
		return nil, err
	}
	planner := internalplanner.New(plannerpkg.Config{Loader: prepared.Loader, Tools: scopedNoRegistry{},
		Profile: prepared.Profile, ExpandPolicy: policy})
	plan, err := planner.Plan(ctx, root)
	if err != nil {
		return nil, err
	}
	plan.Metadata.PackageDigests = maps.Clone(prepared.packageDigests)
	materializer := executor.NewIncludeExecutor(nil, nil, prepared.LazyLoader)
	if err := materializer.MaterializeLazyIncludes(ctx, plan); err != nil {
		return nil, err
	}
	if internalplanner.HasDeferredStaticIncludes(plan) {
		return nil, fmt.Errorf("scoped preparation: static include remains deferred")
	}
	if err := internalplanner.FinalizeMaterializedPlan(plan); err != nil {
		return nil, err
	}
	return plan, nil
}

type scopedNoRegistry struct{}

func (scopedNoRegistry) Lookup(context.Context, string, string) (*schema.ToolDef, error) {
	return nil, fmt.Errorf("scoped preparation: name registry lookup is forbidden")
}
