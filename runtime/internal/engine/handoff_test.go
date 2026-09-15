package engine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	internaldebugprotect "github.com/ormasoftchile/yawr/runtime/internal/debugprotect"
	internalexecutor "github.com/ormasoftchile/yawr/runtime/internal/executor"
	internalexpr "github.com/ormasoftchile/yawr/runtime/internal/expr"
	"github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	"github.com/ormasoftchile/yawr/runtime/internal/runstore"
	enginepkg "github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

type protectedDynamicHandoffResolver struct {
	result *internalexecutor.DynamicIncludeResult
}

func (resolver *protectedDynamicHandoffResolver) Resolve(
	context.Context,
	string,
) (*internalexecutor.DynamicIncludeResult, error) {
	return resolver.result, nil
}

func TestEngineCompensationBubblesHandoffRequest(t *testing.T) {
	store := runstore.NewDirRunStore(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	registry := internalexecutor.NewMapRegistry()
	registry.Register("compensate", internalexecutor.NewCompensateExecutor())
	registry.Register("handoff", internalexecutor.NewHandoffExecutor(&internalexpr.TemplateEvaluator{}))
	registry.Register("cli", stepExecutorFunc(func(_ context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
		return &enginepkg.StepResult{
			StepID: step.ID, Status: enginepkg.StepStatusFailed, Outcome: enginepkg.StepOutcomeFailed,
			Error: errors.New("trigger compensation"), Vars: map[string]any{},
		}, nil
	}))
	config := makeTestConfig()
	config.Executors = registry
	handoff := &schema.HandoffSpec{Handoff: schema.HandoffConfig{
		Runbook: "target.runbook.yaml",
		Reason:  schema.HandoffReason{Code: "continue", Summary: "Continue investigation"},
	}}
	plan := &enginepkg.ExecutionPlan{
		RunID: "compensation-handoff", RunbookPath: "root.runbook.yaml",
		Steps: []enginepkg.ResolvedStep{
			{ID: "register", Kind: "compensate", Spec: &schema.CompensateSpec{Compensate: schema.CompensateConfig{
				On: "failure", Steps: []schema.FlowNode{{Step: &schema.Step{
					ID: "continue", Type: schema.StepTypeHandoff, HandoffSpec: handoff,
				}}},
			}}},
			{ID: "continue", Kind: "handoff", Spec: handoff, Depth: 1, ParentID: "register", ParentKind: "compensate"},
			{ID: "fail", Kind: "cli", Spec: &schema.CLISpec{Command: "fail"}},
		},
		Metadata: enginepkg.PlanMetadata{RunbookID: "root", RunbookName: "Root"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	handle, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{Store: store})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := handle.Next(context.Background()); err != nil {
		t.Fatalf("register compensation: %v", err)
	}
	_, err = handle.Next(context.Background())
	request, ok := enginepkg.HandoffRequestFromError(err)
	if !ok || request.StepID != "continue" || request.QualifiedNodeID != "register/continue" || request.FrameID == "" {
		t.Fatalf("compensation handoff = %#v, %v", request, err)
	}
	state, err := store.LoadState(context.Background(), plan.RunID)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if state.Status != enginepkg.RunStatusHandoffPending || state.PendingHandoff == nil ||
		state.PendingHandoff.FrameID != request.FrameID || len(state.PendingHandoff.CallPath) != 1 ||
		state.PendingHandoff.CallPath[0].StepID != "register" {
		t.Fatalf("durable compensation handoff = %#v", state)
	}
}

func TestEnginePersistsPendingHandoffBeforeReturningSignal(t *testing.T) {
	store := runstore.NewDirRunStore(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	registry := internalexecutor.NewMapRegistry()
	registry.Register("handoff", internalexecutor.NewHandoffExecutor(&internalexpr.TemplateEvaluator{}))
	config := makeTestConfig()
	config.Executors = registry
	config.Store = store
	plan := &enginepkg.ExecutionPlan{
		RunID: "handoff-pending", RunbookPath: "root.runbook.yaml",
		Steps: []enginepkg.ResolvedStep{{
			ID: "continue", Kind: "handoff", Spec: &schema.HandoffSpec{Handoff: schema.HandoffConfig{
				Runbook: "target.runbook.yaml",
				Reason:  schema.HandoffReason{Code: "continue", Summary: "Continue investigation"},
				With:    map[string]string{"server": "${server}"},
			}},
		}},
		Metadata: enginepkg.PlanMetadata{RunbookID: "root", RunbookName: "Root"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	handle, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{
		Mode: enginepkg.RunModeReal, RuntimeVars: map[string]any{"server": "db01"}, Store: store,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := handle.Next(context.Background()); err == nil {
		t.Fatal("Next did not return the handoff signal")
	} else if request, ok := enginepkg.HandoffRequestFromError(err); !ok || request.Context["server"] != "db01" {
		t.Fatalf("handoff signal = %#v, %v", request, err)
	}
	state, err := store.LoadState(context.Background(), plan.RunID)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if state.Status != enginepkg.RunStatusHandoffPending || state.PendingHandoff == nil ||
		state.PendingHandoff.TargetRunbook != "target.runbook.yaml" || state.CursorSet == nil ||
		len(state.CursorSet.Cursors) != 1 || state.CursorSet.Cursors[0].StepID != "continue" ||
		state.CursorSet.Cursors[0].Phase != enginepkg.ExecutionPhaseBefore {
		t.Fatalf("durable handoff state = %#v", state)
	}
	if _, err := New(config).Resume(context.Background(), plan.RunID, enginepkg.RunOptions{Store: store}); !errors.Is(err, enginepkg.ErrHandoffPending) {
		t.Fatalf("standalone Resume error = %v, want ErrHandoffPending", err)
	}
}

func TestEngineRejectsProtectedHandoffReasonBeforePersistence(t *testing.T) {
	store := runstore.NewDirRunStore(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	registry := internalexecutor.NewMapRegistry()
	registry.Register("handoff", internalexecutor.NewHandoffExecutor(&internalexpr.TemplateEvaluator{}))
	config := makeTestConfig()
	config.Executors = registry
	config.Store = store
	plan := &enginepkg.ExecutionPlan{
		RunID: "protected-handoff-reason", RunbookPath: "root.runbook.yaml",
		Steps: []enginepkg.ResolvedStep{{
			ID: "continue", Kind: "handoff", Spec: &schema.HandoffSpec{Handoff: schema.HandoffConfig{
				Runbook: "target.runbook.yaml",
				Reason:  schema.HandoffReason{Code: "continue", Summary: "private-ticket-123"},
			}},
		}},
		Metadata: enginepkg.PlanMetadata{RunbookID: "root", RunbookName: "Root"},
		GovernanceSource: &schema.GovernanceConfig{Redact: []schema.RedactRule{{
			Pattern: `private-ticket-[0-9]+`, Replace: "<redacted>",
		}}},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	_, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{
		Mode: enginepkg.RunModeReal, Store: store,
	})
	if err == nil || strings.Contains(err.Error(), "private-ticket-123") {
		t.Fatalf("Start error = %v, want value-free protected metadata refusal", err)
	}
	if _, loadErr := store.LoadPlan(context.Background(), plan.RunID); !errors.Is(loadErr, os.ErrNotExist) {
		t.Fatalf("protected handoff plan was persisted: %v", loadErr)
	}
}

func TestEngineRejectsProtectedHandoffPlanBeforeSavingIt(t *testing.T) {
	store := runstore.NewDirRunStore(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	registry := internalexecutor.NewMapRegistry()
	registry.Register("handoff", internalexecutor.NewHandoffExecutor(&internalexpr.TemplateEvaluator{}))
	config := makeTestConfig()
	config.Executors = registry
	config.Store = store
	plan := &enginepkg.ExecutionPlan{
		RunID: "protected-handoff-plan", RunbookPath: "root.runbook.yaml",
		Steps: []enginepkg.ResolvedStep{{
			ID: "continue", Kind: "handoff", Spec: &schema.HandoffSpec{Handoff: schema.HandoffConfig{
				Runbook: "target.runbook.yaml",
				Reason:  schema.HandoffReason{Code: "continue", Summary: "a\"b"},
			}},
		}},
		Metadata: enginepkg.PlanMetadata{RunbookID: "root", RunbookName: "Root"},
		Inputs:   map[string]*schema.Input{"credential": {Type: "secret"}},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	_, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{
		Mode: enginepkg.RunModeReal, Store: store,
		RuntimeVars: map[string]any{"credential": "a\"b"},
	})
	if err == nil || strings.Contains(err.Error(), "a\"b") {
		t.Fatalf("Start error = %v, want value-free refusal", err)
	}
	if _, loadErr := store.LoadPlan(context.Background(), plan.RunID); !errors.Is(loadErr, os.ErrNotExist) {
		t.Fatalf("protected source plan was persisted: %v", loadErr)
	}
}

func TestValidateDurableHandoffPlanScansAllSourceFields(t *testing.T) {
	plan := &enginepkg.ExecutionPlan{
		RunbookPath: "source.runbook.yaml",
		Steps: []enginepkg.ResolvedStep{
			{ID: "work", Name: "private-ticket-123", Kind: "noop", Spec: &schema.NoopSpec{}},
			{ID: "next", Kind: "handoff", Spec: &schema.HandoffSpec{Handoff: schema.HandoffConfig{
				Runbook: "target.runbook.yaml",
				Reason:  schema.HandoffReason{Code: "next", Summary: "Continue"},
			}}},
		},
		GovernanceSource: &schema.GovernanceConfig{Redact: []schema.RedactRule{{
			Pattern: `private-ticket-[0-9]+`, Replace: "<redacted>",
		}}},
		Metadata: enginepkg.PlanMetadata{RunbookID: "source", RunbookName: "Source"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	validator := New(makeTestConfig()).(interface {
		ValidateDurableHandoffPlan(context.Context, *enginepkg.ExecutionPlan, enginepkg.RunOptions) error
	})
	err := validator.ValidateDurableHandoffPlan(context.Background(), plan, enginepkg.RunOptions{})
	if err == nil || strings.Contains(err.Error(), "private-ticket-123") {
		t.Fatalf("ValidateDurableHandoffPlan error = %v, want value-free refusal", err)
	}
}

func TestHandoffProtectionSnapshotPreservesUserPayload(t *testing.T) {
	const secret = "private-ticket-123"
	artifact, err := handoffProtectionSnapshot([]byte(
		`{"steps":[{"spec":{"tool":{"args":{"governance":{"redact":"private-ticket-123"}}}}}]}`,
	))
	if err != nil {
		t.Fatalf("handoffProtectionSnapshot: %v", err)
	}
	if internaldebugprotect.ValidateHandoffJSON(
		enginepkg.DebugProtection{SecretValues: []string{secret}}, artifact,
	) == nil {
		t.Fatal("protected user payload escaped scanning")
	}
}

func TestEngineRejectsNestedHandoffAliasOfSecretBeforeSavingTarget(t *testing.T) {
	store := runstore.NewDirRunStore(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	registry := internalexecutor.NewMapRegistry()
	registry.Register("include", internalexecutor.NewIncludeExecutor(&internalexpr.TemplateEvaluator{}, nil, nil))
	config := makeTestConfig()
	config.Executors = registry
	config.Store = store
	plan := &enginepkg.ExecutionPlan{
		RunID: "protected-nested-target", RunbookPath: "target.runbook.yaml",
		Steps: []enginepkg.ResolvedStep{{
			ID: "child", Kind: "include", Spec: &schema.IncludeSpec{
				Include:             schema.IncludeConfig{Runbook: "child.runbook.yaml", With: map[string]string{"alias": "${opaque}"}},
				ResolvedRunbookPath: "child.runbook.yaml",
				ResolvedInputs:      map[string]*schema.Input{"alias": {Type: "string"}},
				ResolvedSteps: []schema.FlowNode{{Step: &schema.Step{
					ID: "continue", Type: schema.StepTypeHandoff,
					HandoffSpec: &schema.HandoffSpec{Handoff: schema.HandoffConfig{
						Runbook: "next.runbook.yaml", Reason: schema.HandoffReason{Code: "next", Summary: "Continue"},
						With: map[string]string{"server": "${alias}"},
					}},
				}}},
			},
		}},
		Inputs:   map[string]*schema.Input{"opaque": {Type: "secret"}},
		Metadata: enginepkg.PlanMetadata{RunbookID: "target", RunbookName: "Target"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	_, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{
		Mode: enginepkg.RunModeReal, Store: store, RuntimeVars: map[string]any{"opaque": "never-persist"},
	})
	if err == nil || strings.Contains(err.Error(), "never-persist") {
		t.Fatalf("Start error = %v, want value-free nested secret refusal", err)
	}
	if _, loadErr := store.LoadPlan(context.Background(), plan.RunID); !errors.Is(loadErr, os.ErrNotExist) {
		t.Fatalf("protected target plan was persisted: %v", loadErr)
	}
}

func TestEngineRejectsTwoLevelNestedSecretAliasBeforeSavingPlan(t *testing.T) {
	store := runstore.NewDirRunStore(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	registry := internalexecutor.NewMapRegistry()
	registry.Register("include", internalexecutor.NewIncludeExecutor(&internalexpr.TemplateEvaluator{}, nil, nil))
	config := makeTestConfig()
	config.Executors = registry
	config.Store = store
	handoff := &schema.HandoffSpec{Handoff: schema.HandoffConfig{
		Runbook: "next.runbook.yaml", Reason: schema.HandoffReason{Code: "next", Summary: "Continue"},
		With: map[string]string{"server": "${alias2}"},
	}}
	inner := &schema.IncludeSpec{
		Include:             schema.IncludeConfig{Runbook: "inner.runbook.yaml", With: map[string]string{"alias2": "${alias1}"}},
		ResolvedRunbookPath: "inner.runbook.yaml",
		ResolvedInputs:      map[string]*schema.Input{"alias2": {Type: "string"}},
		ResolvedSteps: []schema.FlowNode{{Step: &schema.Step{
			ID: "continue", Type: schema.StepTypeHandoff, HandoffSpec: handoff,
		}}},
	}
	outer := &schema.IncludeSpec{
		Include:             schema.IncludeConfig{Runbook: "outer.runbook.yaml", With: map[string]string{"alias1": "${opaque}"}},
		ResolvedRunbookPath: "outer.runbook.yaml",
		ResolvedInputs:      map[string]*schema.Input{"alias1": {Type: "string"}},
		ResolvedSteps: []schema.FlowNode{{Step: &schema.Step{
			ID: "inner", Type: schema.StepTypeInclude, IncludeSpec: inner,
		}}},
	}
	plan := &enginepkg.ExecutionPlan{
		RunID: "two-level-secret-alias", RunbookPath: "root.runbook.yaml",
		Steps:    []enginepkg.ResolvedStep{{ID: "outer", Kind: "include", Spec: outer}},
		Inputs:   map[string]*schema.Input{"opaque": {Type: "secret"}},
		Metadata: enginepkg.PlanMetadata{RunbookID: "root", RunbookName: "Root"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	_, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{
		Mode: enginepkg.RunModeReal, Store: store, RuntimeVars: map[string]any{"opaque": "never-persist"},
	})
	if err == nil || strings.Contains(err.Error(), "never-persist") {
		t.Fatalf("Start error = %v, want value-free two-level alias refusal", err)
	}
	if _, loadErr := store.LoadPlan(context.Background(), plan.RunID); !errors.Is(loadErr, os.ErrNotExist) {
		t.Fatalf("two-level protected plan was persisted: %v", loadErr)
	}
}

func TestEngineRejectsProtectedDynamicClosureBeforePersistingResolution(t *testing.T) {
	const protectedMarker = "private-child-123"
	store := runstore.NewDirRunStore(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	resolver := &protectedDynamicHandoffResolver{result: &internalexecutor.DynamicIncludeResult{
		Flow: []schema.FlowNode{{Step: &schema.Step{
			ID: "continue", Type: schema.StepTypeHandoff,
			HandoffSpec: &schema.HandoffSpec{Handoff: schema.HandoffConfig{
				Runbook: "next.runbook.yaml",
				Reason:  schema.HandoffReason{Code: "continue", Summary: protectedMarker},
			}},
		}}},
		QualifiedID: "pkg/child", RunbookID: "child", RunbookName: "Child",
		ContentHash: strings.Repeat("a", 64), AbsPath: "C:/runbooks/child.runbook.yaml",
		PackageName: "pkg", PackageVersion: "1.0.0",
		FileDigest:    enginepkg.InteractionPayloadDigest([]byte("child-file")),
		PackageDigest: enginepkg.InteractionPayloadDigest([]byte("child-package")),
		ChildGovernance: &schema.GovernanceConfig{
			Redact: []schema.RedactRule{{Pattern: `private-child-[0-9]+`, Replace: "<redacted>"}},
		},
	}}
	registry := internalexecutor.NewDefaultRegistry(internalexecutor.RegistryConfig{
		Evaluator: &internalexpr.TemplateEvaluator{},
		SubStepRunner: func(
			context.Context, internalexecutor.SubStepParent, []schema.FlowNode, map[string]any,
		) ([]*enginepkg.StepResult, error) {
			return nil, nil
		},
		DynamicIncludeResolver: resolver,
	})
	config := makeTestConfig()
	config.Executors = registry
	config.Store = store
	plan := &enginepkg.ExecutionPlan{
		RunID: "protected-dynamic-closure", RunbookPath: "root.runbook.yaml",
		Steps: []enginepkg.ResolvedStep{{
			ID: "child", Kind: "include", Spec: &schema.IncludeSpec{Include: schema.IncludeConfig{
				RunbookRef: "pkg/child", ResolveFrom: schema.ResolveFromCatalog,
			}},
		}},
		Metadata: enginepkg.PlanMetadata{RunbookID: "root", RunbookName: "Root"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	handle, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{Mode: enginepkg.RunModeReal})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	_, _ = handle.Next(context.Background())
	persisted, err := store.LoadState(context.Background(), plan.RunID)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	encoded, err := json.Marshal(persisted)
	if err != nil {
		t.Fatalf("marshal persisted state: %v", err)
	}
	if len(persisted.DynamicIncludes) != 0 || strings.Contains(string(encoded), protectedMarker) {
		t.Fatalf("protected dynamic closure was persisted: %s", encoded)
	}
}

func TestEngineRejectsProtectedDynamicHandoffAliasBeforePersistingResolution(t *testing.T) {
	store := runstore.NewDirRunStore(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	resolver := &protectedDynamicHandoffResolver{result: &internalexecutor.DynamicIncludeResult{
		Flow: []schema.FlowNode{{Step: &schema.Step{
			ID: "continue", Type: schema.StepTypeHandoff,
			HandoffSpec: &schema.HandoffSpec{Handoff: schema.HandoffConfig{
				Runbook: "next.runbook.yaml",
				Reason:  schema.HandoffReason{Code: "continue", Summary: "Continue"},
				With:    map[string]string{"server": "${alias}"},
			}},
		}}},
		QualifiedID: "pkg/child", RunbookID: "child", RunbookName: "Child",
		ContentHash: strings.Repeat("a", 64), AbsPath: "C:/runbooks/child.runbook.yaml",
		PackageName: "pkg", PackageVersion: "1.0.0",
		FileDigest:    enginepkg.InteractionPayloadDigest([]byte("child-file")),
		PackageDigest: enginepkg.InteractionPayloadDigest([]byte("child-package")),
		ChildInputs:   map[string]*schema.Input{"alias": {Type: "string"}},
	}}
	registry := internalexecutor.NewDefaultRegistry(internalexecutor.RegistryConfig{
		Evaluator: &internalexpr.TemplateEvaluator{},
		SubStepRunner: func(
			context.Context, internalexecutor.SubStepParent, []schema.FlowNode, map[string]any,
		) ([]*enginepkg.StepResult, error) {
			return nil, nil
		},
		DynamicIncludeResolver: resolver,
	})
	config := makeTestConfig()
	config.Executors = registry
	config.Store = store
	plan := &enginepkg.ExecutionPlan{
		RunID: "protected-dynamic-alias", RunbookPath: "root.runbook.yaml",
		Steps: []enginepkg.ResolvedStep{{
			ID: "child", Kind: "include", Spec: &schema.IncludeSpec{Include: schema.IncludeConfig{
				RunbookRef: "pkg/child", ResolveFrom: schema.ResolveFromCatalog,
				With: map[string]string{"alias": "${opaque}"},
			}},
		}},
		Inputs:   map[string]*schema.Input{"opaque": {Type: "secret"}},
		Metadata: enginepkg.PlanMetadata{RunbookID: "root", RunbookName: "Root"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	handle, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{
		Mode: enginepkg.RunModeReal, RuntimeVars: map[string]any{"opaque": "never-persist"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	_, _ = handle.Next(context.Background())
	persisted, err := store.LoadState(context.Background(), plan.RunID)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(persisted.DynamicIncludes) != 0 {
		t.Fatalf("protected dynamic alias was persisted: %#v", persisted.DynamicIncludes)
	}
}

func TestValidateDurableHandoffArtifactsIncludesChildGovernanceWithoutHandoff(t *testing.T) {
	registry := internalexecutor.NewMapRegistry()
	registry.Register("include", internalexecutor.NewIncludeExecutor(&internalexpr.TemplateEvaluator{}, nil, nil))
	config := makeTestConfig()
	config.Executors = registry
	plan := &enginepkg.ExecutionPlan{
		RunbookPath: "target.runbook.yaml",
		Steps: []enginepkg.ResolvedStep{{
			ID: "child", Kind: "include", Spec: &schema.IncludeSpec{
				Include:             schema.IncludeConfig{Runbook: "child.runbook.yaml"},
				ResolvedRunbookPath: "child.runbook.yaml",
				ResolvedGovernance: &schema.GovernanceConfig{Redact: []schema.RedactRule{{
					Pattern: `private-child-[0-9]+`, Replace: "<redacted>",
				}}},
				ResolvedSteps: []schema.FlowNode{{Step: &schema.Step{
					ID: "work", Type: schema.StepTypeNoop, Title: "private-child-123", NoopSpec: &schema.NoopSpec{},
				}}},
			},
		}},
		Metadata: enginepkg.PlanMetadata{RunbookID: "target", RunbookName: "Target"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	validator := New(config).(interface {
		ValidateDurableHandoffArtifacts(
			context.Context, *enginepkg.ExecutionPlan, enginepkg.RunOptions, ...json.RawMessage,
		) error
	})
	err := validator.ValidateDurableHandoffArtifacts(
		context.Background(), plan, enginepkg.RunOptions{},
		json.RawMessage(`{"title":"private-child-123"}`),
	)
	if err == nil || strings.Contains(err.Error(), "private-child-123") {
		t.Fatalf("ValidateDurableHandoffArtifacts error = %v, want value-free child policy refusal", err)
	}
}

func TestValidateDurableHandoffArtifactsAllowsOwnRedactionRule(t *testing.T) {
	plan := &enginepkg.ExecutionPlan{
		RunbookPath: "target.runbook.yaml",
		Steps:       []enginepkg.ResolvedStep{{ID: "work", Kind: "noop", Spec: &schema.NoopSpec{}}},
		GovernanceSource: &schema.GovernanceConfig{Redact: []schema.RedactRule{{
			Pattern: "policy-marker", Replace: "<redacted>",
		}}},
		Metadata: enginepkg.PlanMetadata{RunbookID: "target", RunbookName: "Target"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	snapshot, err := plansnapshot.FromExecutionPlan(plan)
	if err != nil {
		t.Fatalf("FromExecutionPlan: %v", err)
	}
	artifact, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	validator := New(makeTestConfig()).(interface {
		ValidateDurableHandoffArtifacts(
			context.Context, *enginepkg.ExecutionPlan, enginepkg.RunOptions, ...json.RawMessage,
		) error
	})
	if err := validator.ValidateDurableHandoffArtifacts(
		context.Background(), plan, enginepkg.RunOptions{}, artifact,
	); err != nil {
		t.Fatalf("policy metadata rejected: %v", err)
	}
}

func TestValidateDurableHandoffArtifactsDoesNotCanonicalizeSpoofedPlanSnapshot(t *testing.T) {
	plan := &enginepkg.ExecutionPlan{
		RunbookPath: "target.runbook.yaml",
		Steps:       []enginepkg.ResolvedStep{{ID: "work", Kind: "noop", Spec: &schema.NoopSpec{}}},
		GovernanceSource: &schema.GovernanceConfig{Redact: []schema.RedactRule{{
			Pattern: "private-ticket-123", Replace: "<redacted>",
		}}},
		Metadata: enginepkg.PlanMetadata{RunbookID: "target", RunbookName: "Target"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	validator := New(makeTestConfig()).(interface {
		ValidateDurableHandoffArtifacts(
			context.Context, *enginepkg.ExecutionPlan, enginepkg.RunOptions, ...json.RawMessage,
		) error
	})
	err := validator.ValidateDurableHandoffArtifacts(
		context.Background(), plan, enginepkg.RunOptions{},
		json.RawMessage(`{"schema_version":"execution-plan/v3","snapshot_digest":"opaque","governance":{"redact":"private-ticket-123"}}`),
	)
	if err == nil || strings.Contains(err.Error(), "private-ticket-123") {
		t.Fatalf("ValidateDurableHandoffArtifacts error = %v, want value-free payload refusal", err)
	}
}

func TestValidateDurableHandoffArtifactsDoesNotTrustInvalidDynamicClosure(t *testing.T) {
	const protectedMarker = "private-ticket-123"
	plan := &enginepkg.ExecutionPlan{
		RunbookPath: "target.runbook.yaml",
		Steps:       []enginepkg.ResolvedStep{{ID: "work", Kind: "noop", Spec: &schema.NoopSpec{}}},
		Inputs:      map[string]*schema.Input{"opaque": {Type: "secret"}},
		Metadata: enginepkg.PlanMetadata{
			RunbookID: "target", RunbookName: "Target",
			DynamicIncludes: []schema.LockedDynamicInclude{{
				StepID: "dynamic", RenderedRef: "pkg/child", QualifiedID: "pkg/child",
				AbsPath: "C:/runbooks/child.runbook.yaml", PackageName: "pkg", PackageVersion: "1.0.0",
				FileDigest:    enginepkg.InteractionPayloadDigest([]byte("child-file")),
				PackageDigest: enginepkg.InteractionPayloadDigest([]byte("child-package")),
				ExecutableClosure: json.RawMessage(`{
					"schema_version":"execution-flow-closure/v3",
					"closure_digest":"forged",
					"nodes":[{"step":{"common":{"id":"child","type":"include"},"spec_kind":"include","spec":{
						"include":{"runbook":"nested.runbook.yaml"},
						"resolved_governance":{"redact":[{"pattern":"private-ticket-123","replace":"<redacted>"}]}
					}}}],
					"unexpected":true
				}`),
			}},
		},
	}
	invalidPins := plan.Metadata.DynamicIncludes
	plan.Metadata.DynamicIncludes = nil
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	snapshot, err := plansnapshot.FromExecutionPlan(plan)
	if err != nil {
		t.Fatalf("FromExecutionPlan: %v", err)
	}
	plan.Metadata.DynamicIncludes = invalidPins
	snapshot.Metadata.DynamicIncludes = invalidPins
	snapshot.SnapshotDigest = ""
	unsigned, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal unsigned snapshot: %v", err)
	}
	snapshot.SnapshotDigest = fmt.Sprintf("sha256:%x", sha256.Sum256(unsigned))
	artifact, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	validator := New(makeTestConfig()).(interface {
		ValidateDurableHandoffArtifacts(
			context.Context, *enginepkg.ExecutionPlan, enginepkg.RunOptions, ...json.RawMessage,
		) error
	})
	err = validator.ValidateDurableHandoffArtifacts(
		context.Background(), plan,
		enginepkg.RunOptions{RuntimeVars: map[string]any{"opaque": protectedMarker}}, artifact,
	)
	if err == nil || strings.Contains(err.Error(), protectedMarker) {
		t.Fatalf("ValidateDurableHandoffArtifacts error = %v, want value-free refusal", err)
	}
}
