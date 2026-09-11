package executor

import (
	"context"
	"reflect"
	"strings"
	"testing"

	internalexpr "github.com/ormasoftchile/yawr/runtime/internal/expr"
	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/testutil"
	"github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

func TestMaterializeAlreadyFrozenTransitiveDependencies(t *testing.T) {
	frozen := func(id, child string) *schema.ToolAction {
		t.Helper()
		closure, err := plansnapshot.EncodeFlowClosure([]schema.FlowNode{{Step: &schema.Step{
			ID: id, Type: schema.StepTypeTool, ToolCall: &schema.ToolCallSpec{Tool: schema.ToolInvocation{Name: child, Action: "run"}},
		}}})
		if err != nil {
			t.Fatal(err)
		}
		return &schema.ToolAction{Execute: &schema.ExecuteSpec{Kind: "runbook", Path: id + ".runbook.yaml"},
			FrozenSubstitution: &schema.FrozenToolSubstitution{RunbookID: id, RunbookPath: id + ".runbook.yaml",
				RunbookContentHash: strings.Repeat("a", 64), ExecutableClosure: closure}}
	}
	root := &schema.ToolDef{Name: "root", Actions: map[string]*schema.ToolAction{"run": frozen("root", "middle")}}
	middle := frozen("middle", "leaf")
	beforeRoot := schema.CloneToolDef(root)
	beforeMiddle := schema.CloneToolAction(middle)
	rt := testutil.NewFakeToolRuntime()
	for name, action := range map[string]*schema.ToolAction{
		"middle": middle, "leaf": {}, "unrelated": {},
	} {
		rt.RegisterDef(name, &tool.ToolDef{Name: name, Actions: map[string]*tool.ToolAction{
			"run": (&tool.ToolAction{Execute: action.Execute}).WithSchemaAction(action),
		}})
	}
	parser := &rejectingSubstitutionParser{}
	registry := NewMapRegistry()
	registry.Register("tool", NewToolExecutorWithSubstitution(rt, &internalexpr.TemplateEvaluator{}, nil, parser, nil))
	for i := 0; i < 3; i++ {
		plan := &engine.ExecutionPlan{Tools: map[string]*schema.ToolDef{"root": root}}
		if missing, err := validateFrozenToolSubstitutions(plan); err != nil || !missing {
			t.Fatalf("negative control: missing=%v err=%v", missing, err)
		}
		if err := MaterializeToolSubstitutions(context.Background(), nil, plan); err == nil {
			t.Fatal("incomplete saved closure accepted without a registry")
		}
		if err := MaterializeToolSubstitutions(context.Background(), registry, plan); err != nil {
			t.Fatal(err)
		}
		if len(plan.Tools) != 3 || plan.Tools["root"] == root || plan.Tools["middle"].Actions["run"] == middle ||
			plan.Tools["leaf"] == nil || plan.Tools["unrelated"] != nil {
			t.Fatalf("wrong owned dependency closure: %#v", plan.Tools)
		}
		if missing, err := validateFrozenToolSubstitutions(plan); err != nil || missing {
			t.Fatalf("incomplete closure: missing=%v err=%v", missing, err)
		}
		// Complete saved plans remain executable without any registry or source.
		if err := MaterializeToolSubstitutions(context.Background(), nil, plan); err != nil {
			t.Fatal(err)
		}
	}
	if parser.calls != 0 || !reflect.DeepEqual(beforeRoot, root) || !reflect.DeepEqual(beforeMiddle, middle) {
		t.Fatal("frozen dependency resolution read mutable source or mutated shared definitions")
	}
}
