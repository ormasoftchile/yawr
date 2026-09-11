package adapter

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	internalengine "github.com/ormasoftchile/yawr/runtime/internal/engine"
	internalexecutor "github.com/ormasoftchile/yawr/runtime/internal/executor"
	internalparser "github.com/ormasoftchile/yawr/runtime/internal/parser"
	internalplanner "github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	internalrunstore "github.com/ormasoftchile/yawr/runtime/internal/runstore"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/pkgcatalog"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

type countingDynamicResolver struct {
	inner internalexecutor.DynamicIncludeResolver
	calls int
}

func (resolver *countingDynamicResolver) Resolve(ctx context.Context, renderedRef string) (*internalexecutor.DynamicIncludeResult, error) {
	resolver.calls++
	return resolver.inner.Resolve(ctx, renderedRef)
}

// buildTestCatalogWithParser creates a workspace with one package that exports
// one runbook and returns the frozen catalog and the child runbook's absolute
// path. Unlike buildTestCatalog the runbook file is always left on disk.
func buildWiringTestCatalog(t *testing.T) (*pkgcatalog.Catalog, string) {
	t.Helper()
	ws := t.TempDir()
	pkgRoot := filepath.Join(ws, "acme-pkg")
	rbDir := filepath.Join(pkgRoot, "runbooks")
	if err := os.MkdirAll(rbDir, 0o755); err != nil {
		t.Fatal(err)
	}

	manifest := `apiVersion: yawr.tool-package/v1
meta:
  name: acme-pkg
  version: "1.0.0"
exports:
  runbooks:
    - id: tsg-network
      path: runbooks/tsg-network.runbook.yaml
`
	childRunbook := `apiVersion: yawr.runbook/v1
id: tsg-network
name: TSG Network
flow: []
`
	manifestPath := filepath.Join(pkgRoot, "yawr-package.yaml")
	childPath := filepath.Join(rbDir, "tsg-network.runbook.yaml")
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(childPath, []byte(childRunbook), 0o644); err != nil {
		t.Fatal(err)
	}

	catalog, errs := pkgcatalog.Build(pkgcatalog.BuildOptions{
		WorkspaceRoot: ws,
		ProjectRequires: []*schema.PackageRequirement{
			{Package: "acme-pkg", Version: "1.0.0", Path: "acme-pkg"},
		},
	})
	if len(errs) > 0 {
		t.Fatalf("catalog build errors: %v", errs)
	}
	return catalog, childPath
}

// noopSubStepRunner returns an empty result slice for any child flow.
func noopSubStepRunner(_ context.Context, _ internalexecutor.SubStepParent, _ []schema.FlowNode, _ map[string]any) ([]*engine.StepResult, error) {
	return nil, nil
}

// TestDynamicWiring_EndToEnd verifies that NewCatalogIncludeResolver and PinRecorder
// are correctly threaded through NewDefaultRegistry (the same code path that
// BuildEngineConfig uses) and that a dynamic include step resolves successfully
// and populates the DynamicIncludes pin list.
func TestDynamicWiring_EndToEnd(t *testing.T) {
	catalog, _ := buildWiringTestCatalog(t)

	plat := platform.Real()
	parser, err := internalparser.New(plat)
	if err != nil {
		t.Fatalf("internalparser.New: %v", err)
	}

	resolver := NewCatalogIncludeResolver(catalog, parser)

	var pins []internalexecutor.DynamicIncludePin
	pinRecorder := internalexecutor.PinRecorder(func(p internalexecutor.DynamicIncludePin) {
		pins = append(pins, p)
	})

	// Build the registry the same way BuildEngineConfig does.
	reg := internalexecutor.NewDefaultRegistry(internalexecutor.RegistryConfig{
		SubStepRunner:          noopSubStepRunner,
		DynamicIncludeResolver: resolver,
		PinRecorder:            pinRecorder,
	})

	exec := reg.Lookup("include")
	if exec == nil {
		t.Fatal("include executor not registered")
	}

	step := engine.ResolvedStep{
		ID:   "s1",
		Kind: "include",
		Spec: &schema.IncludeSpec{
			Include: schema.IncludeConfig{
				RunbookRef:  "acme-pkg/tsg-network",
				ResolveFrom: schema.ResolveFromCatalog,
			},
		},
	}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if res.Status != engine.StepStatusCompleted {
		t.Fatalf("expected completed, got %v", res.Status)
	}

	if len(pins) != 1 {
		t.Fatalf("expected 1 pin recorded, got %d", len(pins))
	}
	pin := pins[0]
	if pin.StepID != "s1" {
		t.Errorf("pin.StepID = %q, want %q", pin.StepID, "s1")
	}
	if pin.QualifiedID != "acme-pkg/tsg-network" {
		t.Errorf("pin.QualifiedID = %q, want %q", pin.QualifiedID, "acme-pkg/tsg-network")
	}
	if pin.PackageName != "acme-pkg" {
		t.Errorf("pin.PackageName = %q, want %q", pin.PackageName, "acme-pkg")
	}
	if pin.PackageVersion != "1.0.0" {
		t.Errorf("pin.PackageVersion = %q, want %q", pin.PackageVersion, "1.0.0")
	}
	if pin.FileDigest == "" {
		t.Error("pin.FileDigest must not be empty")
	}
	if pin.PackageDigest == "" {
		t.Error("pin.PackageDigest must not be empty")
	}
}

func TestDynamicWiring_ResumeUsesDurableResolutionWithoutCatalogLookup(t *testing.T) {
	catalog, childPath := buildWiringTestCatalog(t)
	parserImpl, err := internalparser.New(platform.Real())
	if err != nil {
		t.Fatalf("parser: %v", err)
	}
	baseResolver := NewCatalogIncludeResolver(catalog, parserImpl)
	resolved, err := baseResolver.Resolve(context.Background(), "acme-pkg/tsg-network")
	if err != nil {
		t.Fatalf("Resolve initial pin: %v", err)
	}
	closure, err := plansnapshot.EncodeFlowClosure(resolved.Flow)
	if err != nil {
		t.Fatalf("EncodeFlowClosure: %v", err)
	}
	const runID = "run-durable-dynamic-resolution"
	plan := &engine.ExecutionPlan{
		RunID: runID, RunbookPath: "dynamic.runbook.yaml",
		Steps: []engine.ResolvedStep{{
			ID: "dynamic", Kind: "include", Spec: &schema.IncludeSpec{Include: schema.IncludeConfig{
				RunbookRef: "acme-pkg/tsg-network", ResolveFrom: schema.ResolveFromCatalog,
			}},
		}},
		Metadata: engine.PlanMetadata{RunbookID: "dynamic-resume"},
	}
	if err := internalplanner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	runStoreBase := t.TempDir()
	store := internalrunstore.NewDirRunStore(runStoreBase)
	t.Cleanup(func() { _ = store.Close() })
	firstLease, err := store.AcquireRunLease(context.Background(), runID)
	if err != nil {
		t.Fatalf("AcquireRunLease first: %v", err)
	}
	t.Cleanup(func() { _ = firstLease.Release() })
	if err := store.SavePlan(context.Background(), runID, plan); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	resolutionID := engine.InteractionPayloadDigest([]byte("durable dynamic resolution"))
	state := engine.RunState{
		RunID: runID, RunbookPath: plan.RunbookPath, WriterEpoch: firstLease.Epoch(),
		Status: engine.RunStatusRunning, CheckpointSequence: 1, CurrentStep: "dynamic", CurrentStepIndex: 0,
		CursorSet: &engine.ExecutionCursorSet{
			SchemaVersion: engine.ExecutionCursorSchemaV1,
			Cursors: []engine.ExecutionCursor{{
				QualifiedNodeID: "dynamic", StepID: "dynamic", StepIndex: 0,
				Phase: engine.ExecutionPhaseBefore, Invocation: 1, RetryAttempt: 1,
			}},
		},
		DynamicIncludes: map[string]*engine.DynamicIncludeResolutionState{
			resolutionID: {
				SchemaVersion: engine.DynamicIncludeResolutionStateSchemaV1, ResolutionID: resolutionID,
				WriterEpoch: firstLease.Epoch(), QualifiedNodeID: "dynamic", StepID: "dynamic",
				Invocation: 1, Revision: 1,
				Pin: schema.LockedDynamicInclude{
					StepID: "dynamic", QualifiedNodeID: "dynamic", Invocation: 1, Revision: 1,
					RenderedRef: "acme-pkg/tsg-network", QualifiedID: resolved.QualifiedID,
					RunbookID: resolved.RunbookID, RunbookName: resolved.RunbookName,
					RunbookContentHash: resolved.ContentHash,
					AbsPath:            childPath, PackageName: resolved.PackageName, PackageVersion: resolved.PackageVersion,
					FileDigest: resolved.FileDigest, PackageDigest: resolved.PackageDigest,
					ExecutableClosure: closure, ResolvedInputs: resolved.ChildInputs,
					ResolvedOutputs: resolved.ChildOutputs, ResolvedGovernance: resolved.ChildGovernance,
				},
				Status:      engine.DynamicIncludeResolutionStatusActive,
				CommittedAt: time.Now().UTC().Format(time.RFC3339Nano),
			},
		},
		StartedAt: time.Now().UTC(),
	}
	if err := plansnapshot.ValidateDynamicIncludePin(state.DynamicIncludes[resolutionID].Pin); err != nil {
		t.Fatalf("ValidateDynamicIncludePin: %v", err)
	}
	encodedPin, err := json.Marshal(state.DynamicIncludes[resolutionID].Pin)
	if err != nil {
		t.Fatalf("marshal durable pin: %v", err)
	}
	dispatchID := engine.InteractionPayloadDigest([]byte("durable resolver dispatch"))
	state.Dispatches = map[string]*engine.DispatchState{
		dispatchID: {
			SchemaVersion: engine.DispatchStateSchemaV1, OccurrenceID: dispatchID,
			WriterEpoch: firstLease.Epoch(), QualifiedNodeID: "dynamic", StepID: "dynamic",
			Phase: engine.ExecutionPhaseExecute, Invocation: 1, RetryAttempt: 1, OccurrenceSequence: 1,
			Classification: "read-only", EndpointIdentity: "dynamic-include-resolver",
			RequestDigest:  engine.InteractionPayloadDigest([]byte("request")),
			IdempotencyKey: engine.InteractionPayloadDigest([]byte("idempotency")),
			Status:         engine.DispatchStatusSettled, ResultDigest: engine.InteractionPayloadDigest(encodedPin),
			PreparedAt: time.Now().UTC().Format(time.RFC3339Nano),
			SettledAt:  time.Now().UTC().Format(time.RFC3339Nano),
		},
	}
	if err := store.SaveState(context.Background(), state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	traceCtx := engine.WithRunWriterEpoch(context.Background(), firstLease.Epoch())
	if err := store.WriteTrace(traceCtx, runID, engine.Event{
		EventID: "event-1", RunID: runID, RunbookID: "dynamic-resume", Kind: "run/started",
		Sequence: 1, Timestamp: time.Now().UTC().Format(time.RFC3339Nano), Payload: map[string]any{},
	}); err != nil {
		t.Fatalf("WriteTrace: %v", err)
	}
	if err := firstLease.Release(); err != nil {
		t.Fatalf("Release first lease: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close first process: %v", err)
	}
	if err := os.Remove(childPath); err != nil {
		t.Fatalf("remove pinned source before resume: %v", err)
	}

	resumeStore := internalrunstore.NewDirRunStore(runStoreBase)
	t.Cleanup(func() { _ = resumeStore.Close() })
	resolver := &countingDynamicResolver{inner: baseResolver}
	registry := internalexecutor.NewDefaultRegistry(internalexecutor.RegistryConfig{
		SubStepRunner: noopSubStepRunner, DynamicIncludeResolver: resolver,
		LazyRunbookLoader: NewParserLazyLoader(parserImpl),
	})
	runtime := internalengine.New(engine.EngineConfig{
		Executors: registry, Dispatcher: &noopDispatcher{}, TraceWriter: &noopTraceWriter{},
		Platform: platform.Real(), Store: resumeStore,
	})
	handle, err := runtime.Resume(context.Background(), runID, engine.RunOptions{Store: resumeStore})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	t.Cleanup(func() { _ = handle.Cancel(context.Background(), "test cleanup") })
	result, err := handle.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if result.Status != engine.StepStatusCompleted || resolver.calls != 0 {
		t.Fatalf("result=%#v resolver calls=%d", result, resolver.calls)
	}
	resumed := handle.State()
	if resumed.DynamicIncludes[resolutionID].Status != engine.DynamicIncludeResolutionStatusCompleted {
		t.Fatalf("dynamic resolution = %#v", resumed.DynamicIncludes[resolutionID])
	}
}

func TestDynamicWiring_EngineSettlesResolverIntentWithDurablePin(t *testing.T) {
	catalog, _ := buildWiringTestCatalog(t)
	parserImpl, err := internalparser.New(platform.Real())
	if err != nil {
		t.Fatalf("parser: %v", err)
	}
	resolver := &countingDynamicResolver{inner: NewCatalogIncludeResolver(catalog, parserImpl)}
	runStoreBase := t.TempDir()
	store := internalrunstore.NewDirRunStore(runStoreBase)
	t.Cleanup(func() { _ = store.Close() })
	registry := internalexecutor.NewDefaultRegistry(internalexecutor.RegistryConfig{
		SubStepRunner: noopSubStepRunner, DynamicIncludeResolver: resolver,
		LazyRunbookLoader: NewParserLazyLoader(parserImpl),
	})
	plan := &engine.ExecutionPlan{
		RunID: "run-dynamic-intent", RunbookPath: "dynamic.runbook.yaml",
		Steps: []engine.ResolvedStep{{
			ID: "dynamic", Kind: "include", Spec: &schema.IncludeSpec{Include: schema.IncludeConfig{
				RunbookRef: "acme-pkg/tsg-network", ResolveFrom: schema.ResolveFromCatalog,
			}},
		}},
		Metadata: engine.PlanMetadata{RunbookID: "dynamic-intent"},
	}
	if err := internalplanner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	runtime := internalengine.New(engine.EngineConfig{
		Executors: registry, Dispatcher: &noopDispatcher{}, TraceWriter: &noopTraceWriter{},
		Platform: platform.Real(), Store: store,
	})
	handle, err := runtime.Start(context.Background(), plan, engine.RunOptions{Mode: engine.RunModeReal, Store: store})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = handle.Cancel(context.Background(), "test cleanup") })
	if _, err := handle.Next(context.Background()); err != nil {
		t.Fatalf("Next: %v", err)
	}
	state := handle.State()
	if resolver.calls != 1 || len(state.Dispatches) != 1 || len(state.DynamicIncludes) != 1 {
		t.Fatalf("resolver=%d dispatches=%#v resolutions=%#v", resolver.calls, state.Dispatches, state.DynamicIncludes)
	}
	for _, dispatch := range state.Dispatches {
		if dispatch.Status != engine.DispatchStatusSettled || dispatch.ResultDigest == "" {
			t.Fatalf("resolver dispatch = %#v", dispatch)
		}
	}
	for _, resolution := range state.DynamicIncludes {
		if resolution.Status != engine.DynamicIncludeResolutionStatusCompleted {
			t.Fatalf("dynamic resolution = %#v", resolution)
		}
	}
}

func TestDynamicWiring_PinCheckpointKeepsUnsettledParentCursor(t *testing.T) {
	catalog, _ := buildWiringTestCatalog(t)
	parserImpl, err := internalparser.New(platform.Real())
	if err != nil {
		t.Fatalf("parser: %v", err)
	}
	runnerStarted := make(chan struct{})
	releaseRunner := make(chan struct{})
	registry := internalexecutor.NewDefaultRegistry(internalexecutor.RegistryConfig{
		SubStepRunner: func(_ context.Context, _ internalexecutor.SubStepParent, _ []schema.FlowNode, _ map[string]any) ([]*engine.StepResult, error) {
			close(runnerStarted)
			<-releaseRunner
			return nil, nil
		},
		DynamicIncludeResolver: NewCatalogIncludeResolver(catalog, parserImpl),
		LazyRunbookLoader:      NewParserLazyLoader(parserImpl),
	})
	store := internalrunstore.NewDirRunStore(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	plan := &engine.ExecutionPlan{
		RunID: "run-dynamic-parent-cursor", RunbookPath: "dynamic.runbook.yaml",
		Steps: []engine.ResolvedStep{
			{ID: "dynamic", Kind: "include", Spec: &schema.IncludeSpec{Include: schema.IncludeConfig{
				RunbookRef: "acme-pkg/tsg-network", ResolveFrom: schema.ResolveFromCatalog,
			}}},
			{ID: "after", Kind: "noop", Spec: &schema.NoopSpec{}},
		},
		Metadata: engine.PlanMetadata{RunbookID: "dynamic-parent-cursor"},
	}
	if err := internalplanner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	runtime := internalengine.New(engine.EngineConfig{
		Executors: registry, Dispatcher: &noopDispatcher{}, TraceWriter: &noopTraceWriter{},
		Platform: platform.Real(), Store: store,
	})
	handle, err := runtime.Start(context.Background(), plan, engine.RunOptions{Mode: engine.RunModeReal, Store: store})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, nextErr := handle.Next(context.Background())
		done <- nextErr
	}()
	select {
	case <-runnerStarted:
	case <-time.After(2 * time.Second):
		close(releaseRunner)
		t.Fatal("dynamic include did not reach child runner")
	}
	checkpoint, err := store.LoadState(context.Background(), plan.RunID)
	if err != nil {
		close(releaseRunner)
		t.Fatalf("LoadState: %v", err)
	}
	cursor := checkpoint.CursorSet.Cursors[0]
	if cursor.StepID != "dynamic" || cursor.StepIndex != 0 || cursor.Phase != engine.ExecutionPhaseBefore {
		close(releaseRunner)
		t.Fatalf("pinned unresolved parent cursor = %#v, want dynamic/before", cursor)
	}
	close(releaseRunner)
	if err := <-done; err != nil {
		t.Fatalf("Next: %v", err)
	}
}

// TestDynamicWiring_OnNotFound_Continue verifies that a missing catalog entry
// with on_not_found: continue produces a skipped step with runbook_found=false
// through the fully wired registry.
func TestDynamicWiring_OnNotFound_Continue(t *testing.T) {
	catalog, _ := buildWiringTestCatalog(t)

	plat := platform.Real()
	parser, err := internalparser.New(plat)
	if err != nil {
		t.Fatalf("internalparser.New: %v", err)
	}

	reg := internalexecutor.NewDefaultRegistry(internalexecutor.RegistryConfig{
		SubStepRunner:          noopSubStepRunner,
		DynamicIncludeResolver: NewCatalogIncludeResolver(catalog, parser),
	})

	exec := reg.Lookup("include")
	step := engine.ResolvedStep{
		ID:   "s2",
		Kind: "include",
		Spec: &schema.IncludeSpec{
			Include: schema.IncludeConfig{
				RunbookRef:  "acme-pkg/no-such-runbook",
				ResolveFrom: schema.ResolveFromCatalog,
				OnNotFound:  schema.OnNotFoundContinue,
			},
		},
	}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("expected no error with on_not_found: continue, got %v", err)
	}
	if res.Status != engine.StepStatusSkipped {
		t.Fatalf("expected skipped, got %v", res.Status)
	}
	found, ok := res.Vars["runbook_found"].(bool)
	if !ok || found {
		t.Fatalf("expected runbook_found=false, got %v", res.Vars["runbook_found"])
	}
}
