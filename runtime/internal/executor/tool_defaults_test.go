package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
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

func TestSubstitutionActionDefaults(t *testing.T) {
	for _, frozen := range []bool{false, true} {
		for _, tc := range []struct {
			name    string
			args    map[string]any
			mutate  func(*schema.ToolAction, *schema.Runbook)
			want    map[string]any
			failure string
		}{
			{name: "omitted", want: map[string]any{"hash": "", "verified": false, "role": "0"}},
			{name: "explicit-zero", args: map[string]any{"hash": "", "verified": false, "role": ""}, want: map[string]any{"hash": "", "verified": false, "role": ""}},
			{name: "explicit-values", args: map[string]any{"hash": "synthetic", "verified": true}, want: map[string]any{"hash": "synthetic", "verified": true, "role": "0"}},
			{name: "action-wins", mutate: func(_ *schema.ToolAction, sub *schema.Runbook) { sub.Inputs["hash"].Default = "backing" }, want: map[string]any{"hash": "", "verified": false, "role": "0"}},
			{name: "backing-not-fallback", mutate: func(action *schema.ToolAction, _ *schema.Runbook) { action.Args["hash"].Default = nil }, want: map[string]any{"verified": false, "role": "0"}},
			{name: "required-action", mutate: func(action *schema.ToolAction, sub *schema.Runbook) {
				action.Args["hash"].Default = nil
				action.Args["hash"].Required = true
				sub.Inputs["hash"].Required = true
			}, failure: "required argument"},
			{name: "required-backing", mutate: func(action *schema.ToolAction, sub *schema.Runbook) {
				action.Args["hash"].Default = nil
				sub.Inputs["hash"].Required = true
			}, failure: "required substitute input"},
			{name: "malformed-bool", args: map[string]any{"verified": "not-a-boolean"}, failure: "requires boolean"},
			{name: "malformed-default", mutate: func(action *schema.ToolAction, _ *schema.Runbook) {
				action.Args["verified"].Default = "false"
			}, failure: "requires boolean"},
			{name: "string-bool-is-not-native", args: map[string]any{"verified": "false"}, failure: "requires boolean"},
			{name: "explicit-null-not-default", args: map[string]any{"verified": nil}, failure: "incompatible"},
			{name: "enum-default", mutate: func(action *schema.ToolAction, sub *schema.Runbook) {
				action.Args["role"].Enum = schema.EnumConstraint{"0", "1"}
				sub.Inputs["role"].Enum = schema.EnumConstraint{"0", "1"}
			}, want: map[string]any{"hash": "", "verified": false, "role": "0"}},
			{name: "invalid-enum-default", mutate: func(action *schema.ToolAction, sub *schema.Runbook) {
				action.Args["role"].Enum = schema.EnumConstraint{"1"}
				sub.Inputs["role"].Enum = schema.EnumConstraint{"1"}
			}, failure: "ENUM-008"},
		} {
			t.Run(tc.name+map[bool]string{false: "/mutable", true: "/saved"}[frozen], func(t *testing.T) {
				directory := t.TempDir()
				path := filepath.Join(directory, "child.runbook.yaml")
				if err := os.WriteFile(path, []byte("id: child\n"), 0600); err != nil {
					t.Fatal(err)
				}

				action := &schema.ToolAction{Execute: &schema.ExecuteSpec{Kind: "runbook", Path: "child.runbook.yaml"},
					Args: map[string]*schema.ArgDef{
						"hash": {Type: "string", Default: ""}, "verified": {Type: "boolean", Default: false}, "role": {Type: "string", Default: "0"},
					}}
				sub := &schema.Runbook{ID: "child", Name: "Child", Inputs: map[string]*schema.Input{
					"hash": {Type: "string", Default: ""}, "verified": {Type: "boolean", Default: false}, "role": {Type: "string", Default: "0"},
				}, Flow: []schema.FlowNode{{Step: &schema.Step{ID: "body", Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{}}}}}
				if tc.mutate != nil {
					tc.mutate(action, sub)
				}
				def := &schema.ToolDef{Name: "defaults", Actions: map[string]*schema.ToolAction{"run": action}}
				rt := testutil.NewFakeToolRuntime()
				rt.RegisterDef("defaults", &tool.ToolDef{Name: "defaults", SourcePath: filepath.Join(directory, "defaults.tool.yaml"), PackageRoot: directory,
					Actions: map[string]*tool.ToolAction{"run": (&tool.ToolAction{Execute: action.Execute}).WithSchemaAction(action)}})
				p := &fixedSubstitutionParser{parsed: &parser.ParsedRunbook{Source: path, Runbook: sub}}
				bodyCalls := 0
				runner := func(_ context.Context, _ SubStepParent, _ []schema.FlowNode, vars map[string]any) ([]*engine.StepResult, error) {
					bodyCalls++
					if !reflect.DeepEqual(vars, tc.want) {
						t.Fatalf("child args=%#v, want %#v", vars, tc.want)
					}
					return nil, nil
				}
				executor := NewToolExecutorWithSubstitution(rt, &internalexpr.TemplateEvaluator{}, runner, p, nil)
				ctx := context.Background()
				if frozen {
					closure, err := plansnapshot.EncodeFlowClosure(sub.Flow)
					if err != nil {
						t.Fatal(err)
					}
					action.FrozenSubstitution = &schema.FrozenToolSubstitution{RunbookID: sub.ID, RunbookPath: path, RunbookContentHash: strings.Repeat("a", 64),
						ExecutableClosure: closure, Inputs: sub.Inputs}
					// A serialization round-trip exercises saved-plan declarations.
					data, err := json.Marshal(def)
					if err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(data, &def); err != nil {
						t.Fatal(err)
					}
					ctx = engine.WithPlanTools(ctx, map[string]*schema.ToolDef{"defaults": def})
				}
				result, err := executor.Execute(ctx, engine.ResolvedStep{ID: "call", Spec: &schema.ToolCallSpec{Tool: schema.ToolInvocation{Name: "defaults", Action: "run", Args: tc.args}}},
					map[string]any{"caller_secret": "must-not-inherit", "hash": "caller-value"})
				if err != nil {
					t.Fatal(err)
				}
				if tc.failure != "" {
					if bodyCalls != 0 || result.Error == nil || !strings.Contains(result.Error.Error(), tc.failure) {
						t.Fatalf("expected %s before body, got %#v; calls=%d", tc.failure, result, bodyCalls)
					}
				} else if result.Error != nil || bodyCalls != 1 {
					t.Fatalf("result=%#v calls=%d", result, bodyCalls)
				}
			})
		}
	}
}
