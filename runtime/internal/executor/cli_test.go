package executor

import (
	"context"
	"reflect"
	"testing"

	internalexpr "github.com/ormasoftchile/yawr/runtime/internal/expr"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

type recordingDispatchCommitter struct {
	request   engine.DispatchRequest
	calls     int
	onPrepare func()
}

func (committer *recordingDispatchCommitter) PrepareDispatch(_ context.Context, request engine.DispatchRequest) (engine.DispatchState, error) {
	if committer.onPrepare != nil {
		committer.onPrepare()
	}
	committer.calls++
	committer.request = request
	return engine.DispatchState{
		OccurrenceID:   engine.InteractionPayloadDigest([]byte("executor-dispatch")),
		IdempotencyKey: engine.InteractionPayloadDigest([]byte("executor-idempotency")),
	}, nil
}

func TestCLIExecutor_Success(t *testing.T) {
	fp := platform.NewFakePlatform()
	fp.ExecResults = map[string]*platform.ExecResult{
		"echo": {ExitCode: 0, Stdout: "ok"},
	}
	exec := NewCLIExecutor(fp, nil)
	step := engine.ResolvedStep{ID: "step-1", Kind: "cli", Spec: &schema.CLISpec{Command: "echo"}}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != engine.StepStatusCompleted {
		t.Fatalf("expected completed, got %s", res.Status)
	}
	if res.Output["stdout"] != "ok" {
		t.Fatalf("expected stdout ok, got %v", res.Output["stdout"])
	}
}

func TestCLIExecutor_PreparesRenderedDispatchBeforeExec(t *testing.T) {
	platformFake := platform.NewFakePlatform()
	committer := &recordingDispatchCommitter{onPrepare: func() {
		if len(platformFake.ExecRequests) != 0 {
			t.Fatal("platform executed before dispatch intent committed")
		}
	}}
	ctx := engine.WithDispatchCommitter(context.Background(), committer)
	step := engine.ResolvedStep{ID: "step-1", Kind: "cli", Spec: &schema.CLISpec{
		Command: "${command}", Args: []string{"--region", "${region}"}, Env: map[string]string{"TARGET": "${region}"},
	}}
	if _, err := NewCLIExecutor(platformFake, &internalexpr.TemplateEvaluator{}).Execute(
		ctx, step, map[string]any{"command": "inspect", "region": "westus"},
	); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if committer.calls != 1 {
		t.Fatalf("dispatch prepare calls = %d, want 1", committer.calls)
	}
	request, ok := committer.request.RenderedRequest.(map[string]any)
	if !ok || request["command"] != "inspect" || !reflect.DeepEqual(request["args"], []string{"--region", "westus"}) ||
		!reflect.DeepEqual(request["env"], map[string]string{"TARGET": "westus"}) {
		t.Fatalf("rendered dispatch request = %#v", committer.request.RenderedRequest)
	}
}

func TestCLIExecutor_NonZeroExit(t *testing.T) {
	fp := platform.NewFakePlatform()
	fp.ExecResults = map[string]*platform.ExecResult{
		"fail": {ExitCode: 2, Stderr: "boom"},
	}
	exec := NewCLIExecutor(fp, nil)
	step := engine.ResolvedStep{ID: "step-1", Kind: "cli", Spec: &schema.CLISpec{Command: "fail"}}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != engine.StepStatusFailed {
		t.Fatalf("expected failed, got %s", res.Status)
	}
	if res.Output["exit_code"] != 2 {
		t.Fatalf("expected exit_code 2, got %v", res.Output["exit_code"])
	}
}

func TestCLIExecutor_CaptureVars(t *testing.T) {
	fp := platform.NewFakePlatform()
	fp.ExecResults = map[string]*platform.ExecResult{
		"echo": {ExitCode: 0, Stdout: "data", Stderr: "warn"},
	}
	exec := NewCLIExecutor(fp, nil)
	step := engine.ResolvedStep{
		ID:      "step-1",
		Kind:    "cli",
		Spec:    &schema.CLISpec{Command: "echo"},
		Capture: map[string]string{"out": "stdout", "code": "exit_code"},
	}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Vars["out"] != "data" {
		t.Fatalf("expected capture stdout, got %v", res.Vars["out"])
	}
	if res.Vars["code"] != 0 {
		t.Fatalf("expected capture exit_code 0, got %v", res.Vars["code"])
	}
}

func TestCLIExecutor_TemplateResolution(t *testing.T) {
	fp := platform.NewFakePlatform()
	exec := NewCLIExecutor(fp, &internalexpr.TemplateEvaluator{})
	step := engine.ResolvedStep{
		ID:   "step-1",
		Kind: "cli",
		Spec: &schema.CLISpec{Command: "echo", Args: []string{"hello ${name}"}},
	}

	_, err := exec.Execute(context.Background(), step, map[string]any{"name": "sam"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(fp.ExecRequests) != 1 {
		t.Fatalf("expected 1 exec request, got %d", len(fp.ExecRequests))
	}
	if fp.ExecRequests[0].Args[0] != "hello sam" {
		t.Fatalf("expected templated arg, got %q", fp.ExecRequests[0].Args[0])
	}
}

func TestCLIExecutor_EnvVars(t *testing.T) {
	fp := platform.NewFakePlatform()
	exec := NewCLIExecutor(fp, &internalexpr.TemplateEvaluator{})
	step := engine.ResolvedStep{
		ID:   "step-1",
		Kind: "cli",
		Spec: &schema.CLISpec{Command: "echo", Env: map[string]string{"TOKEN": "${token}"}},
	}

	_, err := exec.Execute(context.Background(), step, map[string]any{"token": "abc"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if fp.ExecRequests[0].Env["TOKEN"] != "abc" {
		t.Fatalf("expected env TOKEN=abc, got %q", fp.ExecRequests[0].Env["TOKEN"])
	}
}

func TestCLIExecutor_Workdir(t *testing.T) {
	fp := platform.NewFakePlatform()
	exec := NewCLIExecutor(fp, &internalexpr.TemplateEvaluator{})
	step := engine.ResolvedStep{
		ID:   "step-1",
		Kind: "cli",
		Spec: &schema.CLISpec{Command: "echo", Workdir: "work-${dir}"},
	}

	_, err := exec.Execute(context.Background(), step, map[string]any{"dir": "work"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if fp.ExecRequests[0].Workdir != "work-work" {
		t.Fatalf("expected workdir work-work, got %q", fp.ExecRequests[0].Workdir)
	}
}
