package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

type capturedIncludeFixture struct {
	loaded *LoadedRunbook
	legacy int
	scoped int
}

func (loader *capturedIncludeFixture) Load(context.Context, string) (*LoadedRunbook, error) {
	loader.legacy++
	return nil, errors.New("filesystem loader must not run")
}

func (loader *capturedIncludeFixture) LoadScoped(context.Context, string, string) (*LoadedRunbook, error) {
	loader.scoped++
	return loader.loaded, nil
}

func TestScopedIncludeCapturedLoaderVerifiesOwnerAndSource(t *testing.T) {
	for _, mutation := range []string{"none", "owner", "source", "lazy-digest", "child-binding", "missing-context"} {
		t.Run(mutation, func(t *testing.T) {
			scopes, steps := scopedExecutorFixture(t)
			child := steps[1]
			flow := []schema.FlowNode{{Step: &schema.Step{ID: child.ID, Type: schema.StepTypeTool,
				LexicalScopeID: child.LexicalScopeID, ToolBindingID: child.ToolBindingID,
				ToolCall: child.Spec.(*schema.ToolCallSpec)}}}
			loader := &capturedIncludeFixture{loaded: &LoadedRunbook{
				RootScopeID: child.LexicalScopeID, SourceDigest: "right", Flow: flow,
			}}
			spec := &schema.IncludeSpec{TargetScopeID: child.LexicalScopeID,
				LazyRunbookPath: "deleted.runbook.yaml", LazyRunbookDigest: "right"}
			ctx := engine.WithToolScopes(context.Background(), scopes)
			switch mutation {
			case "owner":
				loader.loaded.RootScopeID = steps[0].LexicalScopeID
			case "source":
				loader.loaded.SourceDigest = "edited"
			case "lazy-digest":
				spec.LazyRunbookDigest = "edited"
			case "child-binding":
				flow[0].Step.ToolBindingID = steps[0].ToolBindingID
			case "missing-context":
				ctx = context.Background()
			}
			executor := NewIncludeExecutor(nil, nil, loader)
			got, err := executor.loadScopedRunbook(ctx, spec)
			if mutation == "none" {
				if err != nil || got != loader.loaded {
					t.Fatalf("captured load = %v, %v", got, err)
				}
			} else if err == nil {
				t.Fatalf("accepted %s", mutation)
			}
			if loader.legacy != 0 {
				t.Fatal("scoped loading fell back to mutable filesystem")
			}
		})
	}
}
