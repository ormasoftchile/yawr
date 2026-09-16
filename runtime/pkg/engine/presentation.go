package engine

import (
	"context"
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

type toolPresentationKey struct{}
type committedPresentationToolsKey struct{}
type planStepPresentationKey struct{}

type stepPresentationIdentity struct{ scope, step string }
type stepPresentationMetadata struct{ name, includeAlias string }

func WithPlanStepPresentation(ctx context.Context, steps []ResolvedStep) context.Context {
	metadata := make(map[stepPresentationIdentity]stepPresentationMetadata)
	if inherited, ok := ctx.Value(planStepPresentationKey{}).(map[stepPresentationIdentity]stepPresentationMetadata); ok {
		for key, value := range inherited {
			metadata[key] = value
		}
	}
	for _, step := range steps {
		metadata[stepPresentationIdentity{step.LexicalScopeID, step.ID}] = stepPresentationMetadata{step.Name, step.IncludeAlias}
	}
	return context.WithValue(ctx, planStepPresentationKey{}, metadata)
}

func ApplyPlanStepPresentation(ctx context.Context, step *ResolvedStep) {
	metadata, _ := ctx.Value(planStepPresentationKey{}).(map[stepPresentationIdentity]stepPresentationMetadata)
	if value, ok := metadata[stepPresentationIdentity{step.LexicalScopeID, step.ID}]; ok {
		step.Name = value.name
		step.IncludeAlias = value.includeAlias
	}
}

func FrozenToolDefinition(plan *ExecutionPlan, step ResolvedStep) (*schema.ToolDef, error) {
	spec, ok := step.Spec.(*schema.ToolCallSpec)
	if plan == nil || !ok || spec == nil {
		return nil, fmt.Errorf("engine: frozen tool definition requires a tool step and plan")
	}
	if plan.ToolScopes == nil {
		if step.LexicalScopeID != "" || step.ToolBindingID != "" {
			return nil, fmt.Errorf("engine: scoped step has no frozen scope set")
		}
		return schema.CloneToolDef(plan.Tools[spec.Tool.Name]), nil
	}
	action := spec.Tool.Action
	if action == "" {
		action = "run"
	}
	bound, err := plan.ToolScopes.Resolve(step.LexicalScopeID, spec.Tool.Name, action)
	if err != nil {
		return nil, err
	}
	if step.ToolBindingID != bound.BindingID || bound.Definition.Declaration == nil {
		return nil, fmt.Errorf("engine: frozen tool binding does not match step owner")
	}
	return bound.Definition.Declaration, nil
}

type ToolPresentationBinding struct {
	ScopeID          string
	BindingID        string
	DefinitionID     string
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
