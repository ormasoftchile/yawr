package executor

import (
	"context"

	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

// Capture at the resolution commit boundary, not at display time. The same
// materializer used for executable substitutions resolves exact actions from
// this execution's runtime and freezes their nested definitions.
func (e *ToolExecutor) freezeDynamicPresentationTools(ctx context.Context, flow []schema.FlowNode) map[string]*schema.ToolDef {
	lookup, ok := e.runtime.(tool.ToolDefLookup)
	if !ok {
		return nil
	}
	plan := &engine.ExecutionPlan{Tools: map[string]*schema.ToolDef{}}
	for name, definition := range engine.PlanToolsFromContext(ctx) {
		plan.Tools[name] = schema.CloneToolDef(definition)
	}
	materializer := frozenToolMaterializer{plan: plan, parser: e.parser, lookup: lookup}
	if err := materializer.freezeNestedTools(ctx, flow, nil, nil); err != nil || !plansnapshot.HasPresentation(plan.Tools) {
		return nil
	}
	return plan.Tools
}
