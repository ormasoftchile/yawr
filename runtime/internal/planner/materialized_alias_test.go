package planner_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/expand"
	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	plannerpkg "github.com/ormasoftchile/yawr/runtime/pkg/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestMaterializedIncludeAliasDoesNotGuessUnownedOrAmbiguousMetadata(t *testing.T) {
	for _, name := range []string{"ambiguous", "different-source", "missing-owner"} {
		t.Run(name, func(t *testing.T) {
			authored := &schema.Step{ID: "call", Type: schema.StepTypeInclude,
				IncludeSpec: &schema.IncludeSpec{Include: schema.IncludeConfig{Runbook: "leaf.yaml"}, ResolvedRunbookPath: "leaf.yaml"}}
			root := engine.ResolvedStep{ID: "loop", Kind: "iterate", Origin: "root.yaml",
				Spec: &schema.IterateNode{ID: "loop", Steps: []schema.FlowNode{{Step: authored}}}}
			old := engine.ResolvedStep{ID: "call", Kind: "include", Origin: "root.yaml", IncludeAlias: "check",
				Depth: 1, ParentID: "loop", ParentKind: "iterate", Spec: authored.IncludeSpec}
			plan := &engine.ExecutionPlan{Steps: []engine.ResolvedStep{root, old}}
			if err := planner.ValidateExecutionPlan(plan); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "ambiguous":
				old.IncludeAlias = "other"
				plan.Steps = append(plan.Steps, old)
			case "different-source":
				plan.Steps[1].Origin = "other.yaml"
			case "missing-owner":
				plan.Steps[0].Origin, plan.Steps[1].Origin = "", ""
			}
			if err := planner.FinalizeMaterializedPlan(plan); err != nil {
				t.Fatal(err)
			}
			if len(plan.Steps) != 2 || plan.Steps[1].IncludeAlias != "" || authored.IncludeAlias != "" {
				t.Fatalf("guessed include alias without unambiguous local ownership: %+v", plan.Steps)
			}
		})
	}
}

func TestMaterializedIncludeAliasSurvivesEagerAndLazyPlanning(t *testing.T) {
	for _, mode := range []expand.Mode{expand.ModeEager, expand.ModeLazy} {
		t.Run(string(mode), func(t *testing.T) {
			base, err := filepath.Abs("alias-source")
			if err != nil {
				t.Fatal(err)
			}
			childPath := filepath.Join(base, "child.yaml")
			child := &parser.ParsedRunbook{Source: childPath, Runbook: &schema.Runbook{ID: "child", Name: "Child",
				Flow: []schema.FlowNode{{Step: &schema.Step{ID: "leaf", Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{}}}},
			}}
			root := &parser.ParsedRunbook{Source: filepath.Join(base, "root.yaml"), Runbook: &schema.Runbook{ID: "root", Name: "Root",
				Imports: map[string]string{"check": "child.yaml"},
				Flow: []schema.FlowNode{{Iterate: &schema.IterateNode{ID: "loop", Over: "${items}", As: "item",
					Steps: []schema.FlowNode{{Step: &schema.Step{ID: "check_host", Type: schema.StepTypeInclude,
						IncludeSpec: &schema.IncludeSpec{Include: schema.IncludeConfig{Runbook: "child.yaml"}}}}},
				}}},
			}}
			loader := &fakeLoader{runbooks: map[string]*parser.ParsedRunbook{childPath: child}}
			instance := planner.New(plannerpkg.Config{Loader: loader, Tools: &fakeRegistry{}, ExpandPolicy: expand.Policy{Default: mode}})
			plan, err := instance.Plan(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			authored := plan.Steps[0].Spec.(*schema.IterateNode).Steps[0].Step
			if authored.IncludeAlias != "check" {
				t.Fatal("source-local alias was not retained on parsed flow")
			}
			if mode == expand.ModeLazy {
				authored.IncludeSpec.ResolvedSteps = child.Runbook.Flow
				authored.IncludeSpec.ResolvedRunbookPath = childPath
				authored.IncludeSpec.LazyRunbookPath = ""
			}
			loader.runbooks = nil
			root.Runbook.Imports = nil
			if err := planner.FinalizeMaterializedPlan(plan); err != nil {
				t.Fatal(err)
			}
			if len(plan.Steps) != 3 || plan.Steps[1].ID != "check_host" || plan.Steps[1].IncludeAlias != "check" {
				t.Fatalf("materialization erased alias or rewrote IDs: %+v", plan.Steps)
			}
			if plan.Steps[2].IncludeAlias != "" {
				t.Fatal("include label leaked into child execution")
			}
			authored.IncludeAlias = ""
			if err := planner.FinalizeMaterializedPlan(plan); err != nil {
				t.Fatal(err)
			}
			if plan.Steps[1].IncludeAlias != "check" || authored.IncludeAlias != "check" {
				t.Fatal("available owned metadata from an older flat snapshot was erased")
			}
		})
	}
}
