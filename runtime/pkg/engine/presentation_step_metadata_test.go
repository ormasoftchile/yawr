package engine

import (
	"context"
	"testing"
)

func TestStepPresentationMetadataIsLexicalAndOwned(t *testing.T) {
	steps := []ResolvedStep{
		{ID: "include", LexicalScopeID: "left", Name: "Left", IncludeAlias: "left-alias"},
		{ID: "include", LexicalScopeID: "right", Name: "Right", IncludeAlias: "right-alias"},
	}
	ctx := WithPlanStepPresentation(context.Background(), steps)
	steps[0].IncludeAlias = "mutated"
	child := WithPlanStepPresentation(ctx, []ResolvedStep{{ID: "other", LexicalScopeID: "right", Name: "Other"}})
	for _, scope := range []string{"left", "right"} {
		step := ResolvedStep{ID: "include", LexicalScopeID: scope}
		ApplyPlanStepPresentation(child, &step)
		if step.IncludeAlias != scope+"-alias" {
			t.Fatalf("scope %s: alias %q", scope, step.IncludeAlias)
		}
	}
	step := ResolvedStep{ID: "include", LexicalScopeID: "unknown", Name: "Original"}
	ApplyPlanStepPresentation(ctx, &step)
	if step.Name != "Original" || step.IncludeAlias != "" {
		t.Fatalf("unknown scope inherited presentation: %+v", step)
	}
}
