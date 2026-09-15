package engine

import (
	"context"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/toolscope"
)

type toolScopesContextKey struct{}

// ToolScopeBoundary identifies the external graph parent of a nested plan.
// It preserves authored graph references without treating an arbitrary missing
// parent as permission to change lexical ownership.
type ToolScopeBoundary struct {
	ParentID   string `json:"parent_id"`
	ParentKind string `json:"parent_kind"`
	ScopeID    string `json:"scope_id"`
}

func WithToolScopes(ctx context.Context, scopes *toolscope.Set) context.Context {
	return context.WithValue(ctx, toolScopesContextKey{}, scopes)
}

func ToolScopesFromContext(ctx context.Context) *toolscope.Set {
	scopes, _ := ctx.Value(toolScopesContextKey{}).(*toolscope.Set)
	return scopes
}

type planProfileContextKey struct{}

// WithPlanProfile carries already-frozen policy meaning to nested plans. It
// never reloads, merges, or applies overrides to the immutable tool bindings.
func WithPlanProfile(ctx context.Context, profile *schema.RuntimeProfile) context.Context {
	return context.WithValue(ctx, planProfileContextKey{}, clonePlanProfile(profile))
}

func PlanProfileFromContext(ctx context.Context) *schema.RuntimeProfile {
	profile, _ := ctx.Value(planProfileContextKey{}).(*schema.RuntimeProfile)
	return clonePlanProfile(profile)
}

func clonePlanProfile(profile *schema.RuntimeProfile) *schema.RuntimeProfile {
	if profile == nil {
		return nil
	}
	owned := *profile
	if profile.Tools != nil {
		owned.Tools = make(map[string]*schema.ProfileToolOverride, len(profile.Tools))
		for name, override := range profile.Tools {
			if override == nil {
				owned.Tools[name] = nil
			} else {
				copy := *override
				owned.Tools[name] = &copy
			}
		}
	}
	return &owned
}
