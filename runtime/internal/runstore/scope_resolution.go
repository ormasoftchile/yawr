package runstore

import (
	"context"
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/toolscope"
)

func (store *DirRunStore) scopeSetForResolutions(ctx context.Context, runID string, resolutions map[string]*engine.DynamicIncludeResolutionState) (*toolscope.Set, error) {
	for _, resolution := range resolutions {
		if resolution == nil || resolution.SchemaVersion != engine.DynamicIncludeResolutionStateSchemaV2 {
			continue
		}
		plan, err := store.LoadPlan(ctx, runID)
		if err != nil {
			return nil, err
		}
		if plan.ToolScopes == nil {
			return nil, fmt.Errorf("runstore: scoped resolution has a legacy execution plan")
		}
		return plan.ToolScopes, nil
	}
	return nil, nil
}
