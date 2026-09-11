package engine

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/google/uuid"
	"github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/internal/runstore"
	enginepkg "github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/extension"
	"github.com/ormasoftchile/yawr/runtime/pkg/governance"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

type routeControllerFunc func(context.Context, enginepkg.DebugLocation, enginepkg.ResolvedStep) (enginepkg.RouteTestDecision, error)

func (function routeControllerFunc) BeforeStep(ctx context.Context, location enginepkg.DebugLocation, step enginepkg.ResolvedStep) (enginepkg.RouteTestDecision, error) {
	return function(ctx, location, step)
}

type countingExtensionHost struct{ loads, shutdowns int }

func (host *countingExtensionHost) Load(context.Context, *extension.ProjectManifest) error {
	host.loads++
	return nil
}
func (host *countingExtensionHost) Shutdown(context.Context) error             { host.shutdowns++; return nil }
func (*countingExtensionHost) ContributedTools() []*schema.ToolDef             { return nil }
func (*countingExtensionHost) ContributedProviders() []*schema.ProviderDef     { return nil }
func (*countingExtensionHost) ContributedPolicyRules() []governance.PolicyRule { return nil }
func (*countingExtensionHost) Status() []extension.ExtensionStatus             { return nil }

func TestRouteTest_TargetReachPausesBeforeTargetWithoutExtensionLifecycleOrCancellation(t *testing.T) {
	config := makeTestConfig()
	host := &countingExtensionHost{}
	config.ExtensionHost = host
	writer := config.TraceWriter.(*fakeTraceWriter)
	controller := routeControllerFunc(func(_ context.Context, _ enginepkg.DebugLocation, _ enginepkg.ResolvedStep) (enginepkg.RouteTestDecision, error) {
		return enginepkg.RouteTestTargetReached, nil
	})

	handle, err := New(config).Start(context.Background(), enginepkg.ValidatedForTest(makeTestPlan(
		enginepkg.ResolvedStep{ID: "target", Kind: "noop", Spec: &schema.NoopSpec{}},
	)), enginepkg.RunOptions{Mode: enginepkg.RunModeRouteTest, RouteTest: controller})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result, err := handle.Next(context.Background()); err != io.EOF || result != nil {
		t.Fatalf("Next = (%#v, %v), want (nil, EOF)", result, err)
	}
	if handle.State().Status != enginepkg.RunStatusPausedAtBoundary {
		t.Fatalf("status = %s", handle.State().Status)
	}
	if handle.State().StepResults["target"] != nil {
		t.Fatalf("target executed before pause: %#v", handle.State().StepResults["target"])
	}
	if host.loads != 0 || host.shutdowns != 0 {
		t.Fatalf("extension lifecycle called: loads=%d shutdowns=%d", host.loads, host.shutdowns)
	}

	var reached, cancelled bool
	for _, event := range writer.collect() {
		reached = reached || event.Kind == trace.EventKindRouteTestTargetReached
		cancelled = cancelled || event.Kind == trace.EventKindRunCancelled
	}
	if !reached || cancelled {
		t.Fatalf("trace reached=%v cancelled=%v", reached, cancelled)
	}
}

func TestRouteTest_TargetPauseResumesAsLiveAttempt(t *testing.T) {
	base := t.TempDir()
	runID := uuid.NewString()
	plan := &enginepkg.ExecutionPlan{
		RunID: runID, RunbookPath: "route-pause.runbook.yaml",
		Steps: []enginepkg.ResolvedStep{
			{ID: "prefix", Kind: "noop", Spec: &schema.NoopSpec{}},
			{ID: "target", Kind: "noop", Spec: &schema.NoopSpec{}},
			{ID: "after", Kind: "noop", Spec: &schema.NoopSpec{}},
		},
		Metadata: enginepkg.PlanMetadata{RunbookID: "route-pause", RunbookName: "Route Pause"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	controller := routeControllerFunc(func(_ context.Context, location enginepkg.DebugLocation, _ enginepkg.ResolvedStep) (enginepkg.RouteTestDecision, error) {
		if location.StepID == "target" {
			return enginepkg.RouteTestTargetReached, nil
		}
		return enginepkg.RouteTestContinue, nil
	})
	firstStore := runstore.NewDirRunStore(base)
	firstRegistry := newFakeExecutorRegistry()
	firstExecuted := make([]string, 0, 1)
	firstRegistry.Register("noop", stepExecutorFunc(func(_ context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
		firstExecuted = append(firstExecuted, step.ID)
		return &enginepkg.StepResult{StepID: step.ID, Status: enginepkg.StepStatusCompleted, Outcome: enginepkg.StepOutcomeSuccess}, nil
	}))
	firstConfig := makeTestConfig()
	firstConfig.Executors = firstRegistry
	firstConfig.Store = firstStore
	firstHandle, err := New(firstConfig).Start(context.Background(), plan, enginepkg.RunOptions{
		Mode: enginepkg.RunModeRouteTest, RouteTest: controller, Store: firstStore,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result, err := firstHandle.Next(context.Background()); err != nil || result.StepID != "prefix" {
		t.Fatalf("prefix Next = %#v, %v", result, err)
	}
	if result, err := firstHandle.Next(context.Background()); err != io.EOF || result != nil {
		t.Fatalf("pause Next = %#v, %v", result, err)
	}
	paused, err := firstStore.LoadState(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if paused.Status != enginepkg.RunStatusPausedAtBoundary || paused.CursorSet == nil ||
		len(paused.CursorSet.Cursors) != 1 || paused.CursorSet.Cursors[0].StepID != "target" ||
		paused.CursorSet.Cursors[0].Phase != enginepkg.ExecutionPhaseBefore {
		t.Fatalf("paused checkpoint = %#v", paused)
	}
	if len(firstExecuted) != 1 || firstExecuted[0] != "prefix" {
		t.Fatalf("replay process executed %v", firstExecuted)
	}
	if err := firstStore.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}

	secondStore := runstore.NewDirRunStore(base)
	t.Cleanup(func() { _ = secondStore.Close() })
	secondRegistry := newFakeExecutorRegistry()
	secondExecuted := make([]string, 0, 2)
	secondRegistry.Register("noop", stepExecutorFunc(func(_ context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
		secondExecuted = append(secondExecuted, step.ID)
		return &enginepkg.StepResult{StepID: step.ID, Status: enginepkg.StepStatusCompleted, Outcome: enginepkg.StepOutcomeSuccess}, nil
	}))
	secondConfig := makeTestConfig()
	secondConfig.Executors = secondRegistry
	secondConfig.Store = secondStore
	resumed, err := New(secondConfig).Resume(context.Background(), runID, enginepkg.RunOptions{
		Mode: enginepkg.RunModeReal, Store: secondStore,
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	for {
		_, nextErr := resumed.Next(context.Background())
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			t.Fatalf("live Next: %v", nextErr)
		}
	}
	if len(secondExecuted) != 2 || secondExecuted[0] != "target" || secondExecuted[1] != "after" {
		t.Fatalf("live process executed %v", secondExecuted)
	}
}

func TestRouteTest_BoundaryErrorCannotContinueToTarget(t *testing.T) {
	registry := newFakeExecutorRegistry()
	registry.Register("unsafe", stepExecutorFunc(func(context.Context, enginepkg.ResolvedStep, map[string]any) (*enginepkg.StepResult, error) {
		return nil, enginepkg.NewRouteTestBoundaryError(errors.New("invalid reviewed fixture"))
	}))
	config := makeTestConfig()
	config.Executors = registry
	targetReached := false
	controller := routeControllerFunc(func(_ context.Context, location enginepkg.DebugLocation, _ enginepkg.ResolvedStep) (enginepkg.RouteTestDecision, error) {
		if location.StepID == "target" {
			targetReached = true
			return enginepkg.RouteTestTargetReached, nil
		}
		return enginepkg.RouteTestContinue, nil
	})
	plan := enginepkg.ValidatedForTest(&enginepkg.ExecutionPlan{Steps: []enginepkg.ResolvedStep{
		{ID: "fixture", Kind: "unsafe", Spec: &schema.NoopSpec{}, ContinueOnFail: true},
		{ID: "target", Kind: "unsafe", Spec: &schema.NoopSpec{}},
	}})
	handle, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{
		Mode: enginepkg.RunModeRouteTest, RouteTest: controller,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := handle.Next(context.Background()); !enginepkg.IsRouteTestBoundaryError(err) {
		t.Fatalf("Next error = %v, want route-test boundary error", err)
	}
	if targetReached || handle.State().Status != enginepkg.RunStatusFailed {
		t.Fatalf("target reached=%v status=%s", targetReached, handle.State().Status)
	}
}

func TestRouteTest_ParallelBoundaryDominatesJoinRecovery(t *testing.T) {
	registry := newFakeExecutorRegistry()
	ordinaryStarted := make(chan struct{})
	registry.Register("ordinary", stepExecutorFunc(func(context.Context, enginepkg.ResolvedStep, map[string]any) (*enginepkg.StepResult, error) {
		close(ordinaryStarted)
		return nil, errors.New("ordinary branch failure")
	}))
	registry.Register("boundary", stepExecutorFunc(func(ctx context.Context, _ enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
		<-ordinaryStarted
		<-ctx.Done()
		return nil, enginepkg.NewRouteTestBoundaryError(errors.New("missing reviewed fixture"))
	}))
	config := makeTestConfig()
	config.Executors = registry
	targetReached := false
	controller := routeControllerFunc(func(_ context.Context, location enginepkg.DebugLocation, _ enginepkg.ResolvedStep) (enginepkg.RouteTestDecision, error) {
		if location.StepID == "target" {
			targetReached = true
			return enginepkg.RouteTestTargetReached, nil
		}
		return enginepkg.RouteTestContinue, nil
	})
	plan := enginepkg.ValidatedForTest(&enginepkg.ExecutionPlan{Steps: []enginepkg.ResolvedStep{
		{ID: "fanout", Kind: "parallel", Spec: &schema.ParallelNode{
			Branches: []schema.ParallelBranch{
				{Label: "ordinary", Steps: []schema.FlowNode{{Step: &schema.Step{ID: "ordinary", Type: "ordinary"}}}},
				{Label: "boundary", Steps: []schema.FlowNode{{Step: &schema.Step{ID: "boundary", Type: "boundary"}}}},
			},
			Join: &schema.ParallelJoin{OnFailure: "continue"},
		}},
		{ID: "ordinary", Kind: "ordinary", Spec: &schema.NoopSpec{}, Depth: 1, ParentID: "fanout", ParentKind: "parallel", BranchLabel: "ordinary"},
		{ID: "boundary", Kind: "boundary", Spec: &schema.NoopSpec{}, Depth: 1, ParentID: "fanout", ParentKind: "parallel", BranchLabel: "boundary"},
		{ID: "target", Kind: "noop", Spec: &schema.NoopSpec{}},
	}})
	handle, err := New(config).Start(context.Background(), plan, enginepkg.RunOptions{
		Mode: enginepkg.RunModeRouteTest, RouteTest: controller,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := handle.Next(context.Background()); !enginepkg.IsRouteTestBoundaryError(err) {
		t.Fatalf("Next error = %v, want route-test boundary error", err)
	}
	if targetReached || handle.State().Status != enginepkg.RunStatusFailed {
		t.Fatalf("target=%v status=%s", targetReached, handle.State().Status)
	}
}
