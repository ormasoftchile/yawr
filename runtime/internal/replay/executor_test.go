package replay

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	internalexecutor "github.com/ormasoftchile/yawr/runtime/internal/executor"
	internalexpr "github.com/ormasoftchile/yawr/runtime/internal/expr"
	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/governance"
	"github.com/ormasoftchile/yawr/runtime/pkg/hostaction"
	"github.com/ormasoftchile/yawr/runtime/pkg/input"
	sharedreplay "github.com/ormasoftchile/yawr/runtime/pkg/replay"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

type livePromptProvider struct{ calls int }

func (provider *livePromptProvider) PromptChoice(context.Context, input.ChoiceRequest) (*input.ChoiceResponse, error) {
	provider.calls++
	return &input.ChoiceResponse{Selected: []string{"live"}}, nil
}
func (provider *livePromptProvider) PromptDecision(context.Context, input.DecisionRequest) (*input.DecisionResponse, error) {
	provider.calls++
	return &input.DecisionResponse{Label: "live"}, nil
}
func (provider *livePromptProvider) PromptForm(context.Context, input.FormRequest) (*input.FormResponse, error) {
	provider.calls++
	return &input.FormResponse{Values: map[string]any{"live": true}}, nil
}

type liveHostProvider struct{ calls int }

func (provider *liveHostProvider) ExecuteHostAction(context.Context, hostaction.Request) (hostaction.Response, error) {
	provider.calls++
	return hostaction.Response{Status: hostaction.StatusCompleted, Result: map[string]any{"live": true}}, nil
}

type liveApprovalProvider struct{ calls int }

func (provider *liveApprovalProvider) RequestApproval(context.Context, string, string) (governance.ApprovalRecord, error) {
	provider.calls++
	return governance.ApprovalRecord{Approver: "live"}, nil
}

type pureToolHandoffExecutor struct{ calls int }

func (*pureToolHandoffExecutor) IsPureRunbookSubstitution(
	context.Context,
	engine.ResolvedStep,
	map[string]any,
) (bool, error) {
	return true, nil
}

func (executor *pureToolHandoffExecutor) Execute(
	ctx context.Context,
	_ engine.ResolvedStep,
	vars map[string]any,
) (*engine.StepResult, error) {
	executor.calls++
	registry := internalexecutor.ExecutorRegistryFromContext(ctx)
	if registry == nil {
		return nil, context.Canceled
	}
	return registry.Lookup("handoff").Execute(ctx, engine.ResolvedStep{
		ID: "continue", Kind: "handoff", Spec: &schema.HandoffSpec{Handoff: schema.HandoffConfig{
			Runbook: "target.runbook.yaml",
			Reason:  schema.HandoffReason{Code: "continue", Summary: "Continue"},
		}},
	}, vars)
}

func TestReplayExecutor_CLI_MatchedFixture(t *testing.T) {
	scenario := &Scenario{
		Commands: []CommandFixture{{
			Argv:     []string{"echo", "hi"},
			Stdout:   "hi\n",
			Stderr:   "",
			ExitCode: 0,
		}},
	}
	exec := NewReplayExecutor("cli", scenario)
	step := engine.ResolvedStep{
		ID:   "step-1",
		Kind: "cli",
		Spec: &schema.CLISpec{Command: "echo", Args: []string{"hi"}},
	}
	result, err := exec.Execute(context.Background(), step, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Output["stdout"] != "hi\n" {
		t.Fatalf("unexpected stdout: %#v", result.Output)
	}
}

func TestReplayExecutor_CLI_NoMatch(t *testing.T) {
	scenario := &Scenario{Commands: []CommandFixture{}}
	exec := NewReplayExecutor("cli", scenario)
	step := engine.ResolvedStep{
		ID:   "step-1",
		Kind: "cli",
		Spec: &schema.CLISpec{Command: "echo", Args: []string{"hi"}},
	}
	result, err := exec.Execute(context.Background(), step, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != engine.StepStatusFailed {
		t.Fatalf("expected failed status, got %s", result.Status)
	}
}

func TestReplayExecutor_CLI_AllowUnmatched(t *testing.T) {
	scenario := &Scenario{AllowUnmatched: true}
	exec := NewReplayExecutor("cli", scenario)
	step := engine.ResolvedStep{
		ID:   "step-1",
		Kind: "cli",
		Spec: &schema.CLISpec{Command: "echo", Args: []string{"hi"}},
	}
	result, err := exec.Execute(context.Background(), step, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != engine.StepStatusCompleted {
		t.Fatalf("expected completed status, got %s", result.Status)
	}
}

func TestReplayExecutor_Tool(t *testing.T) {
	scenario := &Scenario{
		Tools: map[string]ToolFixture{
			"tool/action": {Response: "{\"ok\":true}", ExitCode: 0},
		},
	}
	exec := NewReplayExecutor("tool", scenario)
	step := engine.ResolvedStep{
		ID:   "step-1",
		Kind: "tool",
		Spec: &schema.ToolCallSpec{Tool: schema.ToolInvocation{Name: "tool", Action: "action"}},
	}
	result, err := exec.Execute(context.Background(), step, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Output["response"] != "{\"ok\":true}" {
		t.Fatalf("unexpected response: %#v", result.Output)
	}
}

func TestReplayExecutor_Manual(t *testing.T) {
	scenario := &Scenario{
		Evidence: map[string]map[string]EvidenceFixture{
			"step-1": {"note": {Kind: "text", Value: "ok"}},
		},
	}
	exec := NewReplayExecutor("manual", scenario)
	step := engine.ResolvedStep{ID: "step-1", Kind: "manual"}
	result, err := exec.Execute(context.Background(), step, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Output["note"] != "ok" {
		t.Fatalf("unexpected output: %#v", result.Output)
	}
}

func TestReplayExecutorRegistry_Wrapping(t *testing.T) {
	registry := internalexecutor.NewMapRegistry()
	registry.Register("cli", &stubExecutor{})
	scenario := &Scenario{}
	wrapped := NewReplayExecutorRegistry(registry, scenario)
	if wrapped.Lookup("unknown") != nil {
		t.Fatalf("expected nil for unknown executor")
	}
	if _, ok := wrapped.Lookup("cli").(*ReplayExecutor); !ok {
		t.Fatalf("expected ReplayExecutor for cli kind")
	}
}

func TestReplayEngineMissingHumanAndHostFixturesFailClosed(t *testing.T) {
	prompts := &livePromptProvider{}
	host := &liveHostProvider{}
	approvals := &liveApprovalProvider{}
	base := internalexecutor.NewMapRegistry()
	base.Register("choice", internalexecutor.NewChoiceExecutor(prompts, nil))
	base.Register("decision", internalexecutor.NewDecisionExecutor(prompts, nil))
	base.Register("collector", internalexecutor.NewCollectorExecutor(prompts, nil, nil))
	base.Register("host_action", internalexecutor.NewHostActionExecutor(host, nil))
	base.Register("approve", internalexecutor.NewApproveExecutor(approvals))
	base.Register("prompt", &stubExecutor{})
	replay := NewReplayExecutorRegistryWithPins(base, &Scenario{}, nil, nil, nil, nil, nil, "replay")
	tests := []struct {
		kind string
		spec engine.StepSpec
	}{
		{kind: "choice", spec: &schema.ChoiceSpec{Prompt: "choose", Variable: "answer", Options: []schema.ChoiceOption{{Label: "One", Value: "one"}}}},
		{kind: "decision", spec: &schema.DecisionSpec{Prompt: "route", Routes: []schema.DecisionRoute{{Label: "one"}}}},
		{kind: "collector", spec: &schema.CollectorSpec{Prompt: "collect", Fields: []schema.CollectorField{{Name: "value", Type: schema.FieldTypeText, Label: "Value"}}}},
		{kind: "host_action", spec: &schema.HostActionSpec{HostAction: schema.HostActionConfig{Capability: "open", Request: map[string]any{}}}},
		{kind: "approve", spec: &schema.ApproveSpec{}},
		{kind: "prompt", spec: &internalexecutor.PromptSpec{}},
	}
	for _, test := range tests {
		t.Run(test.kind, func(t *testing.T) {
			result, err := replay.Lookup(test.kind).Execute(context.Background(), engine.ResolvedStep{
				ID: test.kind, Kind: test.kind, Spec: test.spec,
			}, nil)
			if err == nil && result != nil && result.Status != engine.StepStatusFailed && result.Status != engine.StepStatusDenied {
				t.Fatalf("missing %s fixture succeeded: %#v", test.kind, result)
			}
		})
	}
	if prompts.calls != 0 || host.calls != 0 || approvals.calls != 0 {
		t.Fatalf("live provider calls = prompts:%d host:%d approvals:%d", prompts.calls, host.calls, approvals.calls)
	}
}

func TestReplayEngineUnfrozenPureToolSubstitutionFailsWithoutCallingInner(t *testing.T) {
	toolExecutor := &pureToolHandoffExecutor{}
	base := internalexecutor.NewMapRegistry()
	base.Register("tool", toolExecutor)
	base.Register("handoff", internalexecutor.NewHandoffExecutor(nil))
	replay := NewReplayExecutorRegistryWithPins(base, &Scenario{}, nil, nil, nil, nil, nil, "replay")
	_, err := replay.Lookup("tool").Execute(context.Background(), engine.ResolvedStep{
		ID: "substitute", Kind: "tool", Spec: &schema.ToolCallSpec{},
	}, nil)
	if !engine.IsReplayBoundaryError(err) || toolExecutor.calls != 0 {
		t.Fatalf("unfrozen substitution calls/error = %d/%v", toolExecutor.calls, err)
	}
}

func TestReplayEngineFrozenToolSubstitutionExecutesNestedHandoff(t *testing.T) {
	closure, err := plansnapshot.EncodeFlowClosure([]schema.FlowNode{{Step: &schema.Step{
		ID: "continue", Type: schema.StepTypeHandoff, HandoffSpec: &schema.HandoffSpec{Handoff: schema.HandoffConfig{
			Runbook: "target.runbook.yaml", Reason: schema.HandoffReason{Code: "continue", Summary: "Continue"},
		}},
	}}})
	if err != nil {
		t.Fatalf("EncodeFlowClosure: %v", err)
	}
	definition := &schema.ToolDef{Name: "saved-tool", Actions: map[string]*schema.ToolAction{"run": {
		Execute: &schema.ExecuteSpec{Kind: "runbook", Path: "mutable.runbook.yaml"},
		FrozenSubstitution: &schema.FrozenToolSubstitution{
			PackageName: "saved-package", RunbookPath: "saved.runbook.yaml",
			RunbookID: "saved", RunbookName: "Saved", RunbookContentHash: strings.Repeat("a", 64),
			ExecutableClosure: closure,
		},
	}}}
	runner := func(ctx context.Context, _ internalexecutor.SubStepParent, nodes []schema.FlowNode, vars map[string]any) ([]*engine.StepResult, error) {
		registry := internalexecutor.ExecutorRegistryFromContext(ctx)
		evidenceHook, evidenceBound := internalexecutor.RunEvidenceHookFromContext(ctx)
		if registry == nil || !evidenceBound || evidenceHook != nil || len(nodes) != 1 || nodes[0].Step == nil {
			return nil, errors.New("frozen substitution has no recursive replay registry")
		}
		child := nodes[0].Step
		_, executeErr := registry.Lookup(string(child.Type)).Execute(ctx, engine.ResolvedStep{
			ID: child.ID, Kind: string(child.Type), Spec: child.HandoffSpec,
		}, vars)
		return nil, executeErr
	}
	base := internalexecutor.NewMapRegistry()
	base.Register("tool", internalexecutor.NewToolExecutorWithSubstitution(
		nil, &internalexpr.TemplateEvaluator{}, runner,
		&fakeParser{err: errors.New("mutable substitution parser called")}, nil,
	))
	base.Register("handoff", internalexecutor.NewHandoffExecutor(nil))
	replay := NewReplayExecutorRegistryWithPins(
		base, &Scenario{StrictExactFixtures: true}, &internalexpr.TemplateEvaluator{},
		map[string]*schema.ToolDef{"saved-tool": definition}, nil, nil, nil, "replay",
	).WithEvidenceHook(nil)
	_, err = replay.Lookup("tool").Execute(context.Background(), engine.ResolvedStep{
		ID: "substitute", Kind: "tool", Spec: &schema.ToolCallSpec{Tool: schema.ToolInvocation{
			Name: "saved-tool", Action: "run",
		}},
	}, nil)
	request, handoff := engine.HandoffRequestFromError(err)
	if !handoff || request.TargetRunbook != "target.runbook.yaml" {
		t.Fatalf("frozen substitution handoff/error = %#v/%v", request, err)
	}
}

func TestReplayInteractionFixtureConcurrentConsumeOnce(t *testing.T) {
	const sourceRunID = "source-concurrent-choice"
	scheduler := newReplayBoundaryScheduler(&Scenario{
		SourceRunID: sourceRunID,
		InteractionAnswers: []sharedreplay.InteractionBinding{{
			At: sharedreplay.Selector{
				QualifiedNodeID: "choose", Step: "choose", Phase: "execute", Invocation: 1, Attempt: 1,
			},
			Kind: "choice", Selected: []string{"one"},
			Source: sharedreplay.Source{Kind: "prior-run", RunID: sourceRunID},
		}},
	})
	ctx := engine.WithDispatchExecutionBoundary(context.Background(), engine.DispatchExecutionBoundary{
		QualifiedNodeID: "choose", StepID: "choose", Invocation: 1, RetryAttempt: 1,
	})
	request := input.ChoiceRequest{
		StepID: "choose", Options: []input.Option{{Label: "One", Value: "one"}},
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			response, err := scheduler.PromptChoice(ctx, request)
			if err == nil && (response == nil || len(response.Selected) != 1 || response.Selected[0] != "one") {
				err = context.Canceled
			}
			results <- err
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	succeeded, failed := 0, 0
	for err := range results {
		if err == nil {
			succeeded++
		} else if engine.IsReplayBoundaryError(err) {
			failed++
		} else {
			t.Fatalf("unexpected concurrent error: %v", err)
		}
	}
	if succeeded != 1 || failed != 1 {
		t.Fatalf("concurrent fixture outcomes = success:%d failure:%d", succeeded, failed)
	}
}

type stubExecutor struct{}

func (s *stubExecutor) Execute(_ context.Context, _ engine.ResolvedStep, _ map[string]any) (*engine.StepResult, error) {
	return &engine.StepResult{}, nil
}
