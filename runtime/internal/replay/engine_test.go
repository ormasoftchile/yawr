package replay

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	internalengine "github.com/ormasoftchile/yawr/runtime/internal/engine"
	internaleventbus "github.com/ormasoftchile/yawr/runtime/internal/eventbus"
	internalexecutor "github.com/ormasoftchile/yawr/runtime/internal/executor"
	internalexpr "github.com/ormasoftchile/yawr/runtime/internal/expr"
	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	internalrunstore "github.com/ormasoftchile/yawr/runtime/internal/runstore"
	internaltrace "github.com/ormasoftchile/yawr/runtime/internal/trace"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/eventbus"
	evidencepkg "github.com/ormasoftchile/yawr/runtime/pkg/evidence"
	"github.com/ormasoftchile/yawr/runtime/pkg/extension"
	"github.com/ormasoftchile/yawr/runtime/pkg/governance"
	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	sharedreplay "github.com/ormasoftchile/yawr/runtime/pkg/replay"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	tracepkg "github.com/ormasoftchile/yawr/runtime/pkg/trace"
	"gopkg.in/yaml.v3"
)

type liveReplayDispatcher struct{ calls int }

func (*liveReplayDispatcher) Dispatch(eventbus.InboundEvent) error { return nil }
func (dispatcher *liveReplayDispatcher) Wait(
	context.Context,
	string,
	eventbus.EventFilter,
	time.Duration,
) (*eventbus.InboundEvent, error) {
	dispatcher.calls++
	return &eventbus.InboundEvent{EventID: "live"}, nil
}
func (*liveReplayDispatcher) Cancel(string, string) {}

type liveReplayApprovalGate struct{ calls int }

type liveReplayEvidenceHook struct{ calls int }

func (hook *liveReplayEvidenceHook) Collect(context.Context, engine.ResolvedStep, *engine.StepResult, string) ([]evidencepkg.EvidenceRecord, error) {
	hook.calls++
	return nil, nil
}

type liveReplayExtensionHost struct{ loads int }

func (host *liveReplayExtensionHost) Load(context.Context, *extension.ProjectManifest) error {
	host.loads++
	return nil
}
func (*liveReplayExtensionHost) Shutdown(context.Context) error                  { return nil }
func (*liveReplayExtensionHost) ContributedTools() []*schema.ToolDef             { return nil }
func (*liveReplayExtensionHost) ContributedProviders() []*schema.ProviderDef     { return nil }
func (*liveReplayExtensionHost) ContributedPolicyRules() []governance.PolicyRule { return nil }
func (*liveReplayExtensionHost) Status() []extension.ExtensionStatus             { return nil }

func (gate *liveReplayApprovalGate) RequestApproval(
	context.Context,
	string,
	string,
) (governance.ApprovalRecord, error) {
	gate.calls++
	return governance.ApprovalRecord{Approver: "live"}, nil
}

func writeReplayScenario(t *testing.T, directory string, scenario Scenario) string {
	t.Helper()
	data, err := yaml.Marshal(scenario)
	if err != nil {
		t.Fatalf("Marshal scenario: %v", err)
	}
	path := filepath.Join(directory, "scenario.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile scenario: %v", err)
	}
	return path
}

func writeReplayStartTrace(t *testing.T, directory, runID, planHash string) string {
	t.Helper()
	path := filepath.Join(directory, "start-trace.jsonl")
	writer, err := internaltrace.NewJSONLWriter(path)
	if err != nil {
		t.Fatalf("NewJSONLWriter: %v", err)
	}
	lock := testReplayDependencyLock(planHash)
	payload, _ := json.Marshal(map[string]any{
		"runbook_path": "runbook.yaml", "plan_hash": lock.PlanHash, "graph_hash": lock.GraphHash,
		"catalog_digest": lock.CatalogDigest, "package_lock_digest": lock.PackageLockDigest,
		"tool_digest": lock.ToolDigest, "profile_digest": lock.ProfileDigest,
	})
	if err := writer.Append(tracepkg.TraceEvent{
		EventID: "start", RunID: runID, Kind: tracepkg.EventKindRunStarted,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano), Sequence: 1, Payload: payload,
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

func appendReplayTraceEvents(t *testing.T, writer *internaltrace.JSONLWriter, events []tracepkg.TraceEvent) {
	t.Helper()
	sequences := make(map[string]int64)
	for _, event := range events {
		if event.Sequence == 0 {
			sequences[event.RunID]++
			event.Sequence = sequences[event.RunID]
		} else {
			sequences[event.RunID] = event.Sequence
		}
		if event.Timestamp == "" {
			event.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
		}
		if err := writer.Append(event); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
}

func testReplayDependencyLock(planHash string) sharedreplay.DependencyLock {
	lock, err := sharedreplay.DependencyLockForPlan(&engine.ExecutionPlan{
		Metadata: engine.PlanMetadata{PlanHash: planHash},
	})
	if err != nil {
		panic(err)
	}
	return lock
}

func testReplayStartPayload(runbookPath, planHash string) []byte {
	lock := testReplayDependencyLock(planHash)
	payload, _ := json.Marshal(map[string]any{
		"runbook_path": runbookPath, "plan_hash": lock.PlanHash, "graph_hash": lock.GraphHash,
		"catalog_digest": lock.CatalogDigest, "package_lock_digest": lock.PackageLockDigest,
		"tool_digest": lock.ToolDigest, "profile_digest": lock.ProfileDigest,
	})
	return payload
}

func exactReplaySelector(stepID string) sharedreplay.Selector {
	return sharedreplay.Selector{
		QualifiedNodeID: stepID, Step: stepID, Phase: "execute", Invocation: 1, Attempt: 1,
	}
}

func reviewedReplayFixture() sharedreplay.Review {
	return sharedreplay.Review{
		State: "reviewed", ReviewedBy: "reviewer-id",
		ReviewedAt: "2026-09-01T00:00:00Z", SensitivityReviewed: true,
	}
}

func TestReplayFromTraceStrictRequiresFrozenPlan(t *testing.T) {
	directory := makeReplayDir(t)
	tracePath := writeReplayStartTrace(t, directory, "source-strict", "strict-plan")
	plan := &engine.ExecutionPlan{
		RunID: "replay-strict", RunbookPath: "runbook.yaml",
		Steps:    []engine.ResolvedStep{{ID: "pure", Kind: "noop", Spec: &schema.NoopSpec{}}},
		Metadata: engine.PlanMetadata{RunbookID: "strict", PlanHash: "strict-plan"},
	}
	_, err := NewReplayEngine(engine.EngineConfig{
		Executors: internalexecutor.NewMapRegistry(), Dispatcher: internaleventbus.NewDispatcher(),
		TraceWriter: &captureTraceWriter{}, Platform: platform.NewFakePlatform(),
	}, &fakeParser{result: &parser.ParsedRunbook{Source: "mutable"}}, &fakePlanner{plan: plan}).ReplayFromTrace(
		context.Background(), tracePath, "", engine.RunOptions{},
	)
	if !engine.IsReplayBoundaryError(err) {
		t.Fatalf("ReplayFromTrace error = %v, want frozen-plan replay boundary", err)
	}
}

func TestReplayFromTraceRejectsMalformedCompleteRecord(t *testing.T) {
	directory := makeReplayDir(t)
	tracePath := writeReplayStartTrace(t, directory, "source-malformed", "malformed-plan")
	file, err := os.OpenFile(tracePath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := file.WriteString("{not-json}\n"); err != nil {
		_ = file.Close()
		t.Fatalf("WriteString: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	plan := engine.ValidatedForTest(&engine.ExecutionPlan{
		RunID: "replay-malformed", RunbookPath: "runbook.yaml",
		Steps:    []engine.ResolvedStep{{ID: "pure", Kind: "noop", Spec: &schema.NoopSpec{}}},
		Metadata: engine.PlanMetadata{RunbookID: "malformed", PlanHash: "malformed-plan"},
	})
	_, err = NewReplayEngine(engine.EngineConfig{
		Executors: internalexecutor.NewMapRegistry(), Dispatcher: internaleventbus.NewDispatcher(),
		TraceWriter: &captureTraceWriter{}, Platform: platform.NewFakePlatform(),
	}, nil, nil).WithFrozenPlan(plan).ReplayFromTrace(context.Background(), tracePath, "", engine.RunOptions{})
	if err == nil || !strings.Contains(err.Error(), "strict JSONL line 2 is malformed") {
		t.Fatalf("ReplayFromTrace error = %v", err)
	}
}

func TestReplayFromTraceRejectsScenarioDependencyLockThatDiffersFromTrace(t *testing.T) {
	directory := makeReplayDir(t)
	const sourceRunID = "source-lock"
	tracePath := writeReplayStartTrace(t, directory, sourceRunID, "source-plan")
	scenarioPath := writeReplayScenario(t, directory, Scenario{
		SourceRunID: sourceRunID, DependencyLock: testReplayDependencyLock("forged-plan"),
		WaitEvents: []WaitEventBinding{{
			At: exactReplaySelector("wait"), EventID: "ready", Source: "signal",
			Provenance: sharedreplay.Source{Kind: "prior-run", RunID: sourceRunID}, Review: reviewedReplayFixture(),
		}},
	})
	plan := engine.ValidatedForTest(&engine.ExecutionPlan{
		RunID: "replay-lock", RunbookPath: "runbook.yaml",
		Steps: []engine.ResolvedStep{{ID: "wait", Kind: "wait_for_event", Spec: &schema.WaitForEventSpec{
			Event: schema.WaitEventConfig{Source: schema.EventSourceSignal, ID: "ready"},
		}}},
		Metadata: engine.PlanMetadata{RunbookID: "lock", PlanHash: "forged-plan"},
	})
	_, err := NewReplayEngine(engine.EngineConfig{
		Executors: internalexecutor.NewMapRegistry(), Dispatcher: internaleventbus.NewDispatcher(),
		TraceWriter: &captureTraceWriter{}, Platform: platform.NewFakePlatform(),
	}, nil, nil).WithFrozenPlan(plan).ReplayFromTrace(context.Background(), tracePath, scenarioPath, engine.RunOptions{})
	if !engine.IsReplayBoundaryError(err) {
		t.Fatalf("ReplayFromTrace error = %v, want source dependency-lock boundary", err)
	}
}

func TestReplayFromTraceStrictRejectsIncompleteDependencyMetadata(t *testing.T) {
	directory := makeReplayDir(t)
	tracePath := filepath.Join(directory, "incomplete-trace.jsonl")
	writer, err := internaltrace.NewJSONLWriter(tracePath)
	if err != nil {
		t.Fatalf("NewJSONLWriter: %v", err)
	}
	payload, _ := json.Marshal(map[string]any{"runbook_path": "runbook.yaml", "plan_hash": "incomplete-plan"})
	if err := writer.Append(tracepkg.TraceEvent{
		EventID: "start", RunID: "source-incomplete", Kind: tracepkg.EventKindRunStarted,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano), Sequence: 1, Payload: payload,
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	plan := engine.ValidatedForTest(&engine.ExecutionPlan{
		RunID: "replay-incomplete", RunbookPath: "runbook.yaml",
		Steps:    []engine.ResolvedStep{{ID: "pure", Kind: "noop", Spec: &schema.NoopSpec{}}},
		Metadata: engine.PlanMetadata{RunbookID: "incomplete", PlanHash: "incomplete-plan"},
	})
	_, err = NewReplayEngine(engine.EngineConfig{
		Executors: internalexecutor.NewMapRegistry(), Dispatcher: internaleventbus.NewDispatcher(),
		TraceWriter: &captureTraceWriter{}, Platform: platform.NewFakePlatform(),
	}, nil, nil).WithFrozenPlan(plan).ReplayFromTrace(context.Background(), tracePath, "", engine.RunOptions{})
	if !engine.IsReplayBoundaryError(err) {
		t.Fatalf("ReplayFromTrace error = %v, want incomplete dependency boundary", err)
	}
}

func TestReplayFromTraceStrictRejectsEmptyGraphAndCatalogDependencies(t *testing.T) {
	directory := makeReplayDir(t)
	tracePath := filepath.Join(directory, "empty-dependency-trace.jsonl")
	writer, err := internaltrace.NewJSONLWriter(tracePath)
	if err != nil {
		t.Fatalf("NewJSONLWriter: %v", err)
	}
	lock := testReplayDependencyLock("empty-dependency-plan")
	payload, _ := json.Marshal(map[string]any{
		"runbook_path": "runbook.yaml", "plan_hash": lock.PlanHash,
		"graph_hash": "", "catalog_digest": "", "package_lock_digest": lock.PackageLockDigest,
		"tool_digest": lock.ToolDigest, "profile_digest": lock.ProfileDigest,
	})
	if err := writer.Append(tracepkg.TraceEvent{
		EventID: "start", RunID: "source-empty-dependency", Kind: tracepkg.EventKindRunStarted,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano), Sequence: 1, Payload: payload,
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	plan := engine.ValidatedForTest(&engine.ExecutionPlan{
		RunID: "replay-empty-dependency", RunbookPath: "runbook.yaml",
		Steps:    []engine.ResolvedStep{{ID: "pure", Kind: "noop", Spec: &schema.NoopSpec{}}},
		Metadata: engine.PlanMetadata{RunbookID: "empty-dependency", PlanHash: "empty-dependency-plan"},
	})
	_, err = NewReplayEngine(engine.EngineConfig{
		Executors: internalexecutor.NewMapRegistry(), Dispatcher: internaleventbus.NewDispatcher(),
		TraceWriter: &captureTraceWriter{}, Platform: platform.NewFakePlatform(),
	}, nil, nil).WithFrozenPlan(plan).ReplayFromTrace(context.Background(), tracePath, "", engine.RunOptions{})
	if !engine.IsReplayBoundaryError(err) {
		t.Fatalf("ReplayFromTrace error = %v, want empty dependency boundary", err)
	}
}

func TestReplayFromTraceScenarioRequiresExplicitPatternMode(t *testing.T) {
	directory := makeReplayDir(t)
	tracePath := filepath.Join(directory, "trace.jsonl")
	writeTrace(t, tracePath, "runbook.yaml")
	scenarioPath := filepath.Join(directory, "scenario.yaml")
	if err := os.WriteFile(scenarioPath, []byte(overrideScenarioYAML()), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := NewReplayEngine(engine.EngineConfig{
		Executors: internalexecutor.NewMapRegistry(), Dispatcher: internaleventbus.NewDispatcher(),
		TraceWriter: &captureTraceWriter{}, Platform: platform.NewFakePlatform(),
	}, &fakeParser{result: &parser.ParsedRunbook{Source: "current"}}, &fakePlanner{plan: makePlan()}).ReplayFromTrace(
		context.Background(), tracePath, scenarioPath, engine.RunOptions{},
	)
	if !engine.IsReplayBoundaryError(err) {
		t.Fatalf("ReplayFromTrace error = %v, want explicit pattern-mode boundary", err)
	}
}

func TestReplayFromTraceStrictDisablesLiveExtensionAndEvidenceProviders(t *testing.T) {
	directory := makeReplayDir(t)
	tracePath := writeReplayStartTrace(t, directory, "source-provider-isolation", "provider-plan")
	registry := internalexecutor.NewMapRegistry()
	registry.Register("noop", internalexecutor.NewNoopExecutor(nil))
	plan := engine.ValidatedForTest(&engine.ExecutionPlan{
		RunID: "replay-provider-isolation", RunbookPath: "runbook.yaml",
		Steps: []engine.ResolvedStep{{ID: "pure", Kind: "noop", Spec: &schema.NoopSpec{}}},
		Metadata: engine.PlanMetadata{
			RunbookID: "provider-isolation", PlanHash: "provider-plan",
			Extensions: []*schema.ExtensionRef{{Name: "live-extension"}},
		},
	})
	extensionHost := &liveReplayExtensionHost{}
	evidenceHook := &liveReplayEvidenceHook{}
	handle, err := NewReplayEngine(engine.EngineConfig{
		Executors: registry, Dispatcher: internaleventbus.NewDispatcher(),
		TraceWriter: &captureTraceWriter{}, Platform: platform.NewFakePlatform(),
		ExtensionHost: extensionHost, EvidenceHook: evidenceHook,
	}, nil, nil).WithFrozenPlan(plan).ReplayFromTrace(context.Background(), tracePath, "", engine.RunOptions{})
	if err != nil {
		t.Fatalf("ReplayFromTrace: %v", err)
	}
	if _, err := handle.Next(context.Background()); err != nil {
		t.Fatalf("Next: %v", err)
	}
	if extensionHost.loads != 0 || evidenceHook.calls != 0 {
		t.Fatalf("live provider calls = extension:%d evidence:%d", extensionHost.loads, evidenceHook.calls)
	}
}

func TestReplayFromTraceStoredFrozenToolSubstitutionUsesNoMutableSource(t *testing.T) {
	directory := makeReplayDir(t)
	closure, err := plansnapshot.EncodeFlowClosure([]schema.FlowNode{{Step: &schema.Step{
		ID: "saved-child", Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{},
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
	plan := engine.ValidatedForTest(&engine.ExecutionPlan{
		RunID: "replay-stored-frozen", RunbookPath: "runbook.yaml",
		Steps: []engine.ResolvedStep{{ID: "substitute", Kind: "tool", Spec: &schema.ToolCallSpec{Tool: schema.ToolInvocation{
			Name: "saved-tool", Action: "run",
		}}}},
		Tools:    map[string]*schema.ToolDef{"saved-tool": definition},
		Metadata: engine.PlanMetadata{RunbookID: "stored-frozen", PlanHash: "stored-frozen-plan"},
	})
	lock, err := sharedreplay.DependencyLockForPlan(plan)
	if err != nil {
		t.Fatalf("DependencyLockForPlan: %v", err)
	}
	payload, _ := json.Marshal(map[string]any{
		"runbook_path": plan.RunbookPath, "plan_hash": lock.PlanHash, "graph_hash": lock.GraphHash,
		"catalog_digest": lock.CatalogDigest, "package_lock_digest": lock.PackageLockDigest,
		"tool_digest": lock.ToolDigest, "profile_digest": lock.ProfileDigest,
	})
	tracePath := filepath.Join(directory, "stored-frozen.jsonl")
	writer, err := internaltrace.NewJSONLWriter(tracePath)
	if err != nil {
		t.Fatalf("NewJSONLWriter: %v", err)
	}
	appendReplayTraceEvents(t, writer, []tracepkg.TraceEvent{{
		EventID: "start", RunID: "source-stored-frozen", Kind: tracepkg.EventKindRunStarted, Payload: payload,
	}})
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	base := internalexecutor.NewMapRegistry()
	runner := func(ctx context.Context, _ internalexecutor.SubStepParent, nodes []schema.FlowNode, vars map[string]any) ([]*engine.StepResult, error) {
		registry := internalexecutor.ExecutorRegistryFromContext(ctx)
		if registry == nil || len(nodes) != 1 || nodes[0].Step == nil {
			return nil, errors.New("stored frozen substitution has no recursive replay registry")
		}
		child := nodes[0].Step
		result, executeErr := registry.Lookup(string(child.Type)).Execute(ctx, engine.ResolvedStep{
			ID: child.ID, Kind: string(child.Type), Spec: child.NoopSpec,
		}, vars)
		return []*engine.StepResult{result}, executeErr
	}
	base.Register("tool", internalexecutor.NewToolExecutorWithSubstitution(
		nil, &internalexpr.TemplateEvaluator{}, runner, &fakeParser{err: errors.New("mutable parser called")}, nil,
	))
	base.Register("noop", internalexecutor.NewNoopExecutor(nil))
	store := internalrunstore.NewDirRunStore(filepath.Join(directory, "runs"))
	t.Cleanup(func() { _ = store.Close() })
	handle, err := NewReplayEngine(engine.EngineConfig{
		Executors: base, Dispatcher: internaleventbus.NewDispatcher(), Evaluator: &internalexpr.TemplateEvaluator{},
		TraceWriter: &captureTraceWriter{}, Platform: platform.NewFakePlatform(), Store: store,
	}, nil, nil).WithFrozenPlan(plan).ReplayFromTrace(context.Background(), tracePath, "", engine.RunOptions{Store: store})
	if err != nil {
		t.Fatalf("ReplayFromTrace: %v", err)
	}
	result, err := handle.Next(context.Background())
	if err != nil || result == nil || result.Status != engine.StepStatusCompleted {
		t.Fatalf("stored frozen result/error = %#v/%v", result, err)
	}
}

func TestReplayEngineWaitForEventUsesOnlySavedFixtures(t *testing.T) {
	directory := makeReplayDir(t)
	const sourceRunID = "source-wait-run"
	tracePath := writeReplayStartTrace(t, directory, sourceRunID, "wait-plan")
	scenarioPath := writeReplayScenario(t, directory, Scenario{
		SourceRunID:    sourceRunID,
		DependencyLock: testReplayDependencyLock("wait-plan"),
		WaitEvents: []WaitEventBinding{{
			At: exactReplaySelector("wait"), EventID: "ready", Source: "signal",
			Payload:    map[string]any{"value": "saved"},
			Provenance: sharedreplay.Source{Kind: "prior-run", RunID: sourceRunID},
			Review:     reviewedReplayFixture(),
		}},
	})
	plan := engine.ValidatedForTest(&engine.ExecutionPlan{
		RunID: "replay-wait", RunbookPath: "runbook.yaml",
		Steps: []engine.ResolvedStep{{ID: "wait", Kind: "wait_for_event", Spec: &schema.WaitForEventSpec{
			Event: schema.WaitEventConfig{Source: schema.EventSourceSignal, ID: "ready"},
		}}},
		Metadata: engine.PlanMetadata{RunbookID: "wait", PlanHash: "wait-plan"},
	})
	live := &liveReplayDispatcher{}
	replayEngine := NewReplayEngine(engine.EngineConfig{
		Executors: internalexecutor.NewMapRegistry(), Dispatcher: live,
		TraceWriter: &captureTraceWriter{}, Platform: platform.NewFakePlatform(),
	}, nil, nil).WithFrozenPlan(plan)
	handle, err := replayEngine.ReplayFromTrace(context.Background(), tracePath, scenarioPath, engine.RunOptions{})
	if err != nil {
		t.Fatalf("ReplayFromTrace: %v", err)
	}
	result, err := handle.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if result.Output["value"] != "saved" || live.calls != 0 {
		t.Fatalf("wait replay/live calls = %#v/%d", result, live.calls)
	}
}

func TestReplayEngineApprovalUsesOnlySavedFixtures(t *testing.T) {
	directory := makeReplayDir(t)
	const sourceRunID = "source-approval-run"
	tracePath := writeReplayStartTrace(t, directory, sourceRunID, "approval-plan")
	scenarioPath := writeReplayScenario(t, directory, Scenario{
		SourceRunID:    sourceRunID,
		DependencyLock: testReplayDependencyLock("approval-plan"),
		Approvals: []sharedreplay.ApprovalBinding{{
			At: exactReplaySelector("work"), Approved: true, Approver: "saved-operator",
			Source: sharedreplay.Source{Kind: "prior-run", RunID: sourceRunID, InteractionID: "approval-1"},
			Review: reviewedReplayFixture(),
		}},
	})
	registry := internalexecutor.NewMapRegistry()
	registry.Register("noop", internalexecutor.NewNoopExecutor(nil))
	plan := engine.ValidatedForTest(&engine.ExecutionPlan{
		RunID: "replay-approval", RunbookPath: "runbook.yaml",
		Steps:            []engine.ResolvedStep{{ID: "work", Kind: "noop", Spec: &schema.NoopSpec{}}},
		GovernanceSource: &schema.GovernanceConfig{RequireApproval: true},
		Metadata:         engine.PlanMetadata{RunbookID: "approval", PlanHash: "approval-plan"},
	})
	live := &liveReplayApprovalGate{}
	replayEngine := NewReplayEngine(engine.EngineConfig{
		Executors: registry, Dispatcher: internaleventbus.NewDispatcher(), ApprovalGate: live,
		TraceWriter: &captureTraceWriter{}, Platform: platform.NewFakePlatform(),
	}, nil, nil).WithFrozenPlan(plan)
	handle, err := replayEngine.ReplayFromTrace(context.Background(), tracePath, scenarioPath, engine.RunOptions{})
	if err != nil {
		t.Fatalf("ReplayFromTrace: %v", err)
	}
	result, err := handle.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if result.Status != engine.StepStatusCompleted || live.calls != 0 {
		t.Fatalf("approval replay/live calls = %#v/%d", result, live.calls)
	}
}

func TestReplayMissingChoiceFixtureCannotContinueToHandoff(t *testing.T) {
	directory := makeReplayDir(t)
	tracePath := writeReplayStartTrace(t, directory, "source-missing-choice", "missing-choice-plan")
	live := &livePromptProvider{}
	registry := internalexecutor.NewMapRegistry()
	registry.Register("choice", internalexecutor.NewChoiceExecutor(live, nil))
	registry.Register("handoff", internalexecutor.NewHandoffExecutor(nil))
	plan := engine.ValidatedForTest(&engine.ExecutionPlan{
		RunID: "replay-missing-choice", RunbookPath: "runbook.yaml",
		Steps: []engine.ResolvedStep{
			{ID: "choose", Kind: "choice", OnError: "continue", Spec: &schema.ChoiceSpec{
				Prompt: "Choose", Variable: "answer", Options: []schema.ChoiceOption{{Label: "One", Value: "one"}},
			}},
			{ID: "continue", Kind: "handoff", Spec: &schema.HandoffSpec{Handoff: schema.HandoffConfig{
				Runbook: "target.runbook.yaml", Reason: schema.HandoffReason{Code: "continue", Summary: "Continue"},
			}}},
		},
		Metadata: engine.PlanMetadata{RunbookID: "missing-choice", PlanHash: "missing-choice-plan"},
	})
	handle, err := NewReplayEngine(engine.EngineConfig{
		Executors: registry, Dispatcher: internaleventbus.NewDispatcher(),
		TraceWriter: &captureTraceWriter{}, Platform: platform.NewFakePlatform(),
	}, nil, nil).WithFrozenPlan(plan).ReplayFromTrace(
		context.Background(), tracePath, "", engine.RunOptions{},
	)
	if err != nil {
		t.Fatalf("ReplayFromTrace: %v", err)
	}
	result, err := handle.Next(context.Background())
	if !engine.IsReplayBoundaryError(err) || result != nil || live.calls != 0 {
		t.Fatalf("missing choice result/error/live calls = %#v/%v/%d", result, err, live.calls)
	}
	state := handle.State()
	if state.Status != engine.RunStatusFailed || state.PendingHandoff != nil {
		t.Fatalf("missing choice replay state = %#v", state)
	}
	if _, err := handle.Next(context.Background()); err != io.EOF {
		t.Fatalf("replay continued after safety failure: %v", err)
	}
}

func TestReplayFromTraceRejectsCrossRunBoundaryAuthority(t *testing.T) {
	directory := makeReplayDir(t)
	tracePath := filepath.Join(directory, "cross-run-trace.jsonl")
	writer, err := internaltrace.NewJSONLWriter(tracePath)
	if err != nil {
		t.Fatalf("NewJSONLWriter: %v", err)
	}
	startPayload := testReplayStartPayload("runbook.yaml", "cross-run-plan")
	resultPayload, _ := json.Marshal(map[string]any{
		"step_id": "work", "kind": "cli", "qualified_node_id": "work",
		"invocation": 1, "retry_attempt": 1, "status": "completed", "outcome": "success",
		"output": map[string]any{"stdout": "wrong-run", "exit_code": 0},
	})
	appendReplayTraceEvents(t, writer, []tracepkg.TraceEvent{
		{EventID: "start", RunID: "run-a", Kind: tracepkg.EventKindRunStarted, Payload: startPayload},
		{EventID: "result", RunID: "run-b", Kind: tracepkg.EventKindStepCompleted, Payload: resultPayload},
	})
	_ = writer.Close()
	real := &countingExecutor{}
	registry := internalexecutor.NewMapRegistry()
	registry.Register("cli", real)
	plan := engine.ValidatedForTest(&engine.ExecutionPlan{
		RunID: "replay-cross-run", RunbookPath: "runbook.yaml",
		Steps:    []engine.ResolvedStep{{ID: "work", Kind: "cli", Spec: &schema.CLISpec{Command: "work"}}},
		Metadata: engine.PlanMetadata{RunbookID: "cross-run", PlanHash: "cross-run-plan"},
	})
	handle, err := NewReplayEngine(engine.EngineConfig{
		Executors: registry, Dispatcher: internaleventbus.NewDispatcher(),
		TraceWriter: &captureTraceWriter{}, Platform: platform.NewFakePlatform(),
	}, nil, nil).WithFrozenPlan(plan).ReplayFromTrace(
		context.Background(), tracePath, "", engine.RunOptions{},
	)
	if err != nil {
		t.Fatalf("ReplayFromTrace: %v", err)
	}
	result, err := handle.Next(context.Background())
	if err == nil || real.calls != 0 || result != nil && result.Output["stdout"] == "wrong-run" {
		t.Fatalf("cross-run replay result/live calls = %#v/%d", result, real.calls)
	}
}

func TestReplayFromTraceRejectsCrossRunRunbookPathAuthority(t *testing.T) {
	directory := makeReplayDir(t)
	tracePath := filepath.Join(directory, "cross-run-start.jsonl")
	writer, err := internaltrace.NewJSONLWriter(tracePath)
	if err != nil {
		t.Fatalf("NewJSONLWriter: %v", err)
	}
	emptyPayload, _ := json.Marshal(map[string]any{})
	pathPayload, _ := json.Marshal(map[string]any{"runbook_path": "b.runbook.yaml"})
	resultPayload, _ := json.Marshal(map[string]any{
		"step_id": "work", "kind": "cli", "qualified_node_id": "work",
		"invocation": 1, "retry_attempt": 1, "status": "completed", "outcome": "success",
		"output": map[string]any{"stdout": "cross-run", "exit_code": 0},
	})
	appendReplayTraceEvents(t, writer, []tracepkg.TraceEvent{
		{EventID: "start-a", RunID: "run-a", Kind: tracepkg.EventKindRunStarted, Payload: emptyPayload},
		{EventID: "start-b", RunID: "run-b", Kind: tracepkg.EventKindRunStarted, Payload: pathPayload},
		{EventID: "result-a", RunID: "run-a", Kind: tracepkg.EventKindStepCompleted, Payload: resultPayload},
	})
	_ = writer.Close()
	plan := &engine.ExecutionPlan{
		RunID: "replay", RunbookPath: "b.runbook.yaml",
		Steps:    []engine.ResolvedStep{{ID: "work", Kind: "cli", Spec: &schema.CLISpec{Command: "work"}}},
		Metadata: engine.PlanMetadata{RunbookID: "cross-run-start"},
	}
	registry := internalexecutor.NewMapRegistry()
	registry.Register("cli", &countingExecutor{})
	_, err = NewReplayEngine(engine.EngineConfig{
		Executors: registry, Dispatcher: internaleventbus.NewDispatcher(),
		TraceWriter: &captureTraceWriter{}, Platform: platform.NewFakePlatform(),
	}, &fakeParser{result: &parser.ParsedRunbook{Source: "test"}}, &fakePlanner{plan: plan}).ReplayFromTrace(
		context.Background(), tracePath, "", engine.RunOptions{},
	)
	if err == nil {
		t.Fatal("ReplayFromTrace combined run ID and runbook path from different starts")
	}
}

func TestReplayFromTraceRejectsHandoffTargetDrift(t *testing.T) {
	directory := makeReplayDir(t)
	tracePath := filepath.Join(directory, "handoff-drift.jsonl")
	writer, err := internaltrace.NewJSONLWriter(tracePath)
	if err != nil {
		t.Fatalf("NewJSONLWriter: %v", err)
	}
	startPayload := testReplayStartPayload("runbook.yaml", "handoff-plan")
	handoffPayload, _ := json.Marshal(map[string]any{
		"step_id": "route", "qualified_node_id": "route", "invocation": 1, "retry_attempt": 1,
		"target_runbook": "safe.runbook.yaml", "reason_code": "continue", "reason_summary": "Continue",
	})
	appendReplayTraceEvents(t, writer, []tracepkg.TraceEvent{
		{EventID: "start", RunID: "source-handoff", Kind: tracepkg.EventKindRunStarted, Payload: startPayload},
		{EventID: "handoff", RunID: "source-handoff", Kind: tracepkg.EventKind("handoff/requested"), Payload: handoffPayload},
	})
	_ = writer.Close()
	registry := internalexecutor.NewMapRegistry()
	registry.Register("handoff", internalexecutor.NewHandoffExecutor(nil))
	plan := engine.ValidatedForTest(&engine.ExecutionPlan{
		RunID: "replay-handoff", RunbookPath: "runbook.yaml",
		Steps: []engine.ResolvedStep{{ID: "route", Kind: "handoff", Spec: &schema.HandoffSpec{Handoff: schema.HandoffConfig{
			Runbook: "other.runbook.yaml", Reason: schema.HandoffReason{Code: "continue", Summary: "Continue"},
		}}}},
		Metadata: engine.PlanMetadata{RunbookID: "handoff", PlanHash: "handoff-plan"},
	})
	handle, err := NewReplayEngine(engine.EngineConfig{
		Executors: registry, Dispatcher: internaleventbus.NewDispatcher(),
		TraceWriter: &captureTraceWriter{}, Platform: platform.NewFakePlatform(),
	}, nil, nil).WithFrozenPlan(plan).ReplayFromTrace(
		context.Background(), tracePath, "", engine.RunOptions{},
	)
	if err != nil {
		t.Fatalf("ReplayFromTrace: %v", err)
	}
	result, err := handle.Next(context.Background())
	if result != nil || !engine.IsReplayBoundaryError(err) {
		t.Fatalf("drifted handoff result/error = %#v/%v", result, err)
	}
	state := handle.State()
	if state.Status != engine.RunStatusFailed || state.PendingHandoff != nil {
		t.Fatalf("drifted handoff state = %#v, want failed without pending handoff", state)
	}
}

func TestReplayFromTraceRejectsHandoffStructuralPathDriftBeforeCommit(t *testing.T) {
	directory := makeReplayDir(t)
	const sourceRunID = "source-structural-handoff"
	tracePath := writeReplayStartTrace(t, directory, sourceRunID, "structural-handoff-plan")
	scenarioPath := writeReplayScenario(t, directory, Scenario{
		SourceRunID: sourceRunID, DependencyLock: testReplayDependencyLock("structural-handoff-plan"),
		ExpectedTransitions: []sharedreplay.ExpectedTransition{{
			At: sharedreplay.Selector{
				QualifiedNodeID: "route", Step: "route", Phase: "execute", Invocation: 1, Attempt: 1,
				StructuralPath: []schema.DynamicIncludeFrameIdentity{{
					QualifiedNodeID: "outer", Kind: "include", Invocation: 1,
				}},
			},
			TargetRunbook: "safe.runbook.yaml", ReasonCode: "continue",
		}},
	})
	registry := internalexecutor.NewMapRegistry()
	registry.Register("handoff", internalexecutor.NewHandoffExecutor(nil))
	plan := engine.ValidatedForTest(&engine.ExecutionPlan{
		RunID: "replay-structural-handoff", RunbookPath: "runbook.yaml",
		Steps: []engine.ResolvedStep{{ID: "route", Kind: "handoff", Spec: &schema.HandoffSpec{Handoff: schema.HandoffConfig{
			Runbook: "safe.runbook.yaml", Reason: schema.HandoffReason{Code: "continue", Summary: "Continue"},
		}}}},
		Metadata: engine.PlanMetadata{RunbookID: "handoff", PlanHash: "structural-handoff-plan"},
	})
	handle, err := NewReplayEngine(engine.EngineConfig{
		Executors: registry, Dispatcher: internaleventbus.NewDispatcher(),
		TraceWriter: &captureTraceWriter{}, Platform: platform.NewFakePlatform(),
	}, nil, nil).WithFrozenPlan(plan).ReplayFromTrace(context.Background(), tracePath, scenarioPath, engine.RunOptions{})
	if err != nil {
		t.Fatalf("ReplayFromTrace: %v", err)
	}
	result, err := handle.Next(context.Background())
	if result != nil || !engine.IsReplayBoundaryError(err) {
		t.Fatalf("structural drift result/error = %#v/%v", result, err)
	}
	state := handle.State()
	if state.Status != engine.RunStatusFailed || state.PendingHandoff != nil {
		t.Fatalf("structural drift state = %#v", state)
	}
}

func TestReplayFromTraceFailsBeforeCompletionWhenExpectedHandoffIsMissing(t *testing.T) {
	directory := makeReplayDir(t)
	const sourceRunID = "source-missing-handoff"
	tracePath := writeReplayStartTrace(t, directory, sourceRunID, "missing-handoff-plan")
	scenarioPath := writeReplayScenario(t, directory, Scenario{
		SourceRunID: sourceRunID, DependencyLock: testReplayDependencyLock("missing-handoff-plan"),
		ExpectedTransitions: []sharedreplay.ExpectedTransition{{
			At: exactReplaySelector("route"), TargetRunbook: "safe.runbook.yaml", ReasonCode: "continue",
		}},
	})
	registry := internalexecutor.NewMapRegistry()
	registry.Register("noop", internalexecutor.NewNoopExecutor(nil))
	plan := engine.ValidatedForTest(&engine.ExecutionPlan{
		RunID: "replay-missing-handoff", RunbookPath: "runbook.yaml",
		Steps:    []engine.ResolvedStep{{ID: "work", Kind: "noop", Spec: &schema.NoopSpec{}}},
		Metadata: engine.PlanMetadata{RunbookID: "missing-handoff", PlanHash: "missing-handoff-plan"},
	})
	handle, err := NewReplayEngine(engine.EngineConfig{
		Executors: registry, Dispatcher: internaleventbus.NewDispatcher(),
		TraceWriter: &captureTraceWriter{}, Platform: platform.NewFakePlatform(),
	}, nil, nil).WithFrozenPlan(plan).ReplayFromTrace(context.Background(), tracePath, scenarioPath, engine.RunOptions{})
	if err != nil {
		t.Fatalf("ReplayFromTrace: %v", err)
	}
	if _, err := handle.Next(context.Background()); err != nil {
		t.Fatalf("work Next: %v", err)
	}
	result, err := handle.Next(context.Background())
	if result != nil || !engine.IsReplayBoundaryError(err) {
		t.Fatalf("completion result/error = %#v/%v", result, err)
	}
	if state := handle.State(); state.Status != engine.RunStatusFailed {
		t.Fatalf("completion state = %s, want %s", state.Status, engine.RunStatusFailed)
	}
}

func TestReplayRepeatedBoundariesUseExactOccurrences(t *testing.T) {
	const sourceRunID = "source-repeat"
	selector := func(qualified string, path []string, structural []schema.DynamicIncludeFrameIdentity, invocation int) sharedreplay.Selector {
		return sharedreplay.Selector{
			QualifiedNodeID: qualified, CallPath: path, StructuralPath: structural,
			Step: "work", Phase: "execute", Invocation: invocation, Attempt: 1,
		}
	}
	leftPath := []schema.DynamicIncludeFrameIdentity{{
		QualifiedNodeID: "parallel", Kind: "parallel", BranchLabel: "left", Invocation: 1,
	}}
	rightPath := []schema.DynamicIncludeFrameIdentity{{
		QualifiedNodeID: "parallel", Kind: "parallel", BranchLabel: "right", Invocation: 1,
	}}
	scenario := &Scenario{SourceRunID: sourceRunID, StepResponses: []sharedreplay.StepBinding{
		{At: selector("work", nil, nil, 1), Kind: "cli", Status: "completed", Output: map[string]any{"stdout": "first"}, Source: sharedreplay.Source{RunID: sourceRunID}},
		{At: selector("work", nil, nil, 2), Kind: "cli", Status: "completed", Output: map[string]any{"stdout": "second"}, Source: sharedreplay.Source{RunID: sourceRunID}},
		{At: selector("parallel/work", []string{"parallel"}, leftPath, 1), Kind: "cli", Status: "completed", Output: map[string]any{"stdout": "left"}, Source: sharedreplay.Source{RunID: sourceRunID}},
		{At: selector("parallel/work", []string{"parallel"}, rightPath, 1), Kind: "cli", Status: "completed", Output: map[string]any{"stdout": "right"}, Source: sharedreplay.Source{RunID: sourceRunID}},
	}}
	base := internalexecutor.NewMapRegistry()
	base.Register("cli", &countingExecutor{})
	replay := NewReplayExecutorRegistryWithPins(base, scenario, nil, nil, nil, nil, nil, "replay")
	step := engine.ResolvedStep{ID: "work", Kind: "cli", Spec: &schema.CLISpec{Command: "same"}}
	for index, want := range []string{"first", "second"} {
		result, err := replay.Lookup("cli").Execute(context.Background(), step, nil)
		if err != nil || result.Output["stdout"] != want {
			t.Fatalf("serial result %d = %#v, %v", index, result, err)
		}
	}
	for _, test := range []struct {
		path []schema.DynamicIncludeFrameIdentity
		want string
	}{{leftPath, "left"}, {rightPath, "right"}} {
		ctx := engine.WithDispatchExecutionBoundary(context.Background(), engine.DispatchExecutionBoundary{
			QualifiedNodeID: "parallel/work", CallPath: []engine.DebugCallFrame{{StepID: "parallel"}},
			StepID: "work", Invocation: 1, RetryAttempt: 1,
		})
		ctx = engine.WithDynamicIncludeStructuralPath(ctx, test.path)
		result, err := replay.Lookup("cli").Execute(ctx, step, nil)
		if err != nil || result.Output["stdout"] != test.want {
			t.Fatalf("parallel result %s = %#v, %v", test.want, result, err)
		}
	}
}

func TestReplayFromTraceFrozenPlanDoesNotConsultMutableSource(t *testing.T) {
	directory := makeReplayDir(t)
	tracePath := writeReplayStartTrace(t, directory, "source-frozen", "frozen-plan")
	registry := internalexecutor.NewMapRegistry()
	registry.Register("noop", internalexecutor.NewNoopExecutor(nil))
	plan := engine.ValidatedForTest(&engine.ExecutionPlan{
		RunID: "replay-frozen", RunbookPath: "runbook.yaml",
		Steps:    []engine.ResolvedStep{{ID: "pure", Kind: "noop", Spec: &schema.NoopSpec{}}},
		Metadata: engine.PlanMetadata{RunbookID: "frozen", PlanHash: "frozen-plan"},
	})
	replayEngine := NewReplayEngine(engine.EngineConfig{
		Executors: registry, Dispatcher: internaleventbus.NewDispatcher(),
		TraceWriter: &captureTraceWriter{}, Platform: platform.NewFakePlatform(),
	}, &fakeParser{err: io.ErrUnexpectedEOF}, &fakePlanner{err: io.ErrUnexpectedEOF}).WithFrozenPlan(plan)
	handle, err := replayEngine.ReplayFromTrace(context.Background(), tracePath, "", engine.RunOptions{})
	if err != nil {
		t.Fatalf("ReplayFromTrace consulted mutable source: %v", err)
	}
	result, err := handle.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if result == nil || result.Status != engine.StepStatusCompleted {
		t.Fatalf("frozen replay result = %#v", result)
	}
}

func TestReplayExactBoundaryTraceRoundTrip(t *testing.T) {
	directory := makeReplayDir(t)
	tracePath := filepath.Join(directory, "exact-roundtrip.jsonl")
	writer, err := internaltrace.NewJSONLWriter(tracePath)
	if err != nil {
		t.Fatalf("NewJSONLWriter: %v", err)
	}
	plan := &engine.ExecutionPlan{
		RunID: "source-exact-roundtrip", RunbookPath: "runbook.yaml",
		Steps:    []engine.ResolvedStep{{ID: "work", Kind: "cli", Spec: &schema.CLISpec{Command: "saved"}}},
		Metadata: engine.PlanMetadata{RunbookID: "exact-roundtrip", PlanHash: "exact-roundtrip-plan"},
	}
	realRegistry := internalexecutor.NewMapRegistry()
	realRegistry.Register("cli", replayStepExecutorFunc(func(_ context.Context, step engine.ResolvedStep, _ map[string]any) (*engine.StepResult, error) {
		return &engine.StepResult{
			StepID: step.ID, Status: engine.StepStatusCompleted, Outcome: engine.StepOutcomeSuccess,
			Output: map[string]any{"stdout": "saved-output", "stderr": "", "exit_code": 0}, Vars: map[string]any{},
		}, nil
	}))
	realHandle, err := internalengine.New(engine.EngineConfig{
		Executors: realRegistry, Dispatcher: internaleventbus.NewDispatcher(), TraceWriter: writer,
		Platform: platform.NewFakePlatform(),
	}).Start(context.Background(), engine.ValidatedForTest(plan), engine.RunOptions{})
	if err != nil {
		t.Fatalf("Start source: %v", err)
	}
	if _, err := realHandle.Next(context.Background()); err != nil {
		t.Fatalf("source Next: %v", err)
	}
	if _, err := realHandle.Next(context.Background()); err != io.EOF {
		t.Fatalf("source completion: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close source trace: %v", err)
	}
	live := &countingExecutor{}
	replayRegistry := internalexecutor.NewMapRegistry()
	replayRegistry.Register("cli", live)
	handle, err := NewReplayEngine(engine.EngineConfig{
		Executors: replayRegistry, Dispatcher: internaleventbus.NewDispatcher(),
		TraceWriter: &captureTraceWriter{}, Platform: platform.NewFakePlatform(),
	}, nil, nil).WithFrozenPlan(plan).ReplayFromTrace(
		context.Background(), tracePath, "", engine.RunOptions{},
	)
	if err != nil {
		t.Fatalf("ReplayFromTrace: %v", err)
	}
	result, err := handle.Next(context.Background())
	if err != nil {
		t.Fatalf("replay Next: %v", err)
	}
	if result.Output["stdout"] != "saved-output" || live.calls != 0 {
		t.Fatalf("replayed exact result/live calls = %#v/%d", result, live.calls)
	}
}

func TestReplay_EndToEnd(t *testing.T) {
	dir := makeReplayDir(t)
	tracePath := filepath.Join(dir, "trace.jsonl")
	writeTrace(t, tracePath, "runbook.yaml")

	plan := engine.ValidatedForTest(makePlan())
	parserImpl := &fakeParser{result: &parser.ParsedRunbook{Source: "test"}}
	plannerImpl := &fakePlanner{plan: plan}

	counter := &countingExecutor{}
	registry := internalexecutor.NewMapRegistry()
	registry.Register("cli", counter)

	writer := &captureTraceWriter{}
	cfg := engine.EngineConfig{
		Executors:   registry,
		Dispatcher:  internaleventbus.NewDispatcher(),
		TraceWriter: writer,
		Platform:    platform.NewFakePlatform(),
	}

	replayEngine := NewReplayEngine(cfg, parserImpl, plannerImpl).WithFrozenPlan(plan)
	handle, err := replayEngine.ReplayFromTrace(context.Background(), tracePath, "", engine.RunOptions{})
	if err != nil {
		t.Fatalf("ReplayFromTrace: %v", err)
	}

	var results []*engine.StepResult
	for {
		res, err := handle.Next(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		results = append(results, res)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].Output["stdout"] != "hi\n" {
		t.Fatalf("unexpected stdout: %#v", results[0].Output)
	}
	if counter.calls != 0 {
		t.Fatalf("expected real executor not to be called")
	}
	if !hasRunReplayed(writer.events) {
		t.Fatalf("expected run/replayed event")
	}
}

func TestReplay_ScenarioOverride(t *testing.T) {
	dir := makeReplayDir(t)
	tracePath := filepath.Join(dir, "trace.jsonl")
	writeTrace(t, tracePath, "runbook.yaml")
	scenarioPath := filepath.Join(dir, "scenario.yaml")
	if err := os.WriteFile(scenarioPath, []byte(overrideScenarioYAML()), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	plan := makePlan()
	parserImpl := &fakeParser{result: &parser.ParsedRunbook{Source: "test"}}
	plannerImpl := &fakePlanner{plan: plan}

	registry := internalexecutor.NewMapRegistry()
	registry.Register("cli", &countingExecutor{})

	cfg := engine.EngineConfig{
		Executors:   registry,
		Dispatcher:  internaleventbus.NewDispatcher(),
		TraceWriter: &captureTraceWriter{},
		Platform:    platform.NewFakePlatform(),
	}

	replayEngine := NewReplayEngine(cfg, parserImpl, plannerImpl).WithPatternFixtureReplay()
	handle, err := replayEngine.ReplayFromTrace(context.Background(), tracePath, scenarioPath, engine.RunOptions{})
	if err != nil {
		t.Fatalf("ReplayFromTrace: %v", err)
	}
	res, err := handle.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if res.Output["stdout"] != "override\n" {
		t.Fatalf("expected override stdout, got %#v", res.Output)
	}
}

func writeTrace(t *testing.T, path string, runbookPath string) {
	t.Helper()
	writer, err := internaltrace.NewJSONLWriter(path)
	if err != nil {
		t.Fatalf("NewJSONLWriter: %v", err)
	}
	startPayload := testReplayStartPayload(runbookPath, "replay-test-plan")
	stepPayload, _ := json.Marshal(map[string]any{
		"step_id": "step-1", "kind": "cli", "qualified_node_id": "step-1",
		"invocation": 1, "retry_attempt": 1, "status": "completed", "outcome": "success",
		"output": map[string]any{
			"stdout":    "hi\n",
			"stderr":    "",
			"exit_code": 0,
		},
		"evidence": []evidencepkg.EvidenceRecord{{
			Name:       "stdout",
			Kind:       evidencepkg.EvidenceKindText,
			Value:      "hi\n",
			CapturedAt: time.Now(),
		}},
	})
	stepPayload2, _ := json.Marshal(map[string]any{
		"step_id": "step-2", "kind": "cli", "qualified_node_id": "step-2",
		"invocation": 1, "retry_attempt": 1, "status": "completed", "outcome": "success",
		"output": map[string]any{
			"stdout":    "bye\n",
			"stderr":    "",
			"exit_code": 0,
		},
	})
	if err := writer.Append(tracepkg.TraceEvent{
		EventID:   "evt-1",
		RunID:     "run-1",
		RunbookID: "rb-1",
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Kind:      tracepkg.EventKindRunStarted,
		Sequence:  1,
		Payload:   startPayload,
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := writer.Append(tracepkg.TraceEvent{
		EventID:   "evt-2",
		RunID:     "run-1",
		RunbookID: "rb-1",
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Kind:      tracepkg.EventKindStepCompleted,
		Sequence:  2,
		Payload:   stepPayload,
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := writer.Append(tracepkg.TraceEvent{
		EventID:   "evt-3",
		RunID:     "run-1",
		RunbookID: "rb-1",
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Kind:      tracepkg.EventKindStepCompleted,
		Sequence:  3,
		Payload:   stepPayload2,
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func makePlan() *engine.ExecutionPlan {
	return &engine.ExecutionPlan{
		RunID:       "run-1",
		RunbookPath: "runbook.yaml",
		Steps: []engine.ResolvedStep{
			{ID: "step-1", Kind: "cli", Spec: &schema.CLISpec{Command: "echo", Args: []string{"hi"}}},
			{ID: "step-2", Kind: "cli", Spec: &schema.CLISpec{Command: "echo", Args: []string{"bye"}}},
		},
		Metadata: engine.PlanMetadata{
			RunbookID:   "rb-1",
			RunbookName: "Runbook",
			PlanHash:    "replay-test-plan",
			PlannedAt:   time.Now(),
		},
	}
}

func overrideScenarioYAML() string {
	return `commands:
  - argv: ["echo", "hi"]
    stdout: "override\n"
    stderr: ""
    exit_code: 0
  - argv: ["echo", "bye"]
    stdout: "bye\n"
    stderr: ""
    exit_code: 0
allow_unmatched: false
`
}

type fakeParser struct {
	result *parser.ParsedRunbook
	err    error
}

func (f *fakeParser) Parse(_ context.Context, _ string) (*parser.ParsedRunbook, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

func (f *fakeParser) ParseBytes(_ context.Context, _ []byte) (*parser.ParsedRunbook, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

type fakePlanner struct {
	plan *engine.ExecutionPlan
	err  error
}

func (f *fakePlanner) Plan(_ context.Context, _ *parser.ParsedRunbook) (*engine.ExecutionPlan, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.plan, nil
}

type captureTraceWriter struct {
	events []tracepkg.TraceEvent
}

func (w *captureTraceWriter) Append(event tracepkg.TraceEvent) error {
	w.events = append(w.events, event)
	return nil
}

func (w *captureTraceWriter) Close() error {
	return nil
}

type countingExecutor struct {
	calls int
}

func (c *countingExecutor) Execute(_ context.Context, _ engine.ResolvedStep, _ map[string]any) (*engine.StepResult, error) {
	c.calls++
	return &engine.StepResult{}, nil
}

func hasRunReplayed(events []tracepkg.TraceEvent) bool {
	for _, ev := range events {
		if ev.Kind == tracepkg.EventKind("run/replayed") {
			return true
		}
	}
	return false
}

func makeReplayDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(".", "replay-engine-test-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
