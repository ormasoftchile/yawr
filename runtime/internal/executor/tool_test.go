package executor

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	internalexpr "github.com/ormasoftchile/yawr/runtime/internal/expr"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/testutil"
	"github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

func TestToolExecutor_Echo_Success(t *testing.T) {
	runtime := testutil.NewFakeToolRuntime()
	runtime.RegisterResult("echo", "echo", &tool.ToolResult{Stdout: "hi"})
	exec := NewToolExecutor(runtime, nil)
	step := engine.ResolvedStep{
		ID:   "step-1",
		Kind: "tool",
		Spec: &schema.ToolCallSpec{Tool: schema.ToolInvocation{Name: "echo", Action: "echo", Args: map[string]any{"message": "hi"}}},
	}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != engine.StepStatusCompleted {
		t.Fatalf("expected completed, got %s", res.Status)
	}
	if res.Output["stdout"] != "hi" {
		t.Fatalf("expected stdout hi, got %v", res.Output["stdout"])
	}
}

func TestToolExecutor_PreparesRenderedDispatchBeforeInvoke(t *testing.T) {
	runtime := testutil.NewFakeToolRuntime()
	committer := &recordingDispatchCommitter{onPrepare: func() {
		if len(runtime.Calls) != 0 {
			t.Fatal("tool invoked before dispatch intent committed")
		}
	}}
	ctx := engine.WithDispatchCommitter(context.Background(), committer)
	step := engine.ResolvedStep{
		ID: "step-1", Kind: "tool",
		Spec: &schema.ToolCallSpec{Tool: schema.ToolInvocation{
			Name: "${tool}", Action: "inspect", Args: map[string]any{"region": "${region}"},
		}},
	}
	if _, err := NewToolExecutor(runtime, &internalexpr.TemplateEvaluator{}).Execute(
		ctx, step, map[string]any{"tool": "queryer", "region": "westus"},
	); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	request, ok := committer.request.RenderedRequest.(map[string]any)
	if committer.calls != 1 || !ok || request["tool"] != "queryer" || request["action"] != "inspect" ||
		!reflect.DeepEqual(request["args"], map[string]any{"region": "westus"}) {
		t.Fatalf("rendered dispatch request = %#v", committer.request.RenderedRequest)
	}
}

func TestToolExecutor_Fail_Error(t *testing.T) {
	runtime := testutil.NewFakeToolRuntime()
	runtime.RegisterError("echo", "fail", errors.New("boom"))
	exec := NewToolExecutor(runtime, nil)
	step := engine.ResolvedStep{
		ID:   "step-1",
		Kind: "tool",
		Spec: &schema.ToolCallSpec{Tool: schema.ToolInvocation{Name: "echo", Action: "fail"}},
	}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != engine.StepStatusFailed {
		t.Fatalf("expected failed, got %s", res.Status)
	}
	if res.Error == nil {
		t.Fatalf("expected error in result")
	}
}

func TestToolExecutor_OutputMapping(t *testing.T) {
	runtime := testutil.NewFakeToolRuntime()
	runtime.RegisterResult("echo", "echo", &tool.ToolResult{
		Stdout:   "out",
		Stderr:   "err",
		ExitCode: 3,
		Output:   map[string]any{"result": "ok"},
	})
	exec := NewToolExecutor(runtime, nil)
	step := engine.ResolvedStep{
		ID:   "step-1",
		Kind: "tool",
		Spec: &schema.ToolCallSpec{Tool: schema.ToolInvocation{Name: "echo", Action: "echo"}},
	}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Output["stdout"] != "out" || res.Output["stderr"] != "err" {
		t.Fatalf("expected stdout/stderr mapping, got %v", res.Output)
	}
	if res.Output["exit_code"] != 3 {
		t.Fatalf("expected exit_code 3, got %v", res.Output["exit_code"])
	}
	if res.Output["result"] != "ok" {
		t.Fatalf("expected merged output, got %v", res.Output["result"])
	}
}

func TestToolExecutor_ErrorWithToolResultPreservesDiagnostics(t *testing.T) {
	exec := NewToolExecutor(errorResultRuntime{
		result: &tool.ToolResult{Stdout: "not-json", Stderr: "diagnostic", ExitCode: 0},
		err:    errors.New("native tool queryer action query: result: stdout must be exactly one JSON object"),
	}, nil)
	step := engine.ResolvedStep{
		ID:   "query-step",
		Kind: "tool",
		Spec: &schema.ToolCallSpec{Tool: schema.ToolInvocation{Name: "queryer", Action: "query"}},
	}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != engine.StepStatusFailed {
		t.Fatalf("status: got %s want failed", res.Status)
	}
	if res.Output["stdout"] != "not-json" || res.Output["stderr"] != "diagnostic" || res.Output["exit_code"] != 0 {
		t.Fatalf("failed result diagnostics not preserved: %#v", res.Output)
	}
}

func TestToolExecutor_QueryResultOutputsAreCapturable(t *testing.T) {
	runtime := testutil.NewFakeToolRuntime()
	runtime.RegisterResult("queryer", "query", &tool.ToolResult{
		Stdout:   `{"success":true,"rowCount":1,"columns":["Column1"],"data":[{"Column1":1}]}`,
		Stderr:   "diagnostic",
		ExitCode: 0,
		Output: map[string]any{
			"success":   true,
			"row_count": int64(1),
			"columns":   []any{"Column1"},
			"rows":      []any{map[string]any{"Column1": int64(1)}},
		},
	})
	runtime.RegisterDef("queryer", &tool.ToolDef{Actions: map[string]*tool.ToolAction{"query": {
		Outputs: map[string]*schema.ArgDef{
			"success":   {Type: "boolean"},
			"row_count": {Type: "integer"},
			"columns":   {Type: "array"},
			"rows":      {Type: "array"},
		},
	}}})
	exec := NewToolExecutor(runtime, nil)
	step := engine.ResolvedStep{
		ID:      "query-step",
		Kind:    "tool",
		Spec:    &schema.ToolCallSpec{Tool: schema.ToolInvocation{Name: "queryer", Action: "query"}},
		Capture: map[string]string{"count": "outputs.row_count", "cols": "outputs.columns"},
	}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != engine.StepStatusCompleted {
		t.Fatalf("status: got %s", res.Status)
	}
	if got := res.Vars["count"]; got != 1 {
		t.Fatalf("captured count: got %#v want 1", got)
	}
	if got, want := res.Vars["cols"], []any{"Column1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("captured columns: got %#v want %#v", got, want)
	}
	if res.Output["stdout"] == "" || res.Output["stderr"] != "diagnostic" || res.Output["exit_code"] != 0 {
		t.Fatalf("process diagnostics not preserved: %#v", res.Output)
	}
}

func TestToolExecutor_ExitCodeCapture_Success(t *testing.T) {
	runtime := testutil.NewFakeToolRuntime()
	runtime.RegisterResult("cmd", "run", &tool.ToolResult{
		Stdout:   "done",
		ExitCode: 2,
	})
	exec := NewToolExecutor(runtime, nil)
	step := engine.ResolvedStep{
		ID:      "step-1",
		Kind:    "tool",
		Spec:    &schema.ToolCallSpec{Tool: schema.ToolInvocation{Name: "cmd", Action: "run"}},
		Capture: map[string]string{"code_snake": "exit_code"},
	}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Vars["code_snake"] != 2 {
		t.Fatalf("exit_code capture: expected 2, got %v", res.Vars["code_snake"])
	}
}

func TestToolExecutor_JSONOutput_InOutput(t *testing.T) {
	runtime := testutil.NewFakeToolRuntime()
	runtime.RegisterResult("json", "emit", &tool.ToolResult{
		Output: map[string]any{"value": "ok"},
	})
	exec := NewToolExecutor(runtime, nil)
	step := engine.ResolvedStep{
		ID:   "step-1",
		Kind: "tool",
		Spec: &schema.ToolCallSpec{Tool: schema.ToolInvocation{Name: "json", Action: "emit"}},
	}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Output["value"] != "ok" {
		t.Fatalf("expected JSON output value, got %v", res.Output["value"])
	}
}

func TestToolExecutor_UnknownTool_Error(t *testing.T) {
	runtime := testutil.NewFakeToolRuntime()
	runtime.RegisterError("missing", "run", errors.New("not found"))
	exec := NewToolExecutor(runtime, nil)
	step := engine.ResolvedStep{
		ID:   "step-1",
		Kind: "tool",
		Spec: &schema.ToolCallSpec{Tool: schema.ToolInvocation{Name: "missing", Action: "run"}},
	}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != engine.StepStatusFailed {
		t.Fatalf("expected failed, got %s", res.Status)
	}
}

func TestToolExecutor_ContextCancel(t *testing.T) {
	exec := NewToolExecutor(blockingRuntime{}, nil)
	step := engine.ResolvedStep{
		ID:   "step-1",
		Kind: "tool",
		Spec: &schema.ToolCallSpec{Tool: schema.ToolInvocation{Name: "slow", Action: "run"}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	res, err := exec.Execute(ctx, step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != engine.StepStatusFailed {
		t.Fatalf("expected failed, got %s", res.Status)
	}
	if res.Error == nil {
		t.Fatalf("expected context error")
	}
}

func TestToolExecutor_ArgTemplate(t *testing.T) {
	runtime := testutil.NewFakeToolRuntime()
	exec := NewToolExecutor(runtime, &internalexpr.TemplateEvaluator{})
	step := engine.ResolvedStep{
		ID:   "step-1",
		Kind: "tool",
		Spec: &schema.ToolCallSpec{Tool: schema.ToolInvocation{Name: "echo", Action: "echo", Args: map[string]any{"message": "hi ${name}"}}},
	}

	if _, err := exec.Execute(context.Background(), step, map[string]any{"name": "sam"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(runtime.Calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(runtime.Calls))
	}
	if runtime.Calls[0].Args["message"] != "hi sam" {
		t.Fatalf("expected templated arg, got %v", runtime.Calls[0].Args["message"])
	}
}

func TestToolExecutor_BuiltinStub(t *testing.T) {
	runtime := testutil.NewFakeToolRuntime()
	runtime.RegisterResult("slack-notify", "send-message", &tool.ToolResult{
		Output: map[string]any{"result": "ok", "tool": "slack-notify"},
	})
	exec := NewToolExecutor(runtime, nil)
	step := engine.ResolvedStep{
		ID:   "step-1",
		Kind: "tool",
		Spec: &schema.ToolCallSpec{Tool: schema.ToolInvocation{Name: "slack-notify", Action: "send-message"}},
	}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Output["tool"] != "slack-notify" {
		t.Fatalf("expected stub output, got %v", res.Output["tool"])
	}
}

type blockingRuntime struct{}

func (blockingRuntime) Invoke(ctx context.Context, _ string, _ string, _ map[string]any) (*tool.ToolResult, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

type errorResultRuntime struct {
	result *tool.ToolResult
	err    error
}

func (r errorResultRuntime) Invoke(context.Context, string, string, map[string]any) (*tool.ToolResult, error) {
	return r.result, r.err
}
