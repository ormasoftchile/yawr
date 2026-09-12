package executor

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	internalexpr "github.com/ormasoftchile/yawr/runtime/internal/expr"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/hostaction"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

type fakeHostActionProvider struct {
	request  hostaction.Request
	response hostaction.Response
	err      error
}

func (p *fakeHostActionProvider) ExecuteHostAction(_ context.Context, request hostaction.Request) (hostaction.Response, error) {
	p.request = request
	return p.response, p.err
}

type hostActionProviderFunc func(context.Context, hostaction.Request) (hostaction.Response, error)

func (f hostActionProviderFunc) ExecuteHostAction(ctx context.Context, request hostaction.Request) (hostaction.Response, error) {
	return f(ctx, request)
}

func genericHostActionStep() engine.ResolvedStep {
	return engine.ResolvedStep{
		ID: "open_resource", Kind: "host_action",
		Spec: &schema.HostActionSpec{HostAction: schema.HostActionConfig{
			Capability: "product.open-resource",
			Request: map[string]any{
				"resource": "${resource}",
				"options": map[string]any{
					"focus":   true,
					"retries": 2,
					"labels":  []any{"primary", "${region}"},
				},
			},
		}},
	}
}

func TestHostActionExecutor_ForwardsRenderedOpaquePayload(t *testing.T) {
	provider := &fakeHostActionProvider{response: hostaction.Response{
		Status: hostaction.StatusCompleted,
		Result: map[string]any{"state": "opened"},
	}}
	result, err := NewHostActionExecutor(provider, &internalexpr.TemplateEvaluator{}).Execute(
		context.Background(),
		genericHostActionStep(),
		map[string]any{"resource": "incident-42", "region": "westus"},
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	want := map[string]any{
		"resource": "incident-42",
		"options": map[string]any{
			"focus":   true,
			"retries": 2,
			"labels":  []any{"primary", "westus"},
		},
	}
	if provider.request.Capability != "product.open-resource" || !reflect.DeepEqual(provider.request.Payload, want) {
		t.Fatalf("provider request = %#v", provider.request)
	}
	if result.Output["status"] != string(hostaction.StatusCompleted) || result.Output["capability"] != "product.open-resource" {
		t.Fatalf("result = %#v", result)
	}
}

func TestHostActionExecutor_PreparesRenderedDispatchBeforeProvider(t *testing.T) {
	providerCalled := false
	committer := &recordingDispatchCommitter{onPrepare: func() {
		if providerCalled {
			t.Fatal("host provider called before dispatch intent committed")
		}
	}}
	provider := hostActionProviderFunc(func(ctx context.Context, _ hostaction.Request) (hostaction.Response, error) {
		providerCalled = true
		if dispatch, ok := engine.PreparedDispatchFromContext(ctx); !ok || dispatch.OccurrenceID == "" || dispatch.IdempotencyKey == "" {
			t.Fatalf("prepared dispatch missing from provider context: %#v", dispatch)
		}
		return hostaction.Response{Status: hostaction.StatusCompleted, Result: map[string]any{"opened": true}}, nil
	})
	ctx := engine.WithDispatchCommitter(context.Background(), committer)
	if _, err := NewHostActionExecutor(provider, &internalexpr.TemplateEvaluator{}).Execute(
		ctx, genericHostActionStep(), map[string]any{"resource": "incident-42", "region": "westus"},
	); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	request, ok := committer.request.RenderedRequest.(map[string]any)
	if committer.calls != 1 || !ok || request["capability"] != "product.open-resource" {
		t.Fatalf("rendered dispatch request = %#v", committer.request.RenderedRequest)
	}
}

func TestHostActionExecutor_HeadlessReturnsGenericUnsupported(t *testing.T) {
	result, err := NewHostActionExecutor(nil, nil).Execute(context.Background(), genericHostActionStep(), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != engine.StepStatusCompleted ||
		result.Output["status"] != string(hostaction.StatusUnsupported) ||
		result.Output["outcome"] != "unsupported" {
		t.Fatalf("headless result = %#v", result)
	}
}

func TestHostActionExecutor_RejectsOversizedRenderedPayload(t *testing.T) {
	provider := &fakeHostActionProvider{response: hostaction.Response{
		Status: hostaction.StatusCompleted,
		Result: map[string]any{"state": "opened"},
	}}
	_, err := NewHostActionExecutor(provider, &internalexpr.TemplateEvaluator{}).Execute(
		context.Background(),
		genericHostActionStep(),
		map[string]any{
			"resource": strings.Repeat("x", hostaction.MaxPayloadStringBytes+1),
			"region":   "westus",
		},
	)
	if err == nil {
		t.Fatal("oversized rendered payload was accepted")
	}
	if provider.request.Payload != nil {
		t.Fatalf("provider was invoked with %#v", provider.request.Payload)
	}
}

func TestHostActionExecutor_GenericBridgeOutcomes(t *testing.T) {
	for _, tc := range []struct {
		status  hostaction.Status
		outcome string
		result  map[string]any
	}{
		{status: hostaction.StatusCompleted, outcome: "completed", result: map[string]any{"state": "done"}},
		{status: hostaction.StatusFailed, outcome: "failed"},
		{status: hostaction.StatusTimedOut, outcome: "timed-out"},
	} {
		t.Run(string(tc.status), func(t *testing.T) {
			provider := &fakeHostActionProvider{response: hostaction.Response{Status: tc.status, Result: tc.result}}
			result, err := NewHostActionExecutor(provider, nil).Execute(context.Background(), genericHostActionStep(), nil)
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if result.Output["status"] != string(tc.status) || result.Output["outcome"] != tc.outcome {
				t.Fatalf("result = %#v", result)
			}
			wantResult := tc.result
			if wantResult == nil {
				wantResult = map[string]any{"status": string(tc.status)}
			}
			if !reflect.DeepEqual(result.Output["result"], wantResult) {
				t.Fatalf("outputs.result = %#v, want %#v", result.Output["result"], wantResult)
			}
		})
	}
}

func TestHostActionExecutor_RejectsCompletedWithoutResult(t *testing.T) {
	provider := &fakeHostActionProvider{response: hostaction.Response{Status: hostaction.StatusCompleted}}
	result, err := NewHostActionExecutor(provider, nil).Execute(context.Background(), genericHostActionStep(), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Output["status"] != string(hostaction.StatusFailed) ||
		result.Output["outcome"] != "failed" ||
		!reflect.DeepEqual(result.Output["result"], map[string]any{"status": "failed"}) {
		t.Fatalf("invalid completed response = %#v", result)
	}
}

func TestHostActionExecutor_MissingAcknowledgementRemainsAmbiguous(t *testing.T) {
	provider := &fakeHostActionProvider{err: errors.New("host unavailable")}
	result, err := NewHostActionExecutor(provider, nil).Execute(context.Background(), genericHostActionStep(), nil)
	if result != nil || err == nil || !strings.Contains(err.Error(), "host unavailable") {
		t.Fatalf("result=%#v err=%v, want ambiguous provider error", result, err)
	}
}

func TestHostActionExecutor_UnknownAcknowledgementStatusRemainsAmbiguous(t *testing.T) {
	provider := &fakeHostActionProvider{response: hostaction.Response{Status: "future-status"}}
	result, err := NewHostActionExecutor(provider, nil).Execute(context.Background(), genericHostActionStep(), nil)
	if result != nil || err == nil || !strings.Contains(err.Error(), "unknown acknowledgement status") {
		t.Fatalf("result=%#v err=%v, want unknown-status error", result, err)
	}
}

func TestHostActionExecutor_PropagatesLifecycleCancellation(t *testing.T) {
	exec := NewHostActionExecutor(hostActionProviderFunc(func(ctx context.Context, _ hostaction.Request) (hostaction.Response, error) {
		<-ctx.Done()
		return hostaction.Response{}, ctx.Err()
	}), nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := exec.Execute(ctx, genericHostActionStep(), nil)
	if result != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestHostActionExecutor_ProviderCancellationWhileCallerLiveReturnsError(t *testing.T) {
	exec := NewHostActionExecutor(hostActionProviderFunc(func(context.Context, hostaction.Request) (hostaction.Response, error) {
		return hostaction.Response{}, context.Canceled
	}), nil)

	result, err := exec.Execute(context.Background(), genericHostActionStep(), nil)
	if result != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestHostActionExecutor_RejectsInvalidCompletedResultPayload(t *testing.T) {
	for name, resultPayload := range map[string]map[string]any{
		"oversized": {"value": strings.Repeat("x", hostaction.MaxPayloadStringBytes+1)},
		"non-json":  {"value": make(chan int)},
	} {
		t.Run(name, func(t *testing.T) {
			provider := &fakeHostActionProvider{response: hostaction.Response{
				Status: hostaction.StatusCompleted,
				Result: resultPayload,
			}}

			result, err := NewHostActionExecutor(provider, nil).Execute(context.Background(), genericHostActionStep(), nil)
			if result != nil || err == nil {
				t.Fatalf("result=%#v err=%v", result, err)
			}
		})
	}
}
