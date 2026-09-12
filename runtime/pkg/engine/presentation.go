package engine

import (
	"context"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

type toolPresentationKey struct{}
type committedPresentationToolsKey struct{}
type ToolPresentationBinding struct {
	ToolID           string
	Action           string
	Definition       *schema.ToolDef
	SnapshotDigest   string
	OutputsValidated bool
	FrozenTools      map[string]*schema.ToolDef
}

// Each step owns a separate slot; nested executions cannot overwrite a parent.
func WithToolPresentationCapture(ctx context.Context, digest string) context.Context {
	tools := PlanToolsFromContext(ctx)
	if parent := ToolPresentationFromContext(ctx); parent != nil {
		tools = parent.FrozenTools
	}
	if committed, ok := ctx.Value(committedPresentationToolsKey{}).(map[string]*schema.ToolDef); ok {
		tools = committed
	}
	return context.WithValue(ctx, toolPresentationKey{}, &ToolPresentationBinding{SnapshotDigest: digest, FrozenTools: tools})
}

// WithCommittedPresentationTools is used only after a dynamic resolution has
// committed its executable closure and before entering that resolution's frame.
func WithCommittedPresentationTools(ctx context.Context, tools map[string]*schema.ToolDef) context.Context {
	merged := make(map[string]*schema.ToolDef)
	if parent := ToolPresentationFromContext(ctx); parent != nil {
		for name, definition := range parent.FrozenTools {
			merged[name] = definition
		}
	}
	for name, definition := range tools {
		merged[name] = definition
	}
	return context.WithValue(ctx, committedPresentationToolsKey{}, merged)
}
func RecordToolPresentation(ctx context.Context, toolID, action string, definition *schema.ToolDef) {
	if binding, ok := ctx.Value(toolPresentationKey{}).(*ToolPresentationBinding); ok {
		if frozen, ok := binding.FrozenTools[toolID]; !ok {
			definition = nil
		} else {
			definition = frozen
		}
		binding.ToolID, binding.Action, binding.Definition = toolID, action, definition
	}
}
func ToolPresentationFromContext(ctx context.Context) *ToolPresentationBinding {
	binding, _ := ctx.Value(toolPresentationKey{}).(*ToolPresentationBinding)
	return binding
}

func ApproveToolPresentationOutputs(ctx context.Context) {
	if binding := ToolPresentationFromContext(ctx); binding != nil {
		binding.OutputsValidated = true
	}
}
