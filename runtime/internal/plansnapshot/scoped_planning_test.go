package plansnapshot

import (
	"context"
	"fmt"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	plannerpkg "github.com/ormasoftchile/yawr/runtime/pkg/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/tool"
	"github.com/ormasoftchile/yawr/runtime/pkg/toolscope"
)

type scopedPlanningLoader struct{ child *parser.ParsedRunbook }

func (loader scopedPlanningLoader) Load(context.Context, string) (*parser.ParsedRunbook, error) {
	return loader.child, nil
}

type forbiddenPlanningRegistry struct{ calls int }

func (registry *forbiddenPlanningRegistry) Lookup(context.Context, string, string) (*schema.ToolDef, error) {
	registry.calls++
	return nil, fmt.Errorf("name-only lookup forbidden")
}

func TestPlannerConsumesFrozenScopesWithoutRegistryLookup(t *testing.T) {
	fixture, childID := scopedFixture(t)
	tables := fixture.ToolScopes.Export()
	for id, definition := range tables.Definitions {
		definition.Declaration = &schema.ToolDef{Name: definition.Runtime.Name, Actions: map[string]*schema.ToolAction{"inspect": {
			Args: map[string]*schema.ArgDef{"target": {Type: "string", Enum: schema.EnumConstraint{definition.Runtime.Name}}},
		}}}
		delete(tables.Definitions, id)
		// Adding the retained declaration changes identity before sealing.
		tables.Definitions[id] = definition
	}
	// Rebuild affected definition/binding IDs without changing authored names.
	definitions := tables.Definitions
	tables.Definitions = make(map[string]tool.BoundDefinition)
	for oldID, definition := range definitions {
		newID, err := tool.DefinitionID(definition)
		if err != nil {
			t.Fatal(err)
		}
		tables.Definitions[newID] = definition
		for oldBinding, binding := range tables.Bindings {
			if binding.DefinitionID != oldID {
				continue
			}
			delete(tables.Bindings, oldBinding)
			binding.DefinitionID = newID
			newBinding := tool.BindingID(binding.ScopeID, binding.LogicalName, newID)
			tables.Bindings[newBinding] = binding
			scope := tables.Scopes[binding.ScopeID]
			scope.Bindings[binding.LogicalName] = newBinding
			tables.Scopes[binding.ScopeID] = scope
		}
	}
	scopes, err := toolscope.New(tables)
	if err != nil {
		t.Fatal(err)
	}
	view, err := scopes.Declarations(childID)
	if err != nil || len(view) != 1 || view["query"].Name != "child" {
		t.Fatalf("scope-local declaration view: %v", err)
	}
	view["query"].Name = "mutated"
	again, err := scopes.Declarations(childID)
	if err != nil || again["query"].Name != "child" {
		t.Fatal("declaration view mutated frozen scope")
	}
	childStep := &schema.Step{ID: "inspect", Type: schema.StepTypeTool, LexicalScopeID: childID, ToolCall: &schema.ToolCallSpec{Tool: schema.ToolInvocation{Name: "query", Action: "inspect", Args: map[string]any{"target": "child"}}}}
	child := &parser.ParsedRunbook{Source: "child.yaml", Runbook: &schema.Runbook{APIVersion: "yawr.runbook/v1", ID: "child", Name: "Child", LexicalScopeID: childID,
		Flow: []schema.FlowNode{{Iterate: &schema.IterateNode{ID: "loop", Over: "${items}", As: "item", Steps: []schema.FlowNode{{Step: childStep}}}}}}}
	root := &parser.ParsedRunbook{Source: "parent.yaml", Runbook: &schema.Runbook{APIVersion: "yawr.runbook/v1", ID: "parent", Name: "Parent", LexicalScopeID: fixture.RootScopeID,
		Flow: []schema.FlowNode{{Step: &schema.Step{ID: "include-child", Type: schema.StepTypeInclude, LexicalScopeID: fixture.RootScopeID,
			IncludeSpec: &schema.IncludeSpec{TargetScopeID: childID, Include: schema.IncludeConfig{Runbook: "child.yaml", Expand: "eager"}}}}}}}
	registry := &forbiddenPlanningRegistry{}
	instance := planner.New(plannerpkg.Config{Loader: scopedPlanningLoader{child}, Tools: registry})
	plan, err := instance.Plan(engine.WithToolScopes(context.Background(), scopes), root)
	if err != nil {
		t.Fatal(err)
	}
	if registry.calls != 0 || len(plan.Tools) != 0 || plan.ToolScopes == nil {
		t.Fatal("scoped planning used legacy tool view")
	}
	if len(plan.Steps) < 3 || plan.Steps[2].ToolBindingID == "" {
		t.Fatalf("binding not materialized: %+v", plan.Steps)
	}
	if len(plan.Steps) != 3 || plan.Steps[1].LexicalScopeID != childID {
		t.Fatalf("iterate ownership lost: %+v", plan.Steps)
	}
	if err := planner.FinalizeMaterializedPlan(plan); err != nil {
		t.Fatal(err)
	}
	if plan.Steps[1].LexicalScopeID != childID {
		t.Fatal("materialization lost structural owner")
	}
	if _, err := FromExecutionPlan(plan); err != nil {
		t.Fatal(err)
	}
	childStep.ToolCall.Tool.Args["target"] = "parent"
	if _, err := instance.Plan(engine.WithToolScopes(context.Background(), scopes), root); err == nil {
		t.Fatal("child enum validation used root definition")
	}
	childStep.ToolCall.Tool.Args["target"] = "child"
	childStep.ToolCall.Tool.Name = "root-private"
	if _, err := instance.Plan(engine.WithToolScopes(context.Background(), scopes), root); err == nil {
		t.Fatal("missing child alias accepted")
	}
	if registry.calls != 0 {
		t.Fatal("missing binding fell back to registry")
	}
}
