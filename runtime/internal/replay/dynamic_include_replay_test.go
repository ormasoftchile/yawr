package replay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	internaldebugprotect "github.com/ormasoftchile/yawr/runtime/internal/debugprotect"
	internaleventbus "github.com/ormasoftchile/yawr/runtime/internal/eventbus"
	internalexecutor "github.com/ormasoftchile/yawr/runtime/internal/executor"
	internalexpr "github.com/ormasoftchile/yawr/runtime/internal/expr"
	internalplanner "github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	internaltrace "github.com/ormasoftchile/yawr/runtime/internal/trace"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/eventbus"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	sharedreplay "github.com/ormasoftchile/yawr/runtime/pkg/replay"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	tracepkg "github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

// --- fakes for dynamic-include replay tests ---

// spyIncludeExecutor records the IncludeSpec it was called with so tests
// can assert the pinned path was used (not a catalog lookup).
type spyIncludeExecutor struct {
	capturedSpec  *schema.IncludeSpec
	capturedSpecs []*schema.IncludeSpec
}

type forwardingIncludeExecutor struct {
	inner    engine.StepExecutor
	captured *schema.IncludeSpec
}

type replayStepExecutorFunc func(context.Context, engine.ResolvedStep, map[string]any) (*engine.StepResult, error)

func (execute replayStepExecutorFunc) Execute(
	ctx context.Context,
	step engine.ResolvedStep,
	vars map[string]any,
) (*engine.StepResult, error) {
	return execute(ctx, step, vars)
}

func (executor *forwardingIncludeExecutor) Execute(
	ctx context.Context,
	step engine.ResolvedStep,
	vars map[string]any,
) (*engine.StepResult, error) {
	executor.captured, _ = step.Spec.(*schema.IncludeSpec)
	return executor.inner.Execute(ctx, step, vars)
}

func (s *spyIncludeExecutor) Execute(_ context.Context, step engine.ResolvedStep, _ map[string]any) (*engine.StepResult, error) {
	spec, _ := step.Spec.(*schema.IncludeSpec)
	s.capturedSpec = spec
	s.capturedSpecs = append(s.capturedSpecs, spec)
	now := time.Now()
	return &engine.StepResult{
		StepID:      step.ID,
		Status:      engine.StepStatusCompleted,
		Outcome:     engine.StepOutcomeSuccess,
		StartedAt:   now,
		CompletedAt: now,
	}, nil
}

// registryWithSpy wraps a MapRegistry and overrides "include" with a spy.
type registryWithSpy struct {
	*internalexecutor.MapRegistry
	spy *spyIncludeExecutor
}

func (r *registryWithSpy) Lookup(kind string) engine.StepExecutor {
	if kind == "include" {
		return r.spy
	}
	return r.MapRegistry.Lookup(kind)
}

// --- TestDynamicIncludeReplayExecutor: unit tests ---

func TestDynamicIncludeReplayExecutor_StaticIncludeExecutesNormally(t *testing.T) {
	scenario := &Scenario{AllowUnmatched: true}
	spy := &spyIncludeExecutor{}
	exec := &DynamicIncludeReplayExecutor{
		pins:      map[string][]schema.LockedDynamicInclude{},
		innerExec: spy,
		fallback:  NewReplayExecutor("include", scenario),
	}
	step := engine.ResolvedStep{
		ID:   "inc-1",
		Kind: "include",
		Spec: &schema.IncludeSpec{
			Include: schema.IncludeConfig{Runbook: "child.yaml"}, // static, not dynamic
		},
	}
	res, err := exec.Execute(context.Background(), step, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != engine.StepStatusCompleted {
		t.Errorf("expected completed, got %s", res.Status)
	}
	if spy.capturedSpec == nil || spy.capturedSpec.Include.Runbook != "child.yaml" {
		t.Fatalf("static include did not execute normally: %#v", spy.capturedSpec)
	}
}

func TestReplayStaticIncludeApprovalUsesSavedFixture(t *testing.T) {
	const sourceRunID = "source-static-provider"
	liveApproval := &liveReplayApprovalGate{}
	liveDispatcher := &liveReplayDispatcher{}
	scenario := &Scenario{
		SourceRunID: sourceRunID,
		Approvals: []sharedreplay.ApprovalBinding{{
			At: exactReplaySelector("outer"), Approved: true, Approver: "saved",
			Source: sharedreplay.Source{Kind: "prior-run", RunID: sourceRunID},
		}},
		WaitEvents: []WaitEventBinding{{
			At: exactReplaySelector("outer"), EventID: "ready", Source: "signal",
			Payload:    map[string]any{"value": "saved"},
			Provenance: sharedreplay.Source{Kind: "prior-run", RunID: sourceRunID},
		}},
	}
	base := internalexecutor.NewMapRegistry()
	runner := func(ctx context.Context, _ internalexecutor.SubStepParent, _ []schema.FlowNode, _ map[string]any) ([]*engine.StepResult, error) {
		gate := internalexecutor.RunApprovalGateFromContext(ctx)
		dispatcher := internalexecutor.RunEventDispatcherFromContext(ctx)
		if gate == nil || dispatcher == nil {
			return nil, errors.New("static include did not receive replay providers")
		}
		if _, err := gate.RequestApproval(ctx, "outer", "saved"); err != nil {
			return nil, err
		}
		event, err := dispatcher.Wait(ctx, "outer", eventbus.EventFilter{Source: "signal", ID: "ready"}, 0)
		if err != nil || event.Payload["value"] != "saved" {
			return nil, fmt.Errorf("saved event = %#v, %v", event, err)
		}
		return nil, nil
	}
	base.Register("include", internalexecutor.NewIncludeExecutor(nil, runner, nil).WithApprovalGate(liveApproval))
	replay := NewReplayExecutorRegistryWithPins(base, scenario, nil, nil, nil, nil, nil, "replay")
	result, err := replay.Lookup("include").Execute(context.Background(), engine.ResolvedStep{
		ID: "outer", Kind: "include", Spec: &schema.IncludeSpec{
			Include:       schema.IncludeConfig{Runbook: "child.runbook.yaml"},
			ResolvedSteps: []schema.FlowNode{{Step: &schema.Step{ID: "child", Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{}}}},
		},
	}, nil)
	if err != nil || result.Status != engine.StepStatusCompleted || liveApproval.calls != 0 || liveDispatcher.calls != 0 {
		t.Fatalf("static replay result/live calls = %#v/%d/%d, %v", result, liveApproval.calls, liveDispatcher.calls, err)
	}
}

func TestDynamicIncludeReplayExecutor_NoPin_Skips(t *testing.T) {
	exec := &DynamicIncludeReplayExecutor{
		pins:      map[string][]schema.LockedDynamicInclude{},
		innerExec: &spyIncludeExecutor{},
		fallback:  NewReplayExecutor("include", &Scenario{AllowUnmatched: true}),
	}
	step := engine.ResolvedStep{
		ID:   "call-tsg",
		Kind: "include",
		Spec: &schema.IncludeSpec{
			Include: schema.IncludeConfig{RunbookRef: "${tsg_id}"},
		},
	}
	res, err := exec.Execute(context.Background(), step, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != engine.StepStatusSkipped {
		t.Errorf("expected skipped for unpinned step, got %s", res.Status)
	}
}

func TestDynamicIncludeReplayExecutorMissingPinFailsStrictReplay(t *testing.T) {
	step := engine.ResolvedStep{
		ID: "call-tsg", Kind: "include",
		Spec: &schema.IncludeSpec{Include: schema.IncludeConfig{
			RunbookRef: "${tsg_id}", ResolveFrom: schema.ResolveFromCatalog,
		}},
	}
	for _, test := range []struct {
		name string
		pins []schema.LockedDynamicInclude
		ctx  context.Context
	}{
		{name: "missing", ctx: context.Background()},
		{
			name: "structural mismatch",
			pins: []schema.LockedDynamicInclude{{
				StepID: "call-tsg", QualifiedNodeID: "each/call-tsg", Revision: 1,
				StructuralPath: []schema.DynamicIncludeFrameIdentity{{
					QualifiedNodeID: "each", Kind: "iterate", IterationIndex: 1,
				}},
			}},
			ctx: engine.WithDynamicIncludeStructuralPath(
				engine.WithDispatchExecutionBoundary(context.Background(), engine.DispatchExecutionBoundary{
					QualifiedNodeID: "each/call-tsg", StepID: "call-tsg",
				}),
				[]schema.DynamicIncludeFrameIdentity{{
					QualifiedNodeID: "each", Kind: "iterate", IterationIndex: 2,
				}},
			),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			executor := &DynamicIncludeReplayExecutor{
				pins: buildPinIndex(test.pins), innerExec: &spyIncludeExecutor{},
				fallback: NewReplayExecutor("include", &Scenario{AllowUnmatched: false}),
			}
			if _, err := executor.Execute(test.ctx, step, nil); err == nil {
				t.Fatal("strict replay accepted a dynamic include without an exact pin")
			}
		})
	}
}

func TestDynamicIncludeReplayExecutorReplaysExactNotFoundOutcome(t *testing.T) {
	scenario := &Scenario{DynamicIncludeNotFound: []DynamicIncludeNotFoundFixture{{
		StepID: "call-tsg", QualifiedNodeID: "each/call-tsg", Invocation: 1,
		StructuralPath: []schema.DynamicIncludeFrameIdentity{{
			QualifiedNodeID: "each", Kind: "iterate", IterationIndex: 2,
		}},
		RenderedRef: "missing/tsg", Reason: "dynamic include: not found [DINC-002]",
	}}}
	spy := &spyIncludeExecutor{}
	executor := &DynamicIncludeReplayExecutor{
		pins: map[string][]schema.LockedDynamicInclude{}, innerExec: spy,
		fallback: NewReplayExecutor("include", scenario), evaluator: &internalexpr.TemplateEvaluator{},
	}
	step := engine.ResolvedStep{ID: "call-tsg", Kind: "include", Spec: &schema.IncludeSpec{Include: schema.IncludeConfig{
		RunbookRef: "${tsg_id}", ResolveFrom: schema.ResolveFromCatalog, OnNotFound: schema.OnNotFoundContinue,
	}}}
	ctx := engine.WithDispatchExecutionBoundary(context.Background(), engine.DispatchExecutionBoundary{
		QualifiedNodeID: "each/call-tsg", CallPath: []engine.DebugCallFrame{{StepID: "each"}}, StepID: "call-tsg",
	})
	ctx = engine.WithDynamicIncludeStructuralPath(ctx, []schema.DynamicIncludeFrameIdentity{{
		QualifiedNodeID: "each", Kind: "iterate", IterationIndex: 2,
	}})
	result, err := executor.Execute(ctx, step, map[string]any{"tsg_id": "missing/tsg"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != engine.StepStatusSkipped || result.Outcome != engine.StepOutcomeSkipped ||
		result.Output["skip_reason"] != "include_not_found" || result.Vars["runbook_found"] != false ||
		result.Vars["runbook_skipped_reason"] != scenario.DynamicIncludeNotFound[0].Reason {
		t.Fatalf("replayed not-found result = %#v", result)
	}
	if warning, _ := result.Output["warning"].(string); !strings.Contains(warning, "missing/tsg") {
		t.Fatalf("replayed warning = %q", warning)
	}
	if spy.capturedSpec != nil {
		t.Fatal("recorded not-found outcome invoked the real include executor")
	}
	if _, err := executor.Execute(ctx, step, nil); err == nil {
		t.Fatal("strict replay reused one recorded not-found occurrence")
	}
}

func TestDynamicIncludeReplayExecutorRejectsNotFoundReferenceOrPolicyDrift(t *testing.T) {
	for _, test := range []struct {
		name       string
		runbookRef string
		policy     string
	}{
		{name: "rendered reference", runbookRef: "pkg/other", policy: schema.OnNotFoundContinue},
		{name: "policy", runbookRef: "missing/tsg", policy: schema.OnNotFoundFail},
	} {
		t.Run(test.name, func(t *testing.T) {
			scenario := &Scenario{DynamicIncludeNotFound: []DynamicIncludeNotFoundFixture{{
				StepID: "call-tsg", QualifiedNodeID: "call-tsg", Invocation: 1,
				RenderedRef: "missing/tsg", Reason: "DINC-002: not found",
			}}}
			executor := &DynamicIncludeReplayExecutor{
				pins: map[string][]schema.LockedDynamicInclude{}, innerExec: &spyIncludeExecutor{},
				fallback: NewReplayExecutor("include", scenario),
			}
			step := engine.ResolvedStep{ID: "call-tsg", Kind: "include", Spec: &schema.IncludeSpec{Include: schema.IncludeConfig{
				RunbookRef: test.runbookRef, ResolveFrom: schema.ResolveFromCatalog, OnNotFound: test.policy,
			}}}
			if _, err := executor.Execute(context.Background(), step, nil); err == nil {
				t.Fatal("strict replay accepted a drifted not-found outcome")
			}
		})
	}
}

func TestBuildScenarioFromTraceRecordsExactDynamicIncludeNotFoundOutcome(t *testing.T) {
	var structuralPath []schema.DynamicIncludeFrameIdentity
	payload, err := json.Marshal(map[string]any{
		"step_id": "call-tsg", "qualified_node_id": "call-tsg",
		"invocation":      1,
		"structural_path": structuralPath, "rendered_ref": "missing/tsg",
		"reason": "dynamic include: not found [DINC-002]", "error_code": "DINC-002", "continued": true,
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	skippedPayload, err := json.Marshal(map[string]any{
		"step_id": "call-tsg", "qualified_node_id": "call-tsg",
		"invocation": 1, "structural_path": structuralPath, "reason": "include_not_found",
	})
	if err != nil {
		t.Fatalf("Marshal skipped: %v", err)
	}
	notFoundEvent := tracepkg.TraceEvent{Kind: tracepkg.EventKindIncludeNotFound, Sequence: 1, Payload: payload}
	scenario, _, err := buildScenarioFromTrace(makeDynamicIncludePlan(), []tracepkg.TraceEvent{
		notFoundEvent,
		{Kind: tracepkg.EventKindStepSkipped, Sequence: 2, Payload: skippedPayload},
	})
	if err != nil {
		t.Fatalf("buildScenarioFromTrace: %v", err)
	}
	if len(scenario.DynamicIncludeNotFound) != 1 {
		t.Fatalf("dynamic not-found fixtures = %#v", scenario.DynamicIncludeNotFound)
	}
	fixture := scenario.DynamicIncludeNotFound[0]
	if fixture.StepID != "call-tsg" || fixture.QualifiedNodeID != "call-tsg" || fixture.Invocation != 1 ||
		fixture.RenderedRef != "missing/tsg" || fixture.Reason == "" ||
		!sameDynamicStructuralPath(fixture.StructuralPath, structuralPath) {
		t.Fatalf("dynamic not-found fixture = %#v", fixture)
	}
	uncommitted, _, err := buildScenarioFromTrace(makeDynamicIncludePlan(), []tracepkg.TraceEvent{notFoundEvent})
	if err != nil {
		t.Fatalf("build uncommitted scenario: %v", err)
	}
	if len(uncommitted.DynamicIncludeNotFound) != 0 {
		t.Fatalf("uncommitted not-found fixtures = %#v", uncommitted.DynamicIncludeNotFound)
	}
	parallelOrder, _, err := buildScenarioFromTrace(makeDynamicIncludePlan(), []tracepkg.TraceEvent{
		{Kind: tracepkg.EventKindStepSkipped, Sequence: 1, Payload: skippedPayload},
		{Kind: tracepkg.EventKindIncludeNotFound, Sequence: 2, Payload: payload},
	})
	if err != nil {
		t.Fatalf("build parallel-order scenario: %v", err)
	}
	if len(parallelOrder.DynamicIncludeNotFound) != 1 {
		t.Fatalf("parallel-order fixtures = %#v", parallelOrder.DynamicIncludeNotFound)
	}
}

func TestBuildScenarioFromTraceRejectsCrossRunNotFoundAuthority(t *testing.T) {
	notFoundPayload, _ := json.Marshal(map[string]any{
		"step_id": "call-tsg", "qualified_node_id": "call-tsg", "invocation": 1,
		"rendered_ref": "missing/tsg", "reason": "DINC-002: not found",
		"error_code": "DINC-002", "continued": true,
	})
	skippedPayload, _ := json.Marshal(map[string]any{
		"step_id": "call-tsg", "qualified_node_id": "call-tsg", "invocation": 1,
		"reason": "include_not_found",
	})
	scenario, _, err := buildScenarioFromTrace(makeDynamicIncludePlan(), []tracepkg.TraceEvent{
		{RunID: "run-a", Kind: tracepkg.EventKindIncludeNotFound, Payload: notFoundPayload},
		{RunID: "run-b", Kind: tracepkg.EventKindStepSkipped, Payload: skippedPayload},
	})
	if err != nil {
		t.Fatalf("buildScenarioFromTrace: %v", err)
	}
	if len(scenario.DynamicIncludeNotFound) != 0 {
		t.Fatalf("cross-run not-found fixtures = %#v", scenario.DynamicIncludeNotFound)
	}
}

func TestReplayEngineNestedDynamicIncludeNotFoundFromPinnedClosure(t *testing.T) {
	closure, err := plansnapshot.EncodeFlowClosure([]schema.FlowNode{{Step: &schema.Step{
		ID: "inner", Type: schema.StepTypeInclude, IncludeSpec: &schema.IncludeSpec{Include: schema.IncludeConfig{
			RunbookRef: "missing/inner", ResolveFrom: schema.ResolveFromCatalog,
			OnNotFound: schema.OnNotFoundContinue,
		}},
	}}})
	if err != nil {
		t.Fatalf("EncodeFlowClosure: %v", err)
	}
	plan := &engine.ExecutionPlan{
		RunbookPath: "root.runbook.yaml",
		Steps: []engine.ResolvedStep{{ID: "outer", Kind: "include", Spec: &schema.IncludeSpec{Include: schema.IncludeConfig{
			RunbookRef: "pkg/outer", ResolveFrom: schema.ResolveFromCatalog,
		}}}},
		Metadata: engine.PlanMetadata{RunbookID: "root", DynamicIncludes: []schema.LockedDynamicInclude{{
			StepID: "outer", QualifiedNodeID: "outer", Invocation: 1, Revision: 1,
			RenderedRef: "pkg/outer", QualifiedID: "pkg/outer", ExecutableClosure: closure,
		}}},
	}
	structuralPath := []schema.DynamicIncludeFrameIdentity{{
		QualifiedNodeID: "outer", Kind: "include", Invocation: 1,
	}}
	notFoundPayload, _ := json.Marshal(map[string]any{
		"step_id": "inner", "qualified_node_id": "outer/inner", "invocation": 1,
		"structural_path": structuralPath, "rendered_ref": "missing/inner",
		"reason": "DINC-002: not found", "error_code": "DINC-002", "continued": true,
	})
	skippedPayload, _ := json.Marshal(map[string]any{
		"step_id": "inner", "qualified_node_id": "outer/inner", "invocation": 1,
		"structural_path": structuralPath, "reason": "include_not_found",
	})
	scenario, _, err := buildScenarioFromTrace(plan, []tracepkg.TraceEvent{
		{Kind: tracepkg.EventKindIncludeNotFound, Payload: notFoundPayload},
		{Kind: tracepkg.EventKindStepSkipped, Payload: skippedPayload},
	})
	if err != nil {
		t.Fatalf("buildScenarioFromTrace: %v", err)
	}
	if len(scenario.DynamicIncludeNotFound) != 1 ||
		scenario.DynamicIncludeNotFound[0].QualifiedNodeID != "outer/inner" {
		t.Fatalf("nested not-found fixtures = %#v", scenario.DynamicIncludeNotFound)
	}
}

func TestBuildScenarioFromTraceKeepsWaitFilterPerPinnedOccurrence(t *testing.T) {
	waitClosure := func(t *testing.T, eventID string) []byte {
		t.Helper()
		closure, err := plansnapshot.EncodeFlowClosure([]schema.FlowNode{{Step: &schema.Step{
			ID: "wait", Type: schema.StepTypeWaitForEvent, WaitForEventSpec: &schema.WaitForEventSpec{
				Event: schema.WaitEventConfig{Source: schema.EventSourceSignal, ID: eventID},
			},
		}}})
		if err != nil {
			t.Fatalf("EncodeFlowClosure: %v", err)
		}
		return closure
	}
	plan := &engine.ExecutionPlan{
		RunbookPath: "root.runbook.yaml",
		Steps: []engine.ResolvedStep{{ID: "outer", Kind: "include", Spec: &schema.IncludeSpec{Include: schema.IncludeConfig{
			RunbookRef: "${target}", ResolveFrom: schema.ResolveFromCatalog,
		}}}},
		Metadata: engine.PlanMetadata{RunbookID: "root", DynamicIncludes: []schema.LockedDynamicInclude{
			{StepID: "outer", QualifiedNodeID: "outer", Invocation: 1, Revision: 1, ExecutableClosure: waitClosure(t, "event-a")},
			{StepID: "outer", QualifiedNodeID: "outer", Invocation: 2, Revision: 2, ExecutableClosure: waitClosure(t, "event-b")},
		}},
	}
	event := func(invocation int, value string) tracepkg.TraceEvent {
		path := []schema.DynamicIncludeFrameIdentity{{
			QualifiedNodeID: "outer", Kind: "include", Invocation: invocation,
		}}
		payload, _ := json.Marshal(map[string]any{
			"step_id": "wait", "qualified_node_id": "outer/wait",
			"call_path": []engine.DebugCallFrame{{StepID: "outer"}}, "structural_path": path,
			"invocation": 1, "retry_attempt": 1, "payload": map[string]any{"value": value},
		})
		return tracepkg.TraceEvent{RunID: "source", Kind: tracepkg.EventKindEventReceived, Payload: payload}
	}
	scenario, _, err := buildScenarioFromTrace(plan, []tracepkg.TraceEvent{event(1, "a"), event(2, "b")})
	if err != nil {
		t.Fatalf("buildScenarioFromTrace: %v", err)
	}
	if len(scenario.WaitEvents) != 2 || scenario.WaitEvents[0].EventID != "event-a" ||
		scenario.WaitEvents[1].EventID != "event-b" {
		t.Fatalf("pinned wait fixtures = %#v", scenario.WaitEvents)
	}
}

// TestDynamicIncludeReplayExecutor_DeterministicBinding is the determinism
// proof: even after a "catalog mutation" (a different AbsPath would be
// resolved from the catalog today), the executor uses the pinned AbsPath.
func TestDynamicIncludeReplayExecutor_DeterministicBinding(t *testing.T) {
	dir := t.TempDir()
	originalPath := filepath.Join(dir, "tsg-v1.yaml")
	_ = os.WriteFile(originalPath, []byte("# v1"), 0o644)
	digest, _ := fileDigestSHA256(originalPath)

	// "catalog mutation": a different file exists that a live resolver
	// would return, but the pin points to originalPath.
	mutatedPath := filepath.Join(dir, "tsg-v2.yaml")
	_ = os.WriteFile(mutatedPath, []byte("# v2 — catalog mutation"), 0o644)

	spy := &spyIncludeExecutor{}
	exec := &DynamicIncludeReplayExecutor{
		pins: map[string][]schema.LockedDynamicInclude{
			"call-tsg": {{StepID: "call-tsg", QualifiedID: "pkg/tsg", AbsPath: originalPath, FileDigest: digest}},
		},
		innerExec: spy,
		fallback:  NewReplayExecutor("include", &Scenario{AllowUnmatched: true}),
	}
	step := engine.ResolvedStep{
		ID:   "call-tsg",
		Kind: "include",
		Spec: &schema.IncludeSpec{
			Include: schema.IncludeConfig{RunbookRef: "${tsg_id}", ResolveFrom: "catalog"},
		},
	}
	_, err := exec.Execute(context.Background(), step, map[string]any{"tsg_id": "pkg/tsg"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if spy.capturedSpec == nil {
		t.Fatal("inner executor was not called")
	}
	// The inner executor must have been called with the PINNED path, not the
	// mutated/catalog path.
	if spy.capturedSpec.LazyRunbookPath != originalPath {
		t.Errorf("LazyRunbookPath: got %q, want pinned %q", spy.capturedSpec.LazyRunbookPath, originalPath)
	}
	if spy.capturedSpec.LazyRunbookDigest != digest {
		t.Errorf("LazyRunbookDigest: got %q, want pinned %q", spy.capturedSpec.LazyRunbookDigest, digest)
	}
	// RunbookRef must have been cleared (dynamic resolution disabled).
	if spy.capturedSpec.Include.IsDynamic() {
		t.Error("inner executor received a still-dynamic spec; RunbookRef should have been cleared")
	}
}

func TestDynamicIncludeReplayExecutor_DigestMismatchFailsClosedAfterDriftEvent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tsg.yaml")
	_ = os.WriteFile(path, []byte("# current content"), 0o644)

	writer := &captureTraceWriter{}
	spy := &spyIncludeExecutor{}
	exec := &DynamicIncludeReplayExecutor{
		pins: map[string][]schema.LockedDynamicInclude{
			"s1": {{StepID: "s1", QualifiedID: "pkg/tsg", AbsPath: path, FileDigest: "stale-digest"}},
		},
		innerExec:   spy,
		fallback:    NewReplayExecutor("include", &Scenario{AllowUnmatched: true}),
		traceWriter: writer,
		runID:       "run-42",
	}
	step := engine.ResolvedStep{
		ID:   "s1",
		Kind: "include",
		Spec: &schema.IncludeSpec{
			Include: schema.IncludeConfig{RunbookRef: "${tsg}"},
		},
	}
	if _, err := exec.Execute(context.Background(), step, nil); err == nil || !strings.Contains(err.Error(), "digest changed") {
		t.Fatalf("Execute error = %v, want digest changed", err)
	}
	var found bool
	for _, ev := range writer.events {
		if ev.Kind == tracepkg.EventKindReplayDynamicIncludeDrift {
			found = true
		}
	}
	if !found {
		t.Error("expected replay/dynamicIncludeDrift event on digest mismatch")
	}
	if spy.capturedSpec != nil {
		t.Fatal("changed pinned include reached the inner executor")
	}
}

func TestDynamicIncludeReplayExecutor_MatchingDigest_NoDriftEvent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tsg.yaml")
	_ = os.WriteFile(path, []byte("# stable content"), 0o644)
	digest, _ := fileDigestSHA256(path)

	writer := &captureTraceWriter{}
	spy := &spyIncludeExecutor{}
	exec := &DynamicIncludeReplayExecutor{
		pins: map[string][]schema.LockedDynamicInclude{
			"s1": {{StepID: "s1", QualifiedID: "pkg/tsg", AbsPath: path, FileDigest: digest}},
		},
		innerExec:   spy,
		fallback:    NewReplayExecutor("include", &Scenario{AllowUnmatched: true}),
		traceWriter: writer,
		runID:       "run-42",
	}
	step := engine.ResolvedStep{
		ID:   "s1",
		Kind: "include",
		Spec: &schema.IncludeSpec{
			Include: schema.IncludeConfig{RunbookRef: "${tsg}"},
		},
	}
	if _, err := exec.Execute(context.Background(), step, nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, ev := range writer.events {
		if ev.Kind == tracepkg.EventKindReplayDynamicIncludeDrift {
			t.Errorf("unexpected drift event with matching digest: %+v", ev)
		}
	}
}

func TestDynamicIncludeReplayExecutorUsesOccurrenceSpecificPins(t *testing.T) {
	directory := t.TempDir()
	firstPath := filepath.Join(directory, "first.yaml")
	secondPath := filepath.Join(directory, "second.yaml")
	if err := os.WriteFile(firstPath, []byte("first"), 0o600); err != nil {
		t.Fatalf("write first: %v", err)
	}
	if err := os.WriteFile(secondPath, []byte("second"), 0o600); err != nil {
		t.Fatalf("write second: %v", err)
	}
	firstDigest, _ := fileDigestSHA256(firstPath)
	secondDigest, _ := fileDigestSHA256(secondPath)
	spy := &spyIncludeExecutor{}
	executor := &DynamicIncludeReplayExecutor{
		pins: buildPinIndex([]schema.LockedDynamicInclude{
			{StepID: "child", QualifiedNodeID: "each/child", Invocation: 1, Revision: 1,
				StructuralPath: []schema.DynamicIncludeFrameIdentity{{
					QualifiedNodeID: "each", Kind: "iterate", IterationIndex: 1,
				}},
				QualifiedID: "pkg/first", AbsPath: firstPath, FileDigest: firstDigest},
			{StepID: "child", QualifiedNodeID: "each/child", Invocation: 1, Revision: 2,
				StructuralPath: []schema.DynamicIncludeFrameIdentity{{
					QualifiedNodeID: "each", Kind: "iterate", IterationIndex: 2,
				}},
				QualifiedID: "pkg/second", AbsPath: secondPath, FileDigest: secondDigest},
		}),
		innerExec: spy,
		fallback:  NewReplayExecutor("include", &Scenario{AllowUnmatched: true}),
	}
	step := engine.ResolvedStep{ID: "child", Kind: "include", Spec: &schema.IncludeSpec{Include: schema.IncludeConfig{
		RunbookRef: "${target}", ResolveFrom: schema.ResolveFromCatalog,
	}}}
	ctx := engine.WithDispatchExecutionBoundary(context.Background(), engine.DispatchExecutionBoundary{
		QualifiedNodeID: "each/child", CallPath: []engine.DebugCallFrame{{StepID: "each"}}, StepID: "child",
	})
	secondIteration := engine.WithDynamicIncludeStructuralPath(ctx, []schema.DynamicIncludeFrameIdentity{{
		QualifiedNodeID: "each", Kind: "iterate", IterationIndex: 2,
	}})
	firstIteration := engine.WithDynamicIncludeStructuralPath(ctx, []schema.DynamicIncludeFrameIdentity{{
		QualifiedNodeID: "each", Kind: "iterate", IterationIndex: 1,
	}})
	if _, err := executor.Execute(secondIteration, step, nil); err != nil {
		t.Fatalf("second iteration Execute: %v", err)
	}
	if _, err := executor.Execute(firstIteration, step, nil); err != nil {
		t.Fatalf("first iteration Execute: %v", err)
	}
	if len(spy.capturedSpecs) != 2 || spy.capturedSpecs[0].LazyRunbookPath != secondPath ||
		spy.capturedSpecs[1].LazyRunbookPath != firstPath {
		t.Fatalf("replayed pin paths = %#v", spy.capturedSpecs)
	}
}

func TestDynamicIncludeReplayExecutorRestoresDurableChildMetadata(t *testing.T) {
	childPath := filepath.Join(t.TempDir(), "child.runbook.yaml")
	closure, err := plansnapshot.EncodeFlowClosure([]schema.FlowNode{{Step: &schema.Step{
		ID: "continue", Type: schema.StepTypeHandoff,
		HandoffSpec: &schema.HandoffSpec{Handoff: schema.HandoffConfig{
			Runbook: "next.runbook.yaml", Reason: schema.HandoffReason{Code: "next", Summary: "Continue"},
		}},
	}}})
	if err != nil {
		t.Fatalf("EncodeFlowClosure: %v", err)
	}
	var parent internalexecutor.SubStepParent
	var protection engine.DebugProtection
	runner := func(
		ctx context.Context,
		gotParent internalexecutor.SubStepParent,
		nodes []schema.FlowNode,
		_ map[string]any,
	) ([]*engine.StepResult, error) {
		parent = gotParent
		protection = internaldebugprotect.ProtectionFromContext(ctx)
		if len(nodes) != 1 || nodes[0].Step == nil || nodes[0].Step.HandoffSpec == nil {
			t.Fatalf("restored closure nodes = %#v", nodes)
		}
		return nil, nil
	}
	realInclude := internalexecutor.NewIncludeExecutor(&internalexpr.TemplateEvaluator{}, runner, nil)
	forwarder := &forwardingIncludeExecutor{inner: realInclude}
	replayExecutor := &DynamicIncludeReplayExecutor{
		pins: buildPinIndex([]schema.LockedDynamicInclude{{
			StepID: "child", QualifiedID: "pkg/child", AbsPath: childPath,
			RunbookID: "child-id", RunbookName: "Child", RunbookContentHash: strings.Repeat("a", 64),
			FileDigest: engine.InteractionPayloadDigest([]byte("child")), ExecutableClosure: closure,
			ResolvedOutputs: map[string]*schema.Output{"credential": {Type: "secret"}},
		}}),
		innerExec: forwarder,
		fallback:  NewReplayExecutor("include", &Scenario{AllowUnmatched: true}),
	}
	step := engine.ResolvedStep{ID: "child", Kind: "include", Spec: &schema.IncludeSpec{Include: schema.IncludeConfig{
		RunbookRef: "${target}", ResolveFrom: schema.ResolveFromCatalog,
	}}}
	if _, err := replayExecutor.Execute(context.Background(), step, nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if parent.RunbookPath != childPath {
		t.Fatalf("child runbook path = %q, want %q", parent.RunbookPath, childPath)
	}
	if forwarder.captured == nil || forwarder.captured.ResolvedRunbookID != "child-id" ||
		forwarder.captured.ResolvedRunbookName != "Child" ||
		forwarder.captured.ResolvedRunbookContentHash != strings.Repeat("a", 64) {
		t.Fatalf("restored child identity = %#v", forwarder.captured)
	}
	foundCredential := false
	for _, name := range protection.ProtectedVars {
		foundCredential = foundCredential || name == "credential"
	}
	if !foundCredential {
		t.Fatalf("child output protection = %#v, want credential", protection)
	}
}

func TestReplayEnginePinnedClosureHasZeroExternalDispatch(t *testing.T) {
	closure, err := plansnapshot.EncodeFlowClosure([]schema.FlowNode{{Step: &schema.Step{
		ID: "child-cli", Type: schema.StepTypeCLI,
		CLI: &schema.CLISpec{Command: "dangerous", Args: []string{"--execute"}},
	}}})
	if err != nil {
		t.Fatalf("EncodeFlowClosure: %v", err)
	}
	realCalls := 0
	base := internalexecutor.NewMapRegistry()
	base.Register("cli", replayStepExecutorFunc(func(_ context.Context, step engine.ResolvedStep, _ map[string]any) (*engine.StepResult, error) {
		realCalls++
		return &engine.StepResult{StepID: step.ID, Status: engine.StepStatusCompleted, Outcome: engine.StepOutcomeSuccess}, nil
	}))
	runner := func(
		ctx context.Context,
		_ internalexecutor.SubStepParent,
		nodes []schema.FlowNode,
		vars map[string]any,
	) ([]*engine.StepResult, error) {
		var registry engine.ExecutorRegistry = base
		if replayRegistry := internalexecutor.ExecutorRegistryFromContext(ctx); replayRegistry != nil {
			registry = replayRegistry
		}
		results := make([]*engine.StepResult, 0, len(nodes))
		for _, node := range nodes {
			step := engine.ResolvedStep{ID: node.Step.ID, Kind: string(node.Step.Type), Spec: node.Step.CLI}
			result, err := registry.Lookup(step.Kind).Execute(ctx, step, vars)
			if err != nil {
				return results, err
			}
			results = append(results, result)
		}
		return results, nil
	}
	base.Register("include", internalexecutor.NewIncludeExecutor(&internalexpr.TemplateEvaluator{}, runner, nil))
	scenario := &Scenario{Commands: []CommandFixture{{
		Argv: []string{"dangerous", "--execute"}, Stdout: "saved", ExitCode: 0,
	}}}
	pin := schema.LockedDynamicInclude{
		StepID: "outer", QualifiedNodeID: "outer", Invocation: 1, Revision: 1,
		RenderedRef: "pkg/outer", QualifiedID: "pkg/outer", RunbookID: "outer", RunbookName: "Outer",
		RunbookContentHash: strings.Repeat("a", 64), AbsPath: "C:/runbooks/outer.runbook.yaml",
		PackageName: "pkg", PackageVersion: "1.0.0",
		FileDigest:    engine.InteractionPayloadDigest([]byte("outer")),
		PackageDigest: engine.InteractionPayloadDigest([]byte("package")), ExecutableClosure: closure,
	}
	replayRegistry := NewReplayExecutorRegistryWithPins(
		base, scenario, &internalexpr.TemplateEvaluator{}, nil, nil, []schema.LockedDynamicInclude{pin}, nil, "replay",
	)
	step := engine.ResolvedStep{ID: "outer", Kind: "include", Spec: &schema.IncludeSpec{Include: schema.IncludeConfig{
		RunbookRef: "pkg/outer", ResolveFrom: schema.ResolveFromCatalog,
	}}}
	result, err := replayRegistry.Lookup("include").Execute(context.Background(), step, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != engine.StepStatusCompleted {
		t.Fatalf("replay result = %#v", result)
	}
	if realCalls != 0 {
		t.Fatalf("pinned replay executed %d real child boundaries", realCalls)
	}
}

func TestReplayEnginePinnedClosureApprovalUsesSavedFixture(t *testing.T) {
	closure, err := plansnapshot.EncodeFlowClosure([]schema.FlowNode{{Step: &schema.Step{
		ID: "child", Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{},
	}}})
	if err != nil {
		t.Fatalf("EncodeFlowClosure: %v", err)
	}
	live := &liveReplayApprovalGate{}
	base := internalexecutor.NewMapRegistry()
	base.Register("include", internalexecutor.NewIncludeExecutor(
		&internalexpr.TemplateEvaluator{},
		func(context.Context, internalexecutor.SubStepParent, []schema.FlowNode, map[string]any) ([]*engine.StepResult, error) {
			return nil, nil
		}, nil,
	).WithApprovalGate(live))
	const sourceRunID = "source-pinned-approval"
	scenario := &Scenario{
		SourceRunID: sourceRunID,
		Approvals: []sharedreplay.ApprovalBinding{{
			At: exactReplaySelector("outer"), Approved: true, Approver: "saved-operator",
			Source: sharedreplay.Source{Kind: "prior-run", RunID: sourceRunID, InteractionID: "approval"},
		}},
	}
	pin := schema.LockedDynamicInclude{
		StepID: "outer", QualifiedNodeID: "outer", Invocation: 1, Revision: 1,
		RenderedRef: "pkg/outer", QualifiedID: "pkg/outer", RunbookID: "outer", RunbookName: "Outer",
		RunbookContentHash: strings.Repeat("a", 64), AbsPath: "C:/runbooks/outer.runbook.yaml",
		PackageName: "pkg", PackageVersion: "1.0.0",
		FileDigest:    engine.InteractionPayloadDigest([]byte("outer")),
		PackageDigest: engine.InteractionPayloadDigest([]byte("package")), ExecutableClosure: closure,
		ResolvedGovernance: &schema.GovernanceConfig{RequireApproval: true},
	}
	replay := NewReplayExecutorRegistryWithPins(
		base, scenario, &internalexpr.TemplateEvaluator{}, nil, nil, []schema.LockedDynamicInclude{pin}, nil, "replay",
	)
	result, err := replay.Lookup("include").Execute(context.Background(), engine.ResolvedStep{
		ID: "outer", Kind: "include", Spec: &schema.IncludeSpec{Include: schema.IncludeConfig{
			RunbookRef: "pkg/outer", ResolveFrom: schema.ResolveFromCatalog,
		}},
	}, nil)
	if err != nil || result.Status != engine.StepStatusCompleted || live.calls != 0 {
		t.Fatalf("pinned approval result/live calls = %#v/%d, %v", result, live.calls, err)
	}
}

func TestReplayFromTracePinnedClosureConsumesChildBoundaryFixture(t *testing.T) {
	directory := makeReplayDir(t)
	tracePath := filepath.Join(directory, "trace-pinned-child.jsonl")
	writer, err := internaltrace.NewJSONLWriter(tracePath)
	if err != nil {
		t.Fatalf("NewJSONLWriter: %v", err)
	}
	startPayload := testReplayStartPayload("runbook.yaml", "pinned-child-plan")
	structuralPath := []schema.DynamicIncludeFrameIdentity{{
		QualifiedNodeID: "outer", Kind: "include", Invocation: 1,
	}}
	childPayload, _ := json.Marshal(map[string]any{
		"step_id": "child-cli", "kind": "cli", "qualified_node_id": "outer/child-cli",
		"call_path":       []engine.DebugCallFrame{{StepID: "outer", RunbookPath: "C:/runbooks/outer.runbook.yaml"}},
		"structural_path": structuralPath, "invocation": 1, "retry_attempt": 1,
		"status": "completed", "outcome": "success",
		"output": map[string]any{"stdout": "saved-child", "stderr": "", "exit_code": 0},
	})
	appendReplayTraceEvents(t, writer, []tracepkg.TraceEvent{
		{EventID: "start", RunID: "source-pinned", Kind: tracepkg.EventKindRunStarted, Payload: startPayload},
		{EventID: "child", RunID: "source-pinned", Kind: tracepkg.EventKindStepCompleted, Payload: childPayload},
	})
	_ = writer.Close()
	closure, err := plansnapshot.EncodeFlowClosure([]schema.FlowNode{{Step: &schema.Step{
		ID: "child-cli", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "dangerous"},
	}}})
	if err != nil {
		t.Fatalf("EncodeFlowClosure: %v", err)
	}
	pin := schema.LockedDynamicInclude{
		StepID: "outer", QualifiedNodeID: "outer", Invocation: 1, Revision: 1,
		RenderedRef: "pkg/outer", QualifiedID: "pkg/outer", RunbookID: "outer", RunbookName: "Outer",
		RunbookContentHash: strings.Repeat("a", 64), AbsPath: "C:/runbooks/outer.runbook.yaml",
		PackageName: "pkg", PackageVersion: "1.0.0",
		FileDigest:    engine.InteractionPayloadDigest([]byte("outer")),
		PackageDigest: engine.InteractionPayloadDigest([]byte("package")), ExecutableClosure: closure,
	}
	realCalls := 0
	var savedOutput string
	base := internalexecutor.NewMapRegistry()
	base.Register("cli", replayStepExecutorFunc(func(_ context.Context, step engine.ResolvedStep, _ map[string]any) (*engine.StepResult, error) {
		realCalls++
		return &engine.StepResult{StepID: step.ID, Status: engine.StepStatusCompleted, Outcome: engine.StepOutcomeSuccess}, nil
	}))
	runner := func(ctx context.Context, parent internalexecutor.SubStepParent, nodes []schema.FlowNode, vars map[string]any) ([]*engine.StepResult, error) {
		registry := internalexecutor.ExecutorRegistryFromContext(ctx)
		if registry == nil {
			return nil, errors.New("pinned child has no recursive replay registry")
		}
		callPath := append(engine.DebugCallPathFromContext(ctx), engine.DebugCallFrame{StepID: parent.ID, RunbookPath: parent.RunbookPath})
		childCtx := engine.WithDebugCallPath(ctx, callPath)
		childCtx = engine.WithDynamicIncludeStructuralPath(childCtx, structuralPath)
		childCtx = engine.WithDispatchExecutionBoundary(childCtx, engine.DispatchExecutionBoundary{
			QualifiedNodeID: "outer/child-cli", CallPath: callPath, StepID: "child-cli", Invocation: 1, RetryAttempt: 1,
		})
		child := engine.ResolvedStep{ID: "child-cli", Kind: "cli", Spec: nodes[0].Step.CLI}
		result, err := registry.Lookup("cli").Execute(childCtx, child, vars)
		if err == nil {
			savedOutput, _ = result.Output["stdout"].(string)
		}
		return []*engine.StepResult{result}, err
	}
	base.Register("include", internalexecutor.NewIncludeExecutor(&internalexpr.TemplateEvaluator{}, runner, nil))
	plan := engine.ValidatedForTest(&engine.ExecutionPlan{
		RunID: "replay-pinned", RunbookPath: "runbook.yaml",
		Steps: []engine.ResolvedStep{{ID: "outer", Kind: "include", Spec: &schema.IncludeSpec{Include: schema.IncludeConfig{
			RunbookRef: "pkg/outer", ResolveFrom: schema.ResolveFromCatalog,
		}}}},
		Metadata: engine.PlanMetadata{
			RunbookID: "root", PlanHash: "pinned-child-plan", DynamicIncludes: []schema.LockedDynamicInclude{pin},
		},
	})
	replayEngine := NewReplayEngine(engine.EngineConfig{
		Executors: base, Dispatcher: internaleventbus.NewDispatcher(), Evaluator: &internalexpr.TemplateEvaluator{},
		TraceWriter: &captureTraceWriter{}, Platform: platform.NewFakePlatform(),
	}, nil, nil).WithFrozenPlan(plan)
	handle, err := replayEngine.ReplayFromTrace(context.Background(), tracePath, "", engine.RunOptions{})
	if err != nil {
		t.Fatalf("ReplayFromTrace: %v", err)
	}
	result, err := handle.Next(context.Background())
	if err != nil || result.Status != engine.StepStatusCompleted {
		t.Fatalf("Next result/error = %#v/%v", result, err)
	}
	if realCalls != 0 || savedOutput != "saved-child" {
		t.Fatalf("pinned child real calls/saved output = %d/%q", realCalls, savedOutput)
	}
}

func TestReplayEngineIterateConsumesDynamicNotFoundFixture(t *testing.T) {
	structuralPath := []schema.DynamicIncludeFrameIdentity{{
		QualifiedNodeID: "each", Kind: "iterate", IterationIndex: 1, Invocation: 1,
	}}
	scenario := &Scenario{DynamicIncludeNotFound: []DynamicIncludeNotFoundFixture{{
		StepID: "dynamic", QualifiedNodeID: "each/dynamic", Invocation: 1,
		StructuralPath: structuralPath, RenderedRef: "missing/child", Reason: "DINC-002: not found",
	}}}
	base := internalexecutor.NewMapRegistry()
	spy := &spyIncludeExecutor{}
	base.Register("include", spy)
	base.Register("iterate", replayStepExecutorFunc(func(ctx context.Context, step engine.ResolvedStep, vars map[string]any) (*engine.StepResult, error) {
		registry := internalexecutor.ExecutorRegistryFromContext(ctx)
		if registry == nil {
			return nil, errors.New("iterate has no recursive replay registry")
		}
		childCtx := engine.WithDispatchExecutionBoundary(ctx, engine.DispatchExecutionBoundary{
			QualifiedNodeID: "each/dynamic", CallPath: []engine.DebugCallFrame{{StepID: "each"}},
			StepID: "dynamic", Invocation: 1,
		})
		childCtx = engine.WithDynamicIncludeStructuralPath(childCtx, structuralPath)
		child := engine.ResolvedStep{ID: "dynamic", Kind: "include", Spec: &schema.IncludeSpec{Include: schema.IncludeConfig{
			RunbookRef: "missing/child", ResolveFrom: schema.ResolveFromCatalog, OnNotFound: schema.OnNotFoundContinue,
		}}}
		result, err := registry.Lookup("include").Execute(childCtx, child, vars)
		if err != nil {
			return nil, err
		}
		return &engine.StepResult{
			StepID: step.ID, Status: engine.StepStatusCompleted, Outcome: engine.StepOutcomeSuccess,
			Vars: result.Vars,
		}, nil
	}))
	replayRegistry := NewReplayExecutorRegistryWithPins(
		base, scenario, &internalexpr.TemplateEvaluator{}, nil, nil, nil, nil, "replay",
	)
	result, err := replayRegistry.Lookup("iterate").Execute(context.Background(), engine.ResolvedStep{
		ID: "each", Kind: "iterate", Spec: &schema.IterateNode{ID: "each"},
	}, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != engine.StepStatusCompleted || result.Vars["runbook_found"] != false ||
		!scenario.DynamicIncludeNotFound[0].matched {
		t.Fatalf("iterate replay result/fixture = %#v/%#v", result, scenario.DynamicIncludeNotFound)
	}
	if spy.capturedSpec != nil {
		t.Fatal("iterate replay called the real include executor")
	}
}

// --- TestReplayEngine_DynamicInclude_EndToEnd: through the full engine ---

// writeDynamicIncludeTrace writes a trace file for a run that contained:
//  1. run/started
//  2. step/completed for "cli-step" (a regular CLI step)
//  3. step/completed for "call-tsg" (the dynamic include step)
func writeDynamicIncludeTrace(t *testing.T, dir, runbookPath string) string {
	t.Helper()
	tracePath := filepath.Join(dir, "trace-dinc.jsonl")
	writer, err := internaltrace.NewJSONLWriter(tracePath)
	if err != nil {
		t.Fatalf("NewJSONLWriter: %v", err)
	}

	startPayload := testReplayStartPayload(runbookPath, "dynamic-replay-plan")
	cliPayload, _ := json.Marshal(map[string]any{
		"step_id": "cli-step", "kind": "cli", "qualified_node_id": "cli-step",
		"invocation": 1, "retry_attempt": 1, "status": "completed", "outcome": "success",
		"output": map[string]any{"stdout": "ok\n", "exit_code": 0},
	})
	incPayload, _ := json.Marshal(map[string]any{
		"step_id": "call-tsg",
		"output":  map[string]any{"runbook_found": true},
	})
	appendReplayTraceEvents(t, writer, []tracepkg.TraceEvent{
		{EventID: "e1", RunID: "run-dinc", Kind: tracepkg.EventKindRunStarted, Payload: startPayload},
		{EventID: "e2", RunID: "run-dinc", Kind: tracepkg.EventKindStepCompleted, Payload: cliPayload},
		{EventID: "e3", RunID: "run-dinc", Kind: tracepkg.EventKindStepCompleted, Payload: incPayload},
	})
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return tracePath
}

// makeDynamicIncludePlan returns an execution plan that contains a CLI step
// and a dynamic include step. The dynamic include step uses the IncludeSpec
// format emitted by the planner (RunbookRef set, LazyRunbookPath empty).
func makeDynamicIncludePlan() *engine.ExecutionPlan {
	return &engine.ExecutionPlan{
		RunID:       "run-dinc",
		RunbookPath: "runbook.yaml",
		Steps: []engine.ResolvedStep{
			{ID: "cli-step", Kind: "cli", Spec: &schema.CLISpec{Command: "echo", Args: []string{"ok"}}},
			{ID: "call-tsg", Kind: "include", Spec: &schema.IncludeSpec{
				Include: schema.IncludeConfig{
					RunbookRef:  "${tsg_id}",
					ResolveFrom: "catalog",
				},
				// LazyRunbookPath is empty — this is a dynamic include placeholder
			}},
		},
		Metadata: engine.PlanMetadata{RunbookID: "rb-dinc", RunbookName: "test", PlanHash: "dynamic-replay-plan"},
	}
}

// TestReplayEngine_DynamicInclude_DeterministicViaEngine exercises the
// complete ReplayFromTrace path with a dynamic include step. After a
// "catalog mutation" (the original pinned runbook is unchanged, but a
// different file now exists that a live resolver would return), the engine
// must re-bind from the pinned AbsPath and NOT consult the catalog.
func TestReplayEngine_DynamicInclude_DeterministicViaEngine(t *testing.T) {
	dir := makeReplayDir(t)

	// Two runbook files: original (pinned) and mutated (what a live
	// catalog resolution would return today after the "mutation").
	originalPath := filepath.Join(dir, "tsg-v1.yaml")
	_ = os.WriteFile(originalPath, []byte("# v1 — original pinned runbook"), 0o644)
	originalDigest, _ := fileDigestSHA256(originalPath)

	_ = os.WriteFile(filepath.Join(dir, "tsg-v2.yaml"),
		[]byte("# v2 — mutated catalog would return this"), 0o644)

	tracePath := writeDynamicIncludeTrace(t, dir, "runbook.yaml")
	closure, err := plansnapshot.EncodeFlowClosure([]schema.FlowNode{{Step: &schema.Step{
		ID: "saved-child", Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{},
	}}})
	if err != nil {
		t.Fatalf("EncodeFlowClosure: %v", err)
	}

	plan := makeDynamicIncludePlan()
	plan.Metadata.DynamicIncludes = []schema.LockedDynamicInclude{
		{
			StepID: "call-tsg", QualifiedNodeID: "call-tsg", Invocation: 1, Revision: 1,
			RenderedRef: "pkg/tsg", QualifiedID: "pkg/tsg", AbsPath: originalPath,
			FileDigest: originalDigest, PackageDigest: engine.InteractionPayloadDigest([]byte("package")),
			RunbookID: "child", RunbookName: "Child", RunbookContentHash: strings.Repeat("a", 64),
			ExecutableClosure: closure,
		},
	}

	spy := &spyIncludeExecutor{}
	inner := internalexecutor.NewMapRegistry()
	inner.Register("cli", &countingExecutor{})
	inner.Register("include", spy)

	traceWriter := &captureTraceWriter{}
	cfg := engine.EngineConfig{
		Executors:   inner,
		Dispatcher:  internaleventbus.NewDispatcher(),
		TraceWriter: traceWriter,
		Platform:    platform.NewFakePlatform(),
	}

	replayEngine := NewReplayEngine(cfg, nil, nil).WithFrozenPlan(engine.ValidatedForTest(plan))

	handle, err := replayEngine.ReplayFromTrace(context.Background(), tracePath, "", engine.RunOptions{})
	if err != nil {
		t.Fatalf("ReplayFromTrace: %v", err)
	}

	// Drain all steps.
	for {
		if _, err := handle.Next(context.Background()); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("Next: %v", err)
		}
	}

	// The inner executor receives only the captured closure, never a lazy file path.
	if spy.capturedSpec == nil {
		t.Fatal("include executor was never called — dynamic include step was not routed through it")
	}
	if spy.capturedSpec.LazyRunbookPath != "" || spy.capturedSpec.ResolvedRunbookPath != originalPath ||
		len(spy.capturedSpec.ResolvedSteps) != 1 {
		t.Errorf("frozen re-binding = %#v", spy.capturedSpec)
	}
	if spy.capturedSpec.Include.IsDynamic() {
		t.Error("spec was still dynamic when passed to inner executor; catalog re-resolution could occur")
	}

	// No drift event should have been emitted (file is unchanged).
	for _, ev := range traceWriter.events {
		if ev.Kind == tracepkg.EventKindReplayDynamicIncludeDrift {
			t.Errorf("unexpected drift event: catalog mutation did not change the pinned file; %+v", ev)
		}
	}
}

func TestReplayEngineDynamicIncludeNotFoundContinuesWithoutDispatch(t *testing.T) {
	directory := makeReplayDir(t)
	tracePath := filepath.Join(directory, "trace-not-found.jsonl")
	writer, err := internaltrace.NewJSONLWriter(tracePath)
	if err != nil {
		t.Fatalf("NewJSONLWriter: %v", err)
	}
	startPayload := testReplayStartPayload("runbook.yaml", "not-found-plan")
	notFoundPayload, _ := json.Marshal(map[string]any{
		"step_id": "call-tsg", "qualified_node_id": "call-tsg", "invocation": 1,
		"structural_path": []schema.DynamicIncludeFrameIdentity(nil),
		"rendered_ref":    "missing/tsg", "reason": "DINC-002: not found",
		"error_code": "DINC-002", "continued": true,
	})
	skippedPayload, _ := json.Marshal(map[string]any{
		"step_id": "call-tsg", "qualified_node_id": "call-tsg", "invocation": 1,
		"structural_path": []schema.DynamicIncludeFrameIdentity(nil), "reason": "include_not_found",
	})
	appendReplayTraceEvents(t, writer, []tracepkg.TraceEvent{
		{EventID: "start", RunID: "run-not-found", Kind: tracepkg.EventKindRunStarted, Payload: startPayload},
		{EventID: "not-found", RunID: "run-not-found", Kind: tracepkg.EventKindIncludeNotFound, Payload: notFoundPayload},
		{EventID: "skipped", RunID: "run-not-found", Kind: tracepkg.EventKindStepSkipped, Payload: skippedPayload},
	})
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	plan := engine.ValidatedForTest(&engine.ExecutionPlan{
		RunID: "run-not-found", RunbookPath: "runbook.yaml",
		Steps: []engine.ResolvedStep{
			{ID: "call-tsg", Kind: "include", Spec: &schema.IncludeSpec{Include: schema.IncludeConfig{
				RunbookRef: "missing/tsg", ResolveFrom: schema.ResolveFromCatalog, OnNotFound: schema.OnNotFoundContinue,
			}}},
			{ID: "after", Kind: "noop", Spec: &schema.NoopSpec{}},
		},
		Metadata: engine.PlanMetadata{RunbookID: "not-found", PlanHash: "not-found-plan"},
	})
	spy := &spyIncludeExecutor{}
	registry := internalexecutor.NewMapRegistry()
	registry.Register("include", spy)
	registry.Register("noop", internalexecutor.NewNoopExecutor(&internalexpr.TemplateEvaluator{}))
	replayEngine := NewReplayEngine(engine.EngineConfig{
		Executors: registry, Dispatcher: internaleventbus.NewDispatcher(),
		TraceWriter: &captureTraceWriter{}, Platform: platform.NewFakePlatform(),
	}, nil, nil).WithFrozenPlan(plan)
	handle, err := replayEngine.ReplayFromTrace(context.Background(), tracePath, "", engine.RunOptions{})
	if err != nil {
		t.Fatalf("ReplayFromTrace: %v", err)
	}
	first, err := handle.Next(context.Background())
	if err != nil {
		t.Fatalf("not-found Next: %v", err)
	}
	if first.Status != engine.StepStatusSkipped || first.Vars["runbook_found"] != false {
		t.Fatalf("not-found replay result = %#v", first)
	}
	second, err := handle.Next(context.Background())
	if err != nil {
		t.Fatalf("after Next: %v", err)
	}
	if second.StepID != "after" || second.Status != engine.StepStatusCompleted {
		t.Fatalf("after replay result = %#v", second)
	}
	if spy.capturedSpec != nil {
		t.Fatal("not-found replay called the real include executor")
	}
}

func TestReplayEngineParallelDynamicIncludeNotFoundContinues(t *testing.T) {
	directory := makeReplayDir(t)
	tracePath := filepath.Join(directory, "trace-parallel-not-found.jsonl")
	writer, err := internaltrace.NewJSONLWriter(tracePath)
	if err != nil {
		t.Fatalf("NewJSONLWriter: %v", err)
	}
	structuralPath := []schema.DynamicIncludeFrameIdentity{{
		QualifiedNodeID: "parallel", Kind: "parallel", BranchLabel: "left", Invocation: 1,
	}}
	startPayload := testReplayStartPayload("runbook.yaml", "parallel-not-found-plan")
	skippedPayload, _ := json.Marshal(map[string]any{
		"step_id": "dynamic", "qualified_node_id": "parallel/dynamic", "invocation": 1,
		"structural_path": structuralPath, "reason": "include_not_found",
	})
	notFoundPayload, _ := json.Marshal(map[string]any{
		"step_id": "dynamic", "qualified_node_id": "parallel/dynamic", "invocation": 1,
		"structural_path": structuralPath, "rendered_ref": "missing/child",
		"reason": "DINC-002: not found", "error_code": "DINC-002", "continued": true,
	})
	appendReplayTraceEvents(t, writer, []tracepkg.TraceEvent{
		{EventID: "start", RunID: "parallel-not-found", Kind: tracepkg.EventKindRunStarted, Payload: startPayload},
		{EventID: "skipped", RunID: "parallel-not-found", Kind: tracepkg.EventKindStepSkipped, Payload: skippedPayload},
		{EventID: "not-found", RunID: "parallel-not-found", Kind: tracepkg.EventKindIncludeNotFound, Payload: notFoundPayload},
	})
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	dynamicSpec := &schema.IncludeSpec{Include: schema.IncludeConfig{
		RunbookRef: "missing/child", ResolveFrom: schema.ResolveFromCatalog, OnNotFound: schema.OnNotFoundContinue,
	}}
	parallelSpec := &schema.ParallelNode{ID: "parallel", Branches: []schema.ParallelBranch{{
		Label: "left", Steps: []schema.FlowNode{{Step: &schema.Step{
			ID: "dynamic", Type: schema.StepTypeInclude, IncludeSpec: dynamicSpec,
		}}},
	}}}
	plan := &engine.ExecutionPlan{
		RunID: "parallel-not-found", RunbookPath: "runbook.yaml",
		Steps: []engine.ResolvedStep{
			{ID: "parallel", Kind: "parallel", Spec: parallelSpec},
			{ID: "dynamic", Kind: "include", Spec: dynamicSpec, Depth: 1,
				ParentID: "parallel", ParentKind: "parallel", BranchLabel: "left"},
		},
		Metadata: engine.PlanMetadata{RunbookID: "parallel-not-found", PlanHash: "parallel-not-found-plan"},
	}
	if err := internalplanner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	spy := &spyIncludeExecutor{}
	registry := internalexecutor.NewMapRegistry()
	registry.Register("include", spy)
	replayEngine := NewReplayEngine(engine.EngineConfig{
		Executors: registry, Dispatcher: internaleventbus.NewDispatcher(),
		TraceWriter: &captureTraceWriter{}, Platform: platform.NewFakePlatform(),
	}, nil, nil).WithFrozenPlan(plan)
	handle, err := replayEngine.ReplayFromTrace(context.Background(), tracePath, "", engine.RunOptions{})
	if err != nil {
		t.Fatalf("ReplayFromTrace: %v", err)
	}
	result, err := handle.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if result.StepID != "parallel" || result.Status != engine.StepStatusCompleted {
		t.Fatalf("parallel replay result = %#v", result)
	}
	if spy.capturedSpec != nil {
		t.Fatal("parallel not-found replay called the real include executor")
	}
}

func TestReplayEngineDynamicIncludeWithoutClosureFailsClosed(t *testing.T) {
	dir := makeReplayDir(t)

	path := filepath.Join(dir, "tsg.yaml")
	_ = os.WriteFile(path, []byte("# original content"), 0o644)
	fileDigest, err := fileDigestSHA256(path)
	if err != nil {
		t.Fatalf("fileDigestSHA256: %v", err)
	}

	tracePath := writeDynamicIncludeTrace(t, dir, "runbook.yaml")
	plan := makeDynamicIncludePlan()
	plan.Metadata.DynamicIncludes = []schema.LockedDynamicInclude{
		{
			StepID: "call-tsg", QualifiedNodeID: "call-tsg", Invocation: 1, Revision: 1,
			RenderedRef: "pkg/tsg", QualifiedID: "pkg/tsg", AbsPath: path,
			FileDigest: fileDigest, PackageDigest: engine.InteractionPayloadDigest([]byte("package")),
		},
	}

	spy := &spyIncludeExecutor{}
	inner := internalexecutor.NewMapRegistry()
	inner.Register("cli", &countingExecutor{})
	inner.Register("include", spy)

	traceWriter := &captureTraceWriter{}
	cfg := engine.EngineConfig{
		Executors:   inner,
		Dispatcher:  internaleventbus.NewDispatcher(),
		TraceWriter: traceWriter,
		Platform:    platform.NewFakePlatform(),
	}

	replayEngine := NewReplayEngine(cfg, nil, nil).WithFrozenPlan(engine.ValidatedForTest(plan))

	_, err = replayEngine.ReplayFromTrace(context.Background(), tracePath, "", engine.RunOptions{})
	if !engine.IsReplayBoundaryError(err) || !strings.Contains(err.Error(), "requires an executable closure") {
		t.Fatalf("ReplayFromTrace error = %v, want executable-closure boundary", err)
	}
	if spy.capturedSpec != nil {
		t.Error("path-only pinned include reached the inner executor")
	}
}
