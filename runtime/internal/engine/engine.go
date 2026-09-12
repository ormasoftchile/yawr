package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	internaldebugprotect "github.com/ormasoftchile/yawr/runtime/internal/debugprotect"
	"github.com/ormasoftchile/yawr/runtime/internal/executor"
	internalGov "github.com/ormasoftchile/yawr/runtime/internal/governance"
	internalinput "github.com/ormasoftchile/yawr/runtime/internal/input"
	internalplanner "github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	"github.com/ormasoftchile/yawr/runtime/internal/resume"
	"github.com/ormasoftchile/yawr/runtime/pkg/capture"
	enginepkg "github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/eventbus"
	"github.com/ormasoftchile/yawr/runtime/pkg/evidence"
	"github.com/ormasoftchile/yawr/runtime/pkg/extension"
	"github.com/ormasoftchile/yawr/runtime/pkg/governance"
	otelPkg "github.com/ormasoftchile/yawr/runtime/pkg/otel"
	replaypkg "github.com/ormasoftchile/yawr/runtime/pkg/replay"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

// Compile-time interface check.
var _ enginepkg.Engine = (*impl)(nil)

// impl is the concrete Engine implementation.
type impl struct {
	cfg enginepkg.EngineConfig
}

// tracer returns the configured Tracer, falling back to noop.
func (e *impl) tracer() otelPkg.Tracer {
	return otelPkg.ResolveProvider(e.cfg.TracerProvider).Tracer("github.com/ormasoftchile/yawr/runtime")
}

// New constructs an Engine with the given configuration.
// Panics if required config fields are missing.
func New(cfg enginepkg.EngineConfig) enginepkg.Engine {
	if err := cfg.Validate(); err != nil {
		panic(err)
	}
	return &impl{cfg: cfg}
}

// PrepareDurablePlan materializes every static deferred include and validates
// that the resulting plan is safe to persist before a coordinator publishes
// immutable session references to it.
func (e *impl) PrepareDurablePlan(ctx context.Context, plan *enginepkg.ExecutionPlan) error {
	if plan == nil || plan.Validation == nil {
		return errors.New("engine: validated execution plan is required")
	}
	hadDeferredIncludes := internalplanner.HasDeferredStaticIncludes(plan)
	if err := executor.MaterializeLazyIncludes(ctx, e.cfg.Executors, plan); err != nil {
		return err
	}
	if err := executor.MaterializeToolSubstitutions(ctx, e.cfg.Executors, plan); err != nil {
		return err
	}
	if err := internalplanner.ValidateTypedBoundClosure(plan); err != nil {
		return err
	}
	for _, definition := range plan.Tools {
		if definition == nil {
			continue
		}
		for _, action := range definition.Actions {
			if action == nil || action.FrozenSubstitution == nil {
				continue
			}
			flow, err := plansnapshot.RestoreFlowClosure(action.FrozenSubstitution.ExecutableClosure)
			if err != nil {
				return err
			}
			if err := internalplanner.ValidateTypedBoundFlow(flow, action.FrozenSubstitution.Bindings, plan.Tools); err != nil {
				return err
			}
		}
	}
	if hadDeferredIncludes {
		if err := internalplanner.FinalizeMaterializedPlan(plan); err != nil {
			return err
		}
	}
	return plansnapshot.ValidateResumeSafetyForState(plan, enginepkg.RunState{WriterEpoch: 1})
}

// Start initializes a run from a plan and returns a RunHandle.
func (e *impl) Start(ctx context.Context, plan *enginepkg.ExecutionPlan, opts enginepkg.RunOptions) (enginepkg.RunHandle, error) {
	if plan == nil {
		return nil, &enginepkg.ConfigError{Missing: []string{"plan"}}
	}
	if plan.Validation == nil {
		return nil, errors.New("engine: refusing to start unvalidated plan")
	}
	store, err := durableRunStore(opts.Store, e.cfg.Store)
	if err != nil {
		return nil, err
	}
	if store == nil {
		if err := internalplanner.ValidateTypedBoundClosure(plan); err != nil {
			return nil, err
		}
	}

	if e.cfg.InputProvider == nil {
		e.cfg.InputProvider = internalinput.NewChainProvider(
			internalinput.NewEnvProvider(),
			internalinput.NewVaultProvider(),
			internalinput.NewPromptProvider(os.Stdin, os.Stderr),
		)
	}

	runID := plan.RunID
	if runID == "" {
		runID = uuid.New().String()
	}
	if err := validatePreStartEvents(runID, opts.PreStartEvents); err != nil {
		return nil, err
	}
	if len(opts.PreStartEvents) > 0 {
		if provider, ok := e.cfg.TraceWriter.(interface{ LastSequence(string) int64 }); ok &&
			provider.LastSequence(runID) != int64(len(opts.PreStartEvents)) {
			return nil, fmt.Errorf("%w: incomplete pre-start trace prefix", enginepkg.ErrTraceCommit)
		}
	}
	var lease enginepkg.RunLease
	if store != nil {
		var err error
		lease, err = store.AcquireRunLease(ctx, runID)
		if err != nil {
			return nil, fmt.Errorf("engine: acquire writer lease for %s: %w", runID, err)
		}
		defer func() {
			if lease != nil {
				_ = lease.Release()
			}
		}()
		if err := e.PrepareDurablePlan(ctx, plan); err != nil {
			return nil, fmt.Errorf("engine: materialize deferred includes for %s: %w", runID, err)
		}
		if err := e.ValidateDurableHandoffPlan(ctx, plan, opts); err != nil {
			return nil, fmt.Errorf("engine: validate durable handoff definitions for %s: %w", runID, err)
		}
		if err := store.SavePlan(ctx, runID, plan); err != nil {
			return nil, fmt.Errorf("engine: persist execution plan for %s: %w", runID, err)
		}
		for _, event := range opts.PreStartEvents {
			if err := store.WriteTrace(enginepkg.WithRunWriterEpoch(ctx, lease.Epoch()), runID, event); err != nil {
				return nil, fmt.Errorf("%w: persist pre-start event %s: %w", enginepkg.ErrTraceCommit, event.EventID, err)
			}
		}
	}
	if e.cfg.ExtensionHost != nil && opts.Mode != enginepkg.RunModeRouteTest {
		manifest := buildExtensionManifest(plan.Metadata.Extensions)
		if err := e.cfg.ExtensionHost.Load(ctx, manifest); err != nil {
			return nil, err
		}
	}

	run := enginepkg.NewRun(runID, plan, opts)
	run.BindingScope = enginepkg.CloneBindingScope(enginepkg.BindingScopeFromContext(ctx))
	run.Sequence = int64(len(opts.PreStartEvents))
	if provider, ok := e.cfg.TraceWriter.(interface{ LastSequence(string) int64 }); ok {
		run.Sequence = max(run.Sequence, provider.LastSequence(runID))
	}
	if lease != nil {
		run.WriterEpoch = lease.Epoch()
	}
	debugProtection := buildDebugProtection(ctx, plan, run.Vars)
	debugProtectAllVars := internaldebugprotect.ProtectAllFromContext(ctx)
	var complete bool
	debugProtection, complete = precomputeDebugProtection(ctx, e.cfg.Executors, plan, run.Vars, debugProtection)
	if opts.Debugger != nil {
		debugProtectAllVars = debugProtectAllVars || !complete
	}
	if debugProtectAllVars {
		debugProtection = enginepkg.MergeDebugProtection(debugProtection, allDebugVariablesProtection(run.Vars))
	}
	// Create a cancellable context scoped to this run's lifetime.
	runCtx, runCancel := context.WithCancel(context.Background())

	// Start the run-level OTel span using the caller's context (preserves upstream trace).
	traceCtx, runSpan := e.tracer().Start(ctx, "yawr.run",
		otelPkg.WithAttributes(
			otelPkg.Attribute{Key: otelPkg.AttrRunID, Value: runID},
			otelPkg.Attribute{Key: otelPkg.AttrRunbookPath, Value: plan.RunbookPath},
			otelPkg.Attribute{Key: otelPkg.AttrRunMode, Value: string(opts.Mode)},
			otelPkg.Attribute{Key: otelPkg.AttrActor, Value: opts.Actor},
		),
	)

	h := &runHandle{
		engine:                 e,
		run:                    run,
		events:                 make(chan enginepkg.Event, 256),
		store:                  store,
		onEvent:                opts.OnEvent,
		mu:                     &sync.Mutex{},
		runCtx:                 runCtx,
		cancelFn:               runCancel,
		runSpan:                runSpan,
		traceCtx:               traceCtx,
		debugger:               opts.Debugger,
		routeTest:              opts.RouteTest,
		debugCallPath:          enginepkg.DebugCallPathFromContext(ctx),
		debugInvocations:       executionInvocationTracker(ctx, opts.Debugger, opts.RouteTest),
		debugProtection:        debugProtection,
		debugProtectAllVars:    debugProtectAllVars,
		extensionsEnabled:      opts.Mode != enginepkg.RunModeRouteTest,
		interactionInvocations: interactionInvocationTracker(ctx),
		lease:                  lease,
	}
	lease = nil
	// Build a per-run governance evaluator from the plan's runbook governance
	// config, composed with the engine-wired approval gate. This ensures the
	// runbook-level require_approval flag is active for every step in this run,
	// even if the engine-global GovernanceEvaluator was constructed before the
	// plan was available (i.e., at wiring time without a plan).
	h.governanceEvaluator = internalGov.BuildEvaluator(e.cfg.ApprovalGate, plan.GovernanceSource)
	// Wrap with ProfileEvaluator when a RuntimeProfile is present.
	// Nil profile passes through transparently — no behavior change.
	h.governanceEvaluator = internalGov.NewProfileEvaluator(h.governanceEvaluator, plan.Metadata.Profile)

	// Signal handling goroutine: cancel the run on SIGINT/SIGTERM.
	sigCh := e.cfg.Platform.NotifySignals(runCtx)
	go func() {
		select {
		case sig, ok := <-sigCh:
			if !ok {
				return // run context cancelled, channel closed normally
			}
			h.mu.Lock()
			if h.done.Load() {
				h.mu.Unlock()
				return
			}
			_ = h.cancelRunLocked(context.WithoutCancel(runCtx), "", "run cancelled by signal", map[string]any{
				"signal": sig.Name,
			})
			h.unlockAndDrainCallbacks()
		case <-runCtx.Done():
		}
	}()

	return h, nil
}

func buildExtensionManifest(exts []*schema.ExtensionRef) *extension.ProjectManifest {
	manifest := &extension.ProjectManifest{}
	if len(exts) == 0 {
		return manifest
	}
	manifest.Extensions = make([]extension.ExtensionDecl, 0, len(exts))
	for _, ext := range exts {
		if ext == nil {
			continue
		}
		manifest.Extensions = append(manifest.Extensions, extension.ExtensionDecl{
			Name:   ext.Name,
			Path:   ext.Path,
			Grants: ext.Grants,
		})
	}
	return manifest
}

// Resume restores a previously checkpointed run from the run store.
func (e *impl) Resume(ctx context.Context, runID string, opts enginepkg.RunOptions) (enginepkg.RunHandle, error) {
	if len(opts.PreStartEvents) > 0 {
		return nil, fmt.Errorf("%w: pre-start events cannot be replayed on resume", enginepkg.ErrTraceCommit)
	}
	if opts.Debugger != nil {
		return nil, enginepkg.ErrDebugResumeUnsupported
	}
	if opts.RouteTest != nil {
		return nil, enginepkg.ErrRouteTestResumeUnsupported
	}
	selectedStore := opts.Store
	if selectedStore == nil {
		selectedStore = e.cfg.Store
	}
	if selectedStore == nil {
		return nil, errors.New("engine: RunStore is required for Resume")
	}
	store, err := durableRunStore(selectedStore, nil)
	if err != nil {
		return nil, err
	}
	if _, err := store.LoadPlan(ctx, runID); err != nil {
		if state, stateErr := store.LoadState(ctx, runID); stateErr == nil && state.Status == enginepkg.RunStatusIndeterminate && !opts.AcknowledgeIndeterminate {
			return nil, enginepkg.ErrIndeterminateAcknowledgmentRequired
		}
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("engine: durable execution plan for %s is not available: %w", runID, err)
		}
		return nil, fmt.Errorf("engine: preflight execution plan: %w", err)
	}
	if _, err := store.LoadState(ctx, runID); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("engine: preflight checkpoint: %w", err)
	}
	lease, err := store.AcquireRunLease(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("engine: acquire writer lease for %s: %w", runID, err)
	}
	releaseLease := true
	defer func() {
		if releaseLease {
			_ = lease.Release()
		}
	}()
	if e.cfg.InputProvider == nil {
		e.cfg.InputProvider = internalinput.NewChainProvider(
			internalinput.NewEnvProvider(),
			internalinput.NewVaultProvider(),
			internalinput.NewPromptProvider(os.Stdin, os.Stderr),
		)
	}
	preflightState, preflightStateErr := store.LoadState(ctx, runID)
	if preflightStateErr != nil && !errors.Is(preflightStateErr, os.ErrNotExist) {
		return nil, fmt.Errorf("engine: load checkpoint for %s: %w", runID, preflightStateErr)
	}
	if preflightStateErr == nil && preflightState.Status == enginepkg.RunStatusIndeterminate && !opts.AcknowledgeIndeterminate {
		return nil, enginepkg.ErrIndeterminateAcknowledgmentRequired
	}
	if preflightStateErr == nil && preflightState.Status == enginepkg.RunStatusHandoffPending {
		return nil, enginepkg.ErrHandoffPending
	}

	plan, err := store.LoadPlan(ctx, runID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("engine: durable execution plan for %s is not available: %w", runID, err)
		}
		return nil, fmt.Errorf("engine: load execution plan for %s: %w", runID, err)
	}
	if plan == nil {
		return nil, fmt.Errorf("engine: load execution plan for %s returned no plan", runID)
	}
	durablePlanDigest, _ := store.PlanDigest(runID)

	state := preflightState
	if preflightStateErr != nil {
		if err := plansnapshot.ValidateResumeSafety(plan); err != nil {
			return nil, fmt.Errorf("engine: resume %s: %w", runID, err)
		}
		state, err = store.LoadState(ctx, runID)
		if err != nil {
			return nil, fmt.Errorf("engine: load checkpoint for %s: %w", runID, err)
		}
	}
	if err := plansnapshot.ValidateResumeSafetyForState(plan, state); err != nil {
		return nil, fmt.Errorf("engine: resume %s: %w", runID, err)
	}
	if state.PlanSnapshotDigest == "" {
		return nil, fmt.Errorf("engine: checkpoint for %s is missing plan snapshot digest", runID)
	}
	if durablePlanDigest == "" {
		return nil, fmt.Errorf("engine: checkpoint for %s requires its durable execution plan", runID)
	}
	if state.PlanSnapshotDigest != durablePlanDigest {
		return nil, fmt.Errorf("engine: checkpoint plan digest %q does not match durable plan %q", state.PlanSnapshotDigest, durablePlanDigest)
	}
	if err := validateRestoredTypedState(plan, state); err != nil {
		return nil, fmt.Errorf("engine: typed checkpoint validation: %w", err)
	}
	if recovery, ok := store.(interface {
		RecoverTraceProjection(context.Context, trace.TraceWriter, enginepkg.RunState) error
	}); ok {
		if err := recovery.RecoverTraceProjection(ctx, e.cfg.TraceWriter, state); err != nil {
			return nil, fmt.Errorf("engine: recover authoritative trace projection for %s: %w", runID, err)
		}
	} else if err := recoverPendingTraceEvents(ctx, e.cfg.TraceWriter, store, lease.Epoch(), state.PendingTraceEvents); err != nil {
		return nil, fmt.Errorf("engine: recover trace projection for %s: %w", runID, err)
	}
	state.PendingTraceEvents = nil
	traceContext, err := resume.ScanTrace(ctx, store, runID, state)
	if err != nil {
		return nil, fmt.Errorf("engine: scan trace for %s: %w", runID, err)
	}
	if traceContext.LastSeq > state.CommittedTraceSequence {
		state.CommittedTraceSequence = traceContext.LastSeq
	}
	state, recoveredDispatch, err := recoverUnmatchedDispatches(ctx, store, lease.Epoch(), plan, state)
	if err != nil {
		return nil, fmt.Errorf("engine: recover unmatched dispatch for %s: %w", runID, err)
	}
	if err := recoverPendingTraceEvents(ctx, e.cfg.TraceWriter, store, lease.Epoch(), state.PendingTraceEvents); err != nil {
		return nil, fmt.Errorf("engine: project dispatch recovery for %s: %w", runID, err)
	}
	state.PendingTraceEvents = nil
	if !recoveredDispatch {
		state, err = refenceActiveExecutionState(ctx, store, lease.Epoch(), state)
		if err != nil {
			return nil, fmt.Errorf("engine: re-fence execution frames for %s: %w", runID, err)
		}
		if err := recoverPendingTraceEvents(ctx, e.cfg.TraceWriter, store, lease.Epoch(), state.PendingTraceEvents); err != nil {
			return nil, fmt.Errorf("engine: project writer re-fence for %s: %w", runID, err)
		}
		state.PendingTraceEvents = nil
	}
	if recoveredDispatch || state.Status == enginepkg.RunStatusIndeterminate && !opts.AcknowledgeIndeterminate {
		return nil, enginepkg.ErrIndeterminateAcknowledgmentRequired
	}

	if e.cfg.ExtensionHost != nil {
		manifest := buildExtensionManifest(plan.Metadata.Extensions)
		if err := e.cfg.ExtensionHost.Load(ctx, manifest); err != nil {
			return nil, err
		}
	}

	var resumeRun *enginepkg.Run
	closedResults := state.Results != nil && state.Status == enginepkg.RunStatusCompleted
	if closedResults {
		resumeRun, err = resume.RebuildRun(runID, plan, state, traceContext, opts)
	} else {
		resumeRun, _, err = resume.ResumeFromState(ctx, store, runID, plan, state, opts)
	}
	if err != nil {
		return nil, fmt.Errorf("engine: resume %s: %w", runID, err)
	}
	resumeRun.WriterEpoch = lease.Epoch()
	resumeRun.BindingScope = enginepkg.CloneBindingScope(state.BindingScope)
	resumeRun.Results = cloneResults(state.Results)
	debugProtection := buildDebugProtection(ctx, plan, resumeRun.Vars)
	debugProtectAllVars := internaldebugprotect.ProtectAllFromContext(ctx)
	var complete bool
	debugProtection, complete = precomputeDebugProtection(ctx, e.cfg.Executors, plan, resumeRun.Vars, debugProtection)
	if opts.Debugger != nil {
		debugProtectAllVars = debugProtectAllVars || !complete
	}
	if debugProtectAllVars {
		debugProtection = enginepkg.MergeDebugProtection(debugProtection, allDebugVariablesProtection(resumeRun.Vars))
	}

	runCtx, runCancel := context.WithCancel(context.Background())

	// Start a new run-level span for the resumed run using the caller's context.
	traceCtx, runSpan := e.tracer().Start(ctx, "yawr.run",
		otelPkg.WithAttributes(
			otelPkg.Attribute{Key: otelPkg.AttrRunID, Value: runID},
			otelPkg.Attribute{Key: otelPkg.AttrRunbookPath, Value: plan.RunbookPath},
			otelPkg.Attribute{Key: otelPkg.AttrRunMode, Value: string(opts.Mode)},
		),
	)

	h := &runHandle{
		engine:                 e,
		run:                    resumeRun,
		events:                 make(chan enginepkg.Event, 256),
		store:                  store,
		onEvent:                opts.OnEvent,
		mu:                     &sync.Mutex{},
		runCtx:                 runCtx,
		cancelFn:               runCancel,
		runSpan:                runSpan,
		traceCtx:               traceCtx,
		debugger:               opts.Debugger,
		debugCallPath:          enginepkg.DebugCallPathFromContext(ctx),
		debugInvocations:       executionInvocationTrackerForResume(ctx, state),
		debugProtection:        debugProtection,
		debugProtectAllVars:    debugProtectAllVars,
		interactionInvocations: interactionInvocationTrackerForResume(ctx, state),
		lease:                  lease,
	}
	releaseLease = false
	h.governanceEvaluator = internalGov.BuildEvaluator(e.cfg.ApprovalGate, plan.GovernanceSource)
	// Wrap with ProfileEvaluator when a RuntimeProfile is present.
	h.governanceEvaluator = internalGov.NewProfileEvaluator(h.governanceEvaluator, plan.Metadata.Profile)
	h.started.Store(!(planInvocation(plan) != nil && state.Status == enginepkg.RunStatusPending && state.StartedAt.IsZero()))
	if closedResults {
		h.run.Status, h.run.CompletedAt = state.Status, state.CompletedAt
		h.done.Store(true)
		safeClose(h.events, &h.eventsClosed)
		h.cancelFn()
		h.runSpan.End()
		return h, nil
	}

	h.emitEventLocked(runCtx, trace.EventKindStepResumed, map[string]any{
		"run_id":            runID,
		"source":            "resume",
		"resumed_from_step": state.CurrentStep,
	})
	if h.traceErr != nil {
		_, traceErr := h.haltCheckpointCommit(runCtx, "", nil, h.traceErr)
		return nil, traceErr
	}

	sigCh := e.cfg.Platform.NotifySignals(runCtx)
	go func() {
		select {
		case sig, ok := <-sigCh:
			if !ok {
				return
			}
			h.mu.Lock()
			if h.done.Load() {
				h.mu.Unlock()
				return
			}
			_ = h.cancelRunLocked(context.WithoutCancel(runCtx), "", "run cancelled by signal", map[string]any{
				"signal": sig.Name,
			})
			h.unlockAndDrainCallbacks()
		case <-runCtx.Done():
		}
	}()

	return h, nil
}

func durableRunStore(primary, fallback enginepkg.RunStore) (enginepkg.DurableRunStore, error) {
	store := primary
	if store == nil {
		store = fallback
	}
	if store == nil {
		return nil, nil
	}
	durable, ok := store.(enginepkg.DurableRunStore)
	if !ok {
		return nil, errors.New("engine: configured RunStore does not implement DurableRunStore")
	}
	return durable, nil
}

func refenceActiveExecutionState(
	ctx context.Context,
	store enginepkg.DurableRunStore,
	writerEpoch uint64,
	state enginepkg.RunState,
) (enginepkg.RunState, error) {
	requiresCommit := false
	for _, frame := range state.ExecutionFrames {
		if frame != nil && frame.Status == enginepkg.ExecutionFrameStatusActive && frame.WriterEpoch != writerEpoch {
			requiresCommit = true
			break
		}
	}
	if !requiresCommit {
		for _, resolution := range state.DynamicIncludes {
			if resolution != nil && resolution.Status == enginepkg.DynamicIncludeResolutionStatusActive &&
				resolution.WriterEpoch != writerEpoch {
				requiresCommit = true
				break
			}
		}
	}
	if !requiresCommit {
		return state, nil
	}
	state.WriterEpoch = writerEpoch
	state.UpdatedAt = time.Now().UTC()
	state.ExecutionFrames = cloneExecutionFrameStateMap(state.ExecutionFrames)
	state.DynamicIncludes = cloneDynamicIncludeResolutionStateMap(state.DynamicIncludes)
	for _, frame := range state.ExecutionFrames {
		if frame != nil && frame.Status == enginepkg.ExecutionFrameStatusActive {
			frame.WriterEpoch = writerEpoch
		}
	}
	for _, resolution := range state.DynamicIncludes {
		if resolution != nil && resolution.Status == enginepkg.DynamicIncludeResolutionStatusActive {
			resolution.WriterEpoch = writerEpoch
		}
	}
	state.CheckpointSequence++
	stageResumeCommitEvent(&state, "")
	if err := store.SaveState(ctx, state); err != nil {
		return enginepkg.RunState{}, fmt.Errorf("%w: %w", enginepkg.ErrCheckpointCommit, err)
	}
	return state, nil
}

func recoverUnmatchedDispatches(
	ctx context.Context,
	store enginepkg.DurableRunStore,
	writerEpoch uint64,
	plan *enginepkg.ExecutionPlan,
	state enginepkg.RunState,
) (enginepkg.RunState, bool, error) {
	prepared, err := markPreparedDispatchesIndeterminate(&state, writerEpoch, time.Now().UTC())
	if err != nil {
		return enginepkg.RunState{}, false, err
	}
	if len(prepared) == 0 {
		return state, false, nil
	}
	if cursor, ok := serialCursorAfterRecoveredDispatch(plan, state.CursorSet, prepared); ok {
		state.CursorSet = cursor
	} else if cursors := executionFrameCursors(
		state.CurrentStepIndex, state.ExecutionFrames, state.Dispatches,
		state.Interactions, state.ExecutionInvocationCounts,
	); len(cursors) > 0 {
		state.CursorSet = &enginepkg.ExecutionCursorSet{
			SchemaVersion: enginepkg.ExecutionCursorSchemaV1,
			Cursors:       cursors,
		}
	} else if hasActiveExecutionFrame(state.ExecutionFrames) && state.CurrentStepIndex >= 0 && state.CurrentStepIndex < len(plan.Steps) {
		step := plan.Steps[state.CurrentStepIndex]
		state.CursorSet = &enginepkg.ExecutionCursorSet{
			SchemaVersion: enginepkg.ExecutionCursorSchemaV1,
			Cursors: []enginepkg.ExecutionCursor{{
				QualifiedNodeID: step.ID, StepID: step.ID, StepIndex: state.CurrentStepIndex,
				Phase: enginepkg.ExecutionPhaseBefore, Invocation: 1, RetryAttempt: 1,
			}},
		}
	}
	state.CheckpointSequence++
	runbookID := ""
	if plan != nil {
		runbookID = plan.Metadata.RunbookID
	}
	stageResumeCommitEvent(&state, runbookID)
	if err := store.SaveState(ctx, state); err != nil {
		return enginepkg.RunState{}, false, fmt.Errorf("%w: %w", enginepkg.ErrCheckpointCommit, err)
	}
	return state, true, nil
}

func stageResumeCommitEvent(state *enginepkg.RunState, runbookID string) {
	if state == nil {
		return
	}
	state.CommittedTraceSequence++
	state.PendingTraceEvents = append(state.PendingTraceEvents, enginepkg.Event{
		EventID: uuid.NewString(), RunID: state.RunID, RunbookID: runbookID,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano), Kind: string(trace.EventKindExecutionCommitted),
		Sequence: state.CommittedTraceSequence,
		Payload: map[string]any{
			"checkpoint_sequence": state.CheckpointSequence,
			"status":              string(state.Status),
			"current_step_index":  state.CurrentStepIndex,
			"cursor_set":          state.CursorSet,
		},
	})
}

func markPreparedDispatchesIndeterminate(
	state *enginepkg.RunState,
	writerEpoch uint64,
	now time.Time,
) ([]*enginepkg.DispatchState, error) {
	prepared := make([]*enginepkg.DispatchState, 0)
	for _, dispatch := range state.Dispatches {
		if dispatch != nil && dispatch.Status == enginepkg.DispatchStatusPrepared {
			prepared = append(prepared, dispatch)
		}
	}
	if len(prepared) == 0 {
		return nil, nil
	}
	state.WriterEpoch = writerEpoch
	state.Status = enginepkg.RunStatusIndeterminate
	state.CompletedAt = now
	state.UpdatedAt = now
	state.Dispatches = cloneDispatchStateMap(state.Dispatches)
	state.ExecutionFrames = cloneExecutionFrameStateMap(state.ExecutionFrames)
	state.DynamicIncludes = cloneDynamicIncludeResolutionStateMap(state.DynamicIncludes)
	for _, frame := range state.ExecutionFrames {
		if frame != nil && frame.Status == enginepkg.ExecutionFrameStatusActive {
			frame.WriterEpoch = writerEpoch
		}
	}
	for _, resolution := range state.DynamicIncludes {
		if resolution != nil && resolution.Status == enginepkg.DynamicIncludeResolutionStatusActive {
			resolution.WriterEpoch = writerEpoch
		}
	}
	if state.StepResults == nil {
		state.StepResults = make(map[string]*enginepkg.StepResult)
	}
	sort.Slice(prepared, func(left, right int) bool {
		return prepared[left].OccurrenceSequence < prepared[right].OccurrenceSequence
	})
	for _, previous := range prepared {
		dispatch := cloneDispatchState(*previous)
		dispatch.Status = enginepkg.DispatchStatusIndeterminate
		dispatch.ResultDigest = ""
		dispatch.SettledAt = now.Format(time.RFC3339Nano)
		state.Dispatches[dispatch.OccurrenceID] = &dispatch
		dispatchErr := fmt.Errorf("%w: unmatched dispatch intent %s", enginepkg.ErrIndeterminate, dispatch.OccurrenceID)
		result := &enginepkg.StepResult{
			StepID: dispatch.StepID, Status: enginepkg.StepStatusIndeterminate,
			StartedAt: now, CompletedAt: now, Error: dispatchErr,
			Indeterminate: &enginepkg.IndeterminateRecord{
				RunID: state.RunID, StepID: dispatch.StepID, Classification: dispatch.Classification,
				AttemptNumber: dispatch.RetryAttempt, FailureTime: now,
				TransportErrCategory: "unmatched-dispatch-intent",
			},
		}
		if dispatch.FrameID == "" {
			state.StepResults[dispatch.StepID] = result
			continue
		}
		frame := state.ExecutionFrames[dispatch.FrameID]
		if frame == nil || frame.Status != enginepkg.ExecutionFrameStatusActive ||
			dispatch.FrameStepIndex != frame.NextStepIndex || dispatch.FrameStepIndex >= frame.StepCount {
			return nil, errors.New("engine: unmatched dispatch does not match active execution frame")
		}
		frame.Results[strconv.Itoa(dispatch.FrameStepIndex)] = result
		frame.NextStepIndex++
	}
	return prepared, nil
}

func serialCursorAfterRecoveredDispatch(
	plan *enginepkg.ExecutionPlan,
	cursorSet *enginepkg.ExecutionCursorSet,
	prepared []*enginepkg.DispatchState,
) (*enginepkg.ExecutionCursorSet, bool) {
	if plan == nil || cursorSet == nil || cursorSet.SchemaVersion != enginepkg.ExecutionCursorSchemaV1 ||
		len(cursorSet.Cursors) != 1 || len(prepared) != 1 {
		return nil, false
	}
	cursor := cursorSet.Cursors[0]
	dispatch := prepared[0]
	if cursor.Phase != enginepkg.ExecutionPhaseExecute || len(cursor.CallPath) != 0 ||
		cursor.StepIndex < 0 || cursor.StepIndex >= len(plan.Steps) ||
		cursor.StepID != dispatch.StepID || cursor.QualifiedNodeID != dispatch.QualifiedNodeID {
		return nil, false
	}
	nextIndex := cursor.StepIndex + 1
	for nextIndex < len(plan.Steps) && plan.Steps[nextIndex].Depth > 0 {
		nextIndex++
	}
	next := enginepkg.ExecutionCursor{
		StepIndex: nextIndex, Phase: enginepkg.ExecutionPhaseBefore,
		Invocation: 1, RetryAttempt: 1, AtEnd: nextIndex >= len(plan.Steps),
	}
	if !next.AtEnd {
		next.StepID = plan.Steps[nextIndex].ID
		next.QualifiedNodeID = enginepkg.DebugNodeID(nil, next.StepID)
	}
	return &enginepkg.ExecutionCursorSet{
		SchemaVersion: enginepkg.ExecutionCursorSchemaV1,
		Cursors:       []enginepkg.ExecutionCursor{next},
	}, true
}

// runHandle implements engine.RunHandle.
type runHandle struct {
	engine           *impl
	run              *enginepkg.Run
	events           chan enginepkg.Event
	eventsClosed     atomic.Bool
	store            enginepkg.RunStore
	onEvent          func(enginepkg.Event)
	mu               *sync.Mutex
	executionMu      sync.Mutex
	runCtx           context.Context
	cancelFn         context.CancelFunc
	started          atomic.Bool
	done             atomic.Bool
	traceErr         error
	callbacksArmed   atomic.Bool
	callbackMu       sync.Mutex
	callbackQueue    []*callbackPublication
	callbackPending  map[string]*callbackPublication
	lastCallback     *callbackPublication
	callbacksRunning bool
	callbackIdle     chan struct{}

	// OTel span for the entire run lifetime.
	runSpan  otelPkg.Span
	traceCtx context.Context // carries the run span for child step spans

	// governanceEvaluator is a per-run PolicyEvaluator built from the plan's
	// runbook governance at Start()/Resume() time. It is always non-nil when
	// the engine has an ApprovalGate configured (even when the runbook declares
	// no governance: block). executeStep uses this evaluator (falling back to
	// the engine-global cfg.GovernanceEvaluator) to enforce approval for every
	// step, including direct tool invocations across all transports.
	governanceEvaluator    governance.PolicyEvaluator
	debugger               enginepkg.DebugController
	routeTest              enginepkg.RouteTestController
	debugCallPath          []enginepkg.DebugCallFrame
	debugInvocations       *enginepkg.DebugInvocationTracker
	debugProtection        enginepkg.DebugProtection
	debugProtectAllVars    bool
	extensionsEnabled      bool
	interactionInvocations *enginepkg.InteractionInvocationTracker
	pausedCursor           *enginepkg.ExecutionCursor
	lease                  enginepkg.RunLease
	leaseReleaseOnce       sync.Once
}

func (h *runHandle) shutdownExtensions(ctx context.Context) {
	if h.extensionsEnabled && h.engine.cfg.ExtensionHost != nil {
		_ = h.engine.cfg.ExtensionHost.Shutdown(ctx)
	}
	h.leaseReleaseOnce.Do(func() {
		if h.lease != nil {
			_ = h.lease.Release()
		}
	})
}

func (h *runHandle) unlockAndDrainCallbacks() {
	h.mu.Unlock()
	if h.callbacksArmed.Load() {
		h.drainCallbacks()
	}
}

func (h *runHandle) enqueueCallback(event enginepkg.Event) {
	if h.onEvent == nil && h.engine.cfg.OnEvent == nil {
		return
	}
	h.callbackMu.Lock()
	publication := &callbackPublication{event: event, done: make(chan struct{})}
	if h.callbackPending == nil {
		h.callbackPending = make(map[string]*callbackPublication)
	}
	h.callbackPending[event.EventID] = publication
	h.callbackQueue = append(h.callbackQueue, publication)
	h.lastCallback = publication
	h.callbackMu.Unlock()
}

func (h *runHandle) drainCallbacks() {
	h.drainCallbacksThrough(nil)
}

func (h *runHandle) drainCallbacksThrough(stop *callbackPublication) {
	h.callbackMu.Lock()
	if stop != nil {
		select {
		case <-stop.done:
			h.callbackMu.Unlock()
			return
		default:
		}
	}
	if h.callbacksRunning {
		h.callbackMu.Unlock()
		return
	}
	h.callbacksRunning = true
	h.callbackIdle = make(chan struct{})
	h.callbackMu.Unlock()
	defer func() {
		if recovered := recover(); recovered != nil {
			h.callbackMu.Lock()
			h.callbacksRunning = false
			close(h.callbackIdle)
			h.callbackMu.Unlock()
			panic(recovered)
		}
	}()

	for {
		h.callbackMu.Lock()
		if len(h.callbackQueue) == 0 {
			h.callbacksRunning = false
			close(h.callbackIdle)
			h.callbackMu.Unlock()
			return
		}
		publication := h.callbackQueue[0]
		event := publication.event
		h.callbackQueue[0] = nil
		h.callbackQueue = h.callbackQueue[1:]
		h.callbackMu.Unlock()

		if h.onEvent != nil {
			h.onEvent(event)
		}
		if h.engine.cfg.OnEvent != nil {
			h.engine.cfg.OnEvent(event)
		}
		h.callbackMu.Lock()
		close(publication.done)
		delete(h.callbackPending, event.EventID)
		if publication == stop {
			h.callbacksRunning = false
			close(h.callbackIdle)
			h.callbackMu.Unlock()
			return
		}
		h.callbackMu.Unlock()
	}
}

func (h *runHandle) ConfigureStartup(ctx context.Context, vars map[string]string) error {
	h.executionMu.Lock()
	defer h.executionMu.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.store == nil {
		return errors.New("engine: durable store is required for startup configuration")
	}
	if h.done.Load() || h.started.Load() || h.run.Status != enginepkg.RunStatusPending || h.run.CurrentStepIndex != -1 {
		return errors.New("engine: startup configuration is closed")
	}
	if len(vars) > 64 {
		return errors.New("engine: startup configuration exceeds 64 fields")
	}
	for name, value := range vars {
		declaration := h.run.Plan.Inputs[name]
		if name == "" || len([]byte(name)) > 256 || len([]byte(value)) > 64*1024 ||
			declaration == nil || declaration.Type != "secret" {
			return errors.New("engine: startup configuration contains an invalid private input")
		}
		if _, exists := h.run.Vars[name]; exists {
			return fmt.Errorf("engine: startup input %q is already configured", name)
		}
	}
	previousVars := cloneAnyMap(h.run.Vars)
	previousProtection := h.debugProtection
	previousProtectAll := h.debugProtectAllVars
	for name, value := range vars {
		h.run.Vars[name] = value
	}
	protection := buildDebugProtection(ctx, h.run.Plan, h.run.Vars)
	var complete bool
	protection, complete = precomputeDebugProtection(ctx, h.engine.cfg.Executors, h.run.Plan, h.run.Vars, protection)
	protectAll := previousProtectAll || h.debugger != nil && !complete
	if protectAll {
		protection = enginepkg.MergeDebugProtection(protection, allDebugVariablesProtection(h.run.Vars))
	}
	h.debugProtection = protection
	h.debugProtectAllVars = protectAll

	nextCheckpoint := h.run.CheckpointSequence + 1
	state := h.stateLocked()
	state.CheckpointSequence = nextCheckpoint
	if err := h.store.SaveState(ctx, state); err != nil {
		h.run.Vars = previousVars
		h.debugProtection = previousProtection
		h.debugProtectAllVars = previousProtectAll
		return fmt.Errorf("%w: %w", enginepkg.ErrCheckpointCommit, err)
	}
	h.run.CheckpointSequence = nextCheckpoint
	return nil
}

// Next advances the run by one step.
func (h *runHandle) Next(ctx context.Context) (result *enginepkg.StepResult, err error) {
	h.executionMu.Lock()
	defer h.executionMu.Unlock()
	h.callbacksArmed.Store(true)
	h.mu.Lock()
	defer h.unlockAndDrainCallbacks()
	defer func() {
		if errors.Is(err, errPublicationCancelled) && h.run.Status == enginepkg.RunStatusCancelled {
			err = nil
		}
		if !h.done.Load() && (errors.Is(err, enginepkg.ErrTraceCommit) || errors.Is(err, enginepkg.ErrCheckpointCommit)) {
			result, err = h.haltCheckpointCommit(context.WithoutCancel(ctx), "", result, err)
		}
	}()

	if h.done.Load() {
		return nil, io.EOF
	}

	// Merge caller context with run context so either cancellation stops the step.
	// Use h.traceCtx as the primary context so the run span is carried for child step spans.
	stepCtx := mergeContexts(h.traceCtx, h.runCtx)
	// Also merge with the caller's cancellation.
	stepCtx = mergeContexts(stepCtx, ctx)
	stepCtx = enginepkg.WithRunID(stepCtx, h.run.ID)
	stepCtx = enginepkg.WithPlanTools(stepCtx, h.run.Plan.Tools)
	_, inheritedFrame := enginepkg.ExecutionFrameBindingFromContext(stepCtx)
	if !inheritedFrame && enginepkg.ExecutionFrameCommitterFromContext(stepCtx) == nil {
		stepCtx = enginepkg.WithExecutionFrameCommitter(stepCtx, h)
	}
	if h.store != nil {
		stepCtx = enginepkg.WithInteractionCommitter(stepCtx, h)
		stepCtx = enginepkg.WithExecutionFrameCommitter(stepCtx, h)
		stepCtx = enginepkg.WithDynamicIncludeResolutionCommitter(stepCtx, h)
		if h.run.Mode != enginepkg.RunModeDryRun && h.run.Mode != enginepkg.RunModeReplay && h.run.Mode != enginepkg.RunModeRouteTest {
			stepCtx = enginepkg.WithDispatchCommitter(stepCtx, h)
		}
	}
	stepCtx = enginepkg.WithInteractionInvocationTracker(stepCtx, h.interactionInvocations)
	// First call: emit plan.validated, then run/started, and transition to running.
	if !h.started.Load() {
		h.run.Status = enginepkg.RunStatusRunning
		h.run.StartedAt = time.Now()
		h.started.Store(true)

		if validation := h.run.Plan.Validation; validation != nil {
			payload := map[string]any{
				"runbook_id":       validation.RunbookID,
				"runbook_hash":     validation.RunbookHash,
				"grammar_versions": validation.GrammarVersions,
				"expression_count": validation.ExpressionCount,
				"validated_at":     validation.ValidatedAt.UTC().Format(time.RFC3339Nano),
			}
			// AR-ENUM-10 (barbara-enum-mvp-implementation-gate.md R3):
			// emit every enum-constrained declaration's contract
			// metadata exactly once, in this existing plan/validation
			// record -- never repeated in any per-step (tool/invoked,
			// tool/completed, step/started, step/completed) event
			// payload. C1 redaction is honoured: a redacted declaration
			// carries "<redacted>" plus member_count, never Members.
			if len(validation.EnumConstraints) > 0 {
				payload["enum_constraints"] = enumConstraintsTracePayload(validation.EnumConstraints)
			}
			h.emitEventLocked(stepCtx, trace.EventKindPlanValidated, payload)
		}

		initialVars := make(map[string]any, len(h.run.Vars))
		for k, v := range h.run.Vars {
			initialVars[k] = v
		}
		dependencyLock, lockErr := replaypkg.DependencyLockForPlan(h.run.Plan)
		if lockErr != nil {
			return h.failRun(stepCtx, "", fmt.Errorf("engine: build replay dependency lock: %w", lockErr))
		}
		h.emitEventLocked(stepCtx, trace.EventKindRunStarted, map[string]any{
			"run_id":              h.run.ID,
			"runbook_path":        h.run.Plan.RunbookPath,
			"plan_hash":           dependencyLock.PlanHash,
			"graph_hash":          dependencyLock.GraphHash,
			"catalog_digest":      dependencyLock.CatalogDigest,
			"package_lock_digest": dependencyLock.PackageLockDigest,
			"tool_digest":         dependencyLock.ToolDigest,
			"profile_digest":      dependencyLock.ProfileDigest,
			"actor":               h.run.Actor,
			"mode":                string(h.run.Mode),
			"client":              h.run.Client,
			"vars":                initialVars,
		})

		// PKG-W004: emit one governance/unclassified_action event per tool
		// action that has no declared classification, nudging migration toward
		// explicit read-only/mutating/destructive labeling.
		if h.run.Plan.Tools != nil {
			for toolName, toolDef := range h.run.Plan.Tools {
				if toolDef == nil {
					continue
				}
				for actionName, action := range toolDef.Actions {
					if action == nil || action.Classification != nil {
						continue
					}
					h.emitEventLocked(stepCtx, trace.EventKind("governance/unclassified_action"), map[string]any{
						"tool":    toolName,
						"action":  actionName,
						"warning": "PKG-W004",
						"message": fmt.Sprintf("tool %q action %q has no declared classification; consider adding classification: read-only|mutating|destructive", toolName, actionName),
					})
				}
			}
		}
	}
	if h.traceErr != nil {
		return h.haltCheckpointCommit(stepCtx, "", nil, h.traceErr)
	}
	if err := h.initializeRootBindings(stepCtx); err != nil {
		return h.failRun(stepCtx, "", err)
	}
	stepCtx = enginepkg.WithBindingScope(stepCtx, h.run.BindingScope)

	// Check if run was cancelled between steps.
	if h.done.Load() {
		return nil, io.EOF
	}

	// Advance to next step, skipping sub-steps (Depth > 0).
	// Sub-steps belong to their parent container (iterate, branch, parallel)
	// and are executed via SubStepRunner with the correct loop variable scope.
	// Executing them here would double-execute them without loop
	h.run.CurrentStepIndex++
	for h.run.CurrentStepIndex < len(h.run.Plan.Steps) && h.run.Plan.Steps[h.run.CurrentStepIndex].Depth > 0 {
		h.run.CurrentStepIndex++
	}
	if h.run.CurrentStepIndex >= len(h.run.Plan.Steps) {
		return h.completeRun(stepCtx)
	}

	step := h.run.Plan.Steps[h.run.CurrentStepIndex]
	if restored := h.run.StepResults[step.ID]; restored != nil && h.hasActiveCompensationFrame(step.ID) {
		return h.settleStep(stepCtx, step, cloneStepResult(restored), nil)
	}
	return h.executeStep(stepCtx, step)
}

// executeStep runs a single step and returns the result.
// The caller must hold h.mu.
func (h *runHandle) executeStep(ctx context.Context, step enginepkg.ResolvedStep) (*enginepkg.StepResult, error) {
	snapshotDigest := ""
	if parent := enginepkg.ToolPresentationFromContext(ctx); parent != nil {
		snapshotDigest = parent.SnapshotDigest
	}
	if store, ok := h.store.(enginepkg.DurableRunStore); ok {
		snapshotDigest, _ = store.PlanDigest(h.run.ID)
	}
	if snapshotDigest == "" && step.Kind == "tool" {
		if snapshot, err := plansnapshot.FromExecutionPlan(h.run.Plan); err == nil {
			snapshotDigest = snapshot.SnapshotDigest
		}
	}
	ctx = enginepkg.WithToolPresentationCapture(ctx, snapshotDigest)
	boundary := enginepkg.DispatchExecutionBoundary{
		QualifiedNodeID: enginepkg.DebugNodeID(h.debugCallPath, step.ID),
		CallPath:        append([]enginepkg.DebugCallFrame(nil), h.debugCallPath...),
		StepID:          step.ID, StepIndex: h.run.CurrentStepIndex,
		Phase:      enginepkg.ExecutionPhaseExecute,
		Invocation: h.debugInvocations.Next(h.debugCallPath, step.ID), RetryAttempt: 1, OccurrenceSequence: 1,
	}
	if binding, ok := enginepkg.ExecutionFrameBindingFromContext(ctx); ok {
		boundary.FrameID = binding.FrameID
		boundary.FrameStepIndex = binding.StepOffset + h.run.CurrentStepIndex
	}
	ctx = enginepkg.WithDispatchExecutionBoundary(ctx, boundary)
	var executionLocation enginepkg.DebugLocation
	if h.debugger != nil || h.routeTest != nil {
		executionLocation = h.nextDebugLocation(step, boundary.Invocation)
	}
	if h.routeTest != nil {
		decision, err := h.routeTest.BeforeStep(ctx, executionLocation, step)
		if err != nil {
			return h.failRun(ctx, step.ID, err)
		}
		if decision == enginepkg.RouteTestTargetReached {
			return h.pauseAtRouteTarget(ctx, executionLocation)
		}
		if decision != "" && decision != enginepkg.RouteTestContinue {
			return h.failRun(ctx, step.ID, fmt.Errorf("route test: unsupported decision %q", decision))
		}
	}
	stepProtection := h.debugProtectionBeforeStep(ctx, step, h.debugger != nil)
	// Dispatch based on step kind for built-in composite steps.
	switch step.Kind {
	case "parallel":
		branches, join, err := h.parallelBranches(step)
		if err != nil {
			return h.failRun(ctx, step.ID, err)
		}
		result, err := h.executeParallel(ctx, step, branches, join)
		if err != nil {
			if enginepkg.IsReplayBoundaryError(err) {
				return h.failRun(ctx, step.ID, err)
			}
			if enginepkg.IsRouteTestBoundaryError(err) {
				return h.failRun(ctx, step.ID, err)
			}
			if errors.Is(err, enginepkg.ErrIndeterminate) {
				return h.haltIndeterminate(ctx, step, err, 1, nil)
			}
			if errors.Is(err, enginepkg.ErrTraceCommit) || errors.Is(err, enginepkg.ErrCheckpointCommit) {
				return h.haltCheckpointCommit(ctx, step.ID, result, err)
			}
			return result, err
		}
		return h.settleStep(ctx, step, result, nil)
	case "wait_for_event":
		filter, timeout, onTimeout, err := h.waitEventConfig(step)
		if err != nil {
			return h.failRun(ctx, step.ID, err)
		}
		result, err := h.executeWaitForEvent(ctx, step, filter, timeout)
		if err != nil {
			if errors.Is(err, enginepkg.ErrCheckpointCommit) || errors.Is(err, enginepkg.ErrTraceCommit) {
				return h.haltCheckpointCommit(ctx, step.ID, result, err)
			}
			return result, err
		}
		if err := h.stagePreparedDispatchSettled(step.ID, result, nil); err != nil {
			return h.haltCheckpointCommit(ctx, step.ID, result, err)
		}
		if result != nil && enginepkg.IsReplayBoundaryError(result.Error) {
			return h.failRun(ctx, step.ID, result.Error)
		}
		if result.Status == enginepkg.StepStatusFailed && errors.Is(result.Error, eventbus.ErrEventTimeout) {
			step, err = applyWaitTimeoutRouting(step, onTimeout)
			if err != nil {
				return h.failRun(ctx, step.ID, err)
			}
		}
		return h.settleStep(ctx, step, result, nil)
	}

	// Start step-level OTel span. The span inherits the run span as parent via ctx.
	spanCtx, stepSpan := h.engine.tracer().Start(ctx, "yawr.step."+step.Kind,
		otelPkg.WithAttributes(
			otelPkg.Attribute{Key: otelPkg.AttrStepID, Value: step.ID},
			otelPkg.Attribute{Key: otelPkg.AttrStepKind, Value: step.Kind},
		),
	)
	defer stepSpan.End()

	// Standard step: emit step/started, look up executor, run.
	stepName := step.Name
	if stepName == "" {
		stepName = step.ID
	}
	// Honor step-level pre-execution delay (the runbook's `delay:` field).
	// Emitted BEFORE step/started so the runtime overlay shows the node as
	// `delaying` for the entire pause window, never briefly flickering as
	// `running`. The mutex is released around the sleep so the run isn't
	// blocked, and context cancellation is honored.
	if step.Delay != "" {
		if d, parseErr := time.ParseDuration(step.Delay); parseErr == nil && d > 0 {
			h.emitEventLocked(ctx, trace.EventKindStepDelaying, traceOccurrencePayload(ctx, step.ID, map[string]any{
				"step_id": step.ID,
				"delay":   d.String(),
			}))
			if h.traceErr != nil {
				return h.haltCheckpointCommit(ctx, step.ID, nil, h.traceErr)
			}
			if err := h.publishBoundaryLocked(ctx); err != nil {
				return nil, err
			}
			h.mu.Unlock()
			timer := time.NewTimer(d)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
			}
			h.mu.Lock()
			if err := h.executionErrorLocked(ctx); err != nil {
				return nil, err
			}
		}
	}
	startedPayload := map[string]any{
		"step_id":       step.ID,
		"name":          stepName,
		"kind":          step.Kind,
		"nest_depth":    step.NestDepth,
		"display_order": step.DisplayOrder,
	}
	// Structural metadata — emitted only when set, to keep payloads compact
	// and to avoid implying parent linkage on top-level steps.
	if step.ParentID != "" {
		startedPayload["parent_step_id"] = step.ParentID
	}
	if step.ParentKind != "" {
		startedPayload["parent_kind"] = step.ParentKind
	}
	if step.IncludeAlias != "" {
		startedPayload["include_alias"] = step.IncludeAlias
	}
	if step.BranchLabel != "" {
		startedPayload["branch_label"] = step.BranchLabel
	}
	h.emitEventLocked(ctx, trace.EventKindStepStarted, traceOccurrencePayload(ctx, step.ID, startedPayload))
	if h.traceErr != nil {
		return h.haltCheckpointCommit(ctx, step.ID, nil, h.traceErr)
	}
	if err := h.publishBoundaryLocked(ctx); err != nil {
		return nil, err
	}

	startedAt := time.Now()
	debugLocation := executionLocation
	if h.debugger != nil {
		snapshot := h.sanitizedDebugSnapshotWithProtection(enginepkg.DebugPhaseBefore, debugLocation, nil, stepProtection)
		snapshot.CanStepInto = debugCanStepInto(step)
		decision, debugErr := h.debugDecision(ctx, snapshot)
		if debugErr != nil {
			if errors.Is(debugErr, errDebugStopped) {
				return nil, errors.Join(context.Canceled, enginepkg.ErrDebugStopped)
			}
			if h.runCtx.Err() != nil || errors.Is(debugErr, context.Canceled) || errors.Is(debugErr, context.DeadlineExceeded) {
				h.cancelDebugPauseRunLocked(context.WithoutCancel(ctx), debugErr)
				return nil, debugErr
			}
			return h.failRun(ctx, step.ID, debugErr)
		}
		if decisionErr := validateDebugDecision(snapshot, decision, h.debugProtectAllVars); decisionErr != nil {
			return h.failRun(ctx, step.ID, decisionErr)
		}
		h.applyDebugVars(decision.Vars)
		if len(decision.Vars) > 0 {
			h.emitDebugOverrideLocked(ctx, step.ID, snapshot, nil, false)
		}
	}

	// ── GOVERNANCE PRE-FLIGHT (Phase 4) ──────────────────────────────
	// Select the evaluator: the per-run evaluator (built from plan.GovernanceSource
	// in Start/Resume) takes precedence; the engine-global evaluator (wired in
	// production via BuildEngineConfig/buildEngineConfig) serves as fallback so
	// that an externally-constructed engine with a wired evaluator is also covered.
	govEvaluator := h.governanceEvaluator
	if govEvaluator == nil {
		govEvaluator = h.engine.cfg.GovernanceEvaluator
	}
	if govEvaluator != nil {
		stepInfo := governance.StepInfo{
			ID:      step.ID,
			Kind:    step.Kind,
			Command: extractCommand(step),
			EnvVars: extractEnvVars(step),
		}
		// For tool steps, compose per-tool requires-approval governance.
		// This satisfies the binding constraint: both runbook-level AND
		// per-tool governance must feed the policy (monotone OR: neither
		// can suppress the other). The runbook-level flag lives in the
		// evaluator's policy (built from plan.GovernanceSource above).
		// The tool-level flag is injected here via StepInfo so the
		// evaluator can OR them — without touching the tool transport.
		if step.Kind == "tool" {
			if toolSpec, ok := step.Spec.(*schema.ToolCallSpec); ok && toolSpec != nil {
				if h.run.Plan != nil {
					if toolDef, found := h.run.Plan.Tools[toolSpec.Tool.Name]; found && toolDef != nil {
						if toolDef.Governance != nil && toolDef.Governance.RequiresApproval != nil {
							stepInfo.ToolRequiresApproval = *toolDef.Governance.RequiresApproval
						}
						// Carry the full tri-state and classification for the ProfileEvaluator.
						if toolDef.Governance != nil {
							stepInfo.ToolApprovalTriState = toolDef.Governance.RequiresApproval
						}
						actionName := toolSpec.Tool.Action
						if actionName == "" {
							actionName = "run"
						}
						if action, ok := toolDef.Actions[actionName]; ok && action != nil {
							stepInfo.ToolClassification = action.Classification
						}
					}
				}
			}
		}

		evalResult, evalErr := govEvaluator.Evaluate(ctx, stepInfo)
		if evalErr != nil {
			return h.failRun(ctx, step.ID, fmt.Errorf("governance evaluation failed: %w", evalErr))
		}

		// Emit governance/command_checked trace event.
		h.emitEventLocked(ctx, trace.EventKindGovernanceCommandChecked, map[string]any{
			"step_id":       step.ID,
			"command":       stepInfo.Command,
			"allowed":       evalResult.Allowed,
			"denied":        evalResult.Denied,
			"matched_rules": evalResult.MatchedRules,
			"governance":    evalResult.Evidence,
		})
		if h.traceErr != nil {
			return h.haltCheckpointCommit(ctx, step.ID, nil, h.traceErr)
		}

		// DENIED: return immediately, do NOT call executor.
		if evalResult.Denied {
			result := &enginepkg.StepResult{
				StepID:      step.ID,
				Status:      enginepkg.StepStatusDenied,
				Outcome:     enginepkg.StepOutcomeDenied,
				StartedAt:   startedAt,
				CompletedAt: time.Now(),
				DurationMs:  time.Since(startedAt).Milliseconds(),
				Error: &GovernanceDeniedError{
					StepID: step.ID,
					Reason: evalResult.DenyReason,
				},
				Output: map[string]any{
					"governance_evidence": evalResult.Evidence,
				},
			}
			return h.settleStep(ctx, step, result, stepSpan)
		}

		// APPROVAL REQUIRED: call the gate, record the result.
		if evalResult.RequiresApproval {
			h.emitEventLocked(ctx, trace.EventKindGovernanceApprovalRequested, traceOccurrencePayload(ctx, step.ID, map[string]any{
				"step_id": step.ID,
				"reason":  "policy requires approval",
			}))
			if h.traceErr != nil {
				return h.haltCheckpointCommit(ctx, step.ID, nil, h.traceErr)
			}

			if h.engine.cfg.ApprovalGate == nil {
				return h.failRun(ctx, step.ID, fmt.Errorf("step %s requires approval but no ApprovalGate is configured", step.ID))
			}

			// Release mutex while waiting for human approval.
			if err := h.publishBoundaryLocked(ctx); err != nil {
				return nil, err
			}
			h.mu.Unlock()
			record, approvalErr := h.engine.cfg.ApprovalGate.RequestApproval(ctx, step.ID, "governance policy requires approval")
			h.mu.Lock()
			if err := h.executionErrorLocked(ctx); err != nil {
				return nil, err
			}

			if approvalErr != nil {
				return h.failRun(ctx, step.ID, fmt.Errorf("approval denied for step %s: %w", step.ID, approvalErr))
			}

			h.emitEventLocked(ctx, trace.EventKindGovernanceApprovalReceived, traceOccurrencePayload(ctx, step.ID, map[string]any{
				"step_id":  step.ID,
				"approver": record.Approver,
				"token":    record.Token,
			}))
			if h.traceErr != nil {
				return h.haltCheckpointCommit(ctx, step.ID, nil, h.traceErr)
			}

			// Update evidence with the approval record.
			evalResult.Evidence.ApprovalRecord = &record
		}

		// Replace step env vars with filtered set.
		applyFilteredEnvVars(step, evalResult.FilteredEnvVars)
	}
	// ── END GOVERNANCE PRE-FLIGHT ────────────────────────────────────

	// Look up executor.
	exec := h.engine.cfg.Executors.Lookup(step.Kind)
	if exec == nil {
		result := &enginepkg.StepResult{
			StepID:      step.ID,
			Status:      enginepkg.StepStatusFailed,
			Outcome:     enginepkg.StepOutcomeFailed,
			StartedAt:   startedAt,
			CompletedAt: time.Now(),
			DurationMs:  time.Since(startedAt).Milliseconds(),
			Error:       &ExecutorNotFoundError{Kind: step.Kind},
		}
		return h.settleStep(ctx, step, result, stepSpan)
	}

	// Execute the step (release mutex so long-running steps don't block signals).
	// Pass spanCtx so executors and tools can create child spans.
	// Attach an EventEmitter so executors (e.g. iterate) can emit lifecycle
	// events that aren't tied to a single ResolvedStep. The emitter
	// re-acquires the engine handle's mutex around emitEventLocked so
	// sequence numbers stay monotonic.
	emitterCtx := executor.WithEventEmitter(spanCtx, func(kind string, payload map[string]any) {
		h.mu.Lock()
		h.emitEventLocked(spanCtx, trace.EventKind(kind), payload)
		h.unlockAndDrainCallbacks()
	})
	emitterCtx = enginepkg.WithRunID(emitterCtx, h.run.ID)
	emitterCtx = enginepkg.WithEventForwarder(emitterCtx, h.forwardSubEngineEvent)
	emitterCtx = context.WithValue(emitterCtx, publicationParentKey{}, h)
	emitterCtx = enginepkg.WithDebugController(emitterCtx, h.debugger)
	emitterCtx = enginepkg.WithRouteTestController(emitterCtx, h.routeTest)
	emitterCtx = enginepkg.WithDebugInvocationTracker(emitterCtx, h.debugInvocations)
	emitterCtx = internaldebugprotect.WithProtection(emitterCtx, h.debugProtection)
	emitterCtx = internaldebugprotect.WithProtectAll(emitterCtx, h.debugProtectAllVars)
	emitterCtx = internaldebugprotect.WithSink(emitterCtx, h.mergePublishedDebugProtection)
	emitterCtx = internaldebugprotect.WithHandoffValueValidator(
		emitterCtx, h.effectiveDebugProtection(h.debugProtection),
	)
	if err := h.publishBoundaryLocked(ctx); err != nil {
		return nil, err
	}
	h.mu.Unlock()
	result, execErr := exec.Execute(emitterCtx, step, h.run.Vars)
	h.mu.Lock()
	if enginepkg.IsReplayBoundaryError(execErr) {
		return h.failRun(ctx, step.ID, execErr)
	}
	if request, ok := enginepkg.HandoffRequestFromError(execErr); ok {
		return h.pauseForHandoff(ctx, step, request)
	}
	if errors.Is(execErr, enginepkg.ErrRouteTestTargetReached) {
		location, ok := enginepkg.RouteTestTargetLocation(execErr)
		if !ok {
			return h.failRun(ctx, step.ID, errors.New("route test: nested target has no exact location"))
		}
		return h.pauseAtRouteTarget(ctx, location)
	}
	if errors.Is(execErr, enginepkg.ErrDebugStopped) {
		h.stopDebugRunLocked(context.WithoutCancel(ctx))
		return nil, context.Canceled
	}
	if errors.Is(execErr, enginepkg.ErrIndeterminate) {
		return h.haltIndeterminate(ctx, step, execErr, 1, resolveStepClassification(step, h.run.Plan))
	}

	if h.runCtx.Err() != nil {
		if staged := h.stagePreparedDispatchIndeterminate(step.ID); staged {
			classification := resolveStepClassification(step, h.run.Plan)
			return h.haltIndeterminate(ctx, step, h.runCtx.Err(), 1, classification)
		}
		return nil, h.runCtx.Err()
	}
	if result != nil && enginepkg.IsReplayBoundaryError(result.Error) {
		return h.failRun(ctx, step.ID, result.Error)
	}
	durabilityErr := execErr
	if durabilityErr == nil && result != nil {
		durabilityErr = result.Error
	}
	if errors.Is(durabilityErr, enginepkg.ErrCheckpointCommit) || errors.Is(durabilityErr, enginepkg.ErrTraceCommit) {
		if result == nil {
			result = &enginepkg.StepResult{
				StepID: step.ID, Status: enginepkg.StepStatusFailed, Outcome: enginepkg.StepOutcomeFailed,
				StartedAt: startedAt, CompletedAt: time.Now(), Error: durabilityErr,
			}
		}
		h.run.StepResults[step.ID] = result
		return h.haltCheckpointCommit(ctx, step.ID, result, durabilityErr)
	}

	if execErr != nil {
		if enginepkg.IsRouteTestBoundaryError(execErr) {
			return h.failRun(ctx, step.ID, execErr)
		}
		if _, framed := enginepkg.ExecutionFrameBindingFromContext(ctx); framed {
			if result == nil {
				result = &enginepkg.StepResult{
					StepID: step.ID, Status: enginepkg.StepStatusFailed, Outcome: enginepkg.StepOutcomeFailed,
					StartedAt: startedAt, CompletedAt: time.Now(), Error: execErr,
				}
			}
			return h.settleStep(ctx, step, result, stepSpan)
		}
		// Infrastructure error: apply classification-aware timeout handling.
		// Any error after a mutating, destructive, or unspecified dispatch intent
		// was committed is indeterminate unless the provider returned a definitive
		// result. The authored step kind cannot safely identify every provider.
		stepSpan.RecordError(execErr)
		stepSpan.SetStatus(otelPkg.StatusError, execErr.Error())
		if _, dispatch := h.preparedDispatchForStep(step.ID); dispatch != nil {
			var classification *string
			if dispatch.Classification != "" {
				classification = &dispatch.Classification
			}
			if requiresIndeterminate(classification) {
				h.stagePreparedDispatchIndeterminate(step.ID)
				return h.haltIndeterminate(ctx, step, execErr, 1, classification)
			}
		}
		if step.Kind == "tool" && isTransportLoss(execErr) {
			classification := resolveStepClassification(step, h.run.Plan)
			if requiresIndeterminate(classification) {
				h.stagePreparedDispatchIndeterminate(step.ID)
				return h.haltIndeterminate(ctx, step, execErr, 1, classification)
			}
			// read-only: idempotent actions are retry-eligible (future work);
			// without retry infrastructure, fall through to failRun.
		}
		if err := h.stagePreparedDispatchSettled(step.ID, result, execErr); err != nil {
			return h.haltCheckpointCommit(ctx, step.ID, result, err)
		}
		return h.failRun(ctx, step.ID, execErr)
	}

	// Also intercept step-level transport loss (executor returned StepStatusFailed
	// with a transport-loss error rather than an execErr) for tool steps.
	if step.Kind == "tool" && result.Status == enginepkg.StepStatusFailed &&
		result.Error != nil && isTransportLoss(result.Error) {
		classification := resolveStepClassification(step, h.run.Plan)
		if requiresIndeterminate(classification) {
			h.stagePreparedDispatchIndeterminate(step.ID)
			return h.haltIndeterminate(ctx, step, result.Error, 1, classification)
		}
	}
	if err := h.stagePreparedDispatchSettled(step.ID, result, nil); err != nil {
		return h.haltCheckpointCommit(ctx, step.ID, result, err)
	}

	if h.debugger != nil {
		actualResult := result
		snapshotProtection, protectActual := h.debugProtectionForActual(step, actualResult)
		h.debugProtection = enginepkg.MergeDebugProtection(h.debugProtection, snapshotProtection)
		snapshot := h.sanitizedDebugSnapshotWithOptions(enginepkg.DebugPhaseAfter, debugLocation, actualResult, snapshotProtection, protectActual)
		decision, debugErr := h.debugDecision(ctx, snapshot)
		if debugErr != nil {
			if errors.Is(debugErr, errDebugStopped) {
				return nil, errors.Join(context.Canceled, enginepkg.ErrDebugStopped)
			}
			if h.runCtx.Err() != nil || errors.Is(debugErr, context.Canceled) || errors.Is(debugErr, context.DeadlineExceeded) {
				h.cancelDebugPauseRunLocked(context.WithoutCancel(ctx), debugErr)
				return nil, debugErr
			}
			return h.failRun(ctx, step.ID, debugErr)
		}
		if decisionErr := validateDebugDecision(snapshot, decision, h.debugProtectAllVars); decisionErr != nil {
			return h.failRun(ctx, step.ID, decisionErr)
		}
		stagedResult := result
		if decision.Result != nil {
			var overrideErr error
			stagedResult, overrideErr = applyDebugResultOverride(result, decision.Result)
			if overrideErr != nil {
				return h.failRun(ctx, step.ID, overrideErr)
			}
		}
		h.applyDebugVars(decision.Vars)
		result = stagedResult
		if len(decision.Vars) > 0 || decision.Result != nil {
			h.emitDebugOverrideLocked(ctx, step.ID, snapshot, result, protectActual)
		}
	}

	// ── EMIT STEP OUTPUT (for CLI and display steps) ─────────────────
	// Emit step/output event for stdout/stderr from CLI and tool steps.
	// Note: This is a simplified implementation that emits full stdout at completion.
	// Full line-by-line streaming would require platform.Exec to support streaming.
	if (step.Kind == "cli" || step.Kind == "tool") && result.Output != nil {
		if stdout, ok := result.Output["stdout"].(string); ok && stdout != "" {
			h.emitEventLocked(ctx, trace.EventKindStepOutput, map[string]any{
				"step_id":  step.ID,
				"stream":   "stdout",
				"line":     stdout,
				"sequence": h.run.Sequence,
			})
		}
		if stderr, ok := result.Output["stderr"].(string); ok && stderr != "" {
			h.emitEventLocked(ctx, trace.EventKindStepOutput, map[string]any{
				"step_id":  step.ID,
				"stream":   "stderr",
				"line":     stderr,
				"sequence": h.run.Sequence,
			})
		}
	}
	// Emit step/output for display steps so TUI output panel shows the content.
	if step.Kind == "display" && result.Output != nil {
		if content, ok := result.Output["content"].(string); ok && content != "" {
			h.emitEventLocked(ctx, trace.EventKindStepOutput, map[string]any{
				"step_id":  step.ID,
				"stream":   "stdout",
				"line":     content,
				"sequence": h.run.Sequence,
			})
		}
	}
	// ── END STEP OUTPUT ───────────────────────────────────────────────

	// Resolve validated GCP captures with access to prior step outputs. A
	// failed result (e.g. the tool executor's own ENUM-008/ENUM-009 check)
	// has no reliable Output to capture against -- attempting it anyway
	// would mask the real underlying failure behind an unrelated
	// GCP-RESOLVE-002 error and abort the whole run via failRun instead of
	// surfacing the original one.
	// An include gate returns without a capture continuation. Respect that
	// same terminal decision when resolving validated GCP paths as well.
	if result.Status != enginepkg.StepStatusFailed && !(step.Kind == "include" && isTerminalOutput(result)) &&
		h.run.Plan != nil && h.run.Plan.Validation != nil && hasValidatedGCPCapture(h.run.Plan.Validation, step) {
		captures, capErr := capture.New(h.run.Vars, h.run.StepResults, nil).CaptureStep(h.run.Plan.Validation, step, result)
		if capErr != nil {
			stepSpan.RecordError(capErr)
			stepSpan.SetStatus(otelPkg.StatusError, capErr.Error())
			return h.failRun(ctx, step.ID, capErr)
		}
		for name, val := range capture.ToAnyMap(captures) {
			result.Vars[name] = val
		}
	}

	// ── POST-EXECUTION REDACTION (Phase 4) ────────────────────────────
	if govEvaluator != nil && h.run.Plan.Governance != nil {
		patterns := h.run.Plan.Governance.RedactionPatterns()
		if len(patterns) > 0 {
			redactor, err := internalGov.NewRedactor(patterns)
			if err == nil {
				redactionCount := 0
				if result.Output != nil {
					redactionCount += redactor.RedactMap(result.Output)
				}
				if result.Vars != nil {
					redactionCount += redactor.RedactMap(result.Vars)
				}
				if redactionCount > 0 {
					h.emitEventLocked(ctx, trace.EventKindGovernanceRedactionApplied, map[string]any{
						"step_id":    step.ID,
						"rule_count": redactionCount,
					})
				}
			}
		}
	}
	// ── END REDACTION ─────────────────────────────────────────────────

	// ── EVIDENCE COLLECTION (Phase 11) ──────────────────────────────
	if h.engine.cfg.EvidenceHook != nil && result.Status == enginepkg.StepStatusCompleted {
		runDir := h.runDir()
		records, evErr := h.engine.cfg.EvidenceHook.Collect(ctx, step, result, runDir)
		if evErr == nil && len(records) > 0 {
			result.Evidence = records
		}
	}
	// ── END EVIDENCE COLLECTION ─────────────────────────────────────

	// Populate timing if executor didn't set it.
	if result.StartedAt.IsZero() {
		result.StartedAt = startedAt
	}
	if result.CompletedAt.IsZero() {
		result.CompletedAt = time.Now()
	}
	if result.DurationMs == 0 {
		result.DurationMs = result.CompletedAt.Sub(result.StartedAt).Milliseconds()
	}

	return h.settleStep(ctx, step, result, stepSpan)
}

func (h *runHandle) settleStep(ctx context.Context, step enginepkg.ResolvedStep, result *enginepkg.StepResult, stepSpan otelPkg.Span) (*enginepkg.StepResult, error) {
	// An invoked provider's result must reach the authoritative frame commit
	// even when its caller expires. Explicit run cancellation/fail-stop still
	// wins; this boundary never starts another provider.
	if err := h.publishBoundaryLocked(context.WithoutCancel(ctx)); err != nil {
		return result, err
	}
	if result.Status == enginepkg.StepStatusCompleted {
		if result.PublicOutputs != nil {
			body, err := json.Marshal(result.PublicOutputs)
			if err == nil {
				err = internaldebugprotect.ValidateHandoffJSON(enginepkg.MergeDebugProtection(h.effectiveDebugProtection(h.debugProtection), internaldebugprotect.ProtectionFromContext(ctx)), body)
			}
			if err != nil {
				result.Status, result.Outcome, result.Error = enginepkg.StepStatusFailed, enginepkg.StepOutcomeFailed, errors.New("outputs: canonical public outputs contain protected content")
				result.Vars, result.Results, result.PublicOutputs = nil, nil, nil
			}
		}
		if err := executor.ValidateBindingWrites(h.run.BindingScope, result.Vars); err != nil {
			result.Status, result.Outcome, result.Error = enginepkg.StepStatusFailed, enginepkg.StepOutcomeFailed, err
			result.Vars, result.Results, result.PublicOutputs = nil, nil, nil
		}
	}
	_, childFrame := enginepkg.ExecutionFrameBindingFromContext(ctx)
	if result.Results != nil && !childFrame {
		if step.Kind != "results" || h.run.CurrentStepIndex != lastTopLevelStep(h.run.Plan) {
			result.Status, result.Outcome, result.Error = enginepkg.StepStatusFailed, enginepkg.StepOutcomeFailed, errors.New("results: publication must be the last top-level operation")
			result.Results = nil
		}
	}
	if result.Results != nil && !childFrame {
		if err := h.preparePublication(ctx, result, nil); err != nil {
			result.Status, result.Outcome, result.Error = enginepkg.StepStatusFailed, enginepkg.StepOutcomeFailed, err
			result.Results = nil
		}
	}
	attachTypedDiagnostic(ctx, result)
	if result.Status != enginepkg.StepStatusCompleted {
		result.Results, result.PublicOutputs = nil, nil
		if executor.ValidateBindingWrites(h.run.BindingScope, result.Vars) != nil {
			result.Vars = nil
		}
	}
	h.clearInteractionsForOwner(step.ID)
	if h.run.Status == enginepkg.RunStatusWaiting {
		h.run.Status = enginepkg.RunStatusRunning
	}
	h.run.StepResults[step.ID] = result
	for key, value := range result.Vars {
		h.run.Vars[key] = value
	}
	frameResultCommitted := false
	if committer := enginepkg.ExecutionFrameCommitterFromContext(ctx); committer != nil {
		if binding, ok := enginepkg.ExecutionFrameBindingFromContext(ctx); ok {
			frame, err := committer.CommitExecutionFrameStep(context.WithoutCancel(ctx), enginepkg.ExecutionFrameStepCommit{
				FrameID: binding.FrameID, StepIndex: binding.StepOffset + h.run.CurrentStepIndex, StepKind: step.Kind,
				Result: result, WorkingVars: h.run.Vars,
			})
			if committed := frame.Results[strconv.Itoa(binding.StepOffset+h.run.CurrentStepIndex)]; committed != nil {
				result = cloneStepResult(committed)
				h.run.StepResults[step.ID] = result
			}
			if errors.Is(err, enginepkg.ErrIndeterminate) {
				return result, err
			}
			if err != nil {
				return h.haltCheckpointCommit(ctx, step.ID, result, err)
			}
			frameResultCommitted = true
			if result.Results != nil {
				h.run.Results = cloneResults(result.Results)
			}
		}
	}
	h.finishExecutionFramesForStep(step.ID, result.Status)
	h.finishDynamicIncludeResolutions("", enginepkg.DebugNodeID(h.debugCallPath, step.ID), result.Status)
	if h.debugger != nil {
		h.debugProtection, _ = extendProtectionWithDebugVars(h.debugProtection, result.Vars, h.debugProtectAllVars)
	}
	settledStatus := result.Status
	var errStr string
	if result.Error != nil {
		errStr = result.Error.Error()
	}
	stopRun := false
	if settledStatus == enginepkg.StepStatusFailed {
		if err := h.executeCompensations(ctx); err != nil {
			if request, ok := enginepkg.HandoffRequestFromError(err); ok {
				return h.pauseForHandoff(ctx, enginepkg.ResolvedStep{ID: request.StepID, Kind: "handoff"}, request)
			}
			return h.haltCheckpointCommit(ctx, step.ID, result, err)
		}
		switch onError := resolveOnError(step); {
		case onError == "continue":
			h.run.Vars["__error_message"] = errStr
			h.run.Vars["__error_step_id"] = step.ID
		case onError == "stop":
			h.run.Status = enginepkg.RunStatusFailed
			h.run.CompletedAt = time.Now()
			h.run.Error = fmt.Errorf("step %s failed", step.ID)
			h.done.Store(true)
			stopRun = true
		case strings.HasPrefix(onError, "goto:"):
			targetID := strings.TrimPrefix(onError, "goto:")
			targetIndex := -1
			for index, candidate := range h.run.Plan.Steps {
				if candidate.ID == targetID && candidate.Depth == 0 {
					targetIndex = index
					break
				}
			}
			if targetIndex == -1 {
				return h.failRun(ctx, step.ID, fmt.Errorf("on_error goto target %q not found", targetID))
			}
			h.run.Vars["__error_message"] = errStr
			h.run.Vars["__error_step_id"] = step.ID
			h.run.CurrentStepIndex = targetIndex - 1
		default:
			return h.failRun(ctx, step.ID, fmt.Errorf("invalid on_error value: %s", onError))
		}
	}

	if settledStatus == enginepkg.StepStatusWaiting {
		h.run.Status = enginepkg.RunStatusWaiting
		return result, nil
	}
	terminalRun := settledStatus == enginepkg.StepStatusCompleted && (isTerminalOutput(result) || step.Kind == "results" && result.Results != nil)
	if terminalRun {
		if validator := h.engine.cfg.TransitionValidator; validator != nil && !(step.Kind == "results" && result.Results != nil) {
			if err := validator.ValidateCompletion(ctx); err != nil {
				return h.failRun(ctx, step.ID, err)
			}
		}
		h.run.Status = enginepkg.RunStatusCompleted
		h.run.CompletedAt = time.Now()
		h.done.Store(true)
	}

	committedEvents := make([]traceEventDraft, 0, 3)
	if !frameResultCommitted && step.Kind == "wait_for_event" && settledStatus == enginepkg.StepStatusCompleted {
		committedEvents = append(committedEvents, traceEventDraft{kind: trace.EventKindEventReceived, payload: traceOccurrencePayload(ctx, step.ID, map[string]any{
			"step_id": step.ID,
			"payload": result.Output,
		})}, traceEventDraft{kind: trace.EventKindStepResumed, payload: traceOccurrencePayload(ctx, step.ID, map[string]any{
			"step_id": step.ID,
		})})
	}

	switch settledStatus {
	case enginepkg.StepStatusCompleted:
		if stepSpan != nil {
			stepSpan.SetStatus(otelPkg.StatusOK, "")
		}
		if !frameResultCommitted {
			committedEvents = append(committedEvents, traceEventDraft{kind: trace.EventKindStepCompleted, payload: traceOccurrencePayload(ctx, step.ID, map[string]any{
				"step_id":     step.ID,
				"kind":        step.Kind,
				"status":      result.Status,
				"outcome":     result.Outcome,
				"duration_ms": result.DurationMs,
				"output":      result.Output,
				"captures":    result.Vars,
				"evidence":    result.Evidence,
			})})
		}
	case enginepkg.StepStatusFailed:
		if stepSpan != nil {
			if result.Error != nil {
				stepSpan.RecordError(result.Error)
			}
			stepSpan.SetStatus(otelPkg.StatusError, "step failed")
		}
		if !frameResultCommitted {
			committedEvents = append(committedEvents, traceEventDraft{kind: trace.EventKindStepFailed, payload: traceOccurrencePayload(ctx, step.ID, map[string]any{
				"step_id":     step.ID,
				"kind":        step.Kind,
				"status":      result.Status,
				"outcome":     result.Outcome,
				"error":       errStr,
				"duration_ms": result.DurationMs,
				"output":      result.Output,
				"captures":    result.Vars,
			})})
		}
	case enginepkg.StepStatusDenied:
		if stepSpan != nil {
			if result.Error != nil {
				stepSpan.RecordError(result.Error)
			}
			stepSpan.SetStatus(otelPkg.StatusError, "step denied")
		}
		if !frameResultCommitted {
			committedEvents = append(committedEvents, traceEventDraft{kind: trace.EventKindStepFailed, payload: traceOccurrencePayload(ctx, step.ID, map[string]any{
				"step_id":    step.ID,
				"kind":       step.Kind,
				"status":     result.Status,
				"outcome":    result.Outcome,
				"error":      errStr,
				"error_type": "governance",
				"output":     result.Output,
				"captures":   result.Vars,
			})})
		}
	case enginepkg.StepStatusSkipped:
		reason := "condition_false"
		if result.Output != nil {
			if v, ok := result.Output["skip_reason"].(string); ok && v != "" {
				reason = v
			}
		}
		if !frameResultCommitted {
			committedEvents = append(committedEvents, traceEventDraft{
				kind: trace.EventKindStepSkipped, payload: h.skippedStepTracePayload(ctx, result, reason),
			})
		}
	}
	if stopRun {
		committedEvents = append(committedEvents, traceEventDraft{kind: trace.EventKindRunFailed, payload: map[string]any{
			"run_id": h.run.ID, "error": h.run.Error.Error(),
			"duration_ms": h.run.CompletedAt.Sub(h.run.StartedAt).Milliseconds(),
		}})
	}
	if terminalRun {
		committedEvents = append(committedEvents, traceEventDraft{
			kind: trace.EventKindRunCompleted, payload: h.terminalRunPayload(result),
		})
	}
	if result.Results != nil {
		for index := range committedEvents {
			committedEvents[index].payload["results_publication"] = publicationMetadata(result.Results)
		}
	}

	checkpointCtx := context.WithoutCancel(ctx)
	previousResults := h.run.Results
	pendingPublication := !frameResultCommitted && result.Results != nil
	if !frameResultCommitted && result.Results != nil {
		h.run.Results = cloneResults(result.Results)
	}
	checkpointFile, checkpointErr := h.persistCompletedCheckpoint(checkpointCtx, committedEvents...)
	if checkpointErr != nil {
		if errors.Is(checkpointErr, enginepkg.ErrCheckpointCommit) {
			h.run.Results = previousResults
			result.Results = nil
			if pendingPublication {
				delete(h.run.StepResults, step.ID)
				h.run.CurrentStepIndex--
				for h.run.CurrentStepIndex >= 0 && h.run.Plan.Steps[h.run.CurrentStepIndex].Depth > 0 {
					h.run.CurrentStepIndex--
				}
			}
		}
		return h.haltCheckpointCommit(checkpointCtx, step.ID, result, checkpointErr)
	}

	if checkpointFile != "" {
		h.emitCheckpoint(checkpointCtx, checkpointFile, step.ID)
	}
	if h.traceErr != nil {
		return h.haltCheckpointCommit(checkpointCtx, step.ID, result, h.traceErr)
	}

	if stopRun {
		h.runSpan.RecordError(h.run.Error)
		h.runSpan.SetStatus(otelPkg.StatusError, h.run.Error.Error())
		h.runSpan.End()
		safeClose(h.events, &h.eventsClosed)
		h.cancelFn()
		h.shutdownExtensions(checkpointCtx)
		return result, nil
	}

	if terminalRun {
		if err := h.completeTerminalRun(checkpointCtx, result); err != nil {
			return result, err
		}
	}

	return result, nil
}

func (h *runHandle) haltCheckpointCommit(ctx context.Context, stepID string, result *enginepkg.StepResult, checkpointErr error) (*enginepkg.StepResult, error) {
	now := time.Now().UTC()
	state := h.stateLocked()
	if prepared, markErr := markPreparedDispatchesIndeterminate(&state, h.run.WriterEpoch, now); markErr != nil {
		checkpointErr = errors.Join(checkpointErr, markErr)
	} else if len(prepared) > 0 {
		h.run.StepResults = state.StepResults
		h.run.Dispatches = state.Dispatches
		h.run.ExecutionFrames = state.ExecutionFrames
		h.run.DynamicIncludes = state.DynamicIncludes
	}
	h.run.Status = enginepkg.RunStatusIndeterminate
	h.run.CompletedAt = now
	h.run.Error = checkpointErr
	h.done.Store(true)

	if _, framed := enginepkg.ExecutionFrameBindingFromContext(ctx); framed && h.store == nil {
		// The parent may have committed even when its store returned an error.
		// A storeless child must not project replacement events into that
		// parent's reserved trace sequence while propagating the failure.
		h.runSpan.RecordError(checkpointErr)
		h.runSpan.End()
		safeClose(h.events, &h.eventsClosed)
		h.cancelFn()
		return result, checkpointErr
	}
	checkpointFile, markerErr := h.persistCompletedCheckpoint(ctx,
		traceEventDraft{kind: trace.EventKind("checkpoint/failed"), payload: map[string]any{
			"step_id": stepID, "error": checkpointErr.Error(), "marker_durable": true,
		}},
		traceEventDraft{kind: trace.EventKind("run/indeterminate"), payload: map[string]any{
			"run_id": h.run.ID, "error": checkpointErr.Error(),
		}},
	)
	if markerErr == nil {
		h.emitCheckpoint(ctx, checkpointFile, stepID)
	}
	h.runSpan.RecordError(checkpointErr)
	h.runSpan.SetStatus(otelPkg.StatusError, checkpointErr.Error())
	h.runSpan.End()
	safeClose(h.events, &h.eventsClosed)
	h.cancelFn()
	h.shutdownExtensions(ctx)
	if markerErr != nil {
		return result, errors.Join(checkpointErr, markerErr)
	}
	return result, checkpointErr
}

type traceEventDraft struct {
	kind    trace.EventKind
	payload map[string]any
}

func (h *runHandle) persistCompletedCheckpoint(ctx context.Context, drafts ...traceEventDraft) (string, error) {
	if h.store == nil {
		h.run.CheckpointSequence++
		for _, draft := range drafts {
			if err := h.emitEventLocked(ctx, draft.kind, draft.payload); err != nil {
				return "", err
			}
		}
		return "", nil
	}
	nextSequence := h.run.CheckpointSequence + 1
	previousEventSequence := h.run.Sequence
	previousCommittedSequence := h.run.CommittedTraceSequence
	previousPending := cloneTraceEvents(h.run.PendingTraceEvents)
	for _, draft := range drafts {
		h.stageCommittedTraceEvent(draft.kind, draft.payload)
	}
	h.stageCommittedTraceEvent(trace.EventKindExecutionCommitted, map[string]any{
		"checkpoint_sequence": nextSequence,
		"status":              string(h.run.Status),
		"current_step_index":  h.run.CurrentStepIndex,
		"cursor_set":          h.nextExecutionCursorSet(),
	})
	state := h.stateLocked()
	state.CheckpointSequence = nextSequence
	if err := h.store.SaveState(ctx, state); err != nil {
		h.run.Sequence = previousEventSequence
		h.run.CommittedTraceSequence = previousCommittedSequence
		h.run.PendingTraceEvents = previousPending
		return "", fmt.Errorf("%w: %w", enginepkg.ErrCheckpointCommit, err)
	}
	h.run.CheckpointSequence = nextSequence
	checkpointFile := fmt.Sprintf("checkpoint-%020d.json", nextSequence)
	if err := h.projectPendingTraceEventsLocked(ctx); err != nil {
		return checkpointFile, err
	}
	return checkpointFile, nil
}

func (h *runHandle) stageCommittedTraceEvent(kind trace.EventKind, payload map[string]any) {
	h.run.Sequence++
	h.run.CommittedTraceSequence = h.run.Sequence
	h.run.PendingTraceEvents = append(h.run.PendingTraceEvents, enginepkg.Event{
		EventID: uuid.NewString(), RunID: h.run.ID, RunbookID: h.run.Plan.Metadata.RunbookID,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano), Kind: string(kind), Sequence: h.run.Sequence,
		Payload: classifyPresentationOutput(payload, redactProtectedEventPayload(payload, h.effectiveDebugProtection(h.debugProtection))),
	})
}

func recoverPendingTraceEvents(
	ctx context.Context,
	writer trace.TraceWriter,
	store enginepkg.RunStore,
	writerEpoch uint64,
	events []enginepkg.Event,
) error {
	for _, event := range events {
		if err := writeProjectedTraceEvent(ctx, writer, store, writerEpoch, event); err != nil {
			return err
		}
	}
	return nil
}

func (h *runHandle) projectPendingTraceEventsLocked(ctx context.Context) error {
	if h.traceErr != nil {
		return h.traceErr
	}
	for _, event := range h.run.PendingTraceEvents {
		if err := writeProjectedTraceEvent(ctx, h.engine.cfg.TraceWriter, h.store, h.run.WriterEpoch, event); err != nil {
			h.traceErr = fmt.Errorf("%w: project committed event %s: %w", enginepkg.ErrTraceCommit, event.EventID, err)
			return h.traceErr
		}
		if h.engine.cfg.EventBus != nil {
			h.engine.cfg.EventBus.Publish(eventbus.Event{RunID: event.RunID, Kind: event.Kind, Payload: event.Payload})
		}
		if !h.eventsClosed.Load() {
			select {
			case h.events <- event:
			default:
			}
		}
		h.enqueueCallback(event)
	}
	h.run.PendingTraceEvents = nil
	return nil
}

func writeProjectedTraceEvent(
	ctx context.Context,
	writer trace.TraceWriter,
	store enginepkg.RunStore,
	writerEpoch uint64,
	event enginepkg.Event,
) error {
	payload, err := json.Marshal(event.Payload)
	if err != nil {
		return err
	}
	if store != nil {
		traceCtx := enginepkg.WithRunWriterEpoch(context.WithoutCancel(ctx), writerEpoch)
		if err := store.WriteTrace(traceCtx, event.RunID, event); err != nil {
			return err
		}
	}
	if err := appendConfiguredTraceProjection(writer, store, trace.TraceEvent{
		EventID: event.EventID, RunID: event.RunID, RunbookID: event.RunbookID,
		Timestamp: event.Timestamp, Kind: trace.EventKind(event.Kind), Sequence: event.Sequence, Payload: payload,
	}); err != nil {
		return err
	}
	return nil
}

func traceEventsAreAuthoritative(store enginepkg.RunStore) bool {
	authoritative, ok := store.(interface{ TraceEventsAuthoritative() bool })
	return ok && authoritative.TraceEventsAuthoritative()
}

func appendConfiguredTraceProjection(
	writer trace.TraceWriter,
	store enginepkg.RunStore,
	event trace.TraceEvent,
) error {
	if controller, ok := store.(interface{ TraceProjectionEnabled() bool }); ok && !controller.TraceProjectionEnabled() {
		return nil
	}
	if err := writer.Append(event); err != nil {
		if traceEventsAreAuthoritative(store) {
			if controller, ok := store.(interface{ MarkTraceProjectionFailed() }); ok {
				controller.MarkTraceProjectionFailed()
			}
			return nil
		}
		return err
	}
	return nil
}

func (h *runHandle) PrepareInteraction(ctx context.Context, proposed enginepkg.InteractionState) (enginepkg.InteractionState, error) {
	h.mu.Lock()
	defer h.unlockAndDrainCallbacks()
	if h.traceErr != nil {
		return enginepkg.InteractionState{}, h.traceErr
	}
	if boundary, ok := enginepkg.DispatchExecutionBoundaryFromContext(ctx); ok {
		if proposed.ExecutionInvocation > 0 && proposed.ExecutionInvocation != boundary.Invocation {
			return enginepkg.InteractionState{}, fmt.Errorf("%w: execution invocation changed", enginepkg.ErrInteractionConflict)
		}
		proposed.ExecutionInvocation = boundary.Invocation
	}
	if err := validateInteractionState(proposed, false); err != nil {
		return enginepkg.InteractionState{}, err
	}
	ownerStepID := ""
	if boundary, ok := enginepkg.DispatchExecutionBoundaryFromContext(ctx); ok {
		ownerStepID = boundary.QualifiedNodeID
	} else if h.run.CurrentStepIndex >= 0 && h.run.CurrentStepIndex < len(h.run.Plan.Steps) {
		ownerStepID = h.run.Plan.Steps[h.run.CurrentStepIndex].ID
	}
	if ownerStepID == "" {
		return enginepkg.InteractionState{}, fmt.Errorf("%w: no active owner step", enginepkg.ErrInteractionConflict)
	}
	for _, current := range h.run.Interactions {
		if current == nil || current.OwnerStepID != ownerStepID || current.NodeID != proposed.NodeID ||
			current.FrameID != proposed.FrameID || current.FrameStepIndex != proposed.FrameStepIndex ||
			current.Kind != proposed.Kind || current.Ordinal != proposed.Ordinal {
			continue
		}
		if current.RequestDigest != proposed.RequestDigest {
			return enginepkg.InteractionState{}, fmt.Errorf("%w: occurrence %d request changed", enginepkg.ErrInteractionConflict, proposed.Ordinal)
		}
		if current.ExecutionInvocation > 0 && proposed.ExecutionInvocation > 0 &&
			current.ExecutionInvocation != proposed.ExecutionInvocation {
			return enginepkg.InteractionState{}, fmt.Errorf("%w: execution invocation changed", enginepkg.ErrInteractionConflict)
		}
		if current.Status == enginepkg.InteractionStatusPending && h.run.Status != enginepkg.RunStatusWaiting {
			previousStatus := h.run.Status
			h.run.Status = enginepkg.RunStatusWaiting
			if _, err := h.persistCompletedCheckpoint(context.WithoutCancel(ctx)); err != nil {
				h.run.Status = previousStatus
				return enginepkg.InteractionState{}, err
			}
		}
		return cloneInteractionState(*current), nil
	}
	if current := h.run.Interactions[proposed.TurnID]; current != nil {
		return enginepkg.InteractionState{}, fmt.Errorf("%w: turn %q already exists", enginepkg.ErrInteractionConflict, proposed.TurnID)
	}
	proposed.OwnerStepID = ownerStepID
	previousInteractions := h.run.Interactions
	previousStatus := h.run.Status
	next := cloneInteractionStateMap(previousInteractions)
	committed := cloneInteractionState(proposed)
	next[committed.TurnID] = &committed
	h.run.Interactions = next
	h.run.Status = enginepkg.RunStatusWaiting
	if _, err := h.persistCompletedCheckpoint(ctx); err != nil {
		h.run.Interactions = previousInteractions
		h.run.Status = previousStatus
		return enginepkg.InteractionState{}, err
	}
	return cloneInteractionState(committed), nil
}

func (h *runHandle) PrepareDispatch(ctx context.Context, request enginepkg.DispatchRequest) (enginepkg.DispatchState, error) {
	classification := request.Classification
	if classification == "" {
		classification = "unspecified"
	}
	switch classification {
	case "read-only", "mutating", "destructive", "unspecified":
	default:
		return enginepkg.DispatchState{}, errors.New("engine: invalid dispatch classification")
	}
	if len(request.EndpointIdentity) > 1024 || strings.ContainsAny(request.EndpointIdentity, "\r\n\t@?#\\") {
		return enginepkg.DispatchState{}, errors.New("engine: unsafe dispatch endpoint identity")
	}
	rendered, err := json.Marshal(request.RenderedRequest)
	if err != nil {
		return enginepkg.DispatchState{}, fmt.Errorf("engine: encode rendered dispatch request: %w", err)
	}
	requestDigest := enginepkg.InteractionPayloadDigest(rendered)

	h.mu.Lock()
	defer h.unlockAndDrainCallbacks()
	if h.traceErr != nil {
		return enginepkg.DispatchState{}, h.traceErr
	}
	if h.run.CurrentStepIndex < 0 || h.run.CurrentStepIndex >= len(h.run.Plan.Steps) {
		return enginepkg.DispatchState{}, errors.New("engine: no active dispatch owner step")
	}
	step := h.run.Plan.Steps[h.run.CurrentStepIndex]
	boundary, hasBoundary := enginepkg.DispatchExecutionBoundaryFromContext(ctx)
	if !hasBoundary {
		boundary = enginepkg.DispatchExecutionBoundary{
			QualifiedNodeID: enginepkg.DebugNodeID(h.debugCallPath, step.ID),
			CallPath:        append([]enginepkg.DebugCallFrame(nil), h.debugCallPath...),
			StepID:          step.ID, StepIndex: h.run.CurrentStepIndex, Phase: enginepkg.ExecutionPhaseExecute,
			Invocation: 1, RetryAttempt: 1, OccurrenceSequence: 1,
		}
	}
	if boundary.FrameID != "" {
		frame := h.run.ExecutionFrames[boundary.FrameID]
		if frame == nil || frame.Status != enginepkg.ExecutionFrameStatusActive ||
			boundary.FrameStepIndex != frame.NextStepIndex {
			return enginepkg.DispatchState{}, errors.New("engine: dispatch execution frame is not at the requested child")
		}
	}
	callPath := boundary.CallPath
	qualifiedNodeID := boundary.QualifiedNodeID
	invocation := 1
	occurrenceSequence := int64(1)
	for _, current := range h.run.Dispatches {
		if current == nil {
			continue
		}
		if current.OccurrenceSequence >= occurrenceSequence {
			occurrenceSequence = current.OccurrenceSequence + 1
		}
		if current.FrameID == boundary.FrameID && current.QualifiedNodeID == qualifiedNodeID && current.Invocation >= invocation {
			invocation = current.Invocation + 1
		}
		if current.FrameID == boundary.FrameID && current.QualifiedNodeID == qualifiedNodeID && current.Status == enginepkg.DispatchStatusPrepared {
			if current.RequestDigest != requestDigest || current.Classification != classification ||
				current.EndpointIdentity != request.EndpointIdentity {
				return enginepkg.DispatchState{}, errors.New("engine: prepared dispatch request changed")
			}
			return cloneDispatchState(*current), nil
		}
	}
	identity, _ := json.Marshal(struct {
		RunID              string
		FrameID            string
		FrameStepIndex     int
		QualifiedNodeID    string
		Phase              enginepkg.ExecutionPhase
		Invocation         int
		RetryAttempt       int
		OccurrenceSequence int64
	}{h.run.ID, boundary.FrameID, boundary.FrameStepIndex, qualifiedNodeID, enginepkg.ExecutionPhaseExecute, invocation, 1, occurrenceSequence})
	occurrenceID := enginepkg.InteractionPayloadDigest(identity)
	dispatch := enginepkg.DispatchState{
		SchemaVersion: enginepkg.DispatchStateSchemaV1, OccurrenceID: occurrenceID,
		WriterEpoch: h.run.WriterEpoch, QualifiedNodeID: qualifiedNodeID, CallPath: callPath,
		StepID: boundary.StepID, FrameID: boundary.FrameID, FrameStepIndex: boundary.FrameStepIndex,
		Phase: enginepkg.ExecutionPhaseExecute, Invocation: invocation,
		RetryAttempt: 1, OccurrenceSequence: occurrenceSequence, Classification: classification,
		EndpointIdentity: request.EndpointIdentity, RequestDigest: requestDigest,
		IdempotencyKey:                 enginepkg.InteractionPayloadDigest([]byte("yawr.dispatch/v1\x00" + occurrenceID)),
		ProviderSupportsReconciliation: request.ProviderSupportsReconciliation,
		Status:                         enginepkg.DispatchStatusPrepared, PreparedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	previous := h.run.Dispatches
	next := cloneDispatchStateMap(previous)
	next[occurrenceID] = &dispatch
	h.run.Dispatches = next
	checkpointFile, err := h.persistCompletedCheckpoint(ctx)
	if err != nil {
		h.run.Dispatches = previous
		return enginepkg.DispatchState{}, err
	}
	h.emitCheckpoint(ctx, checkpointFile, step.ID)
	h.emitEventLocked(ctx, trace.EventKind("dispatch/prepared"), map[string]any{
		"occurrence_id": occurrenceID, "step_id": boundary.StepID, "classification": classification,
		"endpoint_identity": request.EndpointIdentity, "writer_epoch": h.run.WriterEpoch,
	})
	if h.traceErr != nil {
		return enginepkg.DispatchState{}, h.traceErr
	}
	return cloneDispatchState(dispatch), nil
}

func (h *runHandle) BeginExecutionFrame(
	ctx context.Context,
	request enginepkg.ExecutionFrameRequest,
) (enginepkg.ExecutionFrameState, error) {
	if request.ParentStepID == "" || request.ParentQualifiedNodeID == "" || request.Kind == "" ||
		len(request.CallPath) == 0 || request.CallPath[len(request.CallPath)-1].StepID != request.ParentStepID ||
		request.StepCount < 1 || len(request.StepIDs) != request.StepCount || request.IterationIndex < 0 ||
		!isSHA256Digest(request.DefinitionDigest) {
		return enginepkg.ExecutionFrameState{}, errors.New("engine: invalid execution frame request")
	}
	h.mu.Lock()
	defer h.unlockAndDrainCallbacks()
	if h.traceErr != nil {
		return enginepkg.ExecutionFrameState{}, h.traceErr
	}
	if request.ParentFrameID != "" {
		parent := h.run.ExecutionFrames[request.ParentFrameID]
		if parent == nil || parent.Status != enginepkg.ExecutionFrameStatusActive {
			return enginepkg.ExecutionFrameState{}, errors.New("engine: execution frame parent is not active")
		}
	}
	for frameID, current := range h.run.ExecutionFrames {
		if current == nil || (current.Status != enginepkg.ExecutionFrameStatusActive &&
			!(current.Status == enginepkg.ExecutionFrameStatusCompleted && current.RunResults != nil && !h.frameOwnerSettled(current))) ||
			current.ParentFrameID != request.ParentFrameID || current.ParentQualifiedNodeID != request.ParentQualifiedNodeID ||
			current.Kind != request.Kind || current.BranchLabel != request.BranchLabel || current.IterationIndex != request.IterationIndex {
			continue
		}
		if current.DefinitionDigest != request.DefinitionDigest || current.StepCount != request.StepCount {
			return enginepkg.ExecutionFrameState{}, fmt.Errorf(
				"engine: resumed execution frame definition changed: saved=%s requested=%s saved_steps=%v requested_steps=%v",
				current.DefinitionDigest, request.DefinitionDigest, current.StepIDs, request.StepIDs,
			)
		}
		if request.RunbookInvocation != nil && current.BindingScope != nil &&
			current.BindingScope.DeclarationDigest != enginepkg.InvocationDigest(request.RunbookInvocation) {
			return enginepkg.ExecutionFrameState{}, errors.New("engine: resumed runbook declaration changed")
		}
		previousFrames := h.run.ExecutionFrames
		nextFrames := cloneExecutionFrameStateMap(previousFrames)
		resumed := nextFrames[frameID]
		resumed.WriterEpoch = h.run.WriterEpoch
		h.run.ExecutionFrames = nextFrames
		checkpointFile, err := h.persistCompletedCheckpoint(ctx)
		if err != nil {
			h.run.ExecutionFrames = previousFrames
			return enginepkg.ExecutionFrameState{}, err
		}
		h.emitCheckpoint(ctx, checkpointFile, request.ParentStepID)
		if h.traceErr != nil {
			return enginepkg.ExecutionFrameState{}, h.traceErr
		}
		return cloneExecutionFrameState(*resumed), nil
	}
	invocation := 1
	for _, current := range h.run.ExecutionFrames {
		if current != nil && current.ParentFrameID == request.ParentFrameID &&
			current.ParentQualifiedNodeID == request.ParentQualifiedNodeID && current.Kind == request.Kind &&
			current.BranchLabel == request.BranchLabel && current.IterationIndex == request.IterationIndex &&
			current.Invocation >= invocation {
			invocation = current.Invocation + 1
		}
	}
	identity, _ := json.Marshal(struct {
		RunID                 string
		ParentFrameID         string
		ParentQualifiedNodeID string
		Kind                  string
		BranchLabel           string
		IterationIndex        int
		Invocation            int
	}{h.run.ID, request.ParentFrameID, request.ParentQualifiedNodeID, request.Kind, request.BranchLabel, request.IterationIndex, invocation})
	frameID := enginepkg.InteractionPayloadDigest(identity)
	frame := enginepkg.ExecutionFrameState{
		SchemaVersion: enginepkg.ExecutionFrameStateSchemaV1, FrameID: frameID,
		ParentFrameID: request.ParentFrameID, WriterEpoch: h.run.WriterEpoch,
		ParentQualifiedNodeID: request.ParentQualifiedNodeID, ParentStepID: request.ParentStepID,
		Kind: request.Kind, CallPath: append([]enginepkg.DebugCallFrame(nil), request.CallPath...),
		BranchLabel: request.BranchLabel, IterationIndex: request.IterationIndex, Invocation: invocation,
		DefinitionDigest: request.DefinitionDigest, StepCount: request.StepCount,
		StepIDs:     append([]string(nil), request.StepIDs...),
		WorkingVars: cloneAnyMap(request.InitialVars), Results: make(map[string]*enginepkg.StepResult),
		Status: enginepkg.ExecutionFrameStatusActive, StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	frame.BindingScope = enginepkg.CloneBindingScope(request.BindingScope)
	if request.RunbookInvocation != nil {
		var err error
		frame.BindingScope, frame.WorkingVars, err = initializeScope(request.RunbookInvocation, frame.WorkingVars)
		if err != nil {
			return enginepkg.ExecutionFrameState{}, typedErrorAt(err, enginepkg.ResultsOrigin{NodeID: enginepkg.DebugNodeID(frame.CallPath, "bindings"), FrameID: frame.FrameID, Invocation: frame.Invocation})
		}
		if frame.BindingScope != nil {
			frame.BindingScope.FrameID = frame.FrameID
		}
	}
	previousFrames := h.run.ExecutionFrames
	nextFrames := cloneExecutionFrameStateMap(previousFrames)
	nextFrames[frameID] = &frame
	h.run.ExecutionFrames = nextFrames
	checkpointFile, err := h.persistCompletedCheckpoint(ctx)
	if err != nil {
		h.run.ExecutionFrames = previousFrames
		return enginepkg.ExecutionFrameState{}, err
	}
	h.emitCheckpoint(ctx, checkpointFile, request.ParentStepID)
	if h.traceErr != nil {
		return enginepkg.ExecutionFrameState{}, h.traceErr
	}
	return cloneExecutionFrameState(frame), nil
}

func (h *runHandle) LookupDynamicIncludeResolution(
	ctx context.Context,
	renderedRef string,
) (enginepkg.DynamicIncludeResolutionState, bool, error) {
	if renderedRef == "" {
		return enginepkg.DynamicIncludeResolutionState{}, false, errors.New("engine: dynamic include rendered reference is required")
	}
	h.mu.Lock()
	defer h.unlockAndDrainCallbacks()
	if h.traceErr != nil {
		return enginepkg.DynamicIncludeResolutionState{}, false, h.traceErr
	}
	boundary, err := h.executionBoundary(ctx)
	if err != nil {
		return enginepkg.DynamicIncludeResolutionState{}, false, err
	}
	for _, resolution := range h.run.DynamicIncludes {
		if resolution != nil && resolution.Status == enginepkg.DynamicIncludeResolutionStatusActive &&
			resolution.QualifiedNodeID == boundary.QualifiedNodeID && resolution.FrameID == boundary.FrameID &&
			resolution.FrameStepIndex == boundary.FrameStepIndex {
			if resolution.Pin.RenderedRef != renderedRef {
				return enginepkg.DynamicIncludeResolutionState{}, false, errors.New("engine: dynamic include rendered reference changed")
			}
			return cloneDynamicIncludeResolutionState(*resolution), true, nil
		}
	}
	return enginepkg.DynamicIncludeResolutionState{}, false, nil
}

func (h *runHandle) CommitDynamicIncludeResolution(
	ctx context.Context,
	pin schema.LockedDynamicInclude,
) (enginepkg.DynamicIncludeResolutionState, error) {
	if pin.RenderedRef == "" || pin.QualifiedID == "" || pin.AbsPath == "" ||
		!isSHA256Digest(pin.FileDigest) || !isSHA256Digest(pin.PackageDigest) {
		return enginepkg.DynamicIncludeResolutionState{}, errors.New("engine: invalid dynamic include resolution pin")
	}
	h.mu.Lock()
	defer h.unlockAndDrainCallbacks()
	if h.traceErr != nil {
		return enginepkg.DynamicIncludeResolutionState{}, h.traceErr
	}
	artifact, err := plansnapshot.CanonicalDynamicIncludeProtectionArtifact(pin)
	if err != nil {
		return enginepkg.DynamicIncludeResolutionState{}, errors.New("engine: dynamic include resolution cannot be scanned")
	}
	protection := enginepkg.MergeDebugProtection(
		h.effectiveDebugProtection(h.debugProtection), internaldebugprotect.ProtectionFromContext(ctx),
	)
	if err := internaldebugprotect.ValidateHandoffJSON(protection, artifact); err != nil {
		return enginepkg.DynamicIncludeResolutionState{}, errors.New("engine: dynamic include resolution contains protected content")
	}
	flow, err := plansnapshot.RestoreFlowClosure(pin.ExecutableClosure)
	if err != nil {
		return enginepkg.DynamicIncludeResolutionState{}, errors.New("engine: dynamic include resolution closure is invalid")
	}
	if err := internaldebugprotect.ValidateHandoffFlow(protection, flow); err != nil {
		return enginepkg.DynamicIncludeResolutionState{}, errors.New("engine: dynamic include resolution contains protected handoff provenance")
	}
	boundary, err := h.executionBoundary(ctx)
	if err != nil {
		return enginepkg.DynamicIncludeResolutionState{}, err
	}
	pin.StepID = boundary.StepID
	for _, resolution := range h.run.DynamicIncludes {
		if resolution == nil || resolution.Status != enginepkg.DynamicIncludeResolutionStatusActive ||
			resolution.QualifiedNodeID != boundary.QualifiedNodeID || resolution.FrameID != boundary.FrameID ||
			resolution.FrameStepIndex != boundary.FrameStepIndex {
			continue
		}
		expectedPin := pin
		expectedPin.QualifiedNodeID = resolution.QualifiedNodeID
		expectedPin.Invocation = resolution.Invocation
		expectedPin.Revision = resolution.Revision
		if !reflect.DeepEqual(resolution.Pin, expectedPin) {
			return enginepkg.DynamicIncludeResolutionState{}, errors.New("engine: dynamic include resolution conflicts with durable pin")
		}
		return cloneDynamicIncludeResolutionState(*resolution), nil
	}
	invocation := 1
	revision := int64(1)
	for _, resolution := range h.run.DynamicIncludes {
		if resolution != nil && resolution.QualifiedNodeID == boundary.QualifiedNodeID &&
			resolution.FrameID == boundary.FrameID && resolution.Invocation >= invocation {
			invocation = resolution.Invocation + 1
		}
		if resolution != nil && resolution.Revision >= revision {
			revision = resolution.Revision + 1
		}
	}
	preparedInvocation := 0
	for _, dispatch := range h.run.Dispatches {
		if dispatch == nil || dispatch.Status != enginepkg.DispatchStatusPrepared ||
			dispatch.EndpointIdentity != "dynamic-include-resolver" ||
			dispatch.QualifiedNodeID != boundary.QualifiedNodeID || dispatch.StepID != boundary.StepID ||
			dispatch.FrameID != boundary.FrameID || dispatch.FrameStepIndex != boundary.FrameStepIndex ||
			dispatch.Phase != enginepkg.ExecutionPhaseExecute ||
			!sameDebugCallPath(dispatch.CallPath, boundary.CallPath) {
			continue
		}
		if preparedInvocation != 0 {
			return enginepkg.DynamicIncludeResolutionState{}, errors.New("engine: dynamic include resolver dispatch is ambiguous")
		}
		preparedInvocation = dispatch.Invocation
	}
	if preparedInvocation > 0 {
		invocation = preparedInvocation
	}
	pin.QualifiedNodeID = boundary.QualifiedNodeID
	pin.Invocation = invocation
	pin.Revision = revision
	pin.StructuralPath = enginepkg.DynamicIncludeStructuralPathFromContext(ctx)
	identity, _ := json.Marshal(struct {
		RunID           string
		QualifiedNodeID string
		FrameID         string
		FrameStepIndex  int
		Invocation      int
	}{h.run.ID, boundary.QualifiedNodeID, boundary.FrameID, boundary.FrameStepIndex, invocation})
	resolutionID := enginepkg.InteractionPayloadDigest(identity)
	resolution := enginepkg.DynamicIncludeResolutionState{
		SchemaVersion: enginepkg.DynamicIncludeResolutionStateSchemaV1, ResolutionID: resolutionID,
		WriterEpoch: h.run.WriterEpoch, QualifiedNodeID: boundary.QualifiedNodeID,
		CallPath: append([]enginepkg.DebugCallFrame(nil), boundary.CallPath...), StepID: boundary.StepID,
		FrameID: boundary.FrameID, FrameStepIndex: boundary.FrameStepIndex, Invocation: invocation,
		Revision: revision,
		Pin:      pin, Status: enginepkg.DynamicIncludeResolutionStatusActive,
		CommittedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	previous := h.run.DynamicIncludes
	previousDispatches := h.run.Dispatches
	if err := h.stagePreparedDispatchForBoundary(boundary, pin); err != nil {
		return enginepkg.DynamicIncludeResolutionState{}, err
	}
	next := cloneDynamicIncludeResolutionStateMap(previous)
	next[resolutionID] = &resolution
	h.run.DynamicIncludes = next
	checkpointFile, err := h.persistCompletedCheckpoint(ctx)
	if err != nil {
		h.run.DynamicIncludes = previous
		h.run.Dispatches = previousDispatches
		return enginepkg.DynamicIncludeResolutionState{}, err
	}
	h.emitCheckpoint(ctx, checkpointFile, boundary.StepID)
	if h.traceErr != nil {
		return enginepkg.DynamicIncludeResolutionState{}, h.traceErr
	}
	return cloneDynamicIncludeResolutionState(resolution), nil
}

func (h *runHandle) executionBoundary(ctx context.Context) (enginepkg.DispatchExecutionBoundary, error) {
	if boundary, ok := enginepkg.DispatchExecutionBoundaryFromContext(ctx); ok {
		return boundary, nil
	}
	if h.run.CurrentStepIndex < 0 || h.run.CurrentStepIndex >= len(h.run.Plan.Steps) {
		return enginepkg.DispatchExecutionBoundary{}, errors.New("engine: no active execution boundary")
	}
	step := h.run.Plan.Steps[h.run.CurrentStepIndex]
	return enginepkg.DispatchExecutionBoundary{
		QualifiedNodeID: enginepkg.DebugNodeID(h.debugCallPath, step.ID),
		CallPath:        append([]enginepkg.DebugCallFrame(nil), h.debugCallPath...),
		StepID:          step.ID, StepIndex: h.run.CurrentStepIndex, Phase: enginepkg.ExecutionPhaseExecute,
		Invocation: 1, RetryAttempt: 1, OccurrenceSequence: 1,
	}, nil
}

func (h *runHandle) CommitExecutionFrameStep(
	ctx context.Context,
	commit enginepkg.ExecutionFrameStepCommit,
) (enginepkg.ExecutionFrameState, error) {
	if commit.FrameID == "" || commit.StepKind == "" || commit.Result == nil || commit.Result.StepID == "" {
		return enginepkg.ExecutionFrameState{}, errors.New("engine: invalid execution frame step commit")
	}
	h.mu.Lock()
	defer h.unlockAndDrainCallbacks()
	if h.traceErr != nil {
		return enginepkg.ExecutionFrameState{}, h.traceErr
	}
	current := h.run.ExecutionFrames[commit.FrameID]
	if current == nil || current.Status != enginepkg.ExecutionFrameStatusActive ||
		current.WriterEpoch != h.run.WriterEpoch || commit.StepIndex != current.NextStepIndex ||
		commit.StepIndex < 0 || commit.StepIndex >= current.StepCount {
		return enginepkg.ExecutionFrameState{}, errors.New("engine: execution frame step commit conflicts with durable progress")
	}
	previousFrames := h.run.ExecutionFrames
	previousDispatches := h.run.Dispatches
	previousInteractions := h.run.Interactions
	nextFrames := cloneExecutionFrameStateMap(previousFrames)
	frame := nextFrames[commit.FrameID]
	committedResult, dispatchIndeterminate := h.frameDispatchResult(ctx, commit.FrameID, commit.StepIndex, commit.Result)
	if committedResult.Results != nil {
		if err := h.preparePublication(ctx, committedResult, frame); err != nil {
			committedResult.Status, committedResult.Outcome, committedResult.Error = enginepkg.StepStatusFailed, enginepkg.StepOutcomeFailed, err
			committedResult.Results = nil
		} else {
			frame.RunResults = cloneResults(committedResult.Results)
		}
	}
	frame.Results[strconv.Itoa(commit.StepIndex)] = committedResult
	frame.WorkingVars = cloneAnyMap(commit.WorkingVars)
	frame.NextStepIndex++
	if frame.RunResults != nil {
		frame.Status = enginepkg.ExecutionFrameStatusCompleted
		frame.CompletedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	h.run.ExecutionFrames = nextFrames
	if err := h.settlePreparedFrameDispatch(commit.FrameID, commit.StepIndex, committedResult); err != nil {
		h.run.ExecutionFrames = previousFrames
		h.run.Dispatches = previousDispatches
		h.run.Interactions = previousInteractions
		return enginepkg.ExecutionFrameState{}, err
	}
	h.clearInteractionsForOwner(enginepkg.DebugNodeID(frame.CallPath, committedResult.StepID))
	h.finishChildExecutionFrames(commit.FrameID, committedResult.StepID, committedResult.Status)
	h.finishDynamicIncludeResolutions(commit.FrameID, enginepkg.DebugNodeID(frame.CallPath, committedResult.StepID), committedResult.Status)
	drafts := make([]traceEventDraft, 0, 3)
	if commit.StepKind == "wait_for_event" && committedResult.Status == enginepkg.StepStatusCompleted {
		drafts = append(drafts,
			traceEventDraft{kind: trace.EventKindEventReceived, payload: traceOccurrencePayload(ctx, committedResult.StepID, map[string]any{
				"step_id": committedResult.StepID, "payload": committedResult.Output,
			})},
			traceEventDraft{kind: trace.EventKindStepResumed, payload: traceOccurrencePayload(ctx, committedResult.StepID, map[string]any{
				"step_id": committedResult.StepID,
			})},
		)
	}
	if draft, ok := h.frameStepResultTraceDraft(ctx, commit.StepKind, committedResult); ok {
		drafts = append(drafts, draft)
	}
	if committedResult.Status == enginepkg.StepStatusCompleted && isTerminalOutput(committedResult) {
		// Settle the unexecuted tail atomically with the return. The owner can
		// then finish this frame, and a resume cannot execute past the return.
		now := time.Now().UTC()
		firstSkipped := frame.NextStepIndex
		for frame.NextStepIndex < frame.StepCount {
			index := frame.NextStepIndex
			skipped := &enginepkg.StepResult{
				StepID: frame.StepIDs[index], Status: enginepkg.StepStatusSkipped, Outcome: enginepkg.StepOutcomeSkipped,
				Output: map[string]any{"skip_reason": "terminal"}, StartedAt: now, CompletedAt: now,
			}
			frame.Results[strconv.Itoa(index)] = skipped
			frame.NextStepIndex++
		}
		if firstSkipped < frame.StepCount {
			drafts = append(drafts, traceEventDraft{
				kind: trace.EventKind("execution/frame-returned"),
				payload: traceOccurrencePayload(ctx, committedResult.StepID, map[string]any{
					"step_id": committedResult.StepID, "frame_id": frame.FrameID, "reason": "terminal",
					"first_skipped_index": firstSkipped, "skipped_step_ids": append([]string(nil), frame.StepIDs[firstSkipped:]...),
				}),
			})
		}
	}
	checkpointFile, err := h.persistCompletedCheckpoint(ctx, drafts...)
	if err != nil {
		if errors.Is(err, enginepkg.ErrCheckpointCommit) {
			h.run.ExecutionFrames = previousFrames
			h.run.Dispatches = previousDispatches
			h.run.Interactions = previousInteractions
		}
		return enginepkg.ExecutionFrameState{}, err
	}
	h.emitCheckpoint(ctx, checkpointFile, committedResult.StepID)
	if h.traceErr != nil {
		return enginepkg.ExecutionFrameState{}, h.traceErr
	}
	committedFrame := cloneExecutionFrameState(*h.run.ExecutionFrames[commit.FrameID])
	if dispatchIndeterminate {
		return committedFrame, enginepkg.ErrIndeterminate
	}
	return committedFrame, nil
}

func (h *runHandle) frameDispatchResult(
	ctx context.Context,
	frameID string,
	stepIndex int,
	result *enginepkg.StepResult,
) (*enginepkg.StepResult, bool) {
	committed := cloneStepResult(result)
	for _, dispatch := range h.run.Dispatches {
		if dispatch == nil || dispatch.Status != enginepkg.DispatchStatusPrepared ||
			dispatch.FrameID != frameID || dispatch.FrameStepIndex != stepIndex {
			continue
		}
		classification := dispatch.Classification
		if !isTransportLoss(committed.Error) || !requiresIndeterminate(&classification) {
			return committed, false
		}
		now := time.Now().UTC()
		committed.Status = enginepkg.StepStatusIndeterminate
		committed.Outcome = enginepkg.StepOutcomeFailed
		committed.Indeterminate = &enginepkg.IndeterminateRecord{
			RunID: h.run.ID, StepID: committed.StepID, Classification: classification,
			AttemptNumber: dispatch.RetryAttempt, FailureTime: now,
			TransportErrCategory: classifyTransportError(committed.Error),
		}
		if deadline, ok := ctx.Deadline(); ok {
			committed.Indeterminate.Deadline = deadline
		}
		return committed, true
	}
	return committed, false
}

func (h *runHandle) finishExecutionFramesForStep(stepID string, status enginepkg.StepStatus) {
	qualifiedNodeID := enginepkg.DebugNodeID(h.debugCallPath, stepID)
	finishExecutionFrames(h.run.ExecutionFrames, "", qualifiedNodeID, status)
}

func (h *runHandle) finishChildExecutionFrames(parentFrameID, stepID string, status enginepkg.StepStatus) {
	parent := h.run.ExecutionFrames[parentFrameID]
	if parent == nil {
		return
	}
	qualifiedNodeID := enginepkg.DebugNodeID(parent.CallPath, stepID)
	finishExecutionFrames(h.run.ExecutionFrames, parentFrameID, qualifiedNodeID, status)
}

func finishExecutionFrames(
	frames map[string]*enginepkg.ExecutionFrameState,
	parentFrameID string,
	parentQualifiedNodeID string,
	status enginepkg.StepStatus,
) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, frame := range frames {
		if frame == nil || frame.Status != enginepkg.ExecutionFrameStatusActive ||
			frame.ParentFrameID != parentFrameID || frame.ParentQualifiedNodeID != parentQualifiedNodeID {
			continue
		}
		if frame.Kind == "compensate" && status == enginepkg.StepStatusFailed {
			continue
		}
		switch status {
		case enginepkg.StepStatusCompleted, enginepkg.StepStatusSkipped:
			if frame.NextStepIndex == frame.StepCount {
				frame.Status = enginepkg.ExecutionFrameStatusCompleted
				frame.CompletedAt = now
			}
		case enginepkg.StepStatusIndeterminate:
			frame.Status = enginepkg.ExecutionFrameStatusIndeterminate
			frame.CompletedAt = now
		default:
			frame.Status = enginepkg.ExecutionFrameStatusFailed
			frame.CompletedAt = now
		}
	}
}

func (h *runHandle) finishDynamicIncludeResolutions(
	frameID string,
	qualifiedNodeID string,
	status enginepkg.StepStatus,
) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, resolution := range h.run.DynamicIncludes {
		if resolution == nil || resolution.Status != enginepkg.DynamicIncludeResolutionStatusActive ||
			resolution.FrameID != frameID || resolution.QualifiedNodeID != qualifiedNodeID {
			continue
		}
		if status == enginepkg.StepStatusCompleted || status == enginepkg.StepStatusSkipped {
			resolution.Status = enginepkg.DynamicIncludeResolutionStatusCompleted
		} else {
			resolution.Status = enginepkg.DynamicIncludeResolutionStatusFailed
		}
		resolution.CompletedAt = now
	}
}

func (h *runHandle) skippedStepTracePayload(
	ctx context.Context,
	result *enginepkg.StepResult,
	reason string,
) map[string]any {
	payload := traceOccurrencePayload(ctx, result.StepID, map[string]any{"step_id": result.StepID, "reason": reason})
	if reason != "include_not_found" {
		return payload
	}
	boundary, found := enginepkg.DispatchExecutionBoundaryFromContext(ctx)
	if !found {
		boundary = enginepkg.DispatchExecutionBoundary{
			QualifiedNodeID: enginepkg.DebugNodeID(h.debugCallPath, result.StepID),
			CallPath:        append([]enginepkg.DebugCallFrame(nil), h.debugCallPath...), StepID: result.StepID,
			Phase: enginepkg.ExecutionPhaseExecute, Invocation: 1, RetryAttempt: 1, OccurrenceSequence: 1,
		}
	}
	invocation := boundary.Invocation
	digest, _ := enginepkg.DynamicIncludeNotFoundResultDigest(result)
	latestSequence := int64(0)
	for _, dispatch := range h.run.Dispatches {
		if dispatch == nil || dispatch.Status != enginepkg.DispatchStatusSettled ||
			dispatch.EndpointIdentity != "dynamic-include-resolver" || dispatch.ResultDigest != digest ||
			dispatch.QualifiedNodeID != boundary.QualifiedNodeID || dispatch.FrameID != boundary.FrameID ||
			dispatch.FrameStepIndex != boundary.FrameStepIndex || dispatch.OccurrenceSequence <= latestSequence {
			continue
		}
		latestSequence = dispatch.OccurrenceSequence
		invocation = dispatch.Invocation
	}
	payload["qualified_node_id"] = boundary.QualifiedNodeID
	payload["structural_path"] = enginepkg.DynamicIncludeStructuralPathFromContext(ctx)
	payload["invocation"] = invocation
	return payload
}

func (h *runHandle) frameStepResultTraceDraft(
	ctx context.Context,
	stepKind string,
	result *enginepkg.StepResult,
) (traceEventDraft, bool) {
	if result == nil {
		return traceEventDraft{}, false
	}
	base := map[string]any{
		"step_id": result.StepID, "kind": stepKind, "status": result.Status, "outcome": result.Outcome,
		"duration_ms": result.DurationMs, "output": result.Output, "captures": result.Vars,
	}
	if result.Results != nil {
		base["results_publication"] = publicationMetadata(result.Results)
	}
	switch result.Status {
	case enginepkg.StepStatusCompleted:
		base["evidence"] = result.Evidence
		return traceEventDraft{
			kind: trace.EventKindStepCompleted, payload: traceOccurrencePayload(ctx, result.StepID, base),
		}, true
	case enginepkg.StepStatusFailed, enginepkg.StepStatusDenied:
		if result.Error != nil {
			base["error"] = result.Error.Error()
		}
		return traceEventDraft{
			kind: trace.EventKindStepFailed, payload: traceOccurrencePayload(ctx, result.StepID, base),
		}, true
	case enginepkg.StepStatusSkipped:
		reason := "condition_false"
		if value, ok := result.Output["skip_reason"].(string); ok && value != "" {
			reason = value
		}
		return traceEventDraft{
			kind: trace.EventKindStepSkipped, payload: h.skippedStepTracePayload(ctx, result, reason),
		}, true
	default:
		return traceEventDraft{}, false
	}
}

func (h *runHandle) settlePreparedFrameDispatch(frameID string, stepIndex int, result *enginepkg.StepResult) error {
	for occurrenceID, current := range h.run.Dispatches {
		if current == nil || current.Status != enginepkg.DispatchStatusPrepared ||
			current.FrameID != frameID || current.FrameStepIndex != stepIndex {
			continue
		}
		settled := cloneDispatchState(*current)
		if result.Status == enginepkg.StepStatusIndeterminate {
			settled.Status = enginepkg.DispatchStatusIndeterminate
			settled.ResultDigest = ""
		} else {
			payload, err := json.Marshal(result)
			if err != nil {
				return fmt.Errorf("%w: encode frame dispatch result: %v", enginepkg.ErrCheckpointCommit, err)
			}
			settled.Status = enginepkg.DispatchStatusSettled
			settled.ResultDigest = enginepkg.InteractionPayloadDigest(payload)
			if current.EndpointIdentity == "dynamic-include-resolver" {
				if notFoundDigest, ok := enginepkg.DynamicIncludeNotFoundResultDigest(result); ok {
					settled.ResultDigest = notFoundDigest
				}
			}
		}
		settled.SettledAt = time.Now().UTC().Format(time.RFC3339Nano)
		next := cloneDispatchStateMap(h.run.Dispatches)
		next[occurrenceID] = &settled
		h.run.Dispatches = next
		return nil
	}
	return nil
}

func (h *runHandle) stagePreparedDispatchForBoundary(
	boundary enginepkg.DispatchExecutionBoundary,
	result any,
) error {
	for occurrenceID, current := range h.run.Dispatches {
		if current == nil || current.Status != enginepkg.DispatchStatusPrepared ||
			current.QualifiedNodeID != boundary.QualifiedNodeID || current.FrameID != boundary.FrameID ||
			current.FrameStepIndex != boundary.FrameStepIndex {
			continue
		}
		payload, err := json.Marshal(result)
		if err != nil {
			return fmt.Errorf("%w: encode dispatch result: %v", enginepkg.ErrCheckpointCommit, err)
		}
		settled := cloneDispatchState(*current)
		settled.Status = enginepkg.DispatchStatusSettled
		settled.ResultDigest = enginepkg.InteractionPayloadDigest(payload)
		settled.SettledAt = time.Now().UTC().Format(time.RFC3339Nano)
		next := cloneDispatchStateMap(h.run.Dispatches)
		next[occurrenceID] = &settled
		h.run.Dispatches = next
		return nil
	}
	return nil
}

func (h *runHandle) stagePreparedDispatchSettled(stepID string, result *enginepkg.StepResult, execErr error) error {
	occurrenceID, current := h.preparedDispatchForStep(stepID)
	if current == nil {
		return nil
	}
	errorText := ""
	if execErr != nil {
		errorText = execErr.Error()
	} else if result != nil && result.Error != nil {
		errorText = result.Error.Error()
	}
	payload, err := json.Marshal(struct {
		Result *enginepkg.StepResult
		Error  string
	}{result, errorText})
	if err != nil {
		return fmt.Errorf("%w: encode dispatch result: %v", enginepkg.ErrCheckpointCommit, err)
	}
	settled := cloneDispatchState(*current)
	settled.Status = enginepkg.DispatchStatusSettled
	settled.ResultDigest = enginepkg.InteractionPayloadDigest(payload)
	if current.EndpointIdentity == "dynamic-include-resolver" {
		if notFoundDigest, ok := enginepkg.DynamicIncludeNotFoundResultDigest(result); ok {
			settled.ResultDigest = notFoundDigest
		}
	}
	settled.SettledAt = time.Now().UTC().Format(time.RFC3339Nano)
	next := cloneDispatchStateMap(h.run.Dispatches)
	next[occurrenceID] = &settled
	h.run.Dispatches = next
	return nil
}

func (h *runHandle) stagePreparedDispatchIndeterminate(stepID string) bool {
	occurrenceID, current := h.preparedDispatchForStep(stepID)
	if current == nil {
		return false
	}
	indeterminate := cloneDispatchState(*current)
	indeterminate.Status = enginepkg.DispatchStatusIndeterminate
	indeterminate.ResultDigest = ""
	indeterminate.SettledAt = time.Now().UTC().Format(time.RFC3339Nano)
	next := cloneDispatchStateMap(h.run.Dispatches)
	next[occurrenceID] = &indeterminate
	h.run.Dispatches = next
	return true
}

func (h *runHandle) preparedDispatchForStep(stepID string) (string, *enginepkg.DispatchState) {
	qualifiedNodeID := enginepkg.DebugNodeID(h.debugCallPath, stepID)
	for occurrenceID, dispatch := range h.run.Dispatches {
		if dispatch != nil && dispatch.QualifiedNodeID == qualifiedNodeID && dispatch.Status == enginepkg.DispatchStatusPrepared {
			return occurrenceID, dispatch
		}
	}
	return "", nil
}

func (h *runHandle) AcceptInteraction(
	ctx context.Context,
	turnID string,
	answerDigest string,
	answer json.RawMessage,
) (enginepkg.InteractionState, error) {
	return h.acceptInteraction(ctx, turnID, "", "", answerDigest, answer)
}

func (h *runHandle) AcceptInteractionCommand(
	ctx context.Context,
	turnID string,
	commandID string,
	commandDigest string,
	answerDigest string,
	answer json.RawMessage,
) (enginepkg.InteractionState, error) {
	if commandID == "" || len(commandID) > 128 || strings.ContainsAny(commandID, "\r\n\x00") ||
		commandDigest == "" {
		return enginepkg.InteractionState{}, fmt.Errorf("%w: invalid answer command id", enginepkg.ErrInteractionConflict)
	}
	return h.acceptInteraction(ctx, turnID, commandID, commandDigest, answerDigest, answer)
}

func (h *runHandle) acceptInteraction(
	ctx context.Context,
	turnID string,
	commandID string,
	commandDigest string,
	answerDigest string,
	answer json.RawMessage,
) (enginepkg.InteractionState, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.traceErr != nil {
		return enginepkg.InteractionState{}, h.traceErr
	}
	current := h.run.Interactions[turnID]
	if current == nil {
		return enginepkg.InteractionState{}, fmt.Errorf("%w: turn %q is not pending", enginepkg.ErrInteractionConflict, turnID)
	}
	if answerDigest == "" || len(answer) == 0 || !json.Valid(answer) {
		return enginepkg.InteractionState{}, fmt.Errorf("%w: turn %q has an invalid answer", enginepkg.ErrInteractionConflict, turnID)
	}
	if current.Status == enginepkg.InteractionStatusAnswered {
		if current.AnswerDigest == answerDigest && bytes.Equal(current.Answer, answer) &&
			(current.AnswerCommandID == "" || commandID == "" ||
				current.AnswerCommandID == commandID && current.AnswerCommandDigest == commandDigest) {
			return cloneInteractionState(*current), nil
		}
		return enginepkg.InteractionState{}, fmt.Errorf("%w: turn %q was answered differently", enginepkg.ErrInteractionConflict, turnID)
	}
	if current.Status != enginepkg.InteractionStatusPending {
		return enginepkg.InteractionState{}, fmt.Errorf("%w: turn %q has status %q", enginepkg.ErrInteractionConflict, turnID, current.Status)
	}
	previousInteractions := h.run.Interactions
	next := cloneInteractionStateMap(previousInteractions)
	accepted := cloneInteractionState(*current)
	accepted.Status = enginepkg.InteractionStatusAnswered
	accepted.AnswerDigest = answerDigest
	accepted.Answer = append(json.RawMessage(nil), answer...)
	accepted.AnswerCommandID = commandID
	accepted.AnswerCommandDigest = commandDigest
	accepted.AcceptedAt = time.Now().UTC().Format(time.RFC3339Nano)
	accepted.AuditToken = turnID
	next[turnID] = &accepted
	h.run.Interactions = next
	if _, err := h.persistCompletedCheckpoint(ctx); err != nil {
		h.run.Interactions = previousInteractions
		return enginepkg.InteractionState{}, err
	}
	return cloneInteractionState(accepted), nil
}

func validateInteractionState(state enginepkg.InteractionState, answered bool) error {
	if state.SchemaVersion != enginepkg.InteractionStateSchemaV1 || state.TurnID == "" || state.NodeID == "" ||
		state.StepID == "" || state.Kind == "" || state.Ordinal < 1 || state.ExecutionInvocation < 0 ||
		state.RequestDigest == "" || len(state.Request) == 0 || !json.Valid(state.Request) {
		return fmt.Errorf("%w: invalid interaction proposal", enginepkg.ErrInteractionConflict)
	}
	if state.RequestDigest != enginepkg.InteractionPayloadDigest(state.Request) {
		return fmt.Errorf("%w: interaction request digest mismatch", enginepkg.ErrInteractionConflict)
	}
	if answered && (state.AnswerDigest == "" || len(state.Answer) == 0 || !json.Valid(state.Answer)) {
		return fmt.Errorf("%w: invalid interaction answer", enginepkg.ErrInteractionConflict)
	}
	if answered && state.AnswerDigest != enginepkg.InteractionPayloadDigest(state.Answer) {
		return fmt.Errorf("%w: interaction answer digest mismatch", enginepkg.ErrInteractionConflict)
	}
	return nil
}

func cloneInteractionState(state enginepkg.InteractionState) enginepkg.InteractionState {
	state.Request = append(json.RawMessage(nil), state.Request...)
	state.Answer = append(json.RawMessage(nil), state.Answer...)
	return state
}

func cloneInteractionStateMap(source map[string]*enginepkg.InteractionState) map[string]*enginepkg.InteractionState {
	result := make(map[string]*enginepkg.InteractionState, len(source))
	for turnID, state := range source {
		if state == nil {
			result[turnID] = nil
			continue
		}
		copy := cloneInteractionState(*state)
		result[turnID] = &copy
	}
	return result
}

func cloneDispatchState(state enginepkg.DispatchState) enginepkg.DispatchState {
	state.CallPath = append([]enginepkg.DebugCallFrame(nil), state.CallPath...)
	return state
}

func cloneDispatchStateMap(source map[string]*enginepkg.DispatchState) map[string]*enginepkg.DispatchState {
	result := make(map[string]*enginepkg.DispatchState, len(source))
	for occurrenceID, state := range source {
		if state == nil {
			result[occurrenceID] = nil
			continue
		}
		copy := cloneDispatchState(*state)
		result[occurrenceID] = &copy
	}
	return result
}

func cloneExecutionFrameState(state enginepkg.ExecutionFrameState) enginepkg.ExecutionFrameState {
	state.BindingScope = enginepkg.CloneBindingScope(state.BindingScope)
	state.RunResults = cloneResults(state.RunResults)
	state.CallPath = append([]enginepkg.DebugCallFrame(nil), state.CallPath...)
	state.StepIDs = append([]string(nil), state.StepIDs...)
	state.WorkingVars = cloneAnyMap(state.WorkingVars)
	results := state.Results
	state.Results = make(map[string]*enginepkg.StepResult, len(results))
	for index, result := range results {
		state.Results[index] = cloneStepResult(result)
	}
	return state
}

func cloneExecutionFrameStateMap(source map[string]*enginepkg.ExecutionFrameState) map[string]*enginepkg.ExecutionFrameState {
	result := make(map[string]*enginepkg.ExecutionFrameState, len(source))
	for frameID, state := range source {
		if state == nil {
			result[frameID] = nil
			continue
		}
		copy := cloneExecutionFrameState(*state)
		result[frameID] = &copy
	}
	return result
}

func cloneDynamicIncludeResolutionState(state enginepkg.DynamicIncludeResolutionState) enginepkg.DynamicIncludeResolutionState {
	state.CallPath = append([]enginepkg.DebugCallFrame(nil), state.CallPath...)
	state.Pin = cloneDebugValue(state.Pin).(schema.LockedDynamicInclude)
	return state
}

func cloneDynamicIncludeResolutionStateMap(
	source map[string]*enginepkg.DynamicIncludeResolutionState,
) map[string]*enginepkg.DynamicIncludeResolutionState {
	result := make(map[string]*enginepkg.DynamicIncludeResolutionState, len(source))
	for resolutionID, state := range source {
		if state == nil {
			result[resolutionID] = nil
			continue
		}
		copy := cloneDynamicIncludeResolutionState(*state)
		result[resolutionID] = &copy
	}
	return result
}

func cloneStepResult(source *enginepkg.StepResult) *enginepkg.StepResult {
	if source == nil {
		return nil
	}
	copy := *source
	copy.PublicOutputs = cloneAnyMap(source.PublicOutputs)
	copy.Results = cloneResults(source.Results)
	copy.Output = cloneAnyMap(source.Output)
	copy.Vars = cloneAnyMap(source.Vars)
	copy.Evidence = append([]evidence.EvidenceRecord(nil), source.Evidence...)
	if source.Indeterminate != nil {
		indeterminate := *source.Indeterminate
		copy.Indeterminate = &indeterminate
	}
	return &copy
}

func cloneStepResultMap(source map[string]*enginepkg.StepResult) map[string]*enginepkg.StepResult {
	if source == nil {
		return nil
	}
	result := make(map[string]*enginepkg.StepResult, len(source))
	for stepID, stepResult := range source {
		result[stepID] = cloneStepResult(stepResult)
	}
	return result
}

func cloneExecutionPlanForInspection(plan *enginepkg.ExecutionPlan) *enginepkg.ExecutionPlan {
	if plan == nil {
		return nil
	}
	if snapshot, err := plansnapshot.FromExecutionPlan(plan); err == nil {
		if restored, restoreErr := plansnapshot.Restore(snapshot); restoreErr == nil {
			return restored
		}
	}
	clone := *plan
	clone.Steps = append([]enginepkg.ResolvedStep(nil), plan.Steps...)
	for index := range clone.Steps {
		clone.Steps[index].Capture = cloneStringMap(plan.Steps[index].Capture)
		clone.Steps[index].CaptureDefaults = cloneAnyMap(plan.Steps[index].CaptureDefaults)
	}
	return &clone
}

func cloneStringMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func cloneAnyMap(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = cloneRuntimeValue(value)
	}
	return result
}

func cloneRuntimeValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneAnyMap(typed)
	case []any:
		cloned := make([]any, len(typed))
		for index, item := range typed {
			cloned[index] = cloneRuntimeValue(item)
		}
		return cloned
	case map[string]string:
		cloned := make(map[string]string, len(typed))
		for key, item := range typed {
			cloned[key] = item
		}
		return cloned
	case []string:
		return append([]string(nil), typed...)
	default:
		return value
	}
}

func isSHA256Digest(value string) bool {
	return strings.HasPrefix(value, "sha256:") && len(value) == 71
}

func (h *runHandle) clearInteractionsForOwner(ownerStepID string) {
	if len(h.run.Interactions) == 0 {
		return
	}
	next := make(map[string]*enginepkg.InteractionState, len(h.run.Interactions))
	for turnID, interaction := range h.run.Interactions {
		if interaction == nil || interaction.OwnerStepID != ownerStepID {
			next[turnID] = interaction
		}
	}
	h.run.Interactions = next
}

func (h *runHandle) emitCheckpoint(ctx context.Context, checkpointFile, stepID string) {
	if checkpointFile == "" {
		return
	}
	h.emitEventLocked(ctx, trace.EventKind("checkpoint"), map[string]any{
		"snapshot_file":     checkpointFile,
		"step_index":        h.run.CurrentStepIndex,
		"last_completed_id": stepID,
	})
}

func debugCanStepInto(step enginepkg.ResolvedStep) bool {
	switch step.Kind {
	case "branch":
		return true
	case "include":
		spec, ok := step.Spec.(*schema.IncludeSpec)
		return ok && spec != nil && !spec.Include.IsDynamic()
	default:
		return false
	}
}

var errDebugStopped = errors.New("debugger stopped run")

func (h *runHandle) nextDebugLocation(step enginepkg.ResolvedStep, invocation int) enginepkg.DebugLocation {
	return enginepkg.DebugLocation{
		RunID:       h.run.ID,
		RunbookPath: h.run.Plan.RunbookPath,
		CallPath:    append([]enginepkg.DebugCallFrame(nil), h.debugCallPath...),
		StepID:      step.ID,
		Invocation:  invocation,
		Attempt:     1,
	}
}

func executionInvocationTracker(ctx context.Context, debugger enginepkg.DebugController, routeTest enginepkg.RouteTestController) *enginepkg.DebugInvocationTracker {
	_, _ = debugger, routeTest
	if tracker := enginepkg.DebugInvocationTrackerFromContext(ctx); tracker != nil {
		return tracker
	}
	return enginepkg.NewDebugInvocationTracker()
}

func executionInvocationTrackerForResume(ctx context.Context, state enginepkg.RunState) *enginepkg.DebugInvocationTracker {
	if tracker := enginepkg.DebugInvocationTrackerFromContext(ctx); tracker != nil {
		return tracker
	}
	tracker := enginepkg.NewDebugInvocationTrackerFromCounts(state.ExecutionInvocationCounts)
	if state.CursorSet == nil {
		return tracker
	}
	rewound := make(map[string]bool)
	for _, cursor := range state.CursorSet.Cursors {
		if cursor.Phase != enginepkg.ExecutionPhaseBefore {
			continue
		}
		for index, frame := range cursor.CallPath {
			path := cursor.CallPath[:index]
			qualifiedNodeID := enginepkg.DebugNodeID(path, frame.StepID)
			if !rewound[qualifiedNodeID] && tracker.Current(path, frame.StepID) > 0 {
				tracker.Rewind(path, frame.StepID)
				rewound[qualifiedNodeID] = true
			}
		}
		qualifiedNodeID := enginepkg.DebugNodeID(cursor.CallPath, cursor.StepID)
		if !rewound[qualifiedNodeID] && tracker.Current(cursor.CallPath, cursor.StepID) == cursor.Invocation {
			tracker.Rewind(cursor.CallPath, cursor.StepID)
			rewound[qualifiedNodeID] = true
		}
	}
	return tracker
}

func interactionInvocationTracker(ctx context.Context) *enginepkg.InteractionInvocationTracker {
	if tracker := enginepkg.InteractionInvocationTrackerFromContext(ctx); tracker != nil {
		return tracker
	}
	return enginepkg.NewInteractionInvocationTracker()
}

func interactionInvocationTrackerForResume(ctx context.Context, state enginepkg.RunState) *enginepkg.InteractionInvocationTracker {
	if tracker := enginepkg.InteractionInvocationTrackerFromContext(ctx); tracker != nil {
		return tracker
	}
	tracker := enginepkg.NewInteractionInvocationTrackerFromCounts(state.InteractionInvocationCounts)
	for _, interaction := range state.Interactions {
		if interaction != nil {
			tracker.RewindOccurrence(
				interaction.NodeID, interaction.Kind, interaction.FrameID, interaction.FrameStepIndex,
			)
		}
	}
	return tracker
}

func (h *runHandle) debugDecision(ctx context.Context, snapshot enginepkg.DebugSnapshot) (enginepkg.DebugDecision, error) {
	if err := h.publishBoundaryLocked(ctx); err != nil {
		return enginepkg.DebugDecision{}, err
	}
	h.mu.Unlock()
	var decision enginepkg.DebugDecision
	var err error
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				err = fmt.Errorf("debugger panic: %v", recovered)
			}
		}()
		decision, err = h.debugger.Pause(newDebugControllerContext(ctx, h.run.ID), cloneDebugSnapshot(snapshot))
	}()
	h.mu.Lock()
	if contextErr := ctx.Err(); contextErr != nil {
		err = contextErr
	}
	if err != nil {
		return enginepkg.DebugDecision{}, fmt.Errorf("debugger: %w", err)
	}
	if h.runCtx.Err() != nil {
		return enginepkg.DebugDecision{}, h.runCtx.Err()
	}
	if decision.Action == enginepkg.DebugActionStop {
		h.stopDebugRunLocked(ctx)
		return enginepkg.DebugDecision{}, errDebugStopped
	}
	if decision.Action != "" && decision.Action != enginepkg.DebugActionContinue {
		return enginepkg.DebugDecision{}, fmt.Errorf("debugger: unsupported action %q", decision.Action)
	}
	return decision, nil
}

func cloneDebugSnapshot(snapshot enginepkg.DebugSnapshot) enginepkg.DebugSnapshot {
	clone := snapshot
	clone.Location.CallPath = append([]enginepkg.DebugCallFrame(nil), snapshot.Location.CallPath...)
	clone.Vars = cloneDebugMap(snapshot.Vars)
	clone.ProtectedVars = append([]string(nil), snapshot.ProtectedVars...)
	clone.Actual = cloneDebugStepResult(snapshot.Actual)
	return clone
}

type debugControllerContext struct {
	cancellation context.Context
	values       context.Context
}

func newDebugControllerContext(ctx context.Context, runID string) context.Context {
	return debugControllerContext{
		cancellation: ctx,
		values:       enginepkg.WithRunID(context.Background(), runID),
	}
}

func (ctx debugControllerContext) Deadline() (time.Time, bool) { return ctx.cancellation.Deadline() }
func (ctx debugControllerContext) Done() <-chan struct{}       { return ctx.cancellation.Done() }
func (ctx debugControllerContext) Err() error                  { return ctx.cancellation.Err() }
func (ctx debugControllerContext) Value(key any) any           { return ctx.values.Value(key) }

func (h *runHandle) stopDebugRunLocked(ctx context.Context) {
	if h.done.Load() {
		return
	}
	h.cancelFn()
	_ = h.cancelRunLocked(context.WithoutCancel(ctx), "debugger-stop", "run cancelled by debugger", nil)
}

func (h *runHandle) cancelDebugPauseRunLocked(ctx context.Context, cause error) {
	if h.done.Load() {
		return
	}
	reason := "debugger-pause-cancelled"
	if errors.Is(cause, context.DeadlineExceeded) {
		reason = "debugger-pause-deadline"
	}
	h.cancelFn()
	_ = h.cancelRunLocked(context.WithoutCancel(ctx), reason, "run cancelled during debugger pause", nil)
}

func (h *runHandle) applyDebugVars(vars map[string]any) {
	for key, value := range vars {
		h.run.Vars[key] = cloneDebugValue(value)
	}
}

type debugProtectionProvider interface {
	ResolveDebugProtection(context.Context, enginepkg.ResolvedStep, map[string]any) (enginepkg.DebugProtection, bool)
}

func (h *runHandle) debugProtectionBeforeStep(
	ctx context.Context,
	step enginepkg.ResolvedStep,
	protectUnresolvedInclude bool,
) enginepkg.DebugProtection {
	executor := h.engine.cfg.Executors.Lookup(step.Kind)
	if provider, ok := executor.(debugProtectionProvider); ok {
		if additional, resolved := provider.ResolveDebugProtection(ctx, step, h.run.Vars); resolved {
			h.debugProtection = enginepkg.MergeDebugProtection(h.debugProtection, additional)
			return h.effectiveDebugProtection(h.debugProtection)
		}
	}
	if step.Kind != "include" || !protectUnresolvedInclude {
		return h.effectiveDebugProtection(h.debugProtection)
	}
	h.debugProtectAllVars = true
	return h.effectiveDebugProtection(h.debugProtection)
}

func (h *runHandle) mergePublishedDebugProtection(additional enginepkg.DebugProtection) {
	h.mu.Lock()
	h.debugProtection = enginepkg.MergeDebugProtection(h.debugProtection, additional)
	h.mu.Unlock()
}

func (h *runHandle) sanitizedDebugSnapshot(phase enginepkg.DebugPhase, location enginepkg.DebugLocation, actual *enginepkg.StepResult) enginepkg.DebugSnapshot {
	return h.sanitizedDebugSnapshotWithProtection(phase, location, actual, h.debugProtection)
}

func (h *runHandle) sanitizedDebugSnapshotWithProtection(phase enginepkg.DebugPhase, location enginepkg.DebugLocation, actual *enginepkg.StepResult, protection enginepkg.DebugProtection) enginepkg.DebugSnapshot {
	return h.sanitizedDebugSnapshotWithOptions(phase, location, actual, protection, false)
}

func (h *runHandle) sanitizedDebugSnapshotWithOptions(phase enginepkg.DebugPhase, location enginepkg.DebugLocation, actual *enginepkg.StepResult, protection enginepkg.DebugProtection, protectActual bool) enginepkg.DebugSnapshot {
	protection = h.effectiveDebugProtection(protection)
	vars, protectedVars := h.sanitizedDebugVarsWithProtection(protection)
	snapshot := enginepkg.DebugSnapshot{
		Phase:         phase,
		Location:      location,
		Vars:          vars,
		ProtectedVars: protectedVars,
	}
	if actual != nil {
		safeActual := *actual
		safeActual.Evidence = nil
		if actual.Indeterminate != nil {
			indeterminate := *actual.Indeterminate
			safeActual.Indeterminate = &indeterminate
		}
		if protectActual {
			safeActual.Output = map[string]any{"_debug_protected": true}
			safeActual.Vars = maskProtectedDebugVars(actual.Vars, protection, true)
			if actual.Error != nil {
				safeActual.Error = errors.New("<redacted>")
			}
			snapshot.OutputProtected = true
		} else {
			outputSanitizer := h.newDebugSnapshotSanitizerWithProtection(protection)
			safeActual.Output = outputSanitizer.mapValue(actual.Output, 0)
			varsSanitizer := h.newDebugSnapshotSanitizerWithProtection(protection)
			safeActual.Vars = varsSanitizer.mapValue(maskProtectedDebugVars(actual.Vars, protection, h.debugProtectAllVars), 0)
			if actual.Error != nil {
				errorSanitizer := h.newDebugSnapshotSanitizerWithProtection(protection)
				safeActual.Error = errors.New(errorSanitizer.stringValue(actual.Error.Error()))
				snapshot.OutputProtected = outputSanitizer.redacted || errorSanitizer.redacted
			} else {
				snapshot.OutputProtected = outputSanitizer.redacted
			}
		}
		if actual.Error != nil && safeActual.Error == nil {
			errorSanitizer := h.newDebugSnapshotSanitizerWithProtection(protection)
			safeActual.Error = errors.New(errorSanitizer.stringValue(actual.Error.Error()))
		}
		snapshot.Actual = &safeActual
	}
	return snapshot
}

func (h *runHandle) debugProtectionForActual(step enginepkg.ResolvedStep, actual *enginepkg.StepResult) (enginepkg.DebugProtection, bool) {
	protection := h.effectiveDebugProtection(h.debugProtection)
	var complete bool
	protection, complete = extendProtectionWithDebugVars(protection, actual.Vars, h.debugProtectAllVars)
	protectActual := !complete
	if !complete {
		h.debugProtectAllVars = true
		return h.effectiveDebugProtection(protection), true
	}

	protected := make(map[string]bool, len(protection.ProtectedVars))
	for _, name := range protection.ProtectedVars {
		protected[name] = true
	}
	hasProtectedCapture := false
	for name := range step.Capture {
		if h.debugProtectAllVars || protected[name] {
			hasProtectedCapture = true
			break
		}
	}
	if !hasProtectedCapture {
		return protection, protectActual
	}
	captures, err := capture.New(h.run.Vars, h.run.StepResults, nil).CaptureStep(h.run.Plan.Validation, step, actual)
	if err != nil {
		return protection, true
	}
	protectedCaptures := make(map[string]any)
	for name, value := range capture.ToAnyMap(captures) {
		if h.debugProtectAllVars || protected[name] {
			protectedCaptures[name] = value
		}
	}
	protection, complete = extendProtectionWithDebugVars(protection, protectedCaptures, h.debugProtectAllVars)
	if !complete {
		h.debugProtectAllVars = true
		protection = h.effectiveDebugProtection(protection)
	}
	return protection, protectActual || !complete
}

func extendProtectionWithDebugVars(protection enginepkg.DebugProtection, vars map[string]any, protectAll bool) (enginepkg.DebugProtection, bool) {
	protected := make(map[string]bool, len(protection.ProtectedVars))
	for _, name := range protection.ProtectedVars {
		protected[name] = true
	}
	additional := enginepkg.DebugProtection{}
	complete := true
	for name, value := range vars {
		if !protectAll && !protected[name] {
			continue
		}
		additional.ProtectedVars = append(additional.ProtectedVars, name)
		if secret, ok := value.(string); ok {
			if secret != "" {
				additional.SecretValues = append(additional.SecretValues, secret)
			}
			continue
		}
		if value != nil {
			complete = false
		}
	}
	return enginepkg.MergeDebugProtection(protection, additional), complete
}

func maskProtectedDebugVars(vars map[string]any, protection enginepkg.DebugProtection, protectAll bool) map[string]any {
	if vars == nil {
		return nil
	}
	protected := make(map[string]bool, len(protection.ProtectedVars))
	for _, name := range protection.ProtectedVars {
		protected[name] = true
	}
	masked := make(map[string]any, len(vars))
	for name, value := range vars {
		if protectAll || protected[name] {
			masked[name] = "<redacted>"
			continue
		}
		masked[name] = value
	}
	return masked
}

func (h *runHandle) sanitizedDebugVars() (map[string]any, []string) {
	return h.sanitizedDebugVarsWithProtection(h.debugProtection)
}

func (h *runHandle) sanitizedDebugVarsWithProtection(protection enginepkg.DebugProtection) (map[string]any, []string) {
	protection = h.effectiveDebugProtection(protection)
	vars := make(map[string]any, len(h.run.Vars))
	protected := make(map[string]bool)
	for _, name := range protection.ProtectedVars {
		protected[name] = true
	}
	for name, value := range h.run.Vars {
		if protected[name] {
			vars[name] = "<redacted>"
			continue
		}
		sanitizer := h.newDebugSnapshotSanitizerWithProtection(protection)
		vars[name] = sanitizer.value(reflect.ValueOf(value), 0)
		if sanitizer.redacted {
			protected[name] = true
		}
	}
	keys := make([]string, 0, len(protected))
	for name := range protected {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	return vars, keys
}

const (
	debugSnapshotMaxDepth       = 8
	debugSnapshotMaxNodes       = 512
	debugSnapshotMaxEntries     = 64
	debugSnapshotMaxStringBytes = 4096
	debugSnapshotMaxBytes       = 64 * 1024
)

type debugSnapshotSanitizer struct {
	redactor  *internalGov.Redactor
	secrets   []string
	nodes     int
	bytes     int
	redacted  bool
	truncated bool
}

func (h *runHandle) newDebugSnapshotSanitizer() *debugSnapshotSanitizer {
	return h.newDebugSnapshotSanitizerWithProtection(h.debugProtection)
}

func (h *runHandle) newDebugSnapshotSanitizerWithProtection(protection enginepkg.DebugProtection) *debugSnapshotSanitizer {
	protection = h.effectiveDebugProtection(protection)
	secrets := append([]string(nil), protection.SecretValues...)
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	return &debugSnapshotSanitizer{redactor: debugRedactor(protection), secrets: secrets}
}

func (s *debugSnapshotSanitizer) mapValue(values map[string]any, depth int) map[string]any {
	if values == nil {
		return nil
	}
	normalized := s.value(reflect.ValueOf(values), depth)
	if result, ok := normalized.(map[string]any); ok {
		return result
	}
	return map[string]any{"_debug_truncated": true}
}

func (s *debugSnapshotSanitizer) value(value reflect.Value, depth int) any {
	if !value.IsValid() {
		return nil
	}
	for value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return nil
		}
		value = value.Elem()
	}
	s.nodes++
	if depth > debugSnapshotMaxDepth || s.nodes > debugSnapshotMaxNodes || s.bytes >= debugSnapshotMaxBytes {
		s.truncated = true
		return map[string]any{"_debug_truncated": true}
	}
	switch value.Kind() {
	case reflect.String:
		return s.stringValue(value.String())
	case reflect.Bool:
		return value.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return value.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return value.Uint()
	case reflect.Float32, reflect.Float64:
		floating := value.Float()
		if math.IsNaN(floating) || math.IsInf(floating, 0) {
			return s.stringValue(fmt.Sprint(floating))
		}
		return floating
	case reflect.Map:
		if value.Type().Key().Kind() != reflect.String {
			s.truncated = true
			return map[string]any{"_debug_truncated": true}
		}
		keys := value.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
		result := make(map[string]any, min(len(keys), debugSnapshotMaxEntries)+1)
		for index, key := range keys {
			if index >= debugSnapshotMaxEntries || s.bytes >= debugSnapshotMaxBytes {
				s.truncated = true
				result["_debug_truncated"] = true
				break
			}
			name := s.stringValue(key.String())
			result[name] = s.value(value.MapIndex(key), depth+1)
		}
		return result
	case reflect.Slice, reflect.Array:
		limit := min(value.Len(), debugSnapshotMaxEntries)
		result := make([]any, 0, limit+1)
		for index := 0; index < limit && s.bytes < debugSnapshotMaxBytes; index++ {
			result = append(result, s.value(value.Index(index), depth+1))
		}
		if limit < value.Len() || s.bytes >= debugSnapshotMaxBytes {
			s.truncated = true
			result = append(result, map[string]any{"_debug_truncated": true, "_debug_omitted": value.Len() - limit})
		}
		return result
	case reflect.Struct:
		result := make(map[string]any)
		typeInfo := value.Type()
		for index := 0; index < value.NumField() && len(result) < debugSnapshotMaxEntries; index++ {
			fieldInfo := typeInfo.Field(index)
			if fieldInfo.PkgPath != "" {
				continue
			}
			name := fieldInfo.Name
			if tag := fieldInfo.Tag.Get("json"); tag != "" {
				parts := strings.Split(tag, ",")
				if parts[0] == "-" {
					continue
				}
				if parts[0] != "" {
					name = parts[0]
				}
			}
			result[s.stringValue(name)] = s.value(value.Field(index), depth+1)
		}
		return result
	default:
		s.truncated = true
		return s.stringValue(fmt.Sprint(value.Interface()))
	}
}

func (s *debugSnapshotSanitizer) stringValue(value string) string {
	for _, secret := range s.secrets {
		if strings.Contains(value, secret) {
			value = strings.ReplaceAll(value, secret, "<redacted>")
			s.redacted = true
		}
	}
	if s.redactor != nil {
		redacted, matches := s.redactor.RedactString(value)
		if matches > 0 {
			value = redacted
			s.redacted = true
		}
	}
	remaining := debugSnapshotMaxBytes - s.bytes
	limit := min(debugSnapshotMaxStringBytes, remaining)
	if limit < 0 {
		limit = 0
	}
	if len(value) > limit {
		value = value[:limit]
		s.truncated = true
	}
	s.bytes += len(value)
	return value
}

func (h *runHandle) debugRedactor() *internalGov.Redactor {
	return debugRedactor(h.debugProtection)
}

func debugRedactor(protection enginepkg.DebugProtection) *internalGov.Redactor {
	if len(protection.RedactionPatterns) == 0 {
		return nil
	}
	redactor, err := internalGov.NewRedactor(protection.RedactionPatterns)
	if err != nil {
		return nil
	}
	return redactor
}

func buildDebugProtection(ctx context.Context, plan *enginepkg.ExecutionPlan, vars map[string]any) enginepkg.DebugProtection {
	var inputs map[string]*schema.Input
	var policy governance.GovernancePolicy
	if plan != nil {
		inputs = plan.Inputs
		policy = plan.Governance
		if policy == nil && plan.GovernanceSource != nil {
			policy = internalGov.BuildPolicy(plan.GovernanceSource)
		}
	}
	inherited := internaldebugprotect.ProtectionFromContext(ctx)
	for _, name := range inherited.ProtectedVars {
		if secret, ok := vars[name].(string); ok && secret != "" {
			inherited.SecretValues = append(inherited.SecretValues, secret)
		}
	}
	return enginepkg.ExtendDebugProtection(inherited, vars, inputs, policy)
}

func precomputeDebugProtection(ctx context.Context, registry enginepkg.ExecutorRegistry, plan *enginepkg.ExecutionPlan, vars map[string]any, protection enginepkg.DebugProtection) (enginepkg.DebugProtection, bool) {
	if registry == nil || plan == nil {
		return protection, true
	}
	complete := true
	for _, step := range plan.Steps {
		if step.Kind != "include" {
			continue
		}
		provider, ok := registry.Lookup(step.Kind).(debugProtectionProvider)
		if !ok {
			complete = false
			continue
		}
		providerCtx := internaldebugprotect.WithProtection(ctx, protection)
		additional, resolved := provider.ResolveDebugProtection(providerCtx, step, vars)
		protection = enginepkg.MergeDebugProtection(protection, additional)
		complete = complete && resolved
	}
	return protection, complete
}

func allDebugVariablesProtection(vars map[string]any) enginepkg.DebugProtection {
	protection := enginepkg.DebugProtection{}
	for name, value := range vars {
		protection.ProtectedVars = append(protection.ProtectedVars, name)
		if secret, ok := value.(string); ok && secret != "" {
			protection.SecretValues = append(protection.SecretValues, secret)
		}
	}
	return enginepkg.MergeDebugProtection(enginepkg.DebugProtection{}, protection)
}

func (h *runHandle) effectiveDebugProtection(protection enginepkg.DebugProtection) enginepkg.DebugProtection {
	if !h.debugProtectAllVars {
		return protection
	}
	return enginepkg.MergeDebugProtection(protection, allDebugVariablesProtection(h.run.Vars))
}

func validateDebugDecision(snapshot enginepkg.DebugSnapshot, decision enginepkg.DebugDecision, protectAllVars ...bool) error {
	if snapshot.Phase == enginepkg.DebugPhaseBefore && decision.Result != nil {
		return errors.New("debugger: result override is only valid after execution")
	}
	protected := make(map[string]bool, len(snapshot.ProtectedVars))
	for _, name := range snapshot.ProtectedVars {
		protected[name] = true
	}
	for name := range decision.Vars {
		if len(protectAllVars) > 0 && protectAllVars[0] {
			return errors.New("debugger: variable overrides are unavailable for a dynamically protected run")
		}
		if protected[name] {
			return fmt.Errorf("debugger: protected variable %q cannot be overridden", name)
		}
	}
	if snapshot.Actual != nil &&
		(snapshot.Actual.Status == enginepkg.StepStatusDenied || snapshot.Actual.Status == enginepkg.StepStatusIndeterminate) &&
		(len(decision.Vars) > 0 || decision.Result != nil) {
		return fmt.Errorf("debugger: actual status %q is immutable", snapshot.Actual.Status)
	}
	if snapshot.OutputProtected && decision.Result != nil && decision.Result.OutputPatch != nil {
		return errors.New("debugger: redacted output cannot be overridden")
	}
	return nil
}

func (h *runHandle) emitDebugOverrideLocked(ctx context.Context, stepID string, snapshot enginepkg.DebugSnapshot, effectiveResult *enginepkg.StepResult, protectActual bool) {
	effectiveVars, _ := h.sanitizedDebugVars()
	payload := map[string]any{
		"step_id":        stepID,
		"phase":          string(snapshot.Phase),
		"call_path":      snapshot.Location.CallPath,
		"invocation":     snapshot.Location.Invocation,
		"attempt":        snapshot.Location.Attempt,
		"actual_vars":    snapshot.Vars,
		"effective_vars": effectiveVars,
	}
	if snapshot.Actual != nil {
		payload["actual"] = debugResultPayload(snapshot.Actual)
	}
	if effectiveResult != nil {
		sanitized := h.sanitizedDebugSnapshotWithOptions(snapshot.Phase, snapshot.Location, effectiveResult, h.debugProtection, protectActual)
		payload["effective"] = debugResultPayload(sanitized.Actual)
	}
	h.emitEventLocked(ctx, trace.EventKind("debug/override_applied"), payload)
}

func applyDebugResultOverride(result *enginepkg.StepResult, override *enginepkg.DebugResultOverride) (*enginepkg.StepResult, error) {
	effective := cloneDebugStepResult(result)
	if override.OutputPatch != nil {
		effective.Output = mergeDebugMap(effective.Output, override.OutputPatch)
	}
	if override.Status != "" {
		effective.Status = override.Status
		switch override.Status {
		case enginepkg.StepStatusCompleted:
			effective.Outcome = enginepkg.StepOutcomeSuccess
			effective.Error = nil
			effective.Indeterminate = nil
		case enginepkg.StepStatusFailed:
			effective.Outcome = enginepkg.StepOutcomeFailed
			effective.Indeterminate = nil
			if override.Error != "" {
				effective.Error = errors.New(override.Error)
			} else if effective.Error == nil {
				effective.Error = errors.New("debug override forced step failure")
			}
		case enginepkg.StepStatusSkipped:
			effective.Outcome = enginepkg.StepOutcomeSkipped
			effective.Error = nil
			effective.Indeterminate = nil
		default:
			return nil, fmt.Errorf("debugger: status %q cannot be overridden", override.Status)
		}
	} else if override.Error != "" {
		if effective.Status != enginepkg.StepStatusFailed {
			return nil, errors.New("debugger: error text requires failed status")
		}
		effective.Error = errors.New(override.Error)
	}
	return effective, nil
}

func mergeDebugMap(current, patch map[string]any) map[string]any {
	merged := cloneDebugMap(current)
	if merged == nil {
		merged = make(map[string]any)
	}
	for key, patchValue := range patch {
		if patchValue == nil {
			delete(merged, key)
			continue
		}
		if patchObject, ok := patchValue.(map[string]any); ok {
			currentObject, _ := merged[key].(map[string]any)
			merged[key] = mergeDebugMap(currentObject, patchObject)
			continue
		}
		merged[key] = cloneDebugValue(patchValue)
	}
	return merged
}

func cloneDebugStepResult(result *enginepkg.StepResult) *enginepkg.StepResult {
	if result == nil {
		return nil
	}
	clone := *result
	clone.PublicOutputs = cloneDebugMap(result.PublicOutputs)
	clone.Results = cloneResults(result.Results)
	clone.Output = cloneDebugMap(result.Output)
	clone.Vars = cloneDebugMap(result.Vars)
	clone.Evidence = make([]evidence.EvidenceRecord, len(result.Evidence))
	for i, record := range result.Evidence {
		clone.Evidence[i] = record
		if record.Items != nil {
			clone.Evidence[i].Items = make(map[string]string, len(record.Items))
			for key, value := range record.Items {
				clone.Evidence[i].Items[key] = value
			}
		}
	}
	if result.Indeterminate != nil {
		indeterminate := *result.Indeterminate
		clone.Indeterminate = &indeterminate
	}
	return &clone
}

func cloneDebugMap(values map[string]any) map[string]any {
	if values == nil {
		return nil
	}
	clone := make(map[string]any, len(values))
	for key, value := range values {
		clone[key] = cloneDebugValue(value)
	}
	return clone
}

func cloneDebugValue(value any) any {
	if value == nil {
		return nil
	}
	return cloneDebugReflect(reflect.ValueOf(value)).Interface()
}

func cloneDebugReflect(value reflect.Value) reflect.Value {
	if !value.IsValid() {
		return value
	}
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := cloneDebugReflect(value.Elem())
		out := reflect.New(value.Type()).Elem()
		out.Set(cloned)
		return out
	case reflect.Pointer:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		out := reflect.New(value.Type().Elem())
		out.Elem().Set(cloneDebugReflect(value.Elem()))
		return out
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		out := reflect.MakeMapWithSize(value.Type(), value.Len())
		iterator := value.MapRange()
		for iterator.Next() {
			out.SetMapIndex(cloneDebugReflect(iterator.Key()), cloneDebugReflect(iterator.Value()))
		}
		return out
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		out := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for i := 0; i < value.Len(); i++ {
			out.Index(i).Set(cloneDebugReflect(value.Index(i)))
		}
		return out
	case reflect.Array:
		out := reflect.New(value.Type()).Elem()
		for i := 0; i < value.Len(); i++ {
			out.Index(i).Set(cloneDebugReflect(value.Index(i)))
		}
		return out
	default:
		return value
	}
}

func debugResultPayload(result *enginepkg.StepResult) map[string]any {
	if result == nil {
		return nil
	}
	payload := map[string]any{
		"status": string(result.Status),
		"output": cloneDebugMap(result.Output),
	}
	if result.Error != nil {
		payload["error"] = result.Error.Error()
	}
	return payload
}

func traceOccurrencePayload(ctx context.Context, stepID string, payload map[string]any) map[string]any {
	result := make(map[string]any, len(payload)+7)
	for key, value := range payload {
		result[key] = value
	}
	if _, terminal := payload["output"]; terminal {
		attachToolPresentation(ctx, result)
	}
	callPath := enginepkg.DebugCallPathFromContext(ctx)
	qualifiedNodeID := enginepkg.DebugNodeID(callPath, stepID)
	invocation, retryAttempt := 1, 1
	occurrenceSequence := int64(1)
	phase := enginepkg.ExecutionPhaseExecute
	if boundary, found := enginepkg.DispatchExecutionBoundaryFromContext(ctx); found {
		if boundary.FrameID != "" {
			result["frame_id"] = boundary.FrameID
			result["frame_step_index"] = boundary.FrameStepIndex
		}
		callPath = append([]enginepkg.DebugCallFrame(nil), boundary.CallPath...)
		if boundary.QualifiedNodeID != "" {
			qualifiedNodeID = boundary.QualifiedNodeID
		}
		if boundary.Invocation > 0 {
			invocation = boundary.Invocation
		}
		if boundary.RetryAttempt > 0 {
			retryAttempt = boundary.RetryAttempt
		}
		if boundary.OccurrenceSequence > 0 {
			occurrenceSequence = boundary.OccurrenceSequence
		}
		if boundary.Phase != "" {
			phase = boundary.Phase
		}
	}
	result["qualified_node_id"] = qualifiedNodeID
	result["call_path"] = callPath
	result["structural_path"] = enginepkg.DynamicIncludeStructuralPathFromContext(ctx)
	result["invocation"] = invocation
	result["retry_attempt"] = retryAttempt
	result["occurrence_sequence"] = occurrenceSequence
	result["phase"] = phase
	return result
}

func mustJSON(value any) []byte {
	encoded, _ := json.Marshal(value)
	return encoded
}

// executeParallel runs all branches concurrently via errgroup.
// Trace events publish as branches run; each occurrence preserves causal order.
// The caller must hold h.mu.
func (h *runHandle) executeParallel(ctx context.Context, step enginepkg.ResolvedStep, branches []enginepkg.BranchSpec, join *schema.ParallelJoin) (*enginepkg.StepResult, error) {
	// Start step-level span for the parallel step.
	spanCtx, stepSpan := h.engine.tracer().Start(ctx, "yawr.step.parallel",
		otelPkg.WithAttributes(
			otelPkg.Attribute{Key: otelPkg.AttrStepID, Value: step.ID},
			otelPkg.Attribute{Key: otelPkg.AttrStepKind, Value: "parallel"},
		),
	)
	defer stepSpan.End()

	h.emitEventLocked(ctx, trace.EventKindStepStarted, traceOccurrencePayload(ctx, step.ID, map[string]any{
		"step_id":  step.ID,
		"kind":     "parallel",
		"branches": len(branches),
	}))
	if h.traceErr != nil {
		return nil, h.traceErr
	}

	startedAt := time.Now()

	type branchOutcome struct {
		results []*enginepkg.StepResult
		err     error
	}
	outcomes := make([]branchOutcome, len(branches))

	// Each start is acknowledged before scheduling any branch work.
	if err := h.publishBoundaryLocked(ctx); err != nil {
		return nil, err
	}
	h.mu.Unlock()

	eg, branchCtx := errgroup.WithContext(spanCtx)
	for i, branch := range branches {
		i, branch := i, branch // capture loop vars
		// Start a branch span as child of the parallel step span.
		bLabel := branch.Label
		if bLabel == "" {
			bLabel = fmt.Sprintf("branch-%d", i)
		}
		branchSpanCtx, branchSpan := h.engine.tracer().Start(branchCtx, "yawr.branch."+bLabel)
		branchSpanCtx = enginepkg.WithConcurrentExecution(branchSpanCtx)
		eg.Go(func() error {
			defer branchSpan.End()
			branchVars := h.snapshotVars()
			branchRunner := &branchHandle{
				engine: h.engine, run: h.run, parent: h, vars: branchVars, stepCtx: branchSpanCtx,
			}
			callPath := enginepkg.DebugCallPathFromContext(branchSpanCtx)
			childCallPath := append(append([]enginepkg.DebugCallFrame(nil), callPath...), enginepkg.DebugCallFrame{StepID: step.ID})
			branchRunner.callPath = childCallPath
			structuralPath := enginepkg.DynamicIncludeStructuralPathFromContext(branchSpanCtx)
			containerInvocation := 1
			if boundary, found := enginepkg.DispatchExecutionBoundaryFromContext(branchSpanCtx); found && boundary.Invocation > 0 {
				containerInvocation = boundary.Invocation
			}
			structuralPath = append(structuralPath, schema.DynamicIncludeFrameIdentity{
				QualifiedNodeID: enginepkg.DebugNodeID(callPath, step.ID), Kind: "parallel",
				BranchLabel: bLabel, Invocation: containerInvocation,
			})
			branchSpanCtx = enginepkg.WithDynamicIncludeStructuralPath(branchSpanCtx, structuralPath)
			if committer := enginepkg.ExecutionFrameCommitterFromContext(branchSpanCtx); committer != nil {
				definitionDigest, digestErr := enginepkg.ExecutionFrameDefinitionDigest(branch.Steps)
				if digestErr != nil {
					outcomes[i] = branchOutcome{err: digestErr}
					return digestErr
				}
				parentFrameID := ""
				if binding, ok := enginepkg.ExecutionFrameBindingFromContext(branchSpanCtx); ok {
					parentFrameID = binding.FrameID
				}
				frame, frameErr := committer.BeginExecutionFrame(context.WithoutCancel(branchSpanCtx), enginepkg.ExecutionFrameRequest{
					ParentFrameID: parentFrameID, ParentQualifiedNodeID: enginepkg.DebugNodeID(callPath, step.ID),
					ParentStepID: step.ID, Kind: "parallel", CallPath: childCallPath,
					BranchLabel: bLabel, DefinitionDigest: definitionDigest, StepCount: len(branch.Steps),
					StepIDs:      enginepkg.ExecutionFrameStepIDs(branch.Steps),
					InitialVars:  branchVars,
					BindingScope: enginepkg.BindingScopeFromContext(branchSpanCtx),
				})
				if frameErr != nil {
					outcomes[i] = branchOutcome{err: frameErr}
					return frameErr
				}
				branchRunner.vars = cloneAnyMap(frame.WorkingVars)
				branchRunner.callPath = append([]enginepkg.DebugCallFrame(nil), frame.CallPath...)
				branchRunner.frameBinding = enginepkg.ExecutionFrameBinding{FrameID: frame.FrameID, Invocation: frame.Invocation}
				structuralPath[len(structuralPath)-1] = schema.DynamicIncludeFrameIdentity{
					QualifiedNodeID: frame.ParentQualifiedNodeID, Kind: frame.Kind,
					BranchLabel: frame.BranchLabel, IterationIndex: frame.IterationIndex,
					Invocation: frame.Invocation,
				}
				branchSpanCtx = enginepkg.WithDynamicIncludeStructuralPath(branchSpanCtx, structuralPath)
				branchRunner.startIndex = frame.NextStepIndex
				branchRunner.priorResults = make([]*enginepkg.StepResult, 0, frame.NextStepIndex)
				for resultIndex := 0; resultIndex < frame.NextStepIndex; resultIndex++ {
					prior := frame.Results[strconv.Itoa(resultIndex)]
					if prior == nil {
						frameErr = fmt.Errorf("execution frame %s is missing result %d", frame.FrameID, resultIndex)
						outcomes[i] = branchOutcome{err: frameErr}
						return frameErr
					}
					branchRunner.priorResults = append(branchRunner.priorResults, prior)
				}
			}
			results, err := branchRunner.executeSteps(branchSpanCtx, branch.Steps)
			outcomes[i] = branchOutcome{results: results, err: err}
			if err != nil {
				branchSpan.RecordError(err)
				branchSpan.SetStatus(otelPkg.StatusError, err.Error())
				return err
			}
			branchSpan.SetStatus(otelPkg.StatusOK, "")
			return nil
		})
	}

	joinErr := eg.Wait()

	h.mu.Lock()
	for _, outcome := range outcomes {
		if enginepkg.IsReplayBoundaryError(outcome.err) {
			return nil, outcome.err
		}
		if enginepkg.IsRouteTestBoundaryError(outcome.err) {
			return nil, outcome.err
		}
	}
	if errors.Is(joinErr, enginepkg.ErrCheckpointCommit) || errors.Is(joinErr, enginepkg.ErrTraceCommit) {
		return nil, joinErr
	}

	for _, o := range outcomes {
		for _, branchResult := range o.results {
			if branchResult != nil {
				h.run.StepResults[branchResult.StepID] = branchResult
			}
		}
	}
	if h.traceErr != nil {
		return nil, h.traceErr
	}
	for _, outcome := range outcomes {
		if errors.Is(outcome.err, enginepkg.ErrIndeterminate) {
			return &enginepkg.StepResult{
				StepID: step.ID, Status: enginepkg.StepStatusIndeterminate, Outcome: enginepkg.StepOutcomeFailed,
				StartedAt: startedAt, CompletedAt: time.Now(), DurationMs: time.Since(startedAt).Milliseconds(),
				Error: enginepkg.ErrIndeterminate,
			}, enginepkg.ErrIndeterminate
		}
	}

	durationMs := time.Since(startedAt).Milliseconds()
	branchFailures := make([]map[string]any, 0)
	for index, outcome := range outcomes {
		if outcome.err == nil {
			continue
		}
		label := branches[index].Label
		if label == "" {
			label = fmt.Sprintf("branch-%d", index)
		}
		branchFailures = append(branchFailures, map[string]any{
			"label": label,
			"error": outcome.err.Error(),
		})
	}

	if joinErr != nil && (join == nil || join.OnFailure != "continue") {
		result := &enginepkg.StepResult{
			StepID:      step.ID,
			Status:      enginepkg.StepStatusFailed,
			Outcome:     enginepkg.StepOutcomeFailed,
			StartedAt:   startedAt,
			CompletedAt: time.Now(),
			DurationMs:  durationMs,
			Error:       joinErr,
		}
		stepSpan.RecordError(joinErr)
		stepSpan.SetStatus(otelPkg.StatusError, "parallel step failed")
		return result, nil
	}

	result := &enginepkg.StepResult{
		StepID:      step.ID,
		Status:      enginepkg.StepStatusCompleted,
		Outcome:     enginepkg.StepOutcomeSuccess,
		StartedAt:   startedAt,
		CompletedAt: time.Now(),
		DurationMs:  durationMs,
	}
	if len(branchFailures) > 0 {
		result.Output = map[string]any{"branch_failures": branchFailures}
	}
	stepSpan.SetStatus(otelPkg.StatusOK, "")
	return result, nil
}

// executeWaitForEvent blocks until the dispatcher delivers an event matching the step's filter.
// The caller must hold h.mu.
func (h *runHandle) executeWaitForEvent(ctx context.Context, step enginepkg.ResolvedStep, filter eventbus.EventFilter, timeout time.Duration) (*enginepkg.StepResult, error) {
	// Start step-level span for the wait_for_event step.
	_, stepSpan := h.engine.tracer().Start(ctx, "yawr.step.wait_for_event",
		otelPkg.WithAttributes(
			otelPkg.Attribute{Key: otelPkg.AttrStepID, Value: step.ID},
			otelPkg.Attribute{Key: otelPkg.AttrStepKind, Value: "wait_for_event"},
		),
	)
	defer stepSpan.End()

	h.emitEventLocked(ctx, trace.EventKindStepStarted, traceOccurrencePayload(ctx, step.ID, map[string]any{
		"step_id": step.ID,
		"kind":    "wait_for_event",
	}))
	if h.traceErr != nil {
		return nil, h.traceErr
	}

	startedAt := time.Now()
	if err := h.publishBoundaryLocked(ctx); err != nil {
		return nil, err
	}
	h.mu.Unlock()
	dispatch, prepareErr := enginepkg.PrepareExternalDispatch(ctx, enginepkg.DispatchRequest{
		Classification: "read-only", EndpointIdentity: "event-dispatcher",
		RenderedRequest: map[string]any{
			"step_id": step.ID, "source": filter.Source, "id": filter.ID,
			"payload": filter.Payload, "timeout_nanoseconds": timeout.Nanoseconds(),
		},
	})
	h.mu.Lock()
	if prepareErr != nil {
		return nil, prepareErr
	}
	if err := h.executionErrorLocked(ctx); err != nil {
		return nil, err
	}
	waitCtx := enginepkg.WithPreparedDispatch(ctx, dispatch)
	// Release mutex while waiting for the external event.
	if err := h.publishBoundaryLocked(ctx); err != nil {
		return nil, err
	}
	h.mu.Unlock()
	ev, waitErr := h.engine.cfg.Dispatcher.Wait(waitCtx, step.ID, filter, timeout)
	h.mu.Lock()
	if err := h.executionErrorLocked(ctx); err != nil {
		return nil, err
	}

	if waitErr != nil {
		result := &enginepkg.StepResult{
			StepID:      step.ID,
			Status:      enginepkg.StepStatusFailed,
			Outcome:     enginepkg.StepOutcomeFailed,
			StartedAt:   startedAt,
			CompletedAt: time.Now(),
			DurationMs:  time.Since(startedAt).Milliseconds(),
			Error:       waitErr,
		}
		stepSpan.RecordError(waitErr)
		stepSpan.SetStatus(otelPkg.StatusError, waitErr.Error())
		return result, nil
	}

	result := &enginepkg.StepResult{
		StepID:      step.ID,
		Status:      enginepkg.StepStatusCompleted,
		Outcome:     enginepkg.StepOutcomeSuccess,
		StartedAt:   startedAt,
		CompletedAt: time.Now(),
		DurationMs:  time.Since(startedAt).Milliseconds(),
	}
	if ev != nil {
		result.Output = ev.Payload
	}
	stepSpan.SetStatus(otelPkg.StatusOK, "")
	return result, nil
}

// completeRun marks the run as completed and emits run/completed.
// The caller must hold h.mu.
func (h *runHandle) completeRun(ctx context.Context) (*enginepkg.StepResult, error) {
	if validator := h.engine.cfg.TransitionValidator; validator != nil {
		if err := validator.ValidateCompletion(ctx); err != nil {
			return h.failRun(ctx, "", err)
		}
	}
	h.run.Status = enginepkg.RunStatusCompleted
	h.run.CompletedAt = time.Now()
	h.done.Store(true)
	eventKind := trace.EventKindRunCompleted
	if h.run.Mode == enginepkg.RunModeReplay {
		eventKind = trace.EventKind("run/replayed")
	}
	checkpointCtx := context.WithoutCancel(ctx)
	checkpointFile, checkpointErr := h.persistCompletedCheckpoint(checkpointCtx, traceEventDraft{
		kind: eventKind, payload: h.terminalRunPayload(nil),
	})
	if checkpointErr != nil {
		return h.haltCheckpointCommit(checkpointCtx, "", nil, checkpointErr)
	}
	h.emitCheckpoint(checkpointCtx, checkpointFile, "")
	defer h.shutdownExtensions(checkpointCtx)

	if h.traceErr != nil {
		return h.haltCheckpointCommit(checkpointCtx, "", nil, h.traceErr)
	}

	h.runSpan.SetStatus(otelPkg.StatusOK, "")
	h.runSpan.End()
	safeClose(h.events, &h.eventsClosed)
	h.cancelFn()
	return nil, io.EOF
}

// pauseAtRouteTarget persists a resumable before-boundary checkpoint at the
// root handle. Nested handles propagate the exact selector without committing.
func (h *runHandle) pauseAtRouteTarget(
	ctx context.Context,
	location enginepkg.DebugLocation,
) (*enginepkg.StepResult, error) {
	h.run.Status = enginepkg.RunStatusPausedAtBoundary
	h.run.CompletedAt = time.Time{}
	h.done.Store(true)
	if len(h.debugCallPath) > 0 {
		h.runSpan.SetStatus(otelPkg.StatusOK, "route-test target paused")
		h.runSpan.End()
		safeClose(h.events, &h.eventsClosed)
		h.cancelFn()
		h.shutdownExtensions(ctx)
		return nil, enginepkg.NewRouteTestTargetError(location)
	}
	pausedCursor, err := h.routeTargetCursor(location)
	if err != nil {
		return h.failRun(ctx, location.StepID, err)
	}
	h.pausedCursor = &pausedCursor
	checkpointFile, checkpointErr := h.persistCompletedCheckpoint(ctx,
		traceEventDraft{kind: trace.EventKindRouteTestTargetReached, payload: map[string]any{
			"step_id": location.StepID, "call_path": location.CallPath,
			"phase": string(enginepkg.ExecutionPhaseBefore), "invocation": location.Invocation,
			"attempt": location.Attempt, "dispatched": false,
		}},
		traceEventDraft{kind: trace.EventKindRunPausedAtBoundary, payload: map[string]any{
			"step_id": location.StepID, "call_path": location.CallPath,
			"phase": string(enginepkg.ExecutionPhaseBefore), "invocation": location.Invocation, "attempt": location.Attempt,
		}},
	)
	if checkpointErr != nil {
		return h.haltCheckpointCommit(ctx, location.StepID, nil, checkpointErr)
	}
	h.emitCheckpoint(ctx, checkpointFile, location.StepID)
	h.runSpan.SetStatus(otelPkg.StatusOK, "route-test target paused")
	h.runSpan.End()
	safeClose(h.events, &h.eventsClosed)
	h.cancelFn()
	h.shutdownExtensions(ctx)
	return nil, io.EOF
}

func (h *runHandle) pauseForHandoff(
	ctx context.Context,
	step enginepkg.ResolvedStep,
	request enginepkg.HandoffRequest,
) (*enginepkg.StepResult, error) {
	if err := internaldebugprotect.ValidateHandoffRequest(
		h.effectiveDebugProtection(h.debugProtection), request,
	); err != nil {
		return h.failRun(ctx, step.ID, fmt.Errorf("engine: handoff values rejected: %w", err))
	}
	if len(h.debugCallPath) > 0 {
		h.runSpan.SetStatus(otelPkg.StatusOK, "handoff requested")
		h.runSpan.End()
		safeClose(h.events, &h.eventsClosed)
		h.cancelFn()
		h.shutdownExtensions(ctx)
		return nil, enginepkg.NewHandoffRequestError(request)
	}
	if validator := h.engine.cfg.TransitionValidator; validator != nil {
		if err := validator.ValidateHandoff(ctx, request); err != nil {
			return h.failRun(ctx, step.ID, err)
		}
	}
	if request.OccurrenceSequence == 0 {
		request.OccurrenceSequence = h.run.CheckpointSequence + 1
	}
	h.run.Status = enginepkg.RunStatusHandoffPending
	h.run.PendingHandoff = cloneHandoffRequest(&request)
	h.run.CompletedAt = time.Time{}
	h.done.Store(true)
	checkpointFile, checkpointErr := h.persistCompletedCheckpoint(ctx, traceEventDraft{
		kind: trace.EventKind("handoff/requested"), payload: traceOccurrencePayload(ctx, request.StepID, map[string]any{
			"step_id": request.StepID, "qualified_node_id": request.QualifiedNodeID,
			"call_path": request.CallPath, "target_runbook": request.TargetRunbook,
			"reason_code": request.ReasonCode, "reason_summary": request.ReasonSummary,
			"invocation": request.Invocation, "retry_attempt": request.RetryAttempt,
		}),
	})
	if checkpointErr != nil {
		return h.haltCheckpointCommit(ctx, step.ID, nil, checkpointErr)
	}
	h.emitCheckpoint(ctx, checkpointFile, step.ID)
	h.runSpan.SetStatus(otelPkg.StatusOK, "handoff requested")
	h.runSpan.End()
	safeClose(h.events, &h.eventsClosed)
	h.cancelFn()
	h.shutdownExtensions(ctx)
	return nil, enginepkg.NewHandoffRequestError(request)
}

func (h *runHandle) routeTargetCursor(location enginepkg.DebugLocation) (enginepkg.ExecutionCursor, error) {
	if len(location.CallPath) == 0 {
		if h.run.CurrentStepIndex < 0 || h.run.CurrentStepIndex >= len(h.run.Plan.Steps) ||
			h.run.Plan.Steps[h.run.CurrentStepIndex].ID != location.StepID {
			return enginepkg.ExecutionCursor{}, errors.New("route test: root target does not match current step")
		}
		return enginepkg.ExecutionCursor{
			QualifiedNodeID: enginepkg.DebugNodeID(nil, location.StepID), StepID: location.StepID,
			StepIndex: h.run.CurrentStepIndex, Phase: enginepkg.ExecutionPhaseBefore,
			Invocation: location.Invocation, RetryAttempt: location.Attempt,
		}, nil
	}
	var matched *enginepkg.ExecutionCursor
	for _, candidate := range executionFrameCursors(
		h.run.CurrentStepIndex, h.run.ExecutionFrames, h.run.Dispatches,
		h.run.Interactions, h.debugInvocations.Snapshot(),
	) {
		if candidate.StepID != location.StepID || !sameDebugCallPath(candidate.CallPath, location.CallPath) {
			continue
		}
		if matched != nil {
			return enginepkg.ExecutionCursor{}, errors.New("route test: target selector is ambiguous across active structural frames")
		}
		copy := candidate
		matched = &copy
	}
	if matched == nil {
		return enginepkg.ExecutionCursor{}, errors.New("route test: target has no active structural frame")
	}
	matched.Phase = enginepkg.ExecutionPhaseBefore
	matched.Invocation = location.Invocation
	matched.RetryAttempt = location.Attempt
	return *matched, nil
}

func sameDebugCallPath(left, right []enginepkg.DebugCallFrame) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// resolveOnError determines the error-handling action for a failed step.
// Explicit on_error takes precedence over the fail-stop default.
// Returns "continue", "stop", or "goto:<step_id>".
func resolveOnError(step enginepkg.ResolvedStep) string {
	if step.OnError != "" {
		return step.OnError
	}
	return "stop"
}

// failRun marks the run as failed and emits run/failed.
// The caller must hold h.mu.
func (h *runHandle) failRun(ctx context.Context, stepID string, err error) (*enginepkg.StepResult, error) {
	now := time.Now()
	h.run.Status = enginepkg.RunStatusFailed
	h.run.CompletedAt = now
	h.run.Error = err
	h.done.Store(true)
	if stepID != "" && h.run.StepResults[stepID] == nil {
		h.run.StepResults[stepID] = &enginepkg.StepResult{
			StepID: stepID, Status: enginepkg.StepStatusFailed, Outcome: enginepkg.StepOutcomeFailed,
			StartedAt: now, CompletedAt: now, Error: err,
		}
	}
	checkpointFile, checkpointErr := h.persistCompletedCheckpoint(ctx,
		traceEventDraft{kind: trace.EventKindStepFailed, payload: map[string]any{
			"step_id": stepID, "error": err.Error(),
		}},
		traceEventDraft{kind: trace.EventKindRunFailed, payload: map[string]any{
			"run_id": h.run.ID, "error": err.Error(),
			"duration_ms": h.run.CompletedAt.Sub(h.run.StartedAt).Milliseconds(),
		}},
	)
	if checkpointErr != nil {
		return h.haltCheckpointCommit(ctx, stepID, nil, errors.Join(err, checkpointErr))
	}
	h.emitCheckpoint(ctx, checkpointFile, stepID)
	defer h.shutdownExtensions(ctx)

	if h.traceErr != nil {
		return h.haltCheckpointCommit(ctx, stepID, nil, errors.Join(err, h.traceErr))
	}

	h.runSpan.RecordError(err)
	h.runSpan.SetStatus(otelPkg.StatusError, err.Error())
	h.runSpan.End()
	safeClose(h.events, &h.eventsClosed)
	h.cancelFn()
	return nil, err
}

// haltIndeterminate halts the run in RunStatusIndeterminate because the given
// tool step's completion could not be established. The caller must hold h.mu.
// It returns the step result (with Status=INDETERMINATE and a populated
// IndeterminateRecord) plus ErrIndeterminate.
func (h *runHandle) haltIndeterminate(ctx context.Context, step enginepkg.ResolvedStep, err error, attempt int, classification *string) (*enginepkg.StepResult, error) {
	now := time.Now()
	rec := buildIndeterminateRecord(h.run.ID, step, err, attempt, classification, ctx, h.run.Plan, now)
	commitCtx := context.WithoutCancel(ctx)

	result := &enginepkg.StepResult{
		StepID:        step.ID,
		Status:        enginepkg.StepStatusIndeterminate,
		Error:         err,
		StartedAt:     now,
		CompletedAt:   now,
		Indeterminate: rec,
		Vars:          make(map[string]any),
	}

	h.run.Status = enginepkg.RunStatusIndeterminate
	h.run.CompletedAt = now
	h.run.Error = err
	h.run.StepResults[step.ID] = result
	h.done.Store(true)
	checkpointFile, checkpointErr := h.persistCompletedCheckpoint(commitCtx,
		traceEventDraft{kind: trace.EventKind("step/indeterminate"), payload: traceOccurrencePayload(ctx, step.ID, map[string]any{
			"step_id": step.ID, "tool_name": rec.ToolName, "action_name": rec.ActionName,
			"classification": rec.Classification, "endpoint_host": rec.EndpointHost,
			"attempt_number": rec.AttemptNumber, "transport_err_category": rec.TransportErrCategory,
		})},
		traceEventDraft{kind: trace.EventKind("run/indeterminate"), payload: map[string]any{
			"run_id": h.run.ID, "step_id": step.ID,
			"duration_ms": now.Sub(h.run.StartedAt).Milliseconds(),
		}},
	)
	if checkpointErr != nil {
		return h.haltCheckpointCommit(commitCtx, step.ID, result, errors.Join(enginepkg.ErrIndeterminate, checkpointErr))
	}
	h.emitCheckpoint(commitCtx, checkpointFile, step.ID)
	defer h.shutdownExtensions(commitCtx)

	if h.traceErr != nil {
		h.runSpan.RecordError(h.traceErr)
		h.runSpan.SetStatus(otelPkg.StatusError, h.traceErr.Error())
		h.runSpan.End()
		safeClose(h.events, &h.eventsClosed)
		h.cancelFn()
		return result, errors.Join(enginepkg.ErrIndeterminate, h.traceErr)
	}

	h.runSpan.RecordError(err)
	h.runSpan.SetStatus(otelPkg.StatusError, "run halted: step completion indeterminate")
	h.runSpan.End()
	safeClose(h.events, &h.eventsClosed)
	h.cancelFn()
	return result, enginepkg.ErrIndeterminate
}

// buildIndeterminateRecord assembles an IndeterminateRecord from available context.
// SECURITY: EndpointHost is set to url.Parse(def.Transport.URL).Host only — the
// parsed hostname. Credentials, tokens, query strings, and full URLs are never included.
func buildIndeterminateRecord(runID string, step enginepkg.ResolvedStep, err error, attempt int, classification *string, ctx context.Context, plan *enginepkg.ExecutionPlan, now time.Time) *enginepkg.IndeterminateRecord {
	rec := &enginepkg.IndeterminateRecord{
		RunID:                runID,
		StepID:               step.ID,
		AttemptNumber:        attempt,
		FailureTime:          now,
		TransportErrCategory: classifyTransportError(err),
	}

	if classification != nil {
		rec.Classification = *classification
	} else {
		rec.Classification = "unspecified"
	}

	if dl, ok := ctx.Deadline(); ok {
		rec.Deadline = dl
	}

	if plan != nil {
		if toolSpec, ok := step.Spec.(*schema.ToolCallSpec); ok && toolSpec != nil {
			rec.ToolName = toolSpec.Tool.Name
			rec.ActionName = toolSpec.Tool.Action
			if toolDef, found := plan.Tools[toolSpec.Tool.Name]; found && toolDef != nil {
				// Host only — credentials and full URLs are never recorded.
				if toolDef.Transport.URL != "" {
					if u, parseErr := url.Parse(toolDef.Transport.URL); parseErr == nil {
						rec.EndpointHost = u.Hostname()
					}
				}
			}
		}
	}

	return rec
}

// resolveStepClassification returns the classification pointer for the action
// invoked by a tool step, or nil when the step is not a tool step, the tool
// definition is absent, or the action has no declared classification.
func resolveStepClassification(step enginepkg.ResolvedStep, plan *enginepkg.ExecutionPlan) *string {
	if step.Kind != "tool" || plan == nil {
		return nil
	}
	toolSpec, ok := step.Spec.(*schema.ToolCallSpec)
	if !ok || toolSpec == nil {
		return nil
	}
	toolDef, found := plan.Tools[toolSpec.Tool.Name]
	if !found || toolDef == nil {
		return nil
	}
	actionName := toolSpec.Tool.Action
	if actionName == "" {
		actionName = "run"
	}
	action, found := toolDef.Actions[actionName]
	if !found || action == nil {
		return nil
	}
	return action.Classification
}

// requiresIndeterminate reports whether a transport-loss event on an action with
// the given classification must halt the run as INDETERMINATE rather than fail
// it normally. Returns true for mutating, destructive, and unspecified (nil).
// Returns false only for "read-only".
//
// Rationale: read-only actions that produce no side effects can be treated as
// failed when their result is lost. All other classifications cannot: we cannot
// know whether a mutating or destructive action completed before communication
// was lost, and unspecified is conservative (we assume the worst).
//
// ORTHOGONALITY: this decision is based solely on classification. RequiresApproval
// (including RequiresApproval: false) has no effect on this logic.
func requiresIndeterminate(classification *string) bool {
	if classification == nil {
		return true // unspecified: conservative — halt
	}
	return *classification != "read-only"
}

// isTransportLoss reports whether err indicates a transport-level timeout or
// lost-connection condition for which the server may have processed the request
// despite the client receiving an error.
func isTransportLoss(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// classifyTransportError returns a stable category string for a transport error.
func classifyTransportError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "context-deadline-exceeded"
	}
	if errors.Is(err, context.Canceled) {
		return "context-canceled"
	}
	return "network-error"
}

// emitEventLocked writes a trace event and fans out to in-process subscribers.
// The caller must hold h.mu (or be in a context where concurrent access is safe).
// enumConstraintsTracePayload serialises ValidatedPlan.EnumConstraints
// (AR-ENUM-10) into the plan/validated payload's "enum_constraints" shape,
// keyed by the same stable "site.declaration" path used at plan time.
// json.Marshal sorts map[string]any keys, so the emitted JSON key order is
// deterministic without any extra sorting here. C1 redaction: a redacted
// declaration carries "<redacted>" plus member_count and no member list.
func enumConstraintsTracePayload(constraints map[string]enginepkg.EnumMeta) map[string]any {
	out := make(map[string]any, len(constraints))
	for key, meta := range constraints {
		entry := map[string]any{"member_count": meta.MemberCount}
		if meta.Redacted {
			entry["members"] = "<redacted>"
		} else {
			entry["members"] = meta.Members
		}
		out[key] = entry
	}
	return out
}

func (h *runHandle) emitEventLocked(ctx context.Context, kind trace.EventKind, payload map[string]any) error {
	if h.traceErr != nil {
		return h.traceErr
	}
	h.run.Sequence++
	if stepID, ok := payload["step_id"].(string); ok && stepID != "" {
		if _, exists := payload["node_id"]; !exists {
			payload["node_id"] = enginepkg.DebugNodeID(h.debugCallPath, stepID)
		}
		if len(h.debugCallPath) > 0 {
			if _, exists := payload["call_path"]; !exists {
				payload["call_path"] = append([]enginepkg.DebugCallFrame(nil), h.debugCallPath...)
			}
		}
	}
	payload = classifyPresentationOutput(payload, redactProtectedEventPayload(payload, h.effectiveDebugProtection(h.debugProtection)))

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		h.traceErr = fmt.Errorf("%w: encode trace event %s: %w", enginepkg.ErrTraceCommit, kind, err)
		return h.traceErr
	}
	traceEvent := trace.TraceEvent{
		EventID:   uuid.New().String(),
		RunID:     h.run.ID,
		RunbookID: h.run.Plan.Metadata.RunbookID,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Kind:      kind,
		Sequence:  h.run.Sequence,
		Payload:   payloadBytes,
	}

	// Build in-process event.
	event := enginepkg.Event{
		EventID:   traceEvent.EventID,
		RunID:     traceEvent.RunID,
		RunbookID: traceEvent.RunbookID,
		Timestamp: traceEvent.Timestamp,
		Kind:      string(kind),
		Sequence:  traceEvent.Sequence,
		Payload:   payload,
	}

	if h.store != nil {
		traceCtx := enginepkg.WithRunWriterEpoch(context.WithoutCancel(ctx), h.run.WriterEpoch)
		if err := h.store.WriteTrace(traceCtx, h.run.ID, event); err != nil {
			h.traceErr = fmt.Errorf("%w: append run trace event %s: %w", enginepkg.ErrTraceCommit, kind, err)
			return h.traceErr
		}
	}

	// Publish configured trace projections only after the durable run store has
	// accepted the event. Session-managed stores use this boundary to journal
	// direct lifecycle traces before any external JSONL projection can lead it.
	if err := appendConfiguredTraceProjection(h.engine.cfg.TraceWriter, h.store, traceEvent); err != nil {
		h.traceErr = fmt.Errorf("%w: append trace event %s: %w", enginepkg.ErrTraceCommit, kind, err)
		return h.traceErr
	}

	// Fan out to event bus (non-blocking, best-effort).
	if h.engine.cfg.EventBus != nil {
		h.engine.cfg.EventBus.Publish(eventbus.Event{
			RunID:   h.run.ID,
			Kind:    string(kind),
			Payload: payload,
		})
	}

	// Send to the events channel (non-blocking, best-effort).
	if !h.eventsClosed.Load() {
		select {
		case h.events <- event:
		default:
			// Channel full; drop (in-process delivery is best-effort).
		}
	}

	// Callbacks are drained synchronously by the outer operation after it
	// releases h.mu. This keeps delivery ordered without making callbacks
	// re-enter the run mutex.
	h.enqueueCallback(event)
	return nil
}

func redactProtectedEventPayload(payload map[string]any, protection enginepkg.DebugProtection) map[string]any {
	if len(protection.SecretValues) == 0 && len(protection.RedactionPatterns) == 0 {
		return payload
	}
	secrets := append([]string(nil), protection.SecretValues...)
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	return redactEventMap(payload, secrets, debugRedactor(protection), false)
}

func redactEventMap(values map[string]any, secrets []string, redactor *internalGov.Redactor, content bool) map[string]any {
	result := make(map[string]any, len(values))
	for key, value := range values {
		nestedContent := content || eventContentKey(key)
		switch typed := value.(type) {
		case string:
			if !nestedContent && eventStructuralStringKey(key) {
				result[key] = typed
			} else {
				result[key] = redactProtectedString(typed, secrets, redactor)
			}
		case map[string]any:
			result[key] = redactEventMap(typed, secrets, redactor, nestedContent)
		case []any:
			items := make([]any, len(typed))
			for index, item := range typed {
				switch nested := item.(type) {
				case string:
					items[index] = redactProtectedString(nested, secrets, redactor)
				case map[string]any:
					items[index] = redactEventMap(nested, secrets, redactor, nestedContent)
				default:
					items[index] = item
				}
			}
			result[key] = items
		default:
			result[key] = value
		}
	}
	return result
}

func eventContentKey(key string) bool {
	switch key {
	case "vars", "captures", "output", "actual", "effective", "actual_vars", "effective_vars",
		"line", "error", "message", "prompt", "result", "request", "args", "value", "values":
		return true
	default:
		return false
	}
}

func eventStructuralStringKey(key string) bool {
	switch key {
	case "step_id", "node_id", "run_id", "runbook_id", "runbook_path", "event_id", "kind", "status",
		"phase", "stream", "actor", "mode", "client", "correlation_id", "capability", "tool", "action":
		return true
	default:
		return false
	}
}

func redactSecretString(value string, secrets []string) string {
	for _, secret := range secrets {
		value = strings.ReplaceAll(value, secret, "<redacted>")
	}
	return value
}

func redactProtectedString(value string, secrets []string, redactor *internalGov.Redactor) string {
	value = redactSecretString(value, secrets)
	if redactor != nil {
		value, _ = redactor.RedactString(value)
	}
	return value
}

// forwardSubEngineEvent receives an event from a sub-engine spawned by a
// SubStepRunner (branch / iterate / compensate) and re-publishes it into
// this run's event channel so consumers (CLI tail, SSE bridge, store)
// see nested step lifecycle. Run-level events from the sub-engine are
// dropped — only step/* and iteration/* lifecycle events are forwarded.
func (h *runHandle) forwardSubEngineEvent(ev enginepkg.Event) {
	if isRunLifecycleEvent(ev.Kind) {
		return
	}
	h.mu.Lock()
	var publication *callbackPublication
	defer func() {
		h.mu.Unlock()
		if publication != nil {
			h.drainCallbacksThrough(publication)
		}
	}()
	h.run.Sequence++
	forwarded := ev
	forwarded.RunID = h.run.ID
	forwarded.Sequence = h.run.Sequence
	if err := writeProjectedTraceEvent(
		h.runCtx, h.engine.cfg.TraceWriter, h.store, h.run.WriterEpoch, forwarded,
	); err != nil {
		h.traceErr = fmt.Errorf("%w: append forwarded trace event %s: %w", enginepkg.ErrTraceCommit, ev.Kind, err)
		return
	}
	if h.engine.cfg.EventBus != nil {
		h.engine.cfg.EventBus.Publish(eventbus.Event{
			RunID:   h.run.ID,
			Kind:    forwarded.Kind,
			Payload: forwarded.Payload,
		})
	}
	if !h.eventsClosed.Load() {
		select {
		case h.events <- forwarded:
		default:
		}
	}
	h.enqueueCallback(forwarded)
	publication = h.lastCallback
}

// isRunLifecycleEvent reports whether kind is a run-level event that
// must not be re-emitted by sub-engines (e.g. they would otherwise
// duplicate run/started, run/completed for the same parent run).
func isRunLifecycleEvent(kind string) bool {
	switch kind {
	case string(trace.EventKindRunStarted),
		string(trace.EventKindRunCompleted),
		string(trace.EventKindRunFailed),
		string(trace.EventKindRunCancelled):
		return true
	}
	return false
}

// snapshotVars returns a copy of the current run vars for branch isolation.
func (h *runHandle) snapshotVars() map[string]any {
	return cloneAnyMap(h.run.Vars)
}

func (h *runHandle) runDir() string {
	if h.store == nil {
		return ""
	}
	if provider, ok := h.store.(interface{ RunDir(string) string }); ok {
		return provider.RunDir(h.run.ID)
	}
	return ""
}

// compensationEntry holds a registered compensation plan.
type compensationEntry struct {
	key            string
	registrationID string
	on             string
	steps          []schema.FlowNode
}

func (h *runHandle) executeCompensations(ctx context.Context) error {
	entries := h.collectCompensations()
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		if !shouldRunCompensation(entry.on) {
			continue
		}
		steps := make([]enginepkg.ResolvedStep, 0, len(entry.steps))
		for _, node := range entry.steps {
			resolved, ok := resolveFlowNode(node)
			if !ok {
				continue
			}
			steps = append(steps, resolved)
		}
		if len(steps) == 0 {
			delete(h.run.Vars, entry.key)
			continue
		}
		definitionDigest, err := enginepkg.ExecutionFrameDefinitionDigest(steps)
		if err != nil {
			return err
		}
		callPath := append(append([]enginepkg.DebugCallFrame(nil), h.debugCallPath...), enginepkg.DebugCallFrame{StepID: entry.registrationID})
		initialVars := cloneAnyMap(h.run.Vars)
		h.mu.Unlock()
		frame, err := h.BeginExecutionFrame(ctx, enginepkg.ExecutionFrameRequest{
			ParentQualifiedNodeID: enginepkg.DebugNodeID(h.debugCallPath, entry.registrationID),
			ParentStepID:          entry.registrationID, Kind: "compensate", CallPath: callPath,
			BranchLabel: entry.registrationID, DefinitionDigest: definitionDigest,
			StepCount: len(steps), StepIDs: enginepkg.ExecutionFrameStepIDs(steps), InitialVars: initialVars,
		})
		h.mu.Lock()
		if err != nil {
			return err
		}
		if h.done.Load() && h.run.Status != enginepkg.RunStatusRunning {
			return context.Canceled
		}
		for stepIndex := frame.NextStepIndex; stepIndex < len(steps); stepIndex++ {
			if err := h.executeCompensationStep(ctx, frame, stepIndex, steps[stepIndex]); err != nil {
				return err
			}
		}
		finishExecutionFrames(
			h.run.ExecutionFrames, "", enginepkg.DebugNodeID(h.debugCallPath, entry.registrationID), enginepkg.StepStatusCompleted,
		)
		delete(h.run.Vars, entry.key)
	}
	return nil
}

func (h *runHandle) collectCompensations() []compensationEntry {
	if h.run == nil || h.run.Plan == nil {
		return nil
	}
	var entries []compensationEntry
	for _, step := range h.run.Plan.Steps {
		key := "__compensation_" + step.ID
		val, ok := h.run.Vars[key]
		if !ok {
			continue
		}
		on := ""
		var steps []schema.FlowNode
		if spec, typed := step.Spec.(*schema.CompensateSpec); typed && spec != nil {
			on = spec.Compensate.On
			steps = spec.Compensate.Steps
		} else {
			on, steps, ok = parseCompensation(val)
			if !ok {
				continue
			}
		}
		entries = append(entries, compensationEntry{key: key, registrationID: step.ID, on: on, steps: steps})
	}
	return entries
}

func parseCompensation(val any) (string, []schema.FlowNode, bool) {
	data, ok := val.(map[string]any)
	if !ok {
		return "", nil, false
	}
	on, _ := data["on"].(string)
	if steps, ok := data["steps"].([]schema.FlowNode); ok {
		return on, steps, true
	}
	encoded, err := json.Marshal(data["steps"])
	if err != nil {
		return on, nil, false
	}
	var steps []schema.FlowNode
	if err := json.Unmarshal(encoded, &steps); err != nil {
		return on, nil, false
	}
	return on, steps, true
}

func shouldRunCompensation(on string) bool {
	switch strings.ToLower(strings.TrimSpace(on)) {
	case "", "any", "failure", "failed", "error":
		return true
	default:
		return false
	}
}

func hasValidatedGCPCapture(vp *enginepkg.ValidatedPlan, step enginepkg.ResolvedStep) bool {
	if vp == nil || vp.GCPPaths == nil || len(step.Capture) == 0 {
		return false
	}
	for name := range step.Capture {
		if vp.GCPPaths[enginepkg.StepRef{StepID: step.ID, FieldPath: "capture." + name}] != nil {
			return true
		}
	}
	return false
}

func resolveFlowNode(node schema.FlowNode) (enginepkg.ResolvedStep, bool) {
	if node.Step != nil {
		step := node.Step
		return enginepkg.ResolvedStep{
			ID:              step.ID,
			Kind:            string(step.Type),
			Spec:            stepSpecForStep(step),
			Capture:         step.Capture,
			CaptureDefaults: step.CaptureDefaults,
			Delay:           step.Delay,
		}, true
	}
	if node.Iterate != nil {
		return enginepkg.ResolvedStep{
			ID:   node.Iterate.ID,
			Kind: "iterate",
			Spec: node.Iterate,
		}, true
	}
	if node.Parallel != nil {
		return enginepkg.ResolvedStep{
			ID:   node.Parallel.ID,
			Kind: "parallel",
			Spec: node.Parallel,
		}, true
	}
	return enginepkg.ResolvedStep{}, false
}

func stepSpecForStep(step *schema.Step) enginepkg.StepSpec {
	switch step.Type {
	case schema.StepTypeCLI:
		if step.CLI != nil {
			return step.CLI
		}
	case schema.StepTypeTool:
		if step.ToolCall != nil {
			return step.ToolCall
		}
	case schema.StepTypeInclude:
		if step.IncludeSpec != nil {
			return step.IncludeSpec
		}
	case schema.StepTypeChoice:
		if step.ChoiceSpec != nil {
			return step.ChoiceSpec
		}
	case schema.StepTypeDecision:
		if step.DecisionSpec != nil {
			return step.DecisionSpec
		}
	case schema.StepTypeCollector:
		if step.CollectorSpec != nil {
			return step.CollectorSpec
		}
	case schema.StepTypeHostAction:
		if step.HostActionSpec != nil {
			return step.HostActionSpec
		}
	case schema.StepTypeHandoff:
		if step.HandoffSpec != nil {
			return step.HandoffSpec
		}
	case schema.StepTypeBranch:
		if step.BranchSpec != nil {
			return step.BranchSpec
		}
	case schema.StepTypeApprove:
		if step.ApproveSpec != nil {
			return step.ApproveSpec
		}
	case schema.StepTypeAssert:
		if step.AssertSpec != nil {
			return step.AssertSpec
		}
	case schema.StepTypeCompensate:
		if step.CompensateSpec != nil {
			return step.CompensateSpec
		}
	case schema.StepTypeWaitForEvent:
		if step.WaitForEventSpec != nil {
			return step.WaitForEventSpec
		}
	case schema.StepTypeEnd:
		if step.EndSpec != nil {
			return step.EndSpec
		}
	case schema.StepTypeNoop:
		if step.NoopSpec != nil {
			return step.NoopSpec
		}
		return &schema.NoopSpec{}
	case schema.StepTypeDisplay:
		if step.DisplaySpec != nil {
			return step.DisplaySpec
		}
		return &schema.DisplaySpec{}
	case schema.StepTypeAssign:
		return step.AssignSpec
	case schema.StepTypeResults:
		return step.ResultsSpec
	}
	return &rawSpec{kind: string(step.Type)}
}

type rawSpec struct{ kind string }

func (r *rawSpec) StepKind() string { return r.kind }

func (h *runHandle) executeCompensationStep(
	ctx context.Context,
	frame enginepkg.ExecutionFrameState,
	stepIndex int,
	step enginepkg.ResolvedStep,
) error {
	startedAt := time.Now()
	exec := h.engine.cfg.Executors.Lookup(step.Kind)
	var result *enginepkg.StepResult
	if exec == nil {
		result = &enginepkg.StepResult{
			StepID:      step.ID,
			Status:      enginepkg.StepStatusFailed,
			Outcome:     enginepkg.StepOutcomeFailed,
			StartedAt:   startedAt,
			CompletedAt: time.Now(),
			DurationMs:  time.Since(startedAt).Milliseconds(),
			Error:       &ExecutorNotFoundError{Kind: step.Kind},
		}
	} else {
		stepCtx := enginepkg.WithRunID(ctx, h.run.ID)
		stepCtx = enginepkg.WithEventForwarder(stepCtx, h.forwardSubEngineEvent)
		stepCtx = context.WithValue(stepCtx, publicationParentKey{}, h)
		stepCtx = enginepkg.WithDebugCallPath(stepCtx, frame.CallPath)
		structuralPath := enginepkg.DynamicIncludeStructuralPathFromContext(ctx)
		structuralPath = append(structuralPath, schema.DynamicIncludeFrameIdentity{
			QualifiedNodeID: frame.ParentQualifiedNodeID, Kind: frame.Kind,
			Invocation: frame.Invocation,
		})
		stepCtx = enginepkg.WithDynamicIncludeStructuralPath(stepCtx, structuralPath)
		stepCtx = enginepkg.WithExecutionFrameBinding(stepCtx, enginepkg.ExecutionFrameBinding{FrameID: frame.FrameID})
		stepCtx = enginepkg.WithDispatchExecutionBoundary(stepCtx, enginepkg.DispatchExecutionBoundary{
			QualifiedNodeID: enginepkg.DebugNodeID(frame.CallPath, step.ID),
			CallPath:        append([]enginepkg.DebugCallFrame(nil), frame.CallPath...), StepID: step.ID,
			StepIndex: stepIndex, FrameID: frame.FrameID, FrameStepIndex: stepIndex,
			Phase: enginepkg.ExecutionPhaseExecute, Invocation: frame.Invocation,
			RetryAttempt: 1, OccurrenceSequence: 1,
		})
		eventCtx := stepCtx
		stepCtx = executor.WithEventEmitter(stepCtx, func(kind string, payload map[string]any) {
			h.mu.Lock()
			h.emitEventLocked(eventCtx, trace.EventKind(kind), payload)
			h.unlockAndDrainCallbacks()
		})
		h.mu.Unlock()
		var execErr error
		result, execErr = exec.Execute(stepCtx, step, h.run.Vars)
		h.mu.Lock()
		if _, handoff := enginepkg.HandoffRequestFromError(execErr); handoff {
			return execErr
		}
		if execErr != nil {
			result = &enginepkg.StepResult{
				StepID: step.ID, Status: enginepkg.StepStatusFailed, Outcome: enginepkg.StepOutcomeFailed,
				StartedAt: startedAt, CompletedAt: time.Now(),
				DurationMs: time.Since(startedAt).Milliseconds(), Error: execErr,
			}
		}
		if result == nil {
			result = &enginepkg.StepResult{
				StepID: step.ID, Status: enginepkg.StepStatusFailed, Outcome: enginepkg.StepOutcomeFailed,
				StartedAt: startedAt, CompletedAt: time.Now(), Error: errors.New("compensation executor returned no result"),
			}
		}
	}
	if result.StepID == "" {
		result.StepID = step.ID
	}
	if result.Status == "" {
		result.Status = enginepkg.StepStatusCompleted
	}
	if result.Outcome == "" {
		switch result.Status {
		case enginepkg.StepStatusCompleted:
			result.Outcome = enginepkg.StepOutcomeSuccess
		case enginepkg.StepStatusSkipped:
			result.Outcome = enginepkg.StepOutcomeSkipped
		case enginepkg.StepStatusFailed:
			result.Outcome = enginepkg.StepOutcomeFailed
		case enginepkg.StepStatusDenied:
			result.Outcome = enginepkg.StepOutcomeDenied
		}
	}
	if result.StartedAt.IsZero() {
		result.StartedAt = startedAt
	}
	if result.CompletedAt.IsZero() {
		result.CompletedAt = time.Now()
	}
	if result.DurationMs == 0 {
		result.DurationMs = result.CompletedAt.Sub(result.StartedAt).Milliseconds()
	}

	h.run.StepResults[step.ID] = result
	for k, v := range result.Vars {
		h.run.Vars[k] = v
	}
	stepCtx := enginepkg.WithDebugCallPath(ctx, frame.CallPath)
	structuralPath := enginepkg.DynamicIncludeStructuralPathFromContext(ctx)
	structuralPath = append(structuralPath, schema.DynamicIncludeFrameIdentity{
		QualifiedNodeID: frame.ParentQualifiedNodeID, Kind: frame.Kind, Invocation: frame.Invocation,
	})
	stepCtx = enginepkg.WithDynamicIncludeStructuralPath(stepCtx, structuralPath)
	stepCtx = enginepkg.WithDispatchExecutionBoundary(stepCtx, enginepkg.DispatchExecutionBoundary{
		QualifiedNodeID: enginepkg.DebugNodeID(frame.CallPath, step.ID),
		CallPath:        append([]enginepkg.DebugCallFrame(nil), frame.CallPath...), StepID: step.ID,
		StepIndex: stepIndex, FrameID: frame.FrameID, FrameStepIndex: stepIndex,
		Phase: enginepkg.ExecutionPhaseExecute, Invocation: frame.Invocation,
		RetryAttempt: 1, OccurrenceSequence: 1,
	})
	workingVars := cloneAnyMap(h.run.Vars)
	h.mu.Unlock()
	_, commitErr := h.CommitExecutionFrameStep(stepCtx, enginepkg.ExecutionFrameStepCommit{
		FrameID: frame.FrameID, StepIndex: stepIndex, StepKind: step.Kind, Result: result, WorkingVars: workingVars,
	})
	h.mu.Lock()
	return commitErr
}

func (h *runHandle) hasActiveCompensationFrame(stepID string) bool {
	if result := h.run.StepResults[stepID]; result == nil || result.Status != enginepkg.StepStatusFailed {
		return false
	}
	for _, frame := range h.run.ExecutionFrames {
		if frame != nil && frame.Status == enginepkg.ExecutionFrameStatusActive && frame.Kind == "compensate" &&
			frame.ParentFrameID == "" {
			if _, registered := h.run.Vars["__compensation_"+frame.ParentStepID]; registered {
				return true
			}
		}
	}
	return false
}

// Approve records an approval decision.
func (h *runHandle) Approve(ctx context.Context, decision enginepkg.ApprovalDecision) error {
	return enginepkg.ErrNotImplemented
}

// SubmitEvidence records user-supplied evidence.
func (h *runHandle) SubmitEvidence(ctx context.Context, stepID string, ev map[string]*enginepkg.EvidenceValue) error {
	return enginepkg.ErrNotImplemented
}

// Cancel requests a graceful stop of the run.
func (h *runHandle) Cancel(ctx context.Context, reason string) error {
	h.callbacksArmed.Store(true)
	// Cancel before taking the mutation mutex. Never wait for executionMu or
	// callback completion: a paused observer may itself be requesting Cancel.
	h.cancelFn()

	h.mu.Lock()
	defer h.unlockAndDrainCallbacks()

	if h.done.Load() {
		return nil
	}
	return h.cancelRunLocked(context.WithoutCancel(ctx), reason, "run cancelled: "+reason, nil)
}

func (h *runHandle) Detach(ctx context.Context, reason string) error {
	h.callbacksArmed.Store(true)
	h.cancelFn()
	h.executionMu.Lock()
	defer h.executionMu.Unlock()
	h.mu.Lock()
	defer h.unlockAndDrainCallbacks()

	if h.done.Load() {
		h.shutdownExtensions(context.WithoutCancel(ctx))
		if h.run.Status == enginepkg.RunStatusIndeterminate {
			return enginepkg.ErrIndeterminate
		}
		return nil
	}
	now := time.Now().UTC()
	state := h.stateLocked()
	prepared, markErr := markPreparedDispatchesIndeterminate(&state, h.run.WriterEpoch, now)
	if markErr != nil {
		_, haltErr := h.haltCheckpointCommit(context.WithoutCancel(ctx), "", nil, markErr)
		return haltErr
	}
	if len(prepared) > 0 {
		h.run.Status = enginepkg.RunStatusIndeterminate
		h.run.CompletedAt = now
		h.run.Error = enginepkg.ErrIndeterminate
		h.run.StepResults = state.StepResults
		h.run.Dispatches = state.Dispatches
		h.run.ExecutionFrames = state.ExecutionFrames
		h.run.DynamicIncludes = state.DynamicIncludes
		h.done.Store(true)
		checkpointFile, checkpointErr := h.persistCompletedCheckpoint(context.WithoutCancel(ctx), traceEventDraft{
			kind: trace.EventKind("run/indeterminate"), payload: map[string]any{
				"run_id": h.run.ID, "reason": reason, "unmatched_dispatches": len(prepared),
			},
		})
		if checkpointErr != nil {
			_, haltErr := h.haltCheckpointCommit(context.WithoutCancel(ctx), "", nil, checkpointErr)
			return haltErr
		}
		h.emitCheckpoint(context.WithoutCancel(ctx), checkpointFile, "")
		h.runSpan.RecordError(enginepkg.ErrIndeterminate)
		h.runSpan.SetStatus(otelPkg.StatusError, "transport detach left dispatch completion indeterminate")
		h.runSpan.End()
		safeClose(h.events, &h.eventsClosed)
		h.shutdownExtensions(context.WithoutCancel(ctx))
		return enginepkg.ErrIndeterminate
	}
	if h.run.CurrentStepIndex >= 0 && h.run.CurrentStepIndex < len(h.run.Plan.Steps) {
		step := h.run.Plan.Steps[h.run.CurrentStepIndex]
		if h.run.StepResults[step.ID] == nil && !hasActiveExecutionFrame(h.run.ExecutionFrames) {
			invocation := h.debugInvocations.Current(h.debugCallPath, step.ID)
			if invocation < 1 {
				invocation = 1
			}
			h.pausedCursor = &enginepkg.ExecutionCursor{
				QualifiedNodeID: enginepkg.DebugNodeID(h.debugCallPath, step.ID),
				CallPath:        append([]enginepkg.DebugCallFrame(nil), h.debugCallPath...),
				StepID:          step.ID, StepIndex: h.run.CurrentStepIndex,
				Phase: enginepkg.ExecutionPhaseBefore, Invocation: invocation, RetryAttempt: 1,
			}
		}
	}
	h.run.Status = enginepkg.RunStatusPausedAtBoundary
	h.run.CompletedAt = time.Time{}
	h.done.Store(true)
	checkpointFile, checkpointErr := h.persistCompletedCheckpoint(context.WithoutCancel(ctx), traceEventDraft{
		kind: trace.EventKindRunPausedAtBoundary, payload: map[string]any{
			"run_id": h.run.ID, "reason": reason,
		},
	})
	if checkpointErr != nil {
		_, haltErr := h.haltCheckpointCommit(context.WithoutCancel(ctx), "", nil, checkpointErr)
		return haltErr
	}
	h.emitCheckpoint(context.WithoutCancel(ctx), checkpointFile, "")
	h.runSpan.SetStatus(otelPkg.StatusOK, "run detached at durable boundary")
	h.runSpan.End()
	safeClose(h.events, &h.eventsClosed)
	h.shutdownExtensions(context.WithoutCancel(ctx))
	return nil
}

func (h *runHandle) cancelRunLocked(
	ctx context.Context,
	reason string,
	spanMessage string,
	extraPayload map[string]any,
) error {
	now := time.Now().UTC()
	state := h.stateLocked()
	prepared, markErr := markPreparedDispatchesIndeterminate(&state, h.run.WriterEpoch, now)
	if markErr != nil {
		_, haltErr := h.haltCheckpointCommit(ctx, "", nil, markErr)
		return haltErr
	}
	if len(prepared) > 0 {
		h.run.Status = enginepkg.RunStatusIndeterminate
		h.run.CompletedAt = now
		h.run.Error = enginepkg.ErrIndeterminate
		h.run.StepResults = state.StepResults
		h.run.Dispatches = state.Dispatches
		h.run.ExecutionFrames = state.ExecutionFrames
		h.run.DynamicIncludes = state.DynamicIncludes
		h.done.Store(true)
		payload := map[string]any{"run_id": h.run.ID, "reason": reason, "unmatched_dispatches": len(prepared)}
		for key, value := range extraPayload {
			payload[key] = value
		}
		checkpointFile, checkpointErr := h.persistCompletedCheckpoint(ctx, traceEventDraft{
			kind: trace.EventKind("run/indeterminate"), payload: payload,
		})
		if checkpointErr != nil {
			_, haltErr := h.haltCheckpointCommit(ctx, "", nil, checkpointErr)
			return haltErr
		}
		h.emitCheckpoint(ctx, checkpointFile, "")
		h.runSpan.RecordError(enginepkg.ErrIndeterminate)
		h.runSpan.SetStatus(otelPkg.StatusError, "run cancellation left dispatch completion indeterminate")
		h.runSpan.End()
		safeClose(h.events, &h.eventsClosed)
		h.cancelFn()
		h.shutdownExtensions(ctx)
		return enginepkg.ErrIndeterminate
	}
	h.run.Status = enginepkg.RunStatusCancelled
	h.run.CompletedAt = now
	h.done.Store(true)
	payload := map[string]any{"run_id": h.run.ID}
	if reason != "" {
		payload["reason"] = reason
	}
	for key, value := range extraPayload {
		payload[key] = value
	}
	checkpointFile, checkpointErr := h.persistCompletedCheckpoint(ctx, traceEventDraft{
		kind: trace.EventKindRunCancelled, payload: payload,
	})
	if checkpointErr != nil {
		_, haltErr := h.haltCheckpointCommit(ctx, "", nil, checkpointErr)
		return haltErr
	}
	h.emitCheckpoint(ctx, checkpointFile, "")
	if h.traceErr != nil {
		_, haltErr := h.haltCheckpointCommit(ctx, "", nil, h.traceErr)
		return haltErr
	}
	h.runSpan.SetStatus(otelPkg.StatusError, spanMessage)
	h.runSpan.End()
	safeClose(h.events, &h.eventsClosed)
	h.cancelFn()
	h.shutdownExtensions(ctx)
	return nil
}

// State returns a read-only snapshot of the current run state.
func (h *runHandle) State() enginepkg.RunState {
	h.callbacksArmed.Store(true)
	h.mu.Lock()
	state := h.stateLocked()
	state.Vars = cloneAnyMap(state.Vars)
	state.StepResults = cloneStepResultMap(state.StepResults)
	state.Plan = cloneExecutionPlanForInspection(state.Plan)
	h.mu.Unlock()
	h.drainCallbacks()
	return state
}

// stateLocked returns a run snapshot while the caller holds h.mu.
func (h *runHandle) stateLocked() enginepkg.RunState {
	currentStep := ""
	if h.run.CurrentStepIndex >= 0 && h.run.CurrentStepIndex < len(h.run.Plan.Steps) {
		currentStep = h.run.Plan.Steps[h.run.CurrentStepIndex].ID
	}

	return enginepkg.RunState{
		Results:                     cloneResults(h.run.Results),
		BindingScope:                enginepkg.CloneBindingScope(h.run.BindingScope),
		RunID:                       h.run.ID,
		RunbookPath:                 h.run.Plan.RunbookPath,
		Mode:                        h.run.Mode,
		WriterEpoch:                 h.run.WriterEpoch,
		CheckpointSequence:          h.run.CheckpointSequence,
		CommittedTraceSequence:      h.run.CommittedTraceSequence,
		PendingTraceEvents:          cloneTraceEvents(h.run.PendingTraceEvents),
		CursorSet:                   h.nextExecutionCursorSet(),
		Status:                      h.run.Status,
		CurrentStep:                 currentStep,
		CurrentStepIndex:            h.run.CurrentStepIndex,
		Vars:                        h.run.Vars,
		StepResults:                 h.run.StepResults,
		Interactions:                cloneInteractionStateMap(h.run.Interactions),
		ExecutionInvocationCounts:   h.debugInvocations.Snapshot(),
		InteractionInvocationCounts: h.interactionInvocations.Snapshot(),
		Dispatches:                  cloneDispatchStateMap(h.run.Dispatches),
		ExecutionFrames:             cloneExecutionFrameStateMap(h.run.ExecutionFrames),
		DynamicIncludes:             cloneDynamicIncludeResolutionStateMap(h.run.DynamicIncludes),
		PendingHandoff:              cloneHandoffRequest(h.run.PendingHandoff),
		StartedAt:                   h.run.StartedAt,
		UpdatedAt:                   time.Now(),
		CompletedAt:                 h.run.CompletedAt,
		Plan:                        h.run.Plan,
	}
}

func cloneTraceEvents(events []enginepkg.Event) []enginepkg.Event {
	if events == nil {
		return nil
	}
	cloned := make([]enginepkg.Event, len(events))
	for index, event := range events {
		cloned[index] = event
		cloned[index].Payload = cloneAnyMap(event.Payload)
	}
	return cloned
}

func cloneHandoffRequest(request *enginepkg.HandoffRequest) *enginepkg.HandoffRequest {
	if request == nil {
		return nil
	}
	cloned := *request
	cloned.CallPath = append([]enginepkg.DebugCallFrame(nil), request.CallPath...)
	cloned.StructuralPath = append([]schema.DynamicIncludeFrameIdentity(nil), request.StructuralPath...)
	cloned.Context = cloneAnyMap(request.Context)
	cloned.Facts = cloneAnyMap(request.Facts)
	cloned.ContextBindings = cloneStringMap(request.ContextBindings)
	cloned.FactBindings = cloneStringMap(request.FactBindings)
	return &cloned
}

func (h *runHandle) nextExecutionCursorSet() *enginepkg.ExecutionCursorSet {
	if h.run.Status == enginepkg.RunStatusPausedAtBoundary && h.pausedCursor != nil {
		cursor := *h.pausedCursor
		cursor.CallPath = append([]enginepkg.DebugCallFrame(nil), cursor.CallPath...)
		return &enginepkg.ExecutionCursorSet{
			SchemaVersion: enginepkg.ExecutionCursorSchemaV1, Cursors: []enginepkg.ExecutionCursor{cursor},
		}
	}
	if h.run.Status == enginepkg.RunStatusHandoffPending && h.run.PendingHandoff != nil {
		request := h.run.PendingHandoff
		location := enginepkg.DebugLocation{
			RunID: h.run.ID, RunbookPath: h.run.Plan.RunbookPath,
			CallPath: request.CallPath, StepID: request.StepID,
			Invocation: request.Invocation, Attempt: request.RetryAttempt,
		}
		cursor, err := h.routeTargetCursor(location)
		if err == nil {
			return &enginepkg.ExecutionCursorSet{
				SchemaVersion: enginepkg.ExecutionCursorSchemaV1, Cursors: []enginepkg.ExecutionCursor{cursor},
			}
		}
	}
	if cursors := h.activeExecutionFrameCursors(); len(cursors) > 0 {
		return &enginepkg.ExecutionCursorSet{SchemaVersion: enginepkg.ExecutionCursorSchemaV1, Cursors: cursors}
	}
	if hasActiveExecutionFrame(h.run.ExecutionFrames) || hasActiveDynamicIncludeResolution(h.run.DynamicIncludes) || h.hasUnreturnedPublication() {
		if index := h.run.CurrentStepIndex; index >= 0 && index < len(h.run.Plan.Steps) {
			step := h.run.Plan.Steps[index]
			invocation := h.debugInvocations.Current(nil, step.ID)
			if invocation < 1 {
				invocation = 1
			}
			return &enginepkg.ExecutionCursorSet{
				SchemaVersion: enginepkg.ExecutionCursorSchemaV1,
				Cursors: []enginepkg.ExecutionCursor{{
					QualifiedNodeID: enginepkg.DebugNodeID(nil, step.ID), StepID: step.ID, StepIndex: index,
					Phase: enginepkg.ExecutionPhaseBefore, Invocation: invocation, RetryAttempt: 1,
				}},
			}
		}
	}
	if h.run.CurrentStepIndex >= 0 && h.run.CurrentStepIndex < len(h.run.Plan.Steps) {
		step := h.run.Plan.Steps[h.run.CurrentStepIndex]
		if _, dispatch := h.preparedDispatchForStep(step.ID); dispatch != nil {
			return &enginepkg.ExecutionCursorSet{
				SchemaVersion: enginepkg.ExecutionCursorSchemaV1,
				Cursors: []enginepkg.ExecutionCursor{{
					QualifiedNodeID: dispatch.QualifiedNodeID,
					CallPath:        append([]enginepkg.DebugCallFrame(nil), dispatch.CallPath...),
					StepID:          dispatch.StepID, StepIndex: h.run.CurrentStepIndex,
					Phase: enginepkg.ExecutionPhaseExecute, Invocation: dispatch.Invocation,
					RetryAttempt: dispatch.RetryAttempt,
				}},
			}
		}
	}
	if len(h.run.Interactions) > 0 && h.run.CurrentStepIndex >= 0 && h.run.CurrentStepIndex < len(h.run.Plan.Steps) {
		step := h.run.Plan.Steps[h.run.CurrentStepIndex]
		callPath := append([]enginepkg.DebugCallFrame(nil), h.debugCallPath...)
		qualifiedNodeID := enginepkg.DebugNodeID(callPath, step.ID)
		invocation := h.debugInvocations.Current(callPath, step.ID)
		for _, interaction := range h.run.Interactions {
			if interaction != nil && interaction.NodeID == qualifiedNodeID && interaction.ExecutionInvocation > 0 {
				invocation = interaction.ExecutionInvocation
				break
			}
		}
		if invocation < 1 {
			invocation = 1
		}
		return &enginepkg.ExecutionCursorSet{
			SchemaVersion: enginepkg.ExecutionCursorSchemaV1,
			Cursors: []enginepkg.ExecutionCursor{{
				QualifiedNodeID: qualifiedNodeID, CallPath: callPath,
				StepID: step.ID, StepIndex: h.run.CurrentStepIndex, Phase: enginepkg.ExecutionPhaseBefore,
				Invocation: invocation, RetryAttempt: 1,
			}},
		}
	}
	nextIndex := h.run.CurrentStepIndex + 1
	for nextIndex < len(h.run.Plan.Steps) && h.run.Plan.Steps[nextIndex].Depth > 0 {
		nextIndex++
	}
	if nextIndex > len(h.run.Plan.Steps) {
		nextIndex = len(h.run.Plan.Steps)
	}
	cursor := enginepkg.ExecutionCursor{
		StepIndex: nextIndex, Phase: enginepkg.ExecutionPhaseBefore,
		Invocation: 1, RetryAttempt: 1, AtEnd: nextIndex >= len(h.run.Plan.Steps),
	}
	if !cursor.AtEnd {
		step := h.run.Plan.Steps[nextIndex]
		cursor.StepID = step.ID
		cursor.CallPath = append([]enginepkg.DebugCallFrame(nil), h.debugCallPath...)
		cursor.QualifiedNodeID = enginepkg.DebugNodeID(cursor.CallPath, step.ID)
		cursor.Invocation = h.debugInvocations.Current(cursor.CallPath, step.ID) + 1
	}
	return &enginepkg.ExecutionCursorSet{
		SchemaVersion: enginepkg.ExecutionCursorSchemaV1,
		Cursors:       []enginepkg.ExecutionCursor{cursor},
	}
}

func (h *runHandle) activeExecutionFrameCursors() []enginepkg.ExecutionCursor {
	if h.run.CurrentStepIndex < 0 || h.run.CurrentStepIndex >= len(h.run.Plan.Steps) {
		return nil
	}
	return executionFrameCursors(
		h.run.CurrentStepIndex, h.run.ExecutionFrames, h.run.Dispatches,
		h.run.Interactions, h.debugInvocations.Snapshot(),
	)
}

func executionFrameCursors(
	rootStepIndex int,
	frames map[string]*enginepkg.ExecutionFrameState,
	dispatches map[string]*enginepkg.DispatchState,
	interactions map[string]*enginepkg.InteractionState,
	invocationCounts map[string]int,
) []enginepkg.ExecutionCursor {
	if rootStepIndex < 0 {
		return nil
	}
	active := make(map[string]*enginepkg.ExecutionFrameState)
	parentHasActiveChild := make(map[string]bool)
	for frameID, frame := range frames {
		if frame == nil || frame.Status != enginepkg.ExecutionFrameStatusActive {
			continue
		}
		active[frameID] = frame
		if frame.ParentFrameID != "" {
			parentHasActiveChild[frame.ParentFrameID] = true
		}
	}
	cursors := make([]enginepkg.ExecutionCursor, 0, len(active))
	for frameID, frame := range active {
		if parentHasActiveChild[frameID] || frame.NextStepIndex < 0 || frame.NextStepIndex >= frame.StepCount ||
			frame.NextStepIndex >= len(frame.StepIDs) {
			continue
		}
		stepID := frame.StepIDs[frame.NextStepIndex]
		qualifiedNodeID := enginepkg.DebugNodeID(frame.CallPath, stepID)
		invocation := invocationCounts[qualifiedNodeID] + 1
		for _, interaction := range interactions {
			if interaction != nil && interaction.FrameID == frame.FrameID &&
				interaction.FrameStepIndex == frame.NextStepIndex && interaction.NodeID == qualifiedNodeID &&
				interaction.ExecutionInvocation > 0 {
				invocation = interaction.ExecutionInvocation
				break
			}
		}
		cursor := enginepkg.ExecutionCursor{
			QualifiedNodeID: qualifiedNodeID,
			CallPath:        append([]enginepkg.DebugCallFrame(nil), frame.CallPath...),
			StepID:          stepID, StepIndex: rootStepIndex, FrameID: frame.FrameID,
			BranchLabel: frame.BranchLabel, IterationIndex: frame.IterationIndex,
			Phase: enginepkg.ExecutionPhaseBefore, Invocation: invocation, RetryAttempt: 1,
		}
		for _, dispatch := range dispatches {
			if dispatch != nil && dispatch.Status == enginepkg.DispatchStatusPrepared &&
				dispatch.FrameID == frame.FrameID && dispatch.FrameStepIndex == frame.NextStepIndex {
				cursor.Phase = enginepkg.ExecutionPhaseExecute
				cursor.Invocation = dispatch.Invocation
				cursor.RetryAttempt = dispatch.RetryAttempt
				break
			}
		}
		cursors = append(cursors, cursor)
	}
	sort.Slice(cursors, func(left, right int) bool {
		if cursors[left].QualifiedNodeID != cursors[right].QualifiedNodeID {
			return cursors[left].QualifiedNodeID < cursors[right].QualifiedNodeID
		}
		if cursors[left].IterationIndex != cursors[right].IterationIndex {
			return cursors[left].IterationIndex < cursors[right].IterationIndex
		}
		return cursors[left].FrameID < cursors[right].FrameID
	})
	return cursors
}

func hasActiveExecutionFrame(frames map[string]*enginepkg.ExecutionFrameState) bool {
	for _, frame := range frames {
		if frame != nil && frame.Status == enginepkg.ExecutionFrameStatusActive {
			return true
		}
	}
	return false
}

func hasActiveDynamicIncludeResolution(resolutions map[string]*enginepkg.DynamicIncludeResolutionState) bool {
	for _, resolution := range resolutions {
		if resolution != nil && resolution.Status == enginepkg.DynamicIncludeResolutionStatusActive {
			return true
		}
	}
	return false
}

// Events returns a read-only channel of structured events.
func (h *runHandle) Events() <-chan enginepkg.Event {
	h.callbacksArmed.Store(true)
	h.drainCallbacks()
	return h.events
}

// --- Interfaces for composite step specs ---

// parallelBranchProvider is implemented by step specs that carry pre-resolved parallel branches.
// The planner wraps schema.ParallelNode branches into engine.BranchSpec form.
type parallelBranchProvider interface {
	enginepkg.StepSpec
	GetBranches() []enginepkg.BranchSpec
}

// waitEventProvider is implemented by step specs for wait_for_event steps.
type waitEventProvider interface {
	enginepkg.StepSpec
	// EventFilter returns the dispatcher filter and optional wait timeout.
	EventFilter() (eventbus.EventFilter, time.Duration)
}

func (h *runHandle) parallelBranches(step enginepkg.ResolvedStep) ([]enginepkg.BranchSpec, *schema.ParallelJoin, error) {
	if provider, ok := step.Spec.(parallelBranchProvider); ok {
		return provider.GetBranches(), nil, nil
	}
	parallel, ok := step.Spec.(*schema.ParallelNode)
	if !ok || parallel == nil {
		return nil, nil, fmt.Errorf("parallel step %q has unsupported spec %T", step.ID, step.Spec)
	}
	if parallel.Join != nil {
		if parallel.Join.WaitFor != "" && parallel.Join.WaitFor != "all" {
			return nil, nil, fmt.Errorf("parallel step %q has unsupported join.wait_for %q", step.ID, parallel.Join.WaitFor)
		}
		if parallel.Join.OnFailure != "" && parallel.Join.OnFailure != "fail" && parallel.Join.OnFailure != "continue" {
			return nil, nil, fmt.Errorf("parallel step %q has unsupported join.on_failure %q", step.ID, parallel.Join.OnFailure)
		}
	}

	directChildren := make(map[string]enginepkg.ResolvedStep)
	for index := h.run.CurrentStepIndex + 1; index < len(h.run.Plan.Steps); index++ {
		candidate := h.run.Plan.Steps[index]
		if candidate.Depth <= step.Depth {
			break
		}
		if candidate.Depth != step.Depth+1 || candidate.ParentID != step.ID || candidate.ParentKind != "parallel" {
			continue
		}
		if _, duplicate := directChildren[candidate.ID]; duplicate {
			return nil, nil, fmt.Errorf("parallel step %q has duplicate child id %q", step.ID, candidate.ID)
		}
		directChildren[candidate.ID] = candidate
	}

	branches := make([]enginepkg.BranchSpec, len(parallel.Branches))
	for branchIndex, branch := range parallel.Branches {
		branches[branchIndex].Label = branch.Label
		branches[branchIndex].Steps = make([]enginepkg.ResolvedStep, 0, len(branch.Steps))
		for nodeIndex, node := range branch.Steps {
			childID, err := directFlowNodeID(node)
			if err != nil {
				return nil, nil, fmt.Errorf("parallel step %q branch %d node %d: %w", step.ID, branchIndex, nodeIndex, err)
			}
			child, found := directChildren[childID]
			if !found {
				return nil, nil, fmt.Errorf("parallel step %q child %q is absent from the frozen plan", step.ID, childID)
			}
			branches[branchIndex].Steps = append(branches[branchIndex].Steps, child)
		}
	}
	return branches, parallel.Join, nil
}

func directFlowNodeID(node schema.FlowNode) (string, error) {
	switch {
	case node.Step != nil && node.Step.ID != "":
		return node.Step.ID, nil
	case node.Iterate != nil && node.Iterate.ID != "":
		return node.Iterate.ID, nil
	case node.Parallel != nil && node.Parallel.ID != "":
		return node.Parallel.ID, nil
	default:
		return "", errors.New("flow node has no id")
	}
}

func (h *runHandle) waitEventConfig(step enginepkg.ResolvedStep) (eventbus.EventFilter, time.Duration, string, error) {
	if provider, ok := step.Spec.(waitEventProvider); ok {
		filter, timeout := provider.EventFilter()
		return filter, timeout, "", nil
	}
	spec, ok := step.Spec.(*schema.WaitForEventSpec)
	if !ok || spec == nil {
		return eventbus.EventFilter{}, 0, "", fmt.Errorf("wait_for_event step %q has unsupported spec %T", step.ID, step.Spec)
	}
	timeout := time.Duration(0)
	if step.Timeout != "" {
		parsed, err := time.ParseDuration(step.Timeout)
		if err != nil || parsed <= 0 {
			return eventbus.EventFilter{}, 0, "", fmt.Errorf("wait_for_event step %q has invalid timeout %q", step.ID, step.Timeout)
		}
		timeout = parsed
	}
	filter := eventbus.EventFilter{
		Source:  string(spec.Event.Source),
		ID:      spec.Event.ID,
		Payload: make(map[string]string, len(spec.Event.Filter)),
	}
	for key, value := range spec.Event.Filter {
		filter.Payload[key] = value
	}
	if evaluator := h.engine.cfg.Evaluator; evaluator != nil {
		resolvedID, err := evaluator.Eval(filter.ID, h.run.Vars)
		if err != nil {
			return eventbus.EventFilter{}, 0, "", fmt.Errorf("wait_for_event step %q event.id: %w", step.ID, err)
		}
		filter.ID = resolvedID
		for key, value := range filter.Payload {
			resolved, err := evaluator.Eval(value, h.run.Vars)
			if err != nil {
				return eventbus.EventFilter{}, 0, "", fmt.Errorf("wait_for_event step %q event.filter.%s: %w", step.ID, key, err)
			}
			filter.Payload[key] = resolved
		}
	}
	return filter, timeout, spec.OnTimeout, nil
}

func applyWaitTimeoutRouting(step enginepkg.ResolvedStep, onTimeout string) (enginepkg.ResolvedStep, error) {
	switch {
	case onTimeout == "" || onTimeout == "fail":
		return step, nil
	case onTimeout == "continue" || onTimeout == "stop" || strings.HasPrefix(onTimeout, "goto:"):
		step.OnError = onTimeout
		return step, nil
	default:
		return step, fmt.Errorf("wait_for_event step %q has unsupported on_timeout %q", step.ID, onTimeout)
	}
}

// --- Branch execution helpers ---

// forwardSubEngineEvent receives an event from a sub-engine spawned by
// a SubStepRunner inside a parallel branch.
func (bh *branchHandle) forwardSubEngineEvent(ev enginepkg.Event) {
	bh.parent.forwardSubEngineEvent(ev)
}

func (bh *branchHandle) emit(ctx context.Context, kind trace.EventKind, payload map[string]any) error {
	bh.parent.mu.Lock()
	defer bh.parent.unlockAndDrainCallbacks()
	return bh.parent.emitEventLocked(ctx, kind, payload)
}

func (bh *branchHandle) publishStart(ctx context.Context, payload map[string]any) error {
	bh.parent.mu.Lock()
	defer bh.parent.mu.Unlock()
	if err := bh.parent.executionErrorLocked(ctx); err != nil {
		return err
	}
	if err := bh.parent.emitEventLocked(ctx, trace.EventKindStepStarted, payload); err != nil {
		return err
	}
	return bh.parent.publishBoundaryLocked(ctx)
}

// branchHandle executes a single parallel branch with isolated variables.
type branchHandle struct {
	engine       *impl
	run          *enginepkg.Run
	parent       *runHandle
	vars         map[string]any
	stepCtx      context.Context
	callPath     []enginepkg.DebugCallFrame
	frameBinding enginepkg.ExecutionFrameBinding
	startIndex   int
	priorResults []*enginepkg.StepResult
}

func (bh *branchHandle) executeSteps(ctx context.Context, steps []enginepkg.ResolvedStep) ([]*enginepkg.StepResult, error) {
	results := append([]*enginepkg.StepResult(nil), bh.priorResults...)
	for index := bh.startIndex; index < len(steps); index++ {
		step := steps[index]
		result, err := bh.executeOne(ctx, index, step)
		if result != nil {
			results = append(results, result)
		}
		if err != nil {
			return results, err
		}
		if result.Status == enginepkg.StepStatusFailed {
			// Honour the nested step's own recovery before propagating.
			if resolveOnError(step) == "continue" {
				continue
			}
			if result.Error != nil {
				return results, result.Error
			}
			return results, fmt.Errorf("step %s failed", step.ID)
		}
	}
	return results, nil
}

func (bh *branchHandle) executeOne(ctx context.Context, stepIndex int, step enginepkg.ResolvedStep) (*enginepkg.StepResult, error) {
	digest := ""
	if parent := enginepkg.ToolPresentationFromContext(ctx); parent != nil {
		digest = parent.SnapshotDigest
	}
	ctx = enginepkg.WithToolPresentationCapture(ctx, digest)
	invocation := bh.frameBinding.Invocation
	if invocation == 0 {
		invocation = 1
	}
	occurrenceCtx := enginepkg.WithDebugCallPath(ctx, bh.callPath)
	occurrenceCtx = enginepkg.WithDispatchExecutionBoundary(occurrenceCtx, enginepkg.DispatchExecutionBoundary{
		QualifiedNodeID: enginepkg.DebugNodeID(bh.callPath, step.ID),
		CallPath:        append([]enginepkg.DebugCallFrame(nil), bh.callPath...),
		StepID:          step.ID, StepIndex: stepIndex, FrameID: bh.frameBinding.FrameID, FrameStepIndex: stepIndex,
		Phase: enginepkg.ExecutionPhaseExecute, Invocation: invocation, RetryAttempt: 1, OccurrenceSequence: 1,
	})
	if err := bh.publishStart(occurrenceCtx, traceOccurrencePayload(occurrenceCtx, step.ID, map[string]any{
		"step_id": step.ID,
		"kind":    step.Kind,
	})); err != nil {
		return nil, err
	}

	startedAt := time.Now()

	exec := bh.engine.cfg.Executors.Lookup(step.Kind)
	if exec == nil {
		err := &ExecutorNotFoundError{Kind: step.Kind}
		result := &enginepkg.StepResult{
			StepID:      step.ID,
			Status:      enginepkg.StepStatusFailed,
			Outcome:     enginepkg.StepOutcomeFailed,
			StartedAt:   startedAt,
			CompletedAt: time.Now(),
			DurationMs:  time.Since(startedAt).Milliseconds(),
			Error:       err,
		}
		bh.emit(occurrenceCtx, trace.EventKindStepFailed, traceOccurrencePayload(occurrenceCtx, step.ID, map[string]any{
			"step_id": step.ID,
			"error":   err.Error(),
		}))
		return result, err
	}

	stepCtx := enginepkg.WithRunID(ctx, bh.run.ID)
	stepCtx = enginepkg.WithEventForwarder(stepCtx, bh.forwardSubEngineEvent)
	stepCtx = context.WithValue(stepCtx, publicationParentKey{}, bh.parent)
	stepCtx = executor.WithEventEmitter(stepCtx, func(kind string, payload map[string]any) {
		bh.emit(ctx, trace.EventKind(kind), payload)
	})
	stepCtx = enginepkg.WithDebugCallPath(stepCtx, bh.callPath)
	stepCtx = enginepkg.WithExecutionFrameBinding(stepCtx, bh.frameBinding)
	stepCtx = enginepkg.WithDispatchExecutionBoundary(stepCtx, enginepkg.DispatchExecutionBoundary{
		QualifiedNodeID: enginepkg.DebugNodeID(bh.callPath, step.ID),
		CallPath:        append([]enginepkg.DebugCallFrame(nil), bh.callPath...),
		StepID:          step.ID, StepIndex: stepIndex, FrameID: bh.frameBinding.FrameID,
		FrameStepIndex: stepIndex, Phase: enginepkg.ExecutionPhaseExecute,
		Invocation: invocation, RetryAttempt: 1, OccurrenceSequence: 1,
	})
	result, execErr := exec.Execute(stepCtx, step, bh.vars)
	if execErr != nil {
		if enginepkg.IsReplayBoundaryError(execErr) {
			return nil, execErr
		}
		if enginepkg.IsRouteTestBoundaryError(execErr) {
			return nil, execErr
		}
		result := &enginepkg.StepResult{
			StepID: step.ID, Status: enginepkg.StepStatusFailed, Outcome: enginepkg.StepOutcomeFailed,
			StartedAt: startedAt, CompletedAt: time.Now(), Error: execErr,
		}
		if commitErr := bh.commitFrameStep(stepCtx, stepIndex, step.Kind, result); commitErr != nil {
			return result, commitErr
		}
		if enginepkg.ExecutionFrameCommitterFromContext(stepCtx) != nil && bh.frameBinding.FrameID != "" {
			return result, execErr
		}
		bh.emit(stepCtx, trace.EventKindStepFailed, traceOccurrencePayload(stepCtx, step.ID, map[string]any{
			"step_id": step.ID,
			"error":   execErr.Error(),
		}))
		return result, execErr
	}

	if result != nil && enginepkg.IsReplayBoundaryError(result.Error) {
		return nil, result.Error
	}
	if result.StartedAt.IsZero() {
		result.StartedAt = startedAt
	}
	if result.CompletedAt.IsZero() {
		result.CompletedAt = time.Now()
	}
	if result.DurationMs == 0 {
		result.DurationMs = result.CompletedAt.Sub(result.StartedAt).Milliseconds()
	}

	if result.Status == enginepkg.StepStatusCompleted {
		if err := executor.ValidateBindingWrites(enginepkg.BindingScopeFromContext(stepCtx), result.Vars); err != nil {
			result.Status, result.Outcome, result.Error = enginepkg.StepStatusFailed, enginepkg.StepOutcomeFailed, err
			result.Vars, result.Results, result.PublicOutputs = nil, nil, nil
		}
	}
	for k, v := range result.Vars {
		bh.vars[k] = v
	}
	frameResultCommitted := enginepkg.ExecutionFrameCommitterFromContext(stepCtx) != nil && bh.frameBinding.FrameID != ""
	if err := bh.commitFrameStep(stepCtx, stepIndex, step.Kind, result); err != nil {
		return result, err
	}
	if frameResultCommitted {
		return result, nil
	}

	switch result.Status {
	case enginepkg.StepStatusCompleted:
		bh.emit(stepCtx, trace.EventKindStepCompleted, traceOccurrencePayload(stepCtx, step.ID, map[string]any{
			"step_id":     step.ID,
			"kind":        step.Kind,
			"status":      result.Status,
			"outcome":     result.Outcome,
			"duration_ms": result.DurationMs,
			"output":      result.Output,
			"captures":    result.Vars,
		}))
	case enginepkg.StepStatusFailed:
		errStr := ""
		if result.Error != nil {
			errStr = result.Error.Error()
		}
		bh.emit(stepCtx, trace.EventKindStepFailed, traceOccurrencePayload(stepCtx, step.ID, map[string]any{
			"step_id":  step.ID,
			"kind":     step.Kind,
			"status":   result.Status,
			"outcome":  result.Outcome,
			"error":    errStr,
			"output":   result.Output,
			"captures": result.Vars,
		}))
	}

	return result, nil
}

func (bh *branchHandle) commitFrameStep(ctx context.Context, stepIndex int, stepKind string, result *enginepkg.StepResult) error {
	committer := enginepkg.ExecutionFrameCommitterFromContext(ctx)
	if committer == nil || bh.frameBinding.FrameID == "" {
		return nil
	}
	frame, err := committer.CommitExecutionFrameStep(context.WithoutCancel(ctx), enginepkg.ExecutionFrameStepCommit{
		FrameID: bh.frameBinding.FrameID, StepIndex: stepIndex, StepKind: stepKind,
		Result: result, WorkingVars: bh.vars,
	})
	if committed := frame.Results[strconv.Itoa(stepIndex)]; committed != nil {
		*result = *cloneStepResult(committed)
	}
	return err
}

// --- Utilities ---

// safeClose closes ch exactly once, guarded by the closed flag.
func safeClose(ch chan enginepkg.Event, closed *atomic.Bool) {
	if closed.CompareAndSwap(false, true) {
		close(ch)
	}
}

// isTerminalOutput returns true when a step marks the run as terminal.
func isTerminalOutput(result *enginepkg.StepResult) bool {
	if result == nil || result.Output == nil {
		return false
	}
	terminal, ok := result.Output["terminal"].(bool)
	return ok && terminal
}

// completeTerminalRun marks the run as completed due to an explicit end step.
func (h *runHandle) completeTerminalRun(ctx context.Context, result *enginepkg.StepResult) error {
	if !h.done.Load() {
		h.run.Status = enginepkg.RunStatusCompleted
		h.run.CompletedAt = time.Now()
		h.done.Store(true)
	}

	h.runSpan.SetStatus(otelPkg.StatusOK, "")
	h.runSpan.End()
	safeClose(h.events, &h.eventsClosed)
	h.cancelFn()
	return nil
}

func (h *runHandle) terminalRunPayload(result *enginepkg.StepResult) map[string]any {
	payload := map[string]any{
		"run_id": h.run.ID, "duration_ms": h.run.CompletedAt.Sub(h.run.StartedAt).Milliseconds(),
	}
	if result != nil && result.Output != nil {
		if category, ok := result.Output["outcome_category"]; ok {
			payload["outcome_category"] = category
		}
		if code, ok := result.Output["outcome_code"]; ok {
			payload["outcome_code"] = code
		}
	}
	return payload
}

// mergeContexts preserves values from a and completes with the originating
// cancellation or deadline error when either input completes.
func mergeContexts(a, b context.Context) context.Context {
	merged := &mergedContext{primary: a, done: make(chan struct{})}
	if first, ok := a.Deadline(); ok {
		merged.deadline, merged.hasDeadline = first, true
	}
	if second, ok := b.Deadline(); ok && (!merged.hasDeadline || second.Before(merged.deadline)) {
		merged.deadline, merged.hasDeadline = second, true
	}
	if err := a.Err(); err != nil {
		merged.finish(err)
		return merged
	}
	if err := b.Err(); err != nil {
		merged.finish(err)
		return merged
	}
	go func() {
		select {
		case <-a.Done():
			merged.finish(a.Err())
		case <-b.Done():
			merged.finish(b.Err())
		}
	}()
	return merged
}

type mergedContext struct {
	primary     context.Context
	done        chan struct{}
	once        sync.Once
	mu          sync.RWMutex
	err         error
	deadline    time.Time
	hasDeadline bool
}

func (ctx *mergedContext) Deadline() (time.Time, bool) { return ctx.deadline, ctx.hasDeadline }
func (ctx *mergedContext) Done() <-chan struct{}       { return ctx.done }
func (ctx *mergedContext) Value(key any) any           { return ctx.primary.Value(key) }

func (ctx *mergedContext) Err() error {
	ctx.mu.RLock()
	defer ctx.mu.RUnlock()
	return ctx.err
}

func (ctx *mergedContext) finish(err error) {
	ctx.once.Do(func() {
		ctx.mu.Lock()
		ctx.err = err
		ctx.mu.Unlock()
		close(ctx.done)
	})
}

// ExecutorNotFoundError is returned when no executor is registered for a step kind.
type ExecutorNotFoundError struct {
	Kind string
}

func (e *ExecutorNotFoundError) Error() string {
	return "engine: no executor registered for step kind: " + e.Kind
}

// GovernanceDeniedError is returned when a step is denied by governance policy.
type GovernanceDeniedError struct {
	StepID string
	Reason string
}

func (e *GovernanceDeniedError) Error() string {
	return "governance: step " + e.StepID + " denied: " + e.Reason
}

// extractCommand returns argv[0] from a CLI step spec.
// Returns "" for non-CLI steps.
func extractCommand(step enginepkg.ResolvedStep) string {
	if cli, ok := step.Spec.(*schema.CLISpec); ok && cli != nil {
		return cli.Command
	}
	return ""
}

// extractEnvVars returns the environment variables from a CLI step spec.
// Returns nil for non-CLI steps.
func extractEnvVars(step enginepkg.ResolvedStep) map[string]string {
	if cli, ok := step.Spec.(*schema.CLISpec); ok && cli != nil {
		return cli.Env
	}
	return nil
}

// applyFilteredEnvVars replaces the step's env vars with the filtered set.
func applyFilteredEnvVars(step enginepkg.ResolvedStep, filtered map[string]string) {
	if cli, ok := step.Spec.(*schema.CLISpec); ok && cli != nil && filtered != nil {
		cli.Env = filtered
	}
}
