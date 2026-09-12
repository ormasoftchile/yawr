package executor

import (
	"context"
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

func TestNestedSubstitutionDefaultsDoNotInheritParentBindings(t *testing.T) {
	for _, frozen := range []bool{false, true} {
		t.Run(map[bool]string{false: "mutable", true: "frozen"}[frozen], func(t *testing.T) {
			dir := t.TempDir()
			parsed := &mappedSubstitutionParser{parsed: map[string]*parser.ParsedRunbook{}}
			def := &schema.ToolDef{Name: "nested-defaults", Actions: map[string]*schema.ToolAction{}}
			runtimeDef := &tool.ToolDef{Name: def.Name, SourcePath: filepath.Join(dir, "tool.yaml"), PackageRoot: dir, Actions: map[string]*tool.ToolAction{}}
			for _, name := range []string{"outer", "inner"} {
				path := filepath.Join(dir, name+".runbook.yaml")
				if err := os.WriteFile(path, []byte("id: fixture\n"), 0600); err != nil {
					t.Fatal(err)
				}
				action := &schema.ToolAction{Args: map[string]*schema.ArgDef{"flag": {Type: "boolean", Default: false}},
					Execute: &schema.ExecuteSpec{Kind: "runbook", Path: name + ".runbook.yaml"}}
				sub := &schema.Runbook{ID: name, Name: name, Inputs: map[string]*schema.Input{"flag": {Type: "boolean", Default: false}},
					Flow: []schema.FlowNode{{Step: &schema.Step{ID: name, Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{}}}}}
				parsed.parsed[filepath.Base(path)] = &parser.ParsedRunbook{Source: path, Runbook: sub}
				if frozen {
					closure, err := plansnapshot.EncodeFlowClosure(sub.Flow)
					if err != nil {
						t.Fatal(err)
					}
					action.FrozenSubstitution = &schema.FrozenToolSubstitution{RunbookID: name, RunbookPath: path,
						RunbookContentHash: strings.Repeat("a", 64), Inputs: sub.Inputs, ExecutableClosure: closure}
				}
				def.Actions[name] = action
				runtimeDef.Actions[name] = (&tool.ToolAction{Execute: action.Execute}).WithSchemaAction(action)
			}
			rt := testutil.NewFakeToolRuntime()
			rt.RegisterDef(def.Name, runtimeDef)
			calls := 0
			var executor *ToolExecutor
			runner := func(ctx context.Context, parent SubStepParent, _ []schema.FlowNode, vars map[string]any) ([]*engine.StepResult, error) {
				calls++
				want := parent.ID == "outer"
				if len(vars) != 1 || vars["flag"] != want {
					t.Fatalf("child %s bindings=%#v", parent.ID, vars)
				}
				if parent.ID == "inner" {
					return nil, nil
				}
				result, err := executor.Execute(ctx, engine.ResolvedStep{ID: "inner", Spec: &schema.ToolCallSpec{
					Tool: schema.ToolInvocation{Name: def.Name, Action: "inner", Args: map[string]any{}},
				}}, vars)
				if err != nil {
					return nil, err
				}
				return []*engine.StepResult{result}, nil
			}
			executor = NewToolExecutorWithSubstitution(rt, &internalexpr.TemplateEvaluator{}, runner, parsed, nil)
			ctx := context.Background()
			if frozen {
				ctx = engine.WithPlanTools(ctx, map[string]*schema.ToolDef{def.Name: def})
			}
			result, err := executor.Execute(ctx, engine.ResolvedStep{ID: "outer", Spec: &schema.ToolCallSpec{
				Tool: schema.ToolInvocation{Name: def.Name, Action: "outer", Args: map[string]any{"flag": true}},
			}}, map[string]any{"unrelated": "caller"})
			if err != nil || result.Error != nil || calls != 2 {
				t.Fatalf("result=%#v err=%v calls=%d", result, err, calls)
			}
		})
	}
}
