package executor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	internaldebugprotect "github.com/ormasoftchile/yawr/runtime/internal/debugprotect"
	internalexpr "github.com/ormasoftchile/yawr/runtime/internal/expr"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestHandoffExecutorReturnsEvaluatedTypedRequest(t *testing.T) {
	executor := NewHandoffExecutor(&internalexpr.TemplateEvaluator{})
	step := engine.ResolvedStep{ID: "continue", Kind: "handoff", Spec: &schema.HandoffSpec{Handoff: schema.HandoffConfig{
		Runbook: "target.runbook.yaml",
		Reason:  schema.HandoffReason{Code: "continue", Summary: "Continue investigation"},
		With:    map[string]string{"server": "${server}"},
		Facts:   map[string]string{"health": "${health}"},
	}}}
	ctx := engine.WithDispatchExecutionBoundary(context.Background(), engine.DispatchExecutionBoundary{
		QualifiedNodeID: "route/continue", CallPath: []engine.DebugCallFrame{{StepID: "route"}},
		StepID: "continue", FrameID: "frame", FrameStepIndex: 2, Invocation: 3, RetryAttempt: 1,
	})
	result, err := executor.Execute(ctx, step, map[string]any{"server": "db01", "health": "degraded"})
	request, ok := engine.HandoffRequestFromError(err)
	if result != nil || !ok {
		t.Fatalf("Execute = %#v, %v", result, err)
	}
	if request.TargetRunbook != "target.runbook.yaml" || request.Context["server"] != "db01" ||
		request.Facts["health"] != "degraded" || request.QualifiedNodeID != "route/continue" ||
		request.ContextBindings["server"] != "server" || request.FactBindings["health"] != "health" ||
		request.Invocation != 3 || request.FrameStepIndex != 2 {
		t.Fatalf("request = %#v", request)
	}
}

func TestHandoffExecutorPreservesExactJSONTypes(t *testing.T) {
	executor := NewHandoffExecutor(&internalexpr.TemplateEvaluator{})
	step := engine.ResolvedStep{ID: "continue", Kind: "handoff", Spec: &schema.HandoffSpec{Handoff: schema.HandoffConfig{
		Runbook: "target.runbook.yaml",
		Reason:  schema.HandoffReason{Code: "continue", Summary: "Continue investigation"},
		With: map[string]string{
			"enabled": "${enabled}", "count": "${count}", "items": "${items}", "labels": "${labels}",
		},
	}}}
	_, err := executor.Execute(context.Background(), step, map[string]any{
		"enabled": true, "count": 2, "items": []any{"a"}, "labels": map[string]any{"region": "westus"},
	})
	request, ok := engine.HandoffRequestFromError(err)
	if !ok {
		t.Fatalf("Execute error = %v, want handoff request", err)
	}
	if request.Context["enabled"] != true || request.Context["count"] != 2 ||
		len(request.Context["items"].([]any)) != 1 || request.Context["labels"].(map[string]any)["region"] != "westus" {
		t.Fatalf("typed context = %#v", request.Context)
	}
}

func TestHandoffExecutorRejectsPrivateSecretAliasAndError(t *testing.T) {
	executor := NewHandoffExecutor(&internalexpr.TemplateEvaluator{})
	step := engine.ResolvedStep{ID: "continue", Kind: "handoff", Spec: &schema.HandoffSpec{Handoff: schema.HandoffConfig{
		Runbook: "target.runbook.yaml",
		Reason:  schema.HandoffReason{Code: "continue", Summary: "Continue investigation"},
		With:    map[string]string{"server": "${alias}"},
	}}}
	for name, value := range map[string]any{
		"plain alias": "secret-value",
		"error alias": errors.New("secret-value"),
	} {
		t.Run(name, func(t *testing.T) {
			ctx := internaldebugprotect.WithHandoffValueValidator(
				context.Background(), engine.DebugProtection{SecretValues: []string{"secret-value"}},
			)
			_, err := executor.Execute(ctx, step, map[string]any{"alias": value})
			if err == nil || strings.Contains(err.Error(), "secret-value") {
				t.Fatalf("Execute error = %v, want value-free private-secret refusal", err)
			}
			if _, ok := engine.HandoffRequestFromError(err); ok {
				t.Fatalf("private secret produced handoff request: %v", err)
			}
		})
	}
}

func TestHandoffExecutorRejectsRuntimeConcurrentAncestry(t *testing.T) {
	executor := NewHandoffExecutor(nil)
	step := engine.ResolvedStep{ID: "continue", Kind: "handoff", Spec: &schema.HandoffSpec{Handoff: schema.HandoffConfig{
		Runbook: "target.runbook.yaml",
		Reason:  schema.HandoffReason{Code: "continue", Summary: "Continue investigation"},
	}}}
	_, err := executor.Execute(engine.WithConcurrentExecution(context.Background()), step, nil)
	if err == nil || !strings.Contains(err.Error(), "concurrent") {
		t.Fatalf("Execute error = %v, want concurrent ancestry refusal", err)
	}
	if _, ok := engine.HandoffRequestFromError(err); ok {
		t.Fatalf("concurrent ancestry produced a handoff request: %v", err)
	}
}

func TestHandoffExecutorRejectsProtectedSourceBeforePersistence(t *testing.T) {
	executor := NewHandoffExecutor(&internalexpr.TemplateEvaluator{})
	step := engine.ResolvedStep{ID: "continue", Kind: "handoff", Spec: &schema.HandoffSpec{Handoff: schema.HandoffConfig{
		Runbook: "target.runbook.yaml",
		Reason:  schema.HandoffReason{Code: "continue", Summary: "Continue investigation"},
		With:    map[string]string{"server": "${opaque}"},
	}}}
	ctx := internaldebugprotect.WithProtection(context.Background(), engine.DebugProtection{ProtectedVars: []string{"opaque"}})
	_, err := executor.Execute(ctx, step, map[string]any{"opaque": "secret-value"})
	if err == nil || strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("Execute error = %v, want value-free protected source refusal", err)
	}
	if _, ok := engine.HandoffRequestFromError(err); ok {
		t.Fatalf("protected source produced a handoff request: %v", err)
	}
}

func TestValidateHandoffRequestRejectsEscapedSecretAndProtectedBinding(t *testing.T) {
	for _, secret := range []string{"a\"b", "line1\nline2", "a<b", "a>b", "a&b"} {
		request := engine.HandoffRequest{ReasonSummary: secret}
		err := internaldebugprotect.ValidateHandoffRequest(engine.DebugProtection{
			SecretValues: []string{secret},
		}, request)
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Fatalf("escaped secret %q error = %v, want value-free refusal", secret, err)
		}
	}
	err := internaldebugprotect.ValidateHandoffRequest(engine.DebugProtection{
		ProtectedVars: []string{"credential"},
	}, engine.HandoffRequest{
		ContextBindings: map[string]string{"server": "credential.value"},
	})
	if err == nil {
		t.Fatal("protected binding provenance was accepted")
	}
}

func TestValidateHandoffJSONScansEveryArtifact(t *testing.T) {
	const secret = "a\"b"
	err := internaldebugprotect.ValidateHandoffJSON(
		engine.DebugProtection{SecretValues: []string{secret}},
		json.RawMessage(`{"clean":true}`),
		json.RawMessage(`{"title":"a\"b"}`),
	)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("multi-artifact error = %v, want value-free refusal", err)
	}
}
