package sessioncoordinator

import (
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestDynamicMaterializationRetainsAndClearsTargetScope(t *testing.T) {
	include := &schema.IncludeSpec{Include: schema.IncludeConfig{
		RunbookRef: "${target}", ResolveFrom: schema.ResolveFromCatalog,
	}}
	flow := []schema.FlowNode{{Step: &schema.Step{
		ID: "child", Type: schema.StepTypeNoop, LexicalScopeID: "child-scope",
		NoopSpec: &schema.NoopSpec{},
	}}}
	setDynamicIncludeMaterialization(include, schema.LockedDynamicInclude{
		TargetScopeID: "child-scope", AbsPath: "child.yaml",
	}, flow)
	if include.TargetScopeID != "child-scope" || include.ResolvedSteps[0].Step.LexicalScopeID != "child-scope" {
		t.Fatalf("canonical include lost frozen target ownership: %+v", include)
	}
	clearDynamicMaterializations(include)
	if include.TargetScopeID != "" || include.ResolvedSteps != nil || include.ResolvedRunbookPath != "" {
		t.Fatalf("base revision retained stale dynamic ownership: %+v", include)
	}
}
