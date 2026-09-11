package executor

import (
	"context"
	"testing"

	internalexpr "github.com/ormasoftchile/yawr/runtime/internal/expr"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/testutil"
	"github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

func TestActionDefaultsReachNativeInvocationWithoutSharingValues(t *testing.T) {
	action := &schema.ToolAction{Args: map[string]*schema.ArgDef{
		"hash":     {Type: "string", Default: ""},
		"verified": {Type: "boolean", Default: false},
		"payload":  {Type: "object", Default: map[string]any{"values": []any{int64(0)}}},
	}}
	rt := testutil.NewFakeToolRuntime()
	rt.RegisterDef("native", &tool.ToolDef{Name: "native", Actions: map[string]*tool.ToolAction{
		"run": (&tool.ToolAction{}).WithSchemaAction(action),
	}})
	executor := NewToolExecutorWithSubstitution(rt, &internalexpr.TemplateEvaluator{}, nil, nil, nil)
	for i := 0; i < 2; i++ {
		result, err := executor.Execute(context.Background(), engine.ResolvedStep{ID: "call", Spec: &schema.ToolCallSpec{
			Tool: schema.ToolInvocation{Name: "native", Action: "run"},
		}}, nil)
		if err != nil || result.Error != nil {
			t.Fatalf("result=%#v err=%v", result, err)
		}
		args := rt.Calls[i].Args
		if args["hash"] != "" || args["verified"] != false ||
			args["payload"].(map[string]any)["values"].([]any)[0] != int64(0) {
			t.Fatalf("defaults=%#v", args)
		}
		args["payload"].(map[string]any)["values"].([]any)[0] = int64(1)
	}
	if action.Args["payload"].Default.(map[string]any)["values"].([]any)[0] != int64(0) {
		t.Fatal("native request mutated its declared default")
	}
}
