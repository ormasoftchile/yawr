package routetest

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	internalexecutor "github.com/ormasoftchile/yawr/runtime/internal/executor"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/hostaction"
	"github.com/ormasoftchile/yawr/runtime/pkg/input"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/testutil"
	"github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

type countingExecutor struct{ calls int }

func (executor *countingExecutor) Execute(_ context.Context, step engine.ResolvedStep, _ map[string]any) (*engine.StepResult, error) {
	executor.calls++
	return &engine.StepResult{StepID: step.ID}, nil
}

type pureSubstitutionExecutor struct{ calls int }

func (*pureSubstitutionExecutor) IsPureRunbookSubstitution(context.Context, engine.ResolvedStep, map[string]any) (bool, error) {
	return true, nil
}

func (executor *pureSubstitutionExecutor) Execute(_ context.Context, step engine.ResolvedStep, _ map[string]any) (*engine.StepResult, error) {
	executor.calls++
	return &engine.StepResult{StepID: step.ID, Status: engine.StepStatusCompleted, Outcome: engine.StepOutcomeSuccess}, nil
}

type mapRegistry map[string]engine.StepExecutor

func (registry mapRegistry) Register(kind string, executor engine.StepExecutor) {
	registry[kind] = executor
}
func (registry mapRegistry) Lookup(kind string) engine.StepExecutor { return registry[kind] }

func TestScheduler_ConsumesExactHumanCheckBindingsAndStopsBeforeTarget(t *testing.T) {
	scheduler, err := NewScheduler(Scenario{
		Target: Selector{CallPath: []string{"execute_failover"}, Step: "dangerous_command", Phase: "before", Invocation: 1, Attempt: 1},
		HostActionResponses: []HostActionBinding{{
			At:         Selector{CallPath: []string{"inspect_replication"}, Step: "open_external_view", Phase: "execute", Invocation: 1, Attempt: 1},
			Capability: "external-view.open",
			Response:   HostActionResponse{Status: "completed", Result: map[string]any{"status": "opened"}},
			Review:     Review{State: "reviewed", ReviewedBy: "operator", ReviewedAt: "2026-08-28T12:05:00Z", SensitivityReviewed: true},
		}},
		InteractionAnswers: []InteractionBinding{{
			At:   Selector{CallPath: []string{"inspect_replication", "handle_external_view_launch"}, Step: "record_findings", Phase: "execute", Invocation: 1, Attempt: 1},
			Kind: "collector",
			Values: map[string]any{
				"extview_check_primary_health":   "unavailable",
				"extview_check_secondary_health": "healthy",
				"extview_check_replication_lag":  "within_limit",
			},
			Review: Review{State: "reviewed", ReviewedBy: "operator", ReviewedAt: "2026-08-28T12:05:00Z", SensitivityReviewed: true},
		}},
	})
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}

	hostContext := engine.WithDebugCallPath(context.Background(), []engine.DebugCallFrame{{StepID: "inspect_replication"}})
	hostContext = hostaction.WithStepID(hostContext, "open_external_view")
	response, err := scheduler.ExecuteHostAction(hostContext, hostaction.Request{Capability: "external-view.open"})
	if err != nil {
		t.Fatalf("ExecuteHostAction: %v", err)
	}
	if response.Status != hostaction.StatusCompleted || response.Result["status"] != "opened" {
		t.Fatalf("host response = %#v", response)
	}
	if _, err := scheduler.ExecuteHostAction(hostContext, hostaction.Request{Capability: "external-view.open"}); err == nil {
		t.Fatal("second host action invocation unexpectedly reused a consumed response")
	}

	collectorContext := engine.WithDebugCallPath(context.Background(), []engine.DebugCallFrame{
		{StepID: "inspect_replication"}, {StepID: "handle_external_view_launch"},
	})
	form, err := scheduler.PromptForm(collectorContext, input.FormRequest{StepID: "record_findings", Fields: []input.FormField{
		{Name: "extview_check_primary_health", Type: "select", Required: true, Options: []input.Option{{Value: "unavailable"}, {Value: "healthy"}}},
		{Name: "extview_check_secondary_health", Type: "select", Required: true, Options: []input.Option{{Value: "unavailable"}, {Value: "healthy"}}},
		{Name: "extview_check_replication_lag", Type: "select", Required: true, Options: []input.Option{{Value: "within_limit"}, {Value: "above_limit"}}},
	}})
	if err != nil {
		t.Fatalf("PromptForm: %v", err)
	}
	if form.Values["extview_check_primary_health"] != "unavailable" {
		t.Fatalf("collector response = %#v", form.Values)
	}

	decision, err := scheduler.BeforeStep(context.Background(), engine.DebugLocation{
		CallPath: []engine.DebugCallFrame{{StepID: "execute_failover"}},
		StepID:   "dangerous_command", Invocation: 1, Attempt: 1,
	}, engine.ResolvedStep{ID: "dangerous_command", Kind: "cli"})
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if decision != engine.RouteTestTargetReached || !scheduler.TargetReached() {
		t.Fatalf("target decision = %#v, reached = %v", decision, scheduler.TargetReached())
	}
	if err := scheduler.Verify(); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestScheduler_WrapRegistrySubstitutesAutomatedResultsWithoutDispatch(t *testing.T) {
	realCLI := &countingExecutor{}
	realNoop := &countingExecutor{}
	realExtension := &countingExecutor{}
	inner := mapRegistry{"cli": realCLI, "noop": realNoop, "extension": realExtension}
	scheduler, err := NewScheduler(Scenario{
		Target: Selector{Step: "target", Phase: "before", Invocation: 1, Attempt: 1},
		StepResponses: []StepBinding{{
			At: Selector{Step: "load_context", Phase: "execute", Invocation: 1, Attempt: 1}, Kind: "cli",
			Status: "completed", Outcome: "success", Output: map[string]any{"stdout": "saved"},
			Review: Review{State: "reviewed", ReviewedBy: "operator", ReviewedAt: "2026-08-28T12:05:00Z", SensitivityReviewed: true},
		}},
	})
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	registry := scheduler.WrapRegistry(inner)

	result, err := registry.Lookup("cli").Execute(context.Background(), engine.ResolvedStep{ID: "load_context", Kind: "cli"}, nil)
	if err != nil {
		t.Fatalf("scheduled CLI: %v", err)
	}
	if result.Output["stdout"] != "saved" || realCLI.calls != 0 {
		t.Fatalf("result = %#v, real calls = %d", result.Output, realCLI.calls)
	}
	if registry.Lookup("noop") != realNoop {
		t.Fatal("pure Yawr executor was replaced")
	}
	if _, err := registry.Lookup("extension").Execute(context.Background(), engine.ResolvedStep{ID: "external", Kind: "extension"}, nil); err == nil {
		t.Fatal("unmodelled external executor was allowed")
	}
	if realExtension.calls != 0 || scheduler.ExternalDispatchCount() != 0 {
		t.Fatalf("external dispatch occurred: real=%d recorded=%d", realExtension.calls, scheduler.ExternalDispatchCount())
	}
	if result.StartedAt.IsZero() || result.CompletedAt.Before(result.StartedAt) || result.CompletedAt.After(time.Now().Add(time.Second)) {
		t.Fatalf("invalid scheduled result timestamps: %#v", result)
	}
}

func TestScheduler_ExternalDispatchCountRecordsBypassedRealBoundary(t *testing.T) {
	scheduler, err := NewScheduler(Scenario{Target: Selector{Step: "target", Phase: "before", Invocation: 1, Attempt: 1}})
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	fakePlatform := platform.NewFakePlatform()
	executor := internalexecutor.NewCLIExecutor(fakePlatform, nil)
	ctx := engine.WithRouteTestController(context.Background(), scheduler)
	_, err = executor.Execute(ctx, engine.ResolvedStep{
		ID: "bypassed", Kind: "cli", Spec: &schema.CLISpec{Command: "external-command"},
	}, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(fakePlatform.ExecRequests) != 1 || scheduler.ExternalDispatchCount() != 1 {
		t.Fatalf("real calls = %d, recorded dispatches = %d", len(fakePlatform.ExecRequests), scheduler.ExternalDispatchCount())
	}
}

func TestScheduler_SavedToolResponsesUseRuntimeOutputContract(t *testing.T) {
	tests := map[string]struct {
		output       map[string]any
		wantStatus   engine.StepStatus
		wantCount    any
		wantBoundary bool
	}{
		"coerces declared output": {
			output: map[string]any{"count": "7"}, wantStatus: engine.StepStatusCompleted, wantCount: 7,
		},
		"rejects missing declared output": {
			output: map[string]any{}, wantBoundary: true,
		},
		"rejects non-coercible declared output": {
			output: map[string]any{"count": "not-an-integer"}, wantBoundary: true,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			runtime := testutil.NewFakeToolRuntime()
			runtime.RegisterDef("queryer", &tool.ToolDef{Actions: map[string]*tool.ToolAction{
				"query": {Outputs: map[string]*schema.ArgDef{"count": {Type: "integer"}}},
			}})
			scheduler, err := NewScheduler(Scenario{
				Target: Selector{Step: "target", Phase: "before", Invocation: 1, Attempt: 1},
				StepResponses: []StepBinding{{
					At: Selector{Step: "query", Phase: "execute", Invocation: 1, Attempt: 1}, Kind: "tool",
					Status: "completed", Outcome: "success", Output: test.output,
					Review: Review{State: "reviewed", ReviewedBy: "operator", ReviewedAt: "2026-08-28T12:05:00Z", SensitivityReviewed: true},
				}},
			})
			if err != nil {
				t.Fatalf("NewScheduler: %v", err)
			}
			registry := scheduler.WrapRegistry(mapRegistry{
				"tool": internalexecutor.NewToolExecutor(runtime, nil),
			})
			step := engine.ResolvedStep{ID: "query", Kind: "tool", Spec: &schema.ToolCallSpec{
				Tool: schema.ToolInvocation{Name: "queryer", Action: "query"},
			}, Capture: map[string]string{"saved_count": "outputs.count"}}

			result, err := registry.Lookup("tool").Execute(context.Background(), step, nil)
			if test.wantBoundary {
				if !engine.IsRouteTestBoundaryError(err) || result != nil {
					t.Fatalf("scheduled tool = %#v, %v; want boundary error", result, err)
				}
				if verifyErr := scheduler.Verify(); verifyErr == nil || !strings.Contains(verifyErr.Error(), "unused") {
					t.Fatalf("invalid fixture was consumed: %v", verifyErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("scheduled tool: %v", err)
			}
			if result.Status != test.wantStatus {
				t.Fatalf("status = %s, want %s (error=%v output=%#v)", result.Status, test.wantStatus, result.Error, result.Output)
			}
			if test.wantCount != nil && result.Output["count"] != test.wantCount {
				t.Fatalf("count = %#v, want %#v", result.Output["count"], test.wantCount)
			}
			if test.wantCount != nil && result.Vars["saved_count"] != test.wantCount {
				t.Fatalf("saved_count = %#v, want %#v", result.Vars["saved_count"], test.wantCount)
			}
			if len(runtime.Calls) != 0 {
				t.Fatalf("saved result dispatched real tool %d time(s)", len(runtime.Calls))
			}
		})
	}
}

func TestScheduler_RejectsContradictorySavedStatusAndOutcome(t *testing.T) {
	_, err := NewScheduler(Scenario{
		Target: Selector{Step: "target", Phase: "before", Invocation: 1, Attempt: 1},
		StepResponses: []StepBinding{{
			At:   Selector{Step: "query", Phase: "execute", Invocation: 1, Attempt: 1},
			Kind: "tool", Status: "failed", Outcome: "success",
			Review: Review{State: "reviewed", ReviewedBy: "operator", ReviewedAt: "2026-08-28T12:05:00Z", SensitivityReviewed: true},
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "inconsistent") {
		t.Fatalf("NewScheduler error = %v, want inconsistent status/outcome", err)
	}
}

func TestScheduler_SavedToolResponsesRejectInvalidBoundaryValues(t *testing.T) {
	tests := map[string]struct {
		declaredType string
		value        any
	}{
		"fractional integer":     {declaredType: "integer", value: 7.5},
		"non-finite integer":     {declaredType: "integer", value: math.NaN()},
		"non-finite number":      {declaredType: "number", value: math.Inf(1)},
		"non-finite number text": {declaredType: "number", value: "NaN"},
		"integer overflow":       {declaredType: "integer", value: math.MaxFloat64},
		"unsigned integer range": {declaredType: "integer", value: uint64(math.MaxUint64)},
		"number precision range": {declaredType: "number", value: int64(9007199254740993)},
		"object as array":        {declaredType: "object", value: []any{"wrong"}},
		"array as object":        {declaredType: "array", value: map[string]any{"wrong": true}},
		"object as string":       {declaredType: "string", value: map[string]any{"wrong": true}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			runtime := testutil.NewFakeToolRuntime()
			runtime.RegisterDef("queryer", &tool.ToolDef{Actions: map[string]*tool.ToolAction{
				"query": {Outputs: map[string]*schema.ArgDef{"value": {Type: test.declaredType}}},
			}})
			scheduler, err := NewScheduler(Scenario{
				Target: Selector{Step: "target", Phase: "before", Invocation: 1, Attempt: 1},
				StepResponses: []StepBinding{{
					At: Selector{Step: "query", Phase: "execute", Invocation: 1, Attempt: 1}, Kind: "tool",
					Status: "completed", Outcome: "success", Output: map[string]any{"value": test.value},
					Review: Review{State: "reviewed", ReviewedBy: "operator", ReviewedAt: "2026-08-28T12:05:00Z", SensitivityReviewed: true},
				}},
			})
			if err != nil {
				t.Fatalf("NewScheduler: %v", err)
			}
			registry := scheduler.WrapRegistry(mapRegistry{
				"tool": internalexecutor.NewToolExecutor(runtime, nil),
			})
			step := engine.ResolvedStep{ID: "query", Kind: "tool", Spec: &schema.ToolCallSpec{
				Tool: schema.ToolInvocation{Name: "queryer", Action: "query"},
			}}

			result, err := registry.Lookup("tool").Execute(context.Background(), step, nil)
			if !engine.IsRouteTestBoundaryError(err) || result != nil {
				t.Fatalf("invalid saved boundary value = %#v, %v; want boundary error", result, err)
			}
			if len(runtime.Calls) != 0 {
				t.Fatalf("saved result dispatched real tool %d time(s)", len(runtime.Calls))
			}
		})
	}
}

func TestScheduler_ConsumesExactTestApprovalOnce(t *testing.T) {
	scheduler, err := NewScheduler(Scenario{
		Target: Selector{Step: "target", Phase: "before", Invocation: 1, Attempt: 1},
		TestApprovals: []ApprovalBinding{{
			At:       Selector{CallPath: []string{"confirm_route"}, Step: "approve_failover", Phase: "execute", Invocation: 1, Attempt: 1},
			Approved: true, Approver: "route-reviewer",
			Review: Review{State: "reviewed", ReviewedBy: "operator", ReviewedAt: "2026-08-28T12:05:00Z", SensitivityReviewed: true},
		}},
	})
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	ctx := engine.WithDebugCallPath(context.Background(), []engine.DebugCallFrame{{StepID: "confirm_route"}})
	record, err := scheduler.RequestApproval(ctx, "approve_failover", "test approval")
	if err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}
	if record.Approver != "route-reviewer" || record.Token == "" {
		t.Fatalf("approval record = %#v", record)
	}
	if _, err := scheduler.RequestApproval(ctx, "approve_failover", "test approval"); err == nil {
		t.Fatal("second approval unexpectedly reused a consumed test approval")
	}
}

func TestScheduler_BeforeStepReachesTargetAndBlocksUnsupportedEnginePaths(t *testing.T) {
	scheduler, err := NewScheduler(Scenario{Target: Selector{Step: "target", Phase: "before", Invocation: 1, Attempt: 1}})
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}

	decision, err := scheduler.BeforeStep(context.Background(), engine.DebugLocation{StepID: "target", Invocation: 1, Attempt: 1}, engine.ResolvedStep{ID: "target", Kind: "cli"})
	if err != nil {
		t.Fatalf("BeforeStep target: %v", err)
	}
	if decision != engine.RouteTestTargetReached || !scheduler.TargetReached() {
		t.Fatalf("decision = %q, reached = %v", decision, scheduler.TargetReached())
	}

	for _, kind := range []string{"wait_for_event", "extension", "prompt"} {
		_, err := scheduler.BeforeStep(context.Background(), engine.DebugLocation{StepID: kind, Invocation: 1, Attempt: 1}, engine.ResolvedStep{ID: kind, Kind: kind})
		if err == nil || !strings.Contains(err.Error(), "route test safety failure") {
			t.Fatalf("kind %s error = %v, want safety failure", kind, err)
		}
	}
	if decision, err := scheduler.BeforeStep(
		context.Background(), engine.DebugLocation{StepID: "parallel", Invocation: 1, Attempt: 1},
		engine.ResolvedStep{ID: "parallel", Kind: "parallel"},
	); err != nil || decision != engine.RouteTestContinue {
		t.Fatalf("pure parallel decision = %q, %v", decision, err)
	}
	dynamic := engine.ResolvedStep{ID: "dynamic", Kind: "include", Spec: &schema.IncludeSpec{Include: schema.IncludeConfig{RunbookRef: "${child}", ResolveFrom: "catalog"}}}
	if _, err := scheduler.BeforeStep(context.Background(), engine.DebugLocation{StepID: "dynamic", Invocation: 1, Attempt: 1}, dynamic); err == nil || !strings.Contains(err.Error(), "dynamic include") {
		t.Fatalf("dynamic include error = %v, want safety failure", err)
	}
}

func TestScheduler_DelegatesPureRunbookToolSubstitutionWithoutFixture(t *testing.T) {
	scheduler, err := NewScheduler(Scenario{
		Target: Selector{Step: "target", Phase: "before", Invocation: 1, Attempt: 1},
	})
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	inner := &pureSubstitutionExecutor{}
	registry := scheduler.WrapRegistry(mapRegistry{"tool": inner})
	result, err := registry.Lookup("tool").Execute(context.Background(), engine.ResolvedStep{
		ID: "substitute", Kind: "tool", Spec: &schema.ToolCallSpec{
			Tool: schema.ToolInvocation{Name: "workflow", Action: "inspect"},
		},
	}, nil)
	if err != nil || result == nil || result.Status != engine.StepStatusCompleted {
		t.Fatalf("pure substitution = %#v, %v", result, err)
	}
	if inner.calls != 1 || scheduler.ExternalDispatchCount() != 0 {
		t.Fatalf("pure substitution calls=%d external=%d", inner.calls, scheduler.ExternalDispatchCount())
	}
}

func TestScheduler_InvalidCollectorAnswerRemainsUnused(t *testing.T) {
	scheduler, err := NewScheduler(Scenario{
		Target: Selector{Step: "target", Phase: "before", Invocation: 1, Attempt: 1},
		InteractionAnswers: []InteractionBinding{{
			At: Selector{Step: "findings", Phase: "execute", Invocation: 1, Attempt: 1}, Kind: "collector",
			Values: map[string]any{"health": "not-declared"},
			Review: Review{State: "reviewed", ReviewedBy: "operator", ReviewedAt: "2026-08-28T12:05:00Z", SensitivityReviewed: true},
		}},
	})
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	_, err = scheduler.PromptForm(context.Background(), input.FormRequest{StepID: "findings", Fields: []input.FormField{{
		Name: "health", Type: "select", Required: true, Options: []input.Option{{Value: "healthy"}},
	}}})
	if err == nil {
		t.Fatal("invalid collector answer was accepted")
	}
	if _, err := scheduler.BeforeStep(context.Background(), engine.DebugLocation{StepID: "target", Invocation: 1, Attempt: 1}, engine.ResolvedStep{ID: "target"}); err != nil {
		t.Fatalf("BeforeStep: %v", err)
	}
	if err := scheduler.Verify(); err == nil || !strings.Contains(err.Error(), "unused collector") {
		t.Fatalf("Verify error = %v, want unused collector binding", err)
	}
}
