package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	internalexpr "github.com/ormasoftchile/yawr/runtime/internal/expr"
	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/testutil"
	"github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

type rejectingSubstitutionParser struct{ calls int }

func (parserImpl *rejectingSubstitutionParser) Parse(context.Context, string) (*parser.ParsedRunbook, error) {
	parserImpl.calls++
	return nil, errors.New("mutable parser called")
}

type fixedSubstitutionParser struct {
	calls  int
	parsed *parser.ParsedRunbook
}

type mappedSubstitutionParser struct {
	parsed map[string]*parser.ParsedRunbook
}

func (parserImpl *mappedSubstitutionParser) Parse(_ context.Context, path string) (*parser.ParsedRunbook, error) {
	parsed := parserImpl.parsed[filepath.Clean(path)]
	if parsed == nil {
		parsed = parserImpl.parsed[filepath.Base(path)]
	}
	if parsed == nil {
		return nil, errors.New("unexpected mutable path: " + path)
	}
	return parsed, nil
}

func (parserImpl *mappedSubstitutionParser) ParseBytes(context.Context, []byte) (*parser.ParsedRunbook, error) {
	return nil, errors.New("unexpected ParseBytes")
}

func (parserImpl *fixedSubstitutionParser) Parse(context.Context, string) (*parser.ParsedRunbook, error) {
	parserImpl.calls++
	return parserImpl.parsed, nil
}

func (parserImpl *fixedSubstitutionParser) ParseBytes(context.Context, []byte) (*parser.ParsedRunbook, error) {
	parserImpl.calls++
	return parserImpl.parsed, nil
}

func (parserImpl *rejectingSubstitutionParser) ParseBytes(context.Context, []byte) (*parser.ParsedRunbook, error) {
	parserImpl.calls++
	return nil, errors.New("mutable parser called")
}

func TestToolExecutorExecutesFrozenSubstitutionWithoutParserOrRuntime(t *testing.T) {
	closure, err := plansnapshot.EncodeFlowClosure([]schema.FlowNode{{Step: &schema.Step{
		ID: "saved-child", Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{},
	}}})
	if err != nil {
		t.Fatalf("EncodeFlowClosure: %v", err)
	}
	parserImpl := &rejectingSubstitutionParser{}
	runnerCalls := 0
	runner := func(_ context.Context, parent SubStepParent, nodes []schema.FlowNode, _ map[string]any) ([]*engine.StepResult, error) {
		runnerCalls++
		if parent.Kind != "tool-substitution" || len(nodes) != 1 || nodes[0].Step == nil || nodes[0].Step.ID != "saved-child" {
			t.Fatalf("frozen runner input = %#v/%#v", parent, nodes)
		}
		return []*engine.StepResult{{
			StepID: "saved-child", Status: engine.StepStatusCompleted, Outcome: engine.StepOutcomeSuccess,
		}}, nil
	}
	executor := NewToolExecutorWithSubstitution(nil, &internalexpr.TemplateEvaluator{}, runner, parserImpl, nil)
	definition := &schema.ToolDef{
		Name: "saved-tool",
		Actions: map[string]*schema.ToolAction{"run": {
			Execute: &schema.ExecuteSpec{Kind: "runbook", Path: "mutable.runbook.yaml"},
			FrozenSubstitution: &schema.FrozenToolSubstitution{
				PackageName: "saved-package", RunbookPath: "saved.runbook.yaml",
				RunbookID: "saved", RunbookName: "Saved", RunbookContentHash: strings.Repeat("a", 64),
				ExecutableClosure: closure,
			},
		}},
	}
	result, err := executor.ExecuteFrozenSubstitution(context.Background(), engine.ResolvedStep{
		ID: "substitute", Kind: "tool", Spec: &schema.ToolCallSpec{Tool: schema.ToolInvocation{
			Name: "saved-tool", Action: "run",
		}},
	}, nil, definition)
	if err != nil {
		t.Fatalf("ExecuteFrozenSubstitution: %v", err)
	}
	if result.Status != engine.StepStatusCompleted || runnerCalls != 1 || parserImpl.calls != 0 {
		t.Fatalf("frozen result/runner/parser = %#v/%d/%d", result, runnerCalls, parserImpl.calls)
	}
}

func TestMaterializeToolSubstitutionsCapturesExecutableClosure(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "child.runbook.yaml"), []byte("id: child\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	action := &schema.ToolAction{Execute: &schema.ExecuteSpec{Kind: "runbook", Path: "child.runbook.yaml"}}
	runtimeAction := (&tool.ToolAction{Execute: action.Execute}).WithSchemaAction(action)
	runtime := testutil.NewFakeToolRuntime()
	runtime.RegisterDef("saved-tool", &tool.ToolDef{
		Name: "saved-tool", SourcePath: filepath.Join(directory, "saved.tool.yaml"), PackageRoot: directory,
		PackageName: "saved-package", Actions: map[string]*tool.ToolAction{"run": runtimeAction},
	})
	parserImpl := &fixedSubstitutionParser{parsed: &parser.ParsedRunbook{
		Source: filepath.Join(directory, "child.runbook.yaml"),
		Runbook: &schema.Runbook{
			ID: "child", Name: "Child",
			Flow: []schema.FlowNode{{Step: &schema.Step{ID: "saved-child", Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{}}}},
		},
	}}
	registry := NewMapRegistry()
	registry.Register("tool", NewToolExecutorWithSubstitution(runtime, &internalexpr.TemplateEvaluator{}, nil, parserImpl, nil))
	planAction := &schema.ToolAction{Execute: &schema.ExecuteSpec{Kind: "runbook", Path: "child.runbook.yaml"}}
	plan := &engine.ExecutionPlan{Tools: map[string]*schema.ToolDef{
		"saved-tool": {Name: "saved-tool", Actions: map[string]*schema.ToolAction{"run": planAction}},
	}}
	if err := MaterializeToolSubstitutions(context.Background(), registry, plan); err != nil {
		t.Fatalf("MaterializeToolSubstitutions: %v", err)
	}
	if planAction.FrozenSubstitution != nil {
		t.Fatal("materialization mutated the supplied definition")
	}
	planAction = plan.Tools["saved-tool"].Actions["run"]
	if parserImpl.calls != 1 || planAction.FrozenSubstitution == nil {
		t.Fatalf("parser calls/frozen substitution = %d/%#v", parserImpl.calls, planAction.FrozenSubstitution)
	}
	if err := plansnapshot.ValidateFrozenToolSubstitution(planAction.FrozenSubstitution); err != nil {
		t.Fatalf("ValidateFrozenToolSubstitution: %v", err)
	}
}

func TestToolExecutorNormalDispatchUsesFrozenSubstitution(t *testing.T) {
	closure, err := plansnapshot.EncodeFlowClosure([]schema.FlowNode{{Step: &schema.Step{
		ID: "saved-operation", Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	parserImpl := &rejectingSubstitutionParser{}
	calls := 0
	runner := func(_ context.Context, _ SubStepParent, nodes []schema.FlowNode, _ map[string]any) ([]*engine.StepResult, error) {
		calls++
		if len(nodes) != 1 || nodes[0].Step.ID != "saved-operation" {
			t.Fatalf("wrong executable body: %#v", nodes)
		}
		return []*engine.StepResult{{StepID: "saved-operation", Status: engine.StepStatusCompleted}}, nil
	}
	definition := &schema.ToolDef{Name: "saved", Actions: map[string]*schema.ToolAction{
		"run": {
			Execute: &schema.ExecuteSpec{Kind: "runbook", Path: "no-longer-present.runbook.yaml"},
			FrozenSubstitution: &schema.FrozenToolSubstitution{
				PackageName: "saved-package", RunbookPath: "saved.runbook.yaml", RunbookID: "saved",
				RunbookContentHash: strings.Repeat("a", 64), ExecutableClosure: closure,
			},
		},
	}}
	ctx := engine.WithPlanTools(context.Background(), map[string]*schema.ToolDef{"saved": definition})
	executor := NewToolExecutorWithSubstitution(nil, &internalexpr.TemplateEvaluator{}, runner, parserImpl, nil)
	step := engine.ResolvedStep{ID: "call", Kind: "tool", Spec: &schema.ToolCallSpec{
		Tool: schema.ToolInvocation{Name: "saved", Action: "run"},
	}}
	pure, err := executor.IsPureRunbookSubstitution(ctx, step, nil)
	if err != nil || !pure {
		t.Fatalf("frozen substitution classified as transport: %v, %v", pure, err)
	}
	result, err := executor.Execute(ctx, step, nil)
	if err != nil || result.Status != engine.StepStatusCompleted || calls != 1 || parserImpl.calls != 0 {
		t.Fatalf("normal dispatch = %#v, %v; body calls=%d parser calls=%d", result, err, calls, parserImpl.calls)
	}
}

func TestMaterializeToolSubstitutionsCapturesStaticIncludeClosure(t *testing.T) {
	directory := t.TempDir()
	childPath := filepath.Join(directory, "child.runbook.yaml")
	grandchildPath := filepath.Join(directory, "grandchild.runbook.yaml")
	for _, path := range []string{childPath, grandchildPath} {
		if err := os.WriteFile(path, []byte("id: fixture\n"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	action := &schema.ToolAction{Execute: &schema.ExecuteSpec{Kind: "runbook", Path: "child.runbook.yaml"}}
	runtime := testutil.NewFakeToolRuntime()
	runtime.RegisterDef("saved-tool", &tool.ToolDef{
		Name: "saved-tool", SourcePath: filepath.Join(directory, "saved.tool.yaml"), PackageRoot: directory,
		PackageName: "saved-package", Actions: map[string]*tool.ToolAction{
			"run": (&tool.ToolAction{Execute: action.Execute}).WithSchemaAction(action),
		},
	})
	parserImpl := &mappedSubstitutionParser{parsed: map[string]*parser.ParsedRunbook{
		filepath.Base(childPath): {
			Source: childPath, Runbook: &schema.Runbook{ID: "child", Name: "Child", Flow: []schema.FlowNode{{Step: &schema.Step{
				ID: "include-grandchild", Type: schema.StepTypeInclude,
				IncludeSpec: &schema.IncludeSpec{Include: schema.IncludeConfig{Runbook: "grandchild.runbook.yaml"}},
			}}}},
		},
		filepath.Base(grandchildPath): {
			Source: grandchildPath, Runbook: &schema.Runbook{ID: "grandchild", Name: "Grandchild", Flow: []schema.FlowNode{{Step: &schema.Step{
				ID: "saved-grandchild", Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{},
			}}}},
		},
	}}
	registry := NewMapRegistry()
	registry.Register("tool", NewToolExecutorWithSubstitution(runtime, &internalexpr.TemplateEvaluator{}, nil, parserImpl, nil))
	planAction := &schema.ToolAction{Execute: &schema.ExecuteSpec{Kind: "runbook", Path: "child.runbook.yaml"}}
	plan := &engine.ExecutionPlan{Tools: map[string]*schema.ToolDef{
		"saved-tool": {Name: "saved-tool", Actions: map[string]*schema.ToolAction{"run": planAction}},
	}}
	if err := MaterializeToolSubstitutions(context.Background(), registry, plan); err != nil {
		t.Fatalf("MaterializeToolSubstitutions: %v", err)
	}
	flow, err := plansnapshot.RestoreFlowClosure(plan.Tools["saved-tool"].Actions["run"].FrozenSubstitution.ExecutableClosure)
	if err != nil {
		t.Fatalf("RestoreFlowClosure: %v", err)
	}
	include := flow[0].Step.IncludeSpec
	if include == nil || include.ResolvedRunbookPath != grandchildPath || len(include.ResolvedSteps) != 1 ||
		include.ResolvedSteps[0].Step == nil || include.ResolvedSteps[0].Step.ID != "saved-grandchild" || include.LazyRunbookPath != "" {
		t.Fatalf("captured static include = %#v", include)
	}
}
