package sessioncoordinator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/google/uuid"
	internaldebugprotect "github.com/ormasoftchile/yawr/runtime/internal/debugprotect"
	internalgovernance "github.com/ormasoftchile/yawr/runtime/internal/governance"
	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/sensitive"
	"github.com/ormasoftchile/yawr/runtime/pkg/session"
)

type Config struct {
	Sessions        session.Store
	Runs            engine.DurableRunStore
	Engine          engine.Engine
	HandoffResolver HandoffResolver
}

type HandoffResolveRequest struct {
	SessionID     string
	SourceSegment session.SegmentRecord
	SourcePlan    *engine.ExecutionPlan
	Handoff       engine.HandoffRequest
}

type HandoffTarget struct {
	Plan  *engine.ExecutionPlan
	Graph json.RawMessage
}

type HandoffResolver interface {
	ResolveHandoff(context.Context, HandoffResolveRequest) (HandoffTarget, error)
}

var errPreparedHandoffMismatch = errors.New("prepared handoff mismatch")

func preparedHandoffMismatch(message string) error {
	return fmt.Errorf("%w: %s", errPreparedHandoffMismatch, message)
}

type Coordinator struct {
	sessions    session.Store
	runs        engine.DurableRunStore
	projections runProjectionStore
	runtime     coordinatorEngine
	handoffs    HandoffResolver
}

type coordinatorEngine interface {
	engine.Engine
	PrepareDurablePlan(context.Context, *engine.ExecutionPlan) error
	ValidateDurableHandoffPlan(context.Context, *engine.ExecutionPlan, engine.RunOptions) error
	ValidateDurableHandoffArtifacts(
		context.Context, *engine.ExecutionPlan, engine.RunOptions, ...json.RawMessage,
	) error
}

type runProjectionStore interface {
	ExportRunProjection(context.Context, string) (json.RawMessage, error)
	RestoreRunProjection(context.Context, string, json.RawMessage) error
	DeleteRun(context.Context, string) error
	RunDir(string) string
	TracePath(string) string
}

func New(config Config) (*Coordinator, error) {
	missing := make([]string, 0, 3)
	if config.Sessions == nil {
		missing = append(missing, "Sessions")
	}
	if config.Runs == nil {
		missing = append(missing, "Runs")
	}
	if config.Engine == nil {
		missing = append(missing, "Engine")
	}
	projections, ok := config.Runs.(runProjectionStore)
	if config.Runs != nil && !ok {
		missing = append(missing, "Runs session projection capabilities")
	}
	runtime, runtimeOK := config.Engine.(coordinatorEngine)
	if config.Engine != nil && !runtimeOK {
		missing = append(missing, "Engine durable session validation capabilities")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("session coordinator: missing configuration %v", missing)
	}
	return &Coordinator{
		sessions: config.Sessions, runs: config.Runs, projections: projections,
		runtime: runtime, handoffs: config.HandoffResolver,
	}, nil
}

type StartRequest struct {
	SessionID             string
	CommandID             string
	SegmentID             string
	RunID                 string
	Plan                  *engine.ExecutionPlan
	Graph                 json.RawMessage
	RunOptions            engine.RunOptions
	DerivedFromSessionID  string
	DerivedFromScenarioID string
}

type ResumeRequest struct {
	SessionID           string
	CommandID           string
	ClientCommandDigest string
	RunOptions          engine.RunOptions
}

type Inspection struct {
	Manifest session.Manifest
	Run      *engine.RunState
}

func (coordinator *Coordinator) Start(ctx context.Context, request StartRequest) (*Handle, error) {
	if request.Plan == nil || request.Plan.Validation == nil {
		return nil, errors.New("session coordinator: validated plan is required")
	}
	if !validRunMode(request.RunOptions.Mode) {
		return nil, fmt.Errorf("session coordinator: invalid run mode %q", request.RunOptions.Mode)
	}
	if !json.Valid(request.Graph) {
		return nil, errors.New("session coordinator: graph snapshot must be valid JSON")
	}
	lease, err := coordinator.sessions.AcquireSessionLease(ctx, request.SessionID)
	if err != nil {
		return nil, err
	}
	release := true
	defer func() {
		if release {
			_ = lease.Release()
		}
	}()

	plan, err := clonePlanForRun(request.Plan, request.RunID)
	if err != nil {
		return nil, err
	}
	if err := coordinator.runtime.PrepareDurablePlan(ctx, plan); err != nil {
		return nil, fmt.Errorf("session coordinator: prepare durable plan: %w", err)
	}
	if err := coordinator.runtime.ValidateDurableHandoffPlan(ctx, plan, request.RunOptions); err != nil {
		return nil, fmt.Errorf("session coordinator: validate durable handoff plan: %w", err)
	}
	canonicalGraph, err := finalizeExecutionPlanGraph(plan, nil, request.Graph)
	if err != nil {
		return nil, fmt.Errorf("session coordinator: validate source graph: %w", err)
	}
	planBlob, err := snapshotBlob(plan)
	if err != nil {
		return nil, err
	}
	boundGraph, err := bindHandoffGraph(plan, planBlob.Digest, canonicalGraph)
	if err != nil {
		return nil, fmt.Errorf("session coordinator: validate source graph: %w", err)
	}
	if err := coordinator.runtime.ValidateDurableHandoffArtifacts(
		ctx, plan, request.RunOptions, planBlob.Data, boundGraph,
	); err != nil {
		return nil, fmt.Errorf("session coordinator: validate source graph: %w", err)
	}
	graphBlob := session.NewJSONBlob(boundGraph)
	sessionRecord := session.SessionRecord{
		Status: session.StatusActive, DerivedFromSessionID: request.DerivedFromSessionID,
		DerivedFromScenarioID: request.DerivedFromScenarioID,
	}
	segmentRecord := session.SegmentRecord{
		SegmentID: request.SegmentID, Ordinal: 1, RunbookID: plan.Metadata.RunbookID,
		RunbookName: plan.Metadata.RunbookName, EntrySelector: session.EntrySelector{Step: "$entry"},
		Status: session.SegmentStatusPrepared, PlanHash: plan.Metadata.PlanHash,
		GraphHash: graphBlob.Digest, ExecutableSnapshotHash: planBlob.Digest,
		CatalogDigest:     plan.Metadata.CatalogDigest,
		PackageLockDigest: session.DigestJSON(plan.Metadata.PackageDigests),
		ProfileDigest:     session.DigestJSON(plan.Metadata.Profile), AttemptRunIDs: []string{request.RunID},
		ExecutableRevision: 1, GraphRevision: 1,
	}
	attemptRecord := session.RunAttemptRecord{
		RunID: request.RunID, SegmentID: request.SegmentID, Ordinal: 1,
		Mode: string(request.RunOptions.Mode), Status: session.AttemptStatusStarting,
		Actor: request.RunOptions.Actor, Client: request.RunOptions.Client, PlanHash: plan.Metadata.PlanHash,
	}
	identity, err := session.NewCreationIdentity(
		request.SessionID, sessionRecord, segmentRecord, attemptRecord,
		request.RunOptions.Vars, request.RunOptions.RuntimeVars, request.RunOptions.ScenarioDir,
	)
	if err != nil {
		return nil, fmt.Errorf("session coordinator: durable start identity: %w", err)
	}
	entryDigest := session.CreationDigest(identity)
	manifest, err := coordinator.sessions.CreateSession(ctx, lease.Epoch(), session.CreateRequest{
		SessionID: request.SessionID, CommandID: request.CommandID, EntryDigest: entryDigest, Identity: identity,
		Session: sessionRecord, Segment: segmentRecord, Attempt: attemptRecord,
		Blobs: []session.JSONBlob{planBlob, graphBlob},
	})
	if err != nil {
		manifest, err = coordinator.recoverCommittedCommand(
			ctx, request.SessionID, request.CommandID, entryDigest, session.EventSessionCreated, manifest, err,
		)
		if err != nil {
			return nil, err
		}
	}
	attempt := manifest.Attempts[request.RunID]
	runHandle, sessionRuns, err := coordinator.startOrResumeRun(
		ctx, request.SessionID, lease.Epoch(), manifest, attempt, plan, request.RunOptions,
	)
	if err != nil {
		return nil, err
	}
	if attempt.ExecutionMutationHash == "" {
		if err := sessionRuns.SaveState(context.WithoutCancel(ctx), runHandle.State()); err != nil {
			_ = runHandle.Cancel(context.Background(), "initial session execution commit failed")
			return nil, err
		}
	}
	manifest = sessionRuns.Manifest()
	handle := &Handle{
		coordinator: coordinator, sessionID: request.SessionID, runID: request.RunID,
		lease: lease, run: runHandle, sessionRuns: sessionRuns, manifest: manifest,
		runOptions: request.RunOptions,
	}
	sessionRuns.SetManifestObserver(handle.setManifest)
	release = false
	return handle, nil
}

func (coordinator *Coordinator) Resume(ctx context.Context, request ResumeRequest) (*Handle, error) {
	lease, err := coordinator.sessions.AcquireSessionLease(ctx, request.SessionID)
	if err != nil {
		return nil, err
	}
	handle, err := coordinator.resumeWithLease(ctx, request, lease)
	if err != nil {
		_ = lease.Release()
	}
	return handle, err
}

func (coordinator *Coordinator) ResumeAttached(
	ctx context.Context,
	request ResumeRequest,
	lease session.Lease,
) (*Handle, error) {
	if lease == nil || lease.Epoch() == 0 {
		return nil, errors.New("session coordinator: attached session lease is required")
	}
	return coordinator.resumeWithLease(ctx, request, lease)
}

func (coordinator *Coordinator) CloseAttached(
	ctx context.Context,
	request session.CloseRequest,
	lease session.Lease,
) (session.Manifest, error) {
	if lease == nil || lease.Epoch() == 0 {
		return session.Manifest{}, errors.New("session coordinator: attached session lease is required")
	}
	request.WriterEpoch = lease.Epoch()
	manifest, err := coordinator.sessions.CloseSession(context.WithoutCancel(ctx), request)
	if err != nil {
		manifest, err = coordinator.recoverCommittedCommand(
			ctx, request.SessionID, request.CommandID, session.CloseDigest(request.Status),
			session.EventSessionClosed, manifest, err,
		)
		if err != nil {
			return session.Manifest{}, err
		}
	}
	receipt, found := manifest.AcceptedCommands[request.CommandID]
	if !found || receipt.ClientCommandDigest != request.ClientCommandDigest {
		return session.Manifest{}, session.ErrCommandConflict
	}
	return manifest, nil
}

func (coordinator *Coordinator) resumeWithLease(
	ctx context.Context,
	request ResumeRequest,
	lease session.Lease,
) (*Handle, error) {
	manifest, err := coordinator.sessions.LoadManifest(ctx, request.SessionID)
	if err != nil {
		return nil, err
	}
	runID := manifest.Session.ActiveRunID
	if runID == "" {
		return nil, errors.New("session coordinator: session has no resumable active attempt")
	}
	attempt := manifest.Attempts[runID]
	attemptMode := engine.RunMode(attempt.Mode)
	if !validRunMode(attemptMode) {
		return nil, fmt.Errorf("session coordinator: journaled attempt has invalid mode %q", attempt.Mode)
	}
	if request.RunOptions.Mode != "" && request.RunOptions.Mode != attemptMode {
		return nil, fmt.Errorf(
			"session coordinator: resume mode %q does not match journaled mode %q",
			request.RunOptions.Mode, attemptMode,
		)
	}
	request.RunOptions.Mode = attemptMode
	if attempt.Status == session.AttemptStatusHandoffPending {
		durableCtx := context.WithoutCancel(ctx)
		_, prepared, preparedErr := solePreparedTransition(manifest)
		if preparedErr != nil {
			return nil, preparedErr
		}
		if prepared {
			manifest, err = coordinator.commitPreparedHandoff(durableCtx, request.SessionID, lease.Epoch(), manifest)
		} else if transitionFromSourceExists(manifest, manifest.Session.ActiveSegmentID) {
			return nil, errors.New("session coordinator: handoff source already has a non-prepared transition")
		} else {
			sourceState, sourcePlan, loadErr := coordinator.loadHandoffSource(durableCtx, request.SessionID, manifest, attempt)
			if loadErr != nil {
				return nil, loadErr
			}
			if sourceState.PendingHandoff == nil {
				return nil, errors.New("session coordinator: durable handoff request is unavailable")
			}
			committed := committedHandoff{}
			err = coordinator.prepareAndCommitHandoff(
				durableCtx, request.SessionID, lease.Epoch(), manifest, sourcePlan,
				&sourceState, *sourceState.PendingHandoff, &committed,
			)
			manifest = committed.manifest
		}
		if err != nil {
			return nil, err
		}
		runID = manifest.Session.ActiveRunID
		attempt = manifest.Attempts[runID]
		attemptMode = engine.RunMode(attempt.Mode)
		if !validRunMode(attemptMode) {
			return nil, fmt.Errorf("session coordinator: prepared handoff target has invalid mode %q", attempt.Mode)
		}
		request.RunOptions.Mode = attemptMode
	}
	resumableIndeterminate := attempt.Status == session.AttemptStatusIndeterminate && request.RunOptions.AcknowledgeIndeterminate
	if attempt.Status != session.AttemptStatusStarting && attempt.Status != session.AttemptStatusRunning &&
		attempt.Status != session.AttemptStatusWaiting && attempt.Status != session.AttemptStatusPausedAtBoundary &&
		!resumableIndeterminate {
		return nil, errors.New("session coordinator: active attempt is not resumable")
	}
	runningDigest := session.AttemptUpdateDigest(runID, session.AttemptStatusRunning)
	resumeDigest := session.ResumeDigest(runID)
	resumeViaSessionEvent := attempt.Status == session.AttemptStatusPausedAtBoundary
	if receipt, found := manifest.AcceptedCommands[request.CommandID]; found {
		switch {
		case receipt.EventKind == session.EventSessionResumed && receipt.CommandDigest == resumeDigest:
			resumeViaSessionEvent = true
		case receipt.EventKind == session.EventAttemptUpdated && receipt.CommandDigest == runningDigest:
		default:
			return nil, session.ErrCommandConflict
		}
	}
	segment := manifest.Segments[attempt.SegmentID]
	encodedPlan, err := coordinator.sessions.ReadBlob(ctx, request.SessionID, segment.ExecutableSnapshotHash)
	if err != nil {
		return nil, err
	}
	plan, err := restorePlanBlob(encodedPlan)
	if err != nil {
		return nil, err
	}
	plan.RunID = runID
	if transition, found, transitionErr := transitionForTargetRun(manifest, runID); transitionErr != nil {
		return nil, transitionErr
	} else if found {
		contextData, readErr := coordinator.sessions.ReadBlob(ctx, request.SessionID, transition.ContextDigest)
		if readErr != nil {
			return nil, fmt.Errorf("session coordinator: load handoff context: %w", readErr)
		}
		var handoffContext session.HandoffContext
		if err := decodeStrictDocument(contextData, &handoffContext); err != nil {
			return nil, fmt.Errorf("session coordinator: decode handoff context: %w", err)
		}
		targetVars, validateErr := validateHandoffInputs(plan, handoffContext.Inputs)
		if validateErr != nil {
			return nil, validateErr
		}
		request.RunOptions.Vars = nil
		request.RunOptions.RuntimeVars = targetVars
	}
	runHandle, sessionRuns, err := coordinator.startOrResumeRun(
		ctx, request.SessionID, lease.Epoch(), manifest, attempt, plan, request.RunOptions,
	)
	if err != nil {
		return nil, err
	}
	manifest = sessionRuns.Manifest()
	if attempt.ExecutionMutationHash == "" {
		if err := sessionRuns.SaveState(context.WithoutCancel(ctx), runHandle.State()); err != nil {
			_ = runHandle.Cancel(context.Background(), "initial resumed session execution commit failed")
			return nil, err
		}
		manifest = sessionRuns.Manifest()
	}
	if resumeViaSessionEvent {
		manifest, err = coordinator.sessions.ResumeSession(context.WithoutCancel(ctx), session.ResumeSessionRequest{
			SessionID: request.SessionID, CommandID: request.CommandID, WriterEpoch: lease.Epoch(),
			ExpectedSequence: manifest.Session.Sequence, ClientCommandDigest: request.ClientCommandDigest,
		})
		if err != nil {
			manifest, err = coordinator.recoverCommittedCommand(
				ctx, request.SessionID, request.CommandID, resumeDigest,
				session.EventSessionResumed, manifest, err,
			)
		}
	} else {
		manifest, err = coordinator.commitAttempt(context.WithoutCancel(ctx), session.AttemptUpdateRequest{
			SessionID: request.SessionID, CommandID: request.CommandID, WriterEpoch: lease.Epoch(),
			ExpectedSequence: manifest.Session.Sequence, RunID: runID, Status: session.AttemptStatusRunning,
		})
	}
	if err != nil {
		_ = runHandle.Cancel(context.Background(), "session resume registration failed")
		return nil, err
	}
	sessionRuns.SetManifest(manifest)
	handle := &Handle{
		coordinator: coordinator, sessionID: request.SessionID, runID: runID,
		lease: lease, run: runHandle, sessionRuns: sessionRuns, manifest: manifest,
		runOptions: request.RunOptions,
	}
	sessionRuns.SetManifestObserver(handle.setManifest)
	return handle, nil
}

func (coordinator *Coordinator) loadHandoffSource(
	ctx context.Context,
	sessionID string,
	manifest session.Manifest,
	attempt session.RunAttemptRecord,
) (engine.RunState, *engine.ExecutionPlan, error) {
	if attempt.Status != session.AttemptStatusHandoffPending || attempt.ExecutionMutationHash == "" {
		return engine.RunState{}, nil, errors.New("session coordinator: handoff source has no committed checkpoint")
	}
	segment, found := manifest.Segments[attempt.SegmentID]
	if !found || manifest.Session.ActiveRunID != attempt.RunID || manifest.Session.ActiveSegmentID != segment.SegmentID {
		return engine.RunState{}, nil, errors.New("session coordinator: handoff source topology is invalid")
	}
	planData, err := coordinator.sessions.ReadBlob(ctx, sessionID, segment.ExecutableSnapshotHash)
	if err != nil {
		return engine.RunState{}, nil, err
	}
	plan, err := restorePlanBlob(planData)
	if err != nil {
		return engine.RunState{}, nil, err
	}
	plan.RunID = attempt.RunID
	mutation, _, archive, err := coordinator.loadAuthoritativeProjection(ctx, sessionID, attempt)
	if err != nil {
		return engine.RunState{}, nil, err
	}
	if err := coordinator.projections.RestoreRunProjection(ctx, attempt.RunID, archive); err != nil {
		return engine.RunState{}, nil, fmt.Errorf("session coordinator: restore handoff source: %w", err)
	}
	state, stateErr := coordinator.runs.LoadState(ctx, attempt.RunID)
	restoredPlan, planErr := coordinator.runs.LoadPlan(ctx, attempt.RunID)
	if stateErr != nil || planErr != nil {
		return engine.RunState{}, nil, errors.Join(stateErr, planErr)
	}
	if err := coordinator.validateRestoredExecutionMutation(mutation, state, plan, restoredPlan); err != nil {
		return engine.RunState{}, nil, err
	}
	if state.Status != engine.RunStatusHandoffPending || state.PendingHandoff == nil {
		return engine.RunState{}, nil, errors.New("session coordinator: restored source is not handoff-pending")
	}
	state.Plan = plan
	return state, plan, nil
}

func validRunMode(mode engine.RunMode) bool {
	switch mode {
	case engine.RunModeReal, engine.RunModeDryRun, engine.RunModeReplay, engine.RunModeRouteTest:
		return true
	default:
		return false
	}
}

func (coordinator *Coordinator) commitAttempt(
	ctx context.Context,
	request session.AttemptUpdateRequest,
) (session.Manifest, error) {
	committed, err := coordinator.sessions.UpdateAttempt(ctx, request)
	if err == nil {
		return committed, nil
	}
	return coordinator.recoverCommittedCommand(
		ctx, request.SessionID, request.CommandID, session.AttemptUpdateDigest(request.RunID, request.Status),
		session.EventAttemptUpdated, committed, err,
	)
}

func (coordinator *Coordinator) commitPreparedHandoff(
	ctx context.Context,
	sessionID string,
	writerEpoch uint64,
	manifest session.Manifest,
) (session.Manifest, error) {
	transition, found, err := solePreparedTransition(manifest)
	if err != nil {
		return session.Manifest{}, err
	}
	if !found {
		return session.Manifest{}, errors.New("session coordinator: handoff-pending attempt has no prepared transition")
	}
	if err := coordinator.verifyPreparedHandoff(ctx, sessionID, manifest, transition); err != nil {
		if errors.Is(err, errPreparedHandoffMismatch) {
			return manifest, fmt.Errorf("session coordinator: prepared handoff recovery blocked: %w", err)
		}
		return session.Manifest{}, err
	}
	commandID := deterministicHandoffID(sessionID, "commit", transition.TransitionID)
	committed, err := coordinator.sessions.CommitTransition(ctx, session.CommitTransitionRequest{
		SessionID: sessionID, CommandID: commandID, WriterEpoch: writerEpoch,
		ExpectedSequence: manifest.Session.Sequence, TransitionID: transition.TransitionID,
	})
	if err != nil {
		committed, err = coordinator.recoverCommittedCommand(
			ctx, sessionID, commandID, session.TransitionCommitDigest(transition.TransitionID),
			session.EventTransitionCommitted, committed, err,
		)
	}
	return committed, err
}

func (coordinator *Coordinator) verifyPreparedHandoff(
	ctx context.Context,
	sessionID string,
	manifest session.Manifest,
	transition session.TransitionRecord,
) error {
	if manifest.Session.ActiveRunID != transition.SourceOccurrence.RunID ||
		manifest.Session.ActiveSegmentID != transition.SourceSegmentID {
		return preparedHandoffMismatch("transition source is not active")
	}
	sourceAttempt := manifest.Attempts[transition.SourceOccurrence.RunID]
	if sourceAttempt.Status != session.AttemptStatusHandoffPending || sourceAttempt.ExecutionMutationHash == "" {
		return preparedHandoffMismatch("transition source has no authoritative handoff checkpoint")
	}
	mutation, _, archive, err := coordinator.loadAuthoritativeProjection(ctx, sessionID, sourceAttempt)
	if err != nil {
		return err
	}
	if mutation.RunStatus != engine.RunStatusHandoffPending {
		return preparedHandoffMismatch("transition source mutation is not handoff-pending")
	}
	if err := coordinator.projections.RestoreRunProjection(ctx, sourceAttempt.RunID, archive); err != nil {
		return fmt.Errorf("restore handoff source projection: %w", err)
	}
	state, err := coordinator.runs.LoadState(ctx, sourceAttempt.RunID)
	if err != nil {
		return fmt.Errorf("load handoff source state: %w", err)
	}
	sourcePlan, err := coordinator.runs.LoadPlan(ctx, sourceAttempt.RunID)
	if err != nil {
		return fmt.Errorf("load handoff source plan: %w", err)
	}
	if state.PendingHandoff == nil || state.CursorSet == nil || len(state.CursorSet.Cursors) != 1 {
		return preparedHandoffMismatch("handoff source selector is unavailable")
	}
	request := state.PendingHandoff
	if err := validateHandoffRequestAgainstState(sourcePlan, state, *request); err != nil {
		return preparedHandoffMismatch(err.Error())
	}
	cursor := state.CursorSet.Cursors[0]
	if cursor.QualifiedNodeID != request.QualifiedNodeID || cursor.StepID != request.StepID ||
		cursor.Phase != engine.ExecutionPhaseBefore || cursor.Invocation != request.Invocation ||
		cursor.RetryAttempt != request.RetryAttempt || cursor.FrameID != request.FrameID ||
		session.DigestJSON(cursor.CallPath) != session.DigestJSON(request.CallPath) {
		return preparedHandoffMismatch("handoff source cursor does not match its pending request")
	}
	expectedPath, err := staticHandoffPath(
		handoffDeclaringRunbookPath(state.RunbookPath, request.CallPath), request.TargetRunbook,
	)
	if err != nil {
		return preparedHandoffMismatch(err.Error())
	}
	targetPlanData, err := coordinator.sessions.ReadBlob(ctx, sessionID, transition.TargetExecutableSnapshotHash)
	if err != nil {
		return err
	}
	targetPlan, err := restorePlanBlob(targetPlanData)
	if err != nil {
		return preparedHandoffMismatch("immutable target plan cannot be decoded")
	}
	if targetPlan.RunID != transition.TargetRunID || filepath.Clean(targetPlan.RunbookPath) != expectedPath ||
		targetPlan.Metadata.RunbookID != transition.TargetRunbookID ||
		targetPlan.Metadata.RunbookName != transition.TargetRunbookName ||
		targetPlan.Metadata.PlanHash != transition.TargetPlanHash {
		return preparedHandoffMismatch("transition does not match its immutable target plan")
	}
	graphData, err := coordinator.sessions.ReadBlob(ctx, sessionID, transition.TargetGraphHash)
	if err != nil {
		return err
	}
	if err := validateBoundHandoffGraph(targetPlan, transition.TargetExecutableSnapshotHash, graphData); err != nil {
		return preparedHandoffMismatch("immutable target GraphJSON does not match its plan")
	}
	contextData, err := coordinator.sessions.ReadBlob(ctx, sessionID, transition.ContextDigest)
	if err != nil {
		return err
	}
	contextBlob := session.NewJSONBlob(contextData)
	var handoffContext session.HandoffContext
	if err := decodeStrictDocument(contextData, &handoffContext); err != nil {
		return preparedHandoffMismatch("immutable handoff context cannot be decoded")
	}
	if session.DigestJSON(handoffContext.Inputs) != session.DigestJSON(request.Context) ||
		session.DigestJSON(handoffContext.Facts) != session.DigestJSON(request.Facts) {
		return preparedHandoffMismatch("context does not match its pending request")
	}
	targetVars, err := validateHandoffInputs(targetPlan, request.Context)
	if err != nil {
		return preparedHandoffMismatch(err.Error())
	}
	sourceScope, err := immutableHandoffScope(sourcePlan, state, *request)
	if err != nil {
		return preparedHandoffMismatch(err.Error())
	}
	if err := validateHandoffTargetArtifacts(
		sourceScope.Protection, targetPlan, targetVars, targetPlanData, graphData,
	); err != nil {
		return preparedHandoffMismatch(err.Error())
	}
	contextBindings, err := normalizedHandoffBindings(request.ContextBindings, request.Context)
	if err != nil {
		return preparedHandoffMismatch(err.Error())
	}
	factBindings, err := normalizedHandoffBindings(request.FactBindings, request.Facts)
	if err != nil {
		return preparedHandoffMismatch(err.Error())
	}
	expectedFactRefs := handoffFactRefs(factBindings)
	expectedOccurrence, err := sourceOccurrenceFromHandoff(sourceAttempt.RunID, state, *request)
	if err != nil {
		return preparedHandoffMismatch(err.Error())
	}
	expectedKey := session.DigestJSON(struct {
		Source        session.SourceOccurrence `json:"source"`
		TargetRunbook string                   `json:"target_runbook"`
		ReasonCode    string                   `json:"reason_code"`
		ContextDigest string                   `json:"context_digest"`
	}{expectedOccurrence, request.TargetRunbook, request.ReasonCode, contextBlob.Digest})
	expectedTransitionID := deterministicHandoffID(sessionID, "transition", expectedKey)
	expectedSegmentID := deterministicHandoffID(sessionID, "segment", expectedKey)
	expectedRunID := deterministicHandoffID(sessionID, "run", expectedKey)
	expectedTransition := session.TransitionRecord{
		TransitionID: expectedTransitionID, Status: session.TransitionStatusPrepared,
		SourceSegmentID: transition.SourceSegmentID, SourceOccurrence: expectedOccurrence,
		TargetSegmentID: expectedSegmentID, TargetRunID: expectedRunID,
		TargetRunbookID: targetPlan.Metadata.RunbookID, TargetRunbookName: targetPlan.Metadata.RunbookName,
		TargetPlanHash: targetPlan.Metadata.PlanHash, TargetGraphHash: transition.TargetGraphHash,
		TargetExecutableSnapshotHash: transition.TargetExecutableSnapshotHash,
		ReasonCode:                   request.ReasonCode, ReasonSummary: request.ReasonSummary,
		ContextBindings: contextBindings, FactRefs: expectedFactRefs,
		ContextDigest: contextBlob.Digest, IdempotencyKey: expectedKey,
	}
	sourceSegment := manifest.Segments[transition.SourceSegmentID]
	expectedSegment := session.SegmentRecord{
		SegmentID: expectedSegmentID, Ordinal: sourceSegment.Ordinal + 1,
		RunbookID: targetPlan.Metadata.RunbookID, RunbookName: targetPlan.Metadata.RunbookName,
		EntrySelector: session.EntrySelector{Step: "$entry"}, Status: session.SegmentStatusPrepared,
		PlanHash: targetPlan.Metadata.PlanHash, GraphHash: transition.TargetGraphHash,
		ExecutableSnapshotHash: transition.TargetExecutableSnapshotHash,
		CatalogDigest:          targetPlan.Metadata.CatalogDigest,
		PackageLockDigest:      session.DigestJSON(targetPlan.Metadata.PackageDigests),
		ProfileDigest:          session.DigestJSON(targetPlan.Metadata.Profile), AttemptRunIDs: []string{expectedRunID},
		ExecutableRevision: 1, GraphRevision: 1,
	}
	expectedAttempt := session.RunAttemptRecord{
		RunID: expectedRunID, SegmentID: expectedSegmentID, Ordinal: 1,
		Mode: sourceAttempt.Mode, Status: session.AttemptStatusStarting,
		Actor: sourceAttempt.Actor, Client: sourceAttempt.Client, PlanHash: targetPlan.Metadata.PlanHash,
	}
	expectedPrepareDigest := session.TransitionPrepareDigest(expectedTransition, expectedSegment, expectedAttempt)
	prepareAccepted := false
	for _, receipt := range manifest.AcceptedCommands {
		if receipt.EventKind == session.EventTransitionPrepared && receipt.CommandDigest == expectedPrepareDigest {
			prepareAccepted = true
			break
		}
	}
	if !prepareAccepted ||
		session.DigestJSON(transition.SourceOccurrence) != session.DigestJSON(expectedOccurrence) ||
		session.TransitionPrepareDigest(transition, expectedSegment, expectedAttempt) !=
			session.TransitionPrepareDigest(expectedTransition, expectedSegment, expectedAttempt) {
		return preparedHandoffMismatch("transition does not match its source checkpoint")
	}
	return nil
}

func solePreparedTransition(manifest session.Manifest) (session.TransitionRecord, bool, error) {
	var result session.TransitionRecord
	found := false
	for _, transition := range manifest.Transitions {
		if transition.Status != session.TransitionStatusPrepared {
			continue
		}
		if found {
			return session.TransitionRecord{}, false, errors.New("session coordinator: multiple prepared transitions")
		}
		result = transition
		found = true
	}
	return result, found, nil
}

func transitionFromSourceExists(manifest session.Manifest, sourceSegmentID string) bool {
	for _, transition := range manifest.Transitions {
		if transition.SourceSegmentID == sourceSegmentID {
			return true
		}
	}
	return false
}

func transitionForTargetRun(
	manifest session.Manifest,
	runID string,
) (session.TransitionRecord, bool, error) {
	var result session.TransitionRecord
	found := false
	for _, transition := range manifest.Transitions {
		if transition.Status != session.TransitionStatusCommitted || transition.TargetRunID != runID {
			continue
		}
		if found {
			return session.TransitionRecord{}, false, errors.New("session coordinator: duplicate transition target run")
		}
		result = transition
		found = true
	}
	return result, found, nil
}

func (coordinator *Coordinator) recoverCommittedCommand(
	ctx context.Context,
	sessionID string,
	commandID string,
	commandDigest string,
	kind session.EventKind,
	partial session.Manifest,
	commitErr error,
) (session.Manifest, error) {
	if !errors.Is(commitErr, session.ErrProjection) {
		return session.Manifest{}, commitErr
	}
	rebuilt, loadErr := coordinator.sessions.LoadManifest(context.WithoutCancel(ctx), sessionID)
	if loadErr != nil && rebuilt.Session.SessionID == "" {
		if partial.Session.SessionID != "" {
			rebuilt = partial
		} else {
			return session.Manifest{}, errors.Join(commitErr, loadErr)
		}
	}
	receipt, found := rebuilt.AcceptedCommands[commandID]
	if !found || receipt.CommandDigest != commandDigest || receipt.EventKind != kind {
		return session.Manifest{}, commitErr
	}
	return rebuilt, nil
}

func (coordinator *Coordinator) startOrResumeRun(
	ctx context.Context,
	sessionID string,
	sessionWriterEpoch uint64,
	manifest session.Manifest,
	attempt session.RunAttemptRecord,
	plan *engine.ExecutionPlan,
	options engine.RunOptions,
) (engine.RunHandle, *sessionRunStore, error) {
	var authoritative *session.ExecutionMutation
	var authoritativeArchive json.RawMessage
	if attempt.ExecutionMutationHash != "" {
		mutation, _, archive, err := coordinator.loadAuthoritativeProjection(ctx, sessionID, attempt)
		if err != nil {
			return nil, nil, err
		}
		authoritative = &mutation
		authoritativeArchive = archive
		if err := coordinator.projections.RestoreRunProjection(ctx, plan.RunID, archive); err != nil {
			return nil, nil, fmt.Errorf("session coordinator: restore authoritative run projection: %w", err)
		}
	} else if attempt.Status == session.AttemptStatusStarting {
		if err := coordinator.projections.DeleteRun(ctx, plan.RunID); err != nil {
			return nil, nil, fmt.Errorf("session coordinator: discard uncommitted run projection: %w", err)
		}
	}
	state, stateErr := coordinator.runs.LoadState(ctx, plan.RunID)
	restoredPlan, planErr := coordinator.runs.LoadPlan(ctx, plan.RunID)
	if stateErr == nil && planErr == nil && authoritative != nil {
		completeArchive, err := completeAuthoritativeTraceArchive(authoritativeArchive, state, attempt)
		if err != nil {
			return nil, nil, err
		}
		if !bytes.Equal(completeArchive, authoritativeArchive) {
			if err := coordinator.projections.RestoreRunProjection(ctx, plan.RunID, completeArchive); err != nil {
				return nil, nil, fmt.Errorf("session coordinator: restore complete trace projection: %w", err)
			}
			state, stateErr = coordinator.runs.LoadState(ctx, plan.RunID)
			restoredPlan, planErr = coordinator.runs.LoadPlan(ctx, plan.RunID)
			if stateErr != nil || planErr != nil {
				return nil, nil, fmt.Errorf(
					"session coordinator: reload completed projection: state=%v plan=%v", stateErr, planErr,
				)
			}
		}
		if err := coordinator.validateRestoredExecutionMutation(*authoritative, state, plan, restoredPlan); err != nil {
			return nil, nil, err
		}
	}
	if stateErr == nil && planErr != nil && authoritative == nil {
		if err := coordinator.runs.SavePlan(ctx, plan.RunID, plan); err != nil {
			return nil, nil, fmt.Errorf("session coordinator: repair run plan projection: %w", err)
		}
		planErr = nil
	}
	if stateErr != nil && attempt.ExecutionMutationHash == "" && attempt.RunProjectionHash != "" {
		archive, err := coordinator.sessions.ReadBlob(ctx, sessionID, attempt.RunProjectionHash)
		if err != nil {
			return nil, nil, fmt.Errorf("session coordinator: load run projection %s: %w", attempt.RunProjectionHash, err)
		}
		if err := coordinator.projections.RestoreRunProjection(ctx, plan.RunID, archive); err != nil {
			return nil, nil, fmt.Errorf("session coordinator: restore run projection: %w", err)
		}
		if _, err := coordinator.runs.LoadState(ctx, plan.RunID); err != nil {
			return nil, nil, fmt.Errorf("session coordinator: restored projection has no state: %w", err)
		}
		if _, err := coordinator.runs.LoadPlan(ctx, plan.RunID); err != nil {
			return nil, nil, fmt.Errorf("session coordinator: restored projection has no plan: %w", err)
		}
		stateErr, planErr = nil, nil
	}
	sessionRuns := newSessionRunStore(coordinator, sessionID, plan.RunID, sessionWriterEpoch, manifest)
	options.Store = sessionRuns
	if stateErr == nil && planErr == nil {
		runHandle, err := coordinator.runtime.Resume(ctx, plan.RunID, options)
		return runHandle, sessionRuns, err
	}
	if attempt.Status != session.AttemptStatusStarting {
		return nil, nil, fmt.Errorf("session coordinator: journaled attempt projection is unavailable: state=%v plan=%v", stateErr, planErr)
	}
	runHandle, err := coordinator.runtime.Start(ctx, plan, options)
	return runHandle, sessionRuns, err
}

func (coordinator *Coordinator) loadExecutionMutation(
	ctx context.Context,
	sessionID string,
	attempt session.RunAttemptRecord,
) (session.ExecutionMutation, error) {
	encoded, err := coordinator.sessions.ReadBlob(ctx, sessionID, attempt.ExecutionMutationHash)
	if err != nil {
		return session.ExecutionMutation{}, fmt.Errorf(
			"session coordinator: load execution mutation %s: %w", attempt.ExecutionMutationHash, err,
		)
	}
	var mutation session.ExecutionMutation
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(&mutation); err != nil {
		return session.ExecutionMutation{}, fmt.Errorf("session coordinator: decode execution mutation: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return session.ExecutionMutation{}, errors.New("session coordinator: execution mutation has trailing JSON")
	}
	expectedStatus := attemptStatusFromRun(mutation.RunStatus)
	if mutation.RunStatus == engine.RunStatusPending {
		expectedStatus = session.AttemptStatusRunning
	}
	if mutation.SchemaVersion != session.ExecutionMutationSchemaV1 || mutation.RunID != attempt.RunID ||
		mutation.StateProjectionHash == "" || mutation.StateProjectionHash != attempt.RunProjectionHash ||
		mutation.CheckpointSequence != attempt.CheckpointSequence ||
		mutation.CommittedTraceSequence != attempt.CommittedTraceSequence || mutation.RunWriterEpoch == 0 ||
		expectedStatus == "" || mutation.AttemptStatus != expectedStatus || mutation.TraceSequenceStart < 0 ||
		mutation.TraceSequenceEnd < mutation.TraceSequenceStart ||
		mutation.TraceSequenceEnd > mutation.CommittedTraceSequence {
		return session.ExecutionMutation{}, errors.New("session coordinator: execution mutation does not match attempt")
	}
	return mutation, nil
}

func completeAuthoritativeTraceArchive(
	archive json.RawMessage,
	state engine.RunState,
	attempt session.RunAttemptRecord,
) (json.RawMessage, error) {
	complete, err := appendTraceEventsToRunProjection(archive, state.PendingTraceEvents)
	if err != nil {
		return nil, fmt.Errorf("session coordinator: complete authoritative trace projection: %w", err)
	}
	lastTraceSequence := attempt.CommittedTraceSequence
	if attempt.JournaledTraceSequence > lastTraceSequence {
		lastTraceSequence = attempt.JournaledTraceSequence
	}
	if err := validateRunProjectionTrace(complete, attempt.RunID, lastTraceSequence); err != nil {
		return nil, err
	}
	return complete, nil
}

func (coordinator *Coordinator) loadAuthoritativeProjection(
	ctx context.Context,
	sessionID string,
	attempt session.RunAttemptRecord,
) (session.ExecutionMutation, json.RawMessage, json.RawMessage, error) {
	mutation, err := coordinator.loadExecutionMutation(ctx, sessionID, attempt)
	if err != nil {
		return session.ExecutionMutation{}, nil, nil, err
	}
	projection, err := coordinator.sessions.ReadBlob(ctx, sessionID, mutation.StateProjectionHash)
	if err != nil {
		return session.ExecutionMutation{}, nil, nil, fmt.Errorf(
			"session coordinator: load authoritative run projection %s: %w", mutation.StateProjectionHash, err,
		)
	}
	archive, err := decodeSessionRunProjection(ctx, projection, func(digest string) (json.RawMessage, error) {
		return coordinator.sessions.ReadBlob(ctx, sessionID, digest)
	})
	if err != nil {
		return session.ExecutionMutation{}, nil, nil, err
	}
	traceSuffix, err := coordinator.loadJournaledTraceSuffix(
		ctx, sessionID, attempt, mutation.CommittedTraceSequence,
	)
	if err != nil {
		return session.ExecutionMutation{}, nil, nil, err
	}
	archive, err = appendTraceEventsToRunProjection(archive, traceSuffix)
	if err != nil {
		return session.ExecutionMutation{}, nil, nil, err
	}
	manifest, err := coordinator.sessions.LoadManifest(ctx, sessionID)
	if err != nil {
		return session.ExecutionMutation{}, nil, nil, err
	}
	segment, found := manifest.Segments[attempt.SegmentID]
	if !found {
		return session.ExecutionMutation{}, nil, nil, errors.New("session coordinator: execution segment is unavailable")
	}
	if segment.ExecutableRevision > 1 || segment.GraphRevision > 1 {
		planData, err := coordinator.sessions.ReadBlob(ctx, sessionID, segment.ExecutableSnapshotHash)
		if err != nil {
			return session.ExecutionMutation{}, nil, nil, err
		}
		plan, err := restorePlanBlob(planData)
		if err != nil {
			return session.ExecutionMutation{}, nil, nil, err
		}
		plan.RunID = attempt.RunID
		resolutions, dispatches, err := coordinator.loadJournaledSegmentResolutions(ctx, sessionID, segment, attempt.RunID)
		if err != nil {
			return session.ExecutionMutation{}, nil, nil, err
		}
		archive, err = alignRunProjectionWithPlan(archive, plan, resolutions, dispatches)
		if err != nil {
			return session.ExecutionMutation{}, nil, nil, err
		}
	}
	if _, _, err := sessionProjectionSnapshot(projection, attempt.RunID, mutation.CheckpointSequence); err != nil {
		return session.ExecutionMutation{}, nil, nil, err
	}
	return mutation, projection, archive, nil
}

func (coordinator *Coordinator) loadJournaledTraceSuffix(
	ctx context.Context,
	sessionID string,
	attempt session.RunAttemptRecord,
	afterSequence int64,
) ([]engine.Event, error) {
	if attempt.JournaledTraceSequence <= afterSequence {
		return nil, nil
	}
	events, err := coordinator.sessions.ReadEvents(ctx, sessionID, 0)
	if err != nil {
		return nil, err
	}
	nextSequence := afterSequence + 1
	result := make([]engine.Event, 0, attempt.JournaledTraceSequence-afterSequence)
	for _, journalEvent := range events {
		if journalEvent.Kind != session.EventTraceCommitted {
			continue
		}
		var payload sessionTraceCommitPayload
		if err := decodeStrictDocument(journalEvent.Payload, &payload); err != nil {
			return nil, fmt.Errorf("session coordinator: decode trace commit: %w", err)
		}
		if payload.RunID != attempt.RunID || payload.TraceSequence <= afterSequence {
			continue
		}
		if payload.TraceSequence != nextSequence || payload.TraceSequence > attempt.JournaledTraceSequence {
			return nil, errors.New("session coordinator: journaled trace suffix is not contiguous")
		}
		encodedTrace, err := coordinator.sessions.ReadBlob(ctx, sessionID, payload.TraceHash)
		if err != nil {
			return nil, fmt.Errorf("session coordinator: load trace event %s: %w", payload.TraceHash, err)
		}
		var traceEvent engine.Event
		if err := decodeStrictDocument(encodedTrace, &traceEvent); err != nil ||
			traceEvent.RunID != attempt.RunID || traceEvent.Sequence != payload.TraceSequence ||
			traceEvent.EventID == "" || traceEvent.Kind == "" || session.ParseTime(traceEvent.Timestamp) != nil {
			return nil, errors.New("session coordinator: invalid journaled trace event")
		}
		result = append(result, traceEvent)
		nextSequence++
	}
	if nextSequence-1 != attempt.JournaledTraceSequence {
		return nil, errors.New("session coordinator: journaled trace suffix is incomplete")
	}
	return result, nil
}

func (coordinator *Coordinator) validateRestoredExecutionMutation(
	mutation session.ExecutionMutation,
	state engine.RunState,
	expectedPlan *engine.ExecutionPlan,
	restoredPlan *engine.ExecutionPlan,
) error {
	if state.RunID != mutation.RunID || state.WriterEpoch != mutation.RunWriterEpoch ||
		state.CheckpointSequence != mutation.CheckpointSequence ||
		state.CommittedTraceSequence != mutation.CommittedTraceSequence || state.Status != mutation.RunStatus {
		return errors.New("session coordinator: restored state does not match execution mutation")
	}
	if len(state.PendingTraceEvents) == 0 {
		if mutation.TraceSequenceStart != 0 || mutation.TraceSequenceEnd != 0 {
			return errors.New("session coordinator: restored trace batch does not match execution mutation")
		}
	} else if mutation.TraceSequenceStart != state.PendingTraceEvents[0].Sequence ||
		mutation.TraceSequenceEnd != state.PendingTraceEvents[len(state.PendingTraceEvents)-1].Sequence {
		return errors.New("session coordinator: restored trace batch does not match execution mutation")
	}
	expectedSnapshot, err := snapshotBlob(expectedPlan)
	if err != nil {
		return err
	}
	restoredSnapshot, err := snapshotBlob(restoredPlan)
	if err != nil {
		return err
	}
	if expectedSnapshot.Digest != restoredSnapshot.Digest {
		return errors.New("session coordinator: restored plan does not match immutable session plan")
	}
	return nil
}

func (coordinator *Coordinator) Inspect(ctx context.Context, sessionID string) (Inspection, error) {
	manifest, err := coordinator.sessions.LoadManifest(ctx, sessionID)
	if err != nil {
		return Inspection{}, err
	}
	runID := manifest.Session.ActiveRunID
	if runID == "" {
		runID = latestAttemptID(manifest)
	}
	inspection := Inspection{Manifest: manifest}
	if runID == "" {
		return inspection, nil
	}
	attempt := manifest.Attempts[runID]
	if attempt.ExecutionMutationHash != "" {
		mutation, projection, archive, err := coordinator.loadAuthoritativeProjection(ctx, sessionID, attempt)
		if err != nil {
			return Inspection{}, err
		}
		snapshotPath, snapshotDigest, err := sessionProjectionSnapshot(
			projection, runID, mutation.CheckpointSequence,
		)
		if err != nil {
			return Inspection{}, err
		}
		segment := manifest.Segments[attempt.SegmentID]
		encodedPlan, err := coordinator.sessions.ReadBlob(ctx, sessionID, segment.ExecutableSnapshotHash)
		if err != nil {
			return Inspection{}, err
		}
		expectedPlan, err := restorePlanBlob(encodedPlan)
		if err != nil {
			return Inspection{}, err
		}
		expectedPlan.RunID = runID
		state, stateErr := coordinator.runs.LoadState(ctx, runID)
		restoredPlan, planErr := coordinator.runs.LoadPlan(ctx, runID)
		completeArchive := archive
		if stateErr == nil && planErr == nil {
			completeArchive, err = completeAuthoritativeTraceArchive(archive, state, attempt)
			if err != nil {
				stateErr = err
			} else if err := coordinator.validateRestoredExecutionMutation(
				mutation, state, expectedPlan, restoredPlan,
			); err != nil {
				stateErr = err
			}
		}
		localSnapshot, snapshotErr := os.ReadFile(filepath.Join(coordinator.projections.RunDir(runID), filepath.FromSlash(snapshotPath)))
		projectionCurrent := stateErr == nil && planErr == nil && snapshotErr == nil &&
			session.DigestBytes(localSnapshot) == snapshotDigest &&
			state.CheckpointSequence == mutation.CheckpointSequence &&
			state.CommittedTraceSequence == mutation.CommittedTraceSequence &&
			runProjectionMatchesArchive(completeArchive, coordinator.projections.RunDir(runID))
		if !projectionCurrent {
			if err := coordinator.projections.RestoreRunProjection(ctx, runID, archive); err != nil {
				return Inspection{}, fmt.Errorf("session coordinator: restore run projection for inspection: %w", err)
			}
			state, stateErr = coordinator.runs.LoadState(ctx, runID)
			restoredPlan, planErr = coordinator.runs.LoadPlan(ctx, runID)
			if stateErr == nil && planErr == nil {
				completeArchive, err = completeAuthoritativeTraceArchive(archive, state, attempt)
				if err == nil && !bytes.Equal(completeArchive, archive) {
					err = coordinator.projections.RestoreRunProjection(ctx, runID, completeArchive)
					if err == nil {
						state, stateErr = coordinator.runs.LoadState(ctx, runID)
						restoredPlan, planErr = coordinator.runs.LoadPlan(ctx, runID)
					}
				}
			}
		}
		if stateErr != nil || planErr != nil || err != nil {
			return Inspection{}, errors.Join(stateErr, planErr, err)
		}
		if err := coordinator.validateRestoredExecutionMutation(mutation, state, expectedPlan, restoredPlan); err != nil {
			return Inspection{}, err
		}
		inspection.Run = &state
		return inspection, nil
	}
	return inspection, nil
}

func latestAttemptID(manifest session.Manifest) string {
	selectedRunID := ""
	selectedSegmentOrdinal, selectedAttemptOrdinal := -1, -1
	for runID, attempt := range manifest.Attempts {
		segmentOrdinal := manifest.Segments[attempt.SegmentID].Ordinal
		if segmentOrdinal > selectedSegmentOrdinal ||
			segmentOrdinal == selectedSegmentOrdinal && attempt.Ordinal > selectedAttemptOrdinal ||
			segmentOrdinal == selectedSegmentOrdinal && attempt.Ordinal == selectedAttemptOrdinal && runID > selectedRunID {
			selectedRunID = runID
			selectedSegmentOrdinal = segmentOrdinal
			selectedAttemptOrdinal = attempt.Ordinal
		}
	}
	return selectedRunID
}

func clonePlanForRun(plan *engine.ExecutionPlan, runID string) (*engine.ExecutionPlan, error) {
	snapshot, err := plansnapshot.FromExecutionPlan(plan)
	if err != nil {
		return nil, fmt.Errorf("session coordinator: snapshot plan: %w", err)
	}
	cloned, err := plansnapshot.Restore(snapshot)
	if err != nil {
		return nil, fmt.Errorf("session coordinator: restore plan clone: %w", err)
	}
	cloned.RunID = runID
	return cloned, nil
}

func snapshotBlob(plan *engine.ExecutionPlan) (session.JSONBlob, error) {
	snapshot, err := plansnapshot.FromExecutionPlan(plan)
	if err != nil {
		return session.JSONBlob{}, fmt.Errorf("session coordinator: snapshot executable plan: %w", err)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return session.JSONBlob{}, err
	}
	return session.NewJSONBlob(encoded), nil
}

func restorePlanBlob(encoded json.RawMessage) (*engine.ExecutionPlan, error) {
	var snapshot plansnapshot.SnapshotV1
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(&snapshot); err != nil {
		return nil, fmt.Errorf("session coordinator: decode plan snapshot: %w", err)
	}
	return plansnapshot.Restore(snapshot)
}

func deterministicCommandID(sessionID string, operation string, runID string, sequence int64) string {
	namespace := uuid.MustParse(sessionID)
	return uuid.NewSHA1(namespace, []byte(fmt.Sprintf("%s:%s:%d", operation, runID, sequence))).String()
}

func deterministicHandoffID(sessionID string, operation string, identity string) string {
	return uuid.NewSHA1(uuid.MustParse(sessionID), []byte("handoff:"+operation+":"+identity)).String()
}

func staticHandoffPath(sourcePath string, target string) (string, error) {
	if strings.TrimSpace(sourcePath) == "" || !schema.IsStaticHandoffTarget(target) {
		return "", errors.New("session coordinator: handoff target must be a static relative .runbook.yaml path")
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(target)))
	return filepath.Clean(filepath.Join(filepath.Dir(sourcePath), filepath.FromSlash(clean))), nil
}

func handoffDeclaringRunbookPath(rootPath string, callPath []engine.DebugCallFrame) string {
	for index := len(callPath) - 1; index >= 0; index-- {
		if strings.TrimSpace(callPath[index].RunbookPath) != "" {
			return filepath.Clean(callPath[index].RunbookPath)
		}
	}
	return filepath.Clean(rootPath)
}

func normalizedHandoffBindings(bindings map[string]string, values map[string]any) (map[string]string, error) {
	if len(values) == 0 {
		if len(bindings) != 0 {
			return nil, errors.New("session coordinator: handoff allowlist has no evaluated values")
		}
		return nil, nil
	}
	if len(bindings) == 0 {
		return nil, errors.New("session coordinator: handoff values have no explicit provenance")
	}
	if len(bindings) != len(values) {
		return nil, errors.New("session coordinator: handoff values do not match their allowlist")
	}
	result := make(map[string]string, len(bindings))
	for name, source := range bindings {
		if strings.TrimSpace(name) == "" || strings.TrimSpace(source) == "" {
			return nil, errors.New("session coordinator: handoff allowlist contains an empty binding")
		}
		if _, found := values[name]; !found {
			return nil, errors.New("session coordinator: handoff allowlist value is missing")
		}
		if err := validateHandoffSourcePath(source); err != nil {
			return nil, err
		}
		result[name] = source
	}
	return result, nil
}

func validateHandoffSourcePath(source string) error {
	segments := strings.Split(source, ".")
	if len(segments) == 0 || segments[0] == "vars" {
		return errors.New("session coordinator: handoff source path escapes the explicit variable scope")
	}
	for _, segment := range segments {
		if strings.TrimSpace(segment) == "" || sensitive.Name(segment) {
			return errors.New("session coordinator: handoff source path is sensitive")
		}
	}
	return nil
}

func resolveHandoffSourceValue(vars map[string]any, source string) (any, error) {
	if err := validateHandoffSourcePath(source); err != nil {
		return nil, err
	}
	segments := strings.Split(source, ".")
	value, found := vars[segments[0]]
	if !found {
		return nil, errors.New("session coordinator: handoff source path is unavailable")
	}
	for _, segment := range segments[1:] {
		nested, ok := value.(map[string]any)
		if !ok {
			return nil, errors.New("session coordinator: handoff source path is unavailable")
		}
		value, found = nested[segment]
		if !found {
			return nil, errors.New("session coordinator: handoff source path is unavailable")
		}
	}
	return value, nil
}

func validateHandoffRequestAgainstState(
	plan *engine.ExecutionPlan,
	state engine.RunState,
	request engine.HandoffRequest,
) error {
	scope, err := immutableHandoffScope(plan, state, request)
	if err != nil {
		return err
	}
	expectedContextBindings, err := handoffBindingSources(scope.Spec.Handoff.With)
	if err != nil {
		return err
	}
	expectedFactBindings, err := handoffBindingSources(scope.Spec.Handoff.Facts)
	if err != nil {
		return err
	}
	if request.TargetRunbook != scope.Spec.Handoff.Runbook || request.ReasonCode != scope.Spec.Handoff.Reason.Code ||
		request.ReasonSummary != scope.Spec.Handoff.Reason.Summary ||
		session.DigestJSON(request.ContextBindings) != session.DigestJSON(expectedContextBindings) ||
		session.DigestJSON(request.FactBindings) != session.DigestJSON(expectedFactBindings) {
		return errors.New("session coordinator: handoff request does not match its immutable definition")
	}
	contextBindings, err := normalizedHandoffBindings(request.ContextBindings, request.Context)
	if err != nil {
		return err
	}
	factBindings, err := normalizedHandoffBindings(request.FactBindings, request.Facts)
	if err != nil {
		return err
	}
	for name, source := range contextBindings {
		if handoffSourceIsProtected(scope.Protection, source) {
			return errors.New("session coordinator: handoff context references protected content")
		}
		value, resolveErr := resolveHandoffSourceValue(scope.Vars, source)
		if resolveErr != nil || session.DigestJSON(value) != session.DigestJSON(request.Context[name]) {
			return errors.New("session coordinator: handoff context does not match its source checkpoint")
		}
	}
	for name, source := range factBindings {
		if handoffSourceIsProtected(scope.Protection, source) {
			return errors.New("session coordinator: handoff facts reference protected content")
		}
		value, resolveErr := resolveHandoffSourceValue(scope.Vars, source)
		if resolveErr != nil || session.DigestJSON(value) != session.DigestJSON(request.Facts[name]) {
			return errors.New("session coordinator: handoff facts do not match their source checkpoint")
		}
	}
	if err := internaldebugprotect.ValidateHandoffRequest(scope.Protection, request); err != nil {
		return errors.New("session coordinator: handoff values contain protected content")
	}
	return nil
}

type handoffValidationScope struct {
	Spec       *schema.HandoffSpec
	Vars       map[string]any
	Protection engine.DebugProtection
}

func immutableHandoffScope(
	plan *engine.ExecutionPlan,
	state engine.RunState,
	request engine.HandoffRequest,
) (handoffValidationScope, error) {
	if plan == nil {
		return handoffValidationScope{}, errors.New("session coordinator: immutable source plan is unavailable")
	}
	scopeVars := state.Vars
	if request.FrameID != "" {
		frame := state.ExecutionFrames[request.FrameID]
		if frame == nil || frame.Status != engine.ExecutionFrameStatusActive ||
			request.FrameStepIndex < 0 || request.FrameStepIndex >= frame.StepCount ||
			request.FrameStepIndex >= len(frame.StepIDs) || frame.NextStepIndex != request.FrameStepIndex ||
			frame.StepIDs[request.FrameStepIndex] != request.StepID ||
			frame.Invocation != request.Invocation ||
			session.DigestJSON(frame.CallPath) != session.DigestJSON(request.CallPath) {
			return handoffValidationScope{}, errors.New("session coordinator: handoff request does not match its active frame")
		}
		scopeVars = frame.WorkingVars
	} else if request.FrameStepIndex != 0 {
		return handoffValidationScope{}, errors.New("session coordinator: root handoff has a frame step index")
	}
	if request.QualifiedNodeID != engine.DebugNodeID(request.CallPath, request.StepID) {
		return handoffValidationScope{}, errors.New("session coordinator: handoff qualified node does not match its call path")
	}
	declaringPath := filepath.Clean(handoffDeclaringRunbookPath(state.RunbookPath, request.CallPath))
	var matched *schema.HandoffSpec
	for index := range plan.Steps {
		step := &plan.Steps[index]
		spec, ok := step.Spec.(*schema.HandoffSpec)
		if !ok || spec == nil || step.ID != request.StepID || step.Kind != "handoff" {
			continue
		}
		origin := step.Origin
		if origin == "" {
			origin = plan.RunbookPath
		}
		callPath, exact := flattenedStepCallPath(plan, index)
		if filepath.Clean(origin) != declaringPath || !exact || !sameHandoffCallPath(callPath, request.CallPath) {
			continue
		}
		if matched != nil {
			return handoffValidationScope{}, errors.New("session coordinator: handoff definition is ambiguous")
		}
		matched = spec
	}
	if matched == nil {
		closureSpec, err := handoffSpecFromImmutableClosure(plan, state, request, declaringPath)
		if err != nil {
			return handoffValidationScope{}, err
		}
		matched = closureSpec
	}
	if matched == nil {
		return handoffValidationScope{}, errors.New("session coordinator: handoff definition is not in the immutable source plan")
	}
	inputs, outputs, governance, rootScope, err := handoffDeclaringScopeMetadata(
		plan, state, request, declaringPath,
	)
	if err != nil {
		return handoffValidationScope{}, err
	}
	policy := plan.Governance
	if !rootScope {
		policy = internalgovernance.BuildPolicy(governance)
	}
	protection := engine.ExtendDebugProtection(engine.DebugProtection{}, scopeVars, inputs, policy)
	for name, declaration := range outputs {
		if declaration == nil || declaration.Type != "secret" {
			continue
		}
		protection.ProtectedVars = append(protection.ProtectedVars, name)
		if secret, ok := scopeVars[name].(string); ok && secret != "" {
			protection.SecretValues = append(protection.SecretValues, secret)
		}
	}
	return handoffValidationScope{Spec: matched, Vars: scopeVars, Protection: protection}, nil
}

func handoffSpecFromImmutableClosure(
	plan *engine.ExecutionPlan,
	state engine.RunState,
	request engine.HandoffRequest,
	declaringPath string,
) (*schema.HandoffSpec, error) {
	includeStepID := declaringIncludeStepID(request.CallPath, declaringPath)
	var matches []*schema.HandoffSpec
	for index := range plan.Steps {
		collectHandoffSpecsFromStepSpec(
			plan.Steps[index].ID, plan.Steps[index].Spec,
			includeStepID, declaringPath, request.StepID, &matches,
		)
	}
	resolution, found, err := exactDynamicHandoffResolution(state, request, includeStepID, declaringPath)
	if err != nil {
		return nil, err
	}
	if found {
		if len(resolution.Pin.ExecutableClosure) == 0 {
			return nil, errors.New("session coordinator: dynamic handoff closure is unavailable")
		}
		nodes, err := plansnapshot.RestoreFlowClosure(resolution.Pin.ExecutableClosure)
		if err != nil {
			return nil, errors.New("session coordinator: dynamic handoff closure is invalid")
		}
		collectHandoffSpecsInDeclaringFlow(nodes, request.StepID, &matches)
	}
	if len(matches) > 1 {
		return nil, errors.New("session coordinator: handoff definition is ambiguous")
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	return nil, nil
}

func exactDynamicHandoffResolution(
	state engine.RunState,
	request engine.HandoffRequest,
	includeStepID string,
	declaringPath string,
) (*engine.DynamicIncludeResolutionState, bool, error) {
	if request.FrameID == "" || len(request.CallPath) == 0 || includeStepID == "" {
		return nil, false, nil
	}
	childFrame := state.ExecutionFrames[request.FrameID]
	if childFrame == nil || childFrame.ParentStepID != includeStepID {
		return nil, false, errors.New("session coordinator: dynamic handoff child frame is invalid")
	}
	parentCallPath := request.CallPath[:len(request.CallPath)-1]
	expectedQualifiedNodeID := engine.DebugNodeID(parentCallPath, includeStepID)
	expectedFrameID := childFrame.ParentFrameID
	expectedFrameStepIndex := 0
	if expectedFrameID != "" {
		parentFrame := state.ExecutionFrames[expectedFrameID]
		if parentFrame == nil || parentFrame.Status != engine.ExecutionFrameStatusActive ||
			parentFrame.NextStepIndex < 0 || parentFrame.NextStepIndex >= parentFrame.StepCount ||
			parentFrame.NextStepIndex >= len(parentFrame.StepIDs) ||
			parentFrame.StepIDs[parentFrame.NextStepIndex] != includeStepID {
			return nil, false, errors.New("session coordinator: dynamic handoff parent frame is invalid")
		}
		expectedFrameStepIndex = parentFrame.NextStepIndex
	}
	var matched *engine.DynamicIncludeResolutionState
	for _, resolution := range state.DynamicIncludes {
		if resolution == nil || resolution.Status != engine.DynamicIncludeResolutionStatusActive ||
			resolution.StepID != includeStepID || resolution.Pin.StepID != includeStepID ||
			filepath.Clean(resolution.Pin.AbsPath) != declaringPath ||
			resolution.QualifiedNodeID != expectedQualifiedNodeID ||
			resolution.FrameID != expectedFrameID || resolution.FrameStepIndex != expectedFrameStepIndex ||
			resolution.Pin.QualifiedNodeID != resolution.QualifiedNodeID ||
			resolution.Pin.Invocation != resolution.Invocation ||
			!sameHandoffCallPath(resolution.CallPath, parentCallPath) {
			continue
		}
		if matched != nil {
			return nil, false, errors.New("session coordinator: dynamic handoff resolution is ambiguous")
		}
		matched = resolution
	}
	return matched, matched != nil, nil
}

func sameHandoffCallPath(left, right []engine.DebugCallFrame) bool {
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

func flattenedStepCallPath(plan *engine.ExecutionPlan, stepIndex int) ([]engine.DebugCallFrame, bool) {
	if plan == nil || stepIndex < 0 || stepIndex >= len(plan.Steps) {
		return nil, false
	}
	parentID := plan.Steps[stepIndex].ParentID
	searchBefore := stepIndex
	reversed := make([]engine.DebugCallFrame, 0, plan.Steps[stepIndex].Depth)
	for parentID != "" {
		parentIndex := -1
		for index := searchBefore - 1; index >= 0; index-- {
			if plan.Steps[index].ID == parentID {
				parentIndex = index
				break
			}
		}
		if parentIndex < 0 {
			return nil, false
		}
		parent := &plan.Steps[parentIndex]
		frame := engine.DebugCallFrame{StepID: parent.ID}
		if include, ok := parent.Spec.(*schema.IncludeSpec); ok && include != nil {
			frame.RunbookPath = include.ResolvedRunbookPath
			if frame.RunbookPath == "" {
				frame.RunbookPath = include.LazyRunbookPath
			}
		}
		reversed = append(reversed, frame)
		parentID = parent.ParentID
		searchBefore = parentIndex
	}
	result := make([]engine.DebugCallFrame, len(reversed))
	for index := range reversed {
		result[len(reversed)-1-index] = reversed[index]
	}
	return result, true
}

func handoffCallPathPrefix(prefix, path []engine.DebugCallFrame) bool {
	return len(prefix) <= len(path) && sameHandoffCallPath(prefix, path[:len(prefix)])
}

func collectHandoffSpecsFromStepSpec(
	ownerStepID string,
	spec engine.StepSpec,
	includeStepID string,
	declaringPath string,
	handoffStepID string,
	matches *[]*schema.HandoffSpec,
) {
	include, ok := spec.(*schema.IncludeSpec)
	if !ok || include == nil {
		return
	}
	if ownerStepID == includeStepID && filepath.Clean(include.ResolvedRunbookPath) == declaringPath {
		collectHandoffSpecsInDeclaringFlow(include.ResolvedSteps, handoffStepID, matches)
	}
	collectHandoffSpecsInNestedIncludes(include.ResolvedSteps, includeStepID, declaringPath, handoffStepID, matches)
}

func collectHandoffSpecsInNestedIncludes(
	nodes []schema.FlowNode,
	includeStepID string,
	declaringPath string,
	handoffStepID string,
	matches *[]*schema.HandoffSpec,
) {
	for index := range nodes {
		node := &nodes[index]
		if node.Step != nil {
			step := node.Step
			if step.IncludeSpec != nil {
				if step.ID == includeStepID && filepath.Clean(step.IncludeSpec.ResolvedRunbookPath) == declaringPath {
					collectHandoffSpecsInDeclaringFlow(step.IncludeSpec.ResolvedSteps, handoffStepID, matches)
				}
				collectHandoffSpecsInNestedIncludes(
					step.IncludeSpec.ResolvedSteps, includeStepID, declaringPath, handoffStepID, matches,
				)
			}
			for _, branch := range handoffStructuralChildFlows(step) {
				collectHandoffSpecsInNestedIncludes(branch, includeStepID, declaringPath, handoffStepID, matches)
			}
		}
		if node.Iterate != nil {
			collectHandoffSpecsInNestedIncludes(node.Iterate.Steps, includeStepID, declaringPath, handoffStepID, matches)
		}
		if node.Parallel != nil {
			for _, branch := range node.Parallel.Branches {
				collectHandoffSpecsInNestedIncludes(branch.Steps, includeStepID, declaringPath, handoffStepID, matches)
			}
		}
	}
}

func collectHandoffSpecsInDeclaringFlow(
	nodes []schema.FlowNode,
	stepID string,
	matches *[]*schema.HandoffSpec,
) {
	for index := range nodes {
		node := &nodes[index]
		if node.Step != nil {
			step := node.Step
			if step.ID == stepID && step.Type == schema.StepTypeHandoff && step.HandoffSpec != nil {
				*matches = append(*matches, step.HandoffSpec)
			}
			for _, branch := range handoffStructuralChildFlows(step) {
				collectHandoffSpecsInDeclaringFlow(branch, stepID, matches)
			}
		}
		if node.Iterate != nil {
			collectHandoffSpecsInDeclaringFlow(node.Iterate.Steps, stepID, matches)
		}
		if node.Parallel != nil {
			for _, branch := range node.Parallel.Branches {
				collectHandoffSpecsInDeclaringFlow(branch.Steps, stepID, matches)
			}
		}
	}
}

func handoffStructuralChildFlows(step *schema.Step) [][]schema.FlowNode {
	var result [][]schema.FlowNode
	if step.BranchSpec != nil {
		for _, branch := range step.BranchSpec.Branches {
			result = append(result, branch.Steps)
		}
	}
	if step.CompensateSpec != nil {
		result = append(result, step.CompensateSpec.Compensate.Steps)
	}
	return result
}

func declaringIncludeStepID(callPath []engine.DebugCallFrame, declaringPath string) string {
	for index := len(callPath) - 1; index >= 0; index-- {
		if filepath.Clean(callPath[index].RunbookPath) == declaringPath {
			return callPath[index].StepID
		}
	}
	return ""
}

func handoffDeclaringScopeMetadata(
	plan *engine.ExecutionPlan,
	state engine.RunState,
	request engine.HandoffRequest,
	declaringPath string,
) (map[string]*schema.Input, map[string]*schema.Output, *schema.GovernanceConfig, bool, error) {
	if filepath.Clean(plan.RunbookPath) == declaringPath {
		return plan.Inputs, plan.Outputs, plan.GovernanceSource, true, nil
	}
	includeStepID := declaringIncludeStepID(request.CallPath, declaringPath)
	var flattenedMatch *schema.IncludeSpec
	for index := range plan.Steps {
		step := &plan.Steps[index]
		include, ok := step.Spec.(*schema.IncludeSpec)
		if !ok || include == nil || step.ID != includeStepID ||
			filepath.Clean(include.ResolvedRunbookPath) != declaringPath {
			continue
		}
		callPath, exact := flattenedStepCallPath(plan, index)
		if !exact {
			continue
		}
		callPath = append(callPath, engine.DebugCallFrame{StepID: step.ID, RunbookPath: include.ResolvedRunbookPath})
		if !handoffCallPathPrefix(callPath, request.CallPath) {
			continue
		}
		if flattenedMatch != nil {
			return nil, nil, nil, false, errors.New("session coordinator: declaring handoff scope is ambiguous")
		}
		flattenedMatch = include
	}
	if flattenedMatch != nil {
		return flattenedMatch.ResolvedInputs, flattenedMatch.ResolvedOutputs,
			flattenedMatch.ResolvedGovernance, false, nil
	}
	var includeMatches []*schema.IncludeSpec
	for index := range plan.Steps {
		collectDeclaringIncludeSpecs(plan.Steps[index].Spec, includeStepID, declaringPath, &includeMatches)
	}
	if len(includeMatches) > 1 {
		return nil, nil, nil, false, errors.New("session coordinator: declaring handoff scope is ambiguous")
	}
	if len(includeMatches) == 1 {
		include := includeMatches[0]
		return include.ResolvedInputs, include.ResolvedOutputs, include.ResolvedGovernance, false, nil
	}
	resolution, found, err := exactDynamicHandoffResolution(state, request, includeStepID, declaringPath)
	if err != nil {
		return nil, nil, nil, false, err
	}
	if found {
		return resolution.Pin.ResolvedInputs, resolution.Pin.ResolvedOutputs,
			resolution.Pin.ResolvedGovernance, false, nil
	}
	return nil, nil, nil, false, errors.New("session coordinator: declaring handoff scope is not immutable")
}

func collectDeclaringIncludeSpecs(
	spec engine.StepSpec,
	stepID string,
	declaringPath string,
	matches *[]*schema.IncludeSpec,
) {
	include, ok := spec.(*schema.IncludeSpec)
	if !ok || include == nil {
		return
	}
	if filepath.Clean(include.ResolvedRunbookPath) == declaringPath {
		*matches = append(*matches, include)
	}
	collectDeclaringIncludeSpecsInFlow(include.ResolvedSteps, stepID, declaringPath, matches)
}

func collectDeclaringIncludeSpecsInFlow(
	nodes []schema.FlowNode,
	stepID string,
	declaringPath string,
	matches *[]*schema.IncludeSpec,
) {
	for index := range nodes {
		node := &nodes[index]
		if node.Step != nil {
			step := node.Step
			if step.IncludeSpec != nil {
				if step.ID == stepID && filepath.Clean(step.IncludeSpec.ResolvedRunbookPath) == declaringPath {
					*matches = append(*matches, step.IncludeSpec)
				}
				collectDeclaringIncludeSpecsInFlow(step.IncludeSpec.ResolvedSteps, stepID, declaringPath, matches)
			}
			for _, branch := range handoffStructuralChildFlows(step) {
				collectDeclaringIncludeSpecsInFlow(branch, stepID, declaringPath, matches)
			}
		}
		if node.Iterate != nil {
			collectDeclaringIncludeSpecsInFlow(node.Iterate.Steps, stepID, declaringPath, matches)
		}
		if node.Parallel != nil {
			for _, branch := range node.Parallel.Branches {
				collectDeclaringIncludeSpecsInFlow(branch.Steps, stepID, declaringPath, matches)
			}
		}
	}
}

func handoffSourceIsProtected(protection engine.DebugProtection, source string) bool {
	protected := make(map[string]bool, len(protection.ProtectedVars))
	for _, name := range protection.ProtectedVars {
		protected[name] = true
	}
	for _, segment := range strings.Split(source, ".") {
		if protected[segment] {
			return true
		}
	}
	return false
}

func handoffBindingSources(bindings map[string]string) (map[string]string, error) {
	if len(bindings) == 0 {
		return nil, nil
	}
	result := make(map[string]string, len(bindings))
	for name, expression := range bindings {
		source, ok := schema.HandoffBindingSource(expression)
		if !ok {
			return nil, errors.New("session coordinator: immutable handoff binding is invalid")
		}
		result[name] = source
	}
	return result, nil
}

func handoffFactRefs(bindings map[string]string) []session.FactRef {
	if len(bindings) == 0 {
		return nil
	}
	names := make([]string, 0, len(bindings))
	for name := range bindings {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]session.FactRef, 0, len(names))
	for _, name := range names {
		result = append(result, session.FactRef{Name: name, Source: bindings[name]})
	}
	return result
}

func sourceOccurrenceFromHandoff(
	runID string,
	state engine.RunState,
	request engine.HandoffRequest,
) (session.SourceOccurrence, error) {
	occurrence := session.SourceOccurrence{
		RunID: runID, QualifiedNodeID: request.QualifiedNodeID,
		CallPath: append([]engine.DebugCallFrame(nil), request.CallPath...), StepID: request.StepID,
		FrameID: request.FrameID, FrameStepIndex: request.FrameStepIndex,
		Phase: string(engine.ExecutionPhaseExecute), Invocation: request.Invocation,
		RetryAttempt: request.RetryAttempt, OccurrenceSequence: request.OccurrenceSequence,
	}
	if request.FrameID == "" {
		if request.FrameStepIndex != 0 {
			return session.SourceOccurrence{}, errors.New("session coordinator: root handoff has a frame step index")
		}
		return occurrence, nil
	}
	frame := state.ExecutionFrames[request.FrameID]
	if frame == nil || frame.Status != engine.ExecutionFrameStatusActive ||
		request.FrameStepIndex < 0 || request.FrameStepIndex >= frame.StepCount ||
		request.FrameStepIndex >= len(frame.StepIDs) || frame.StepIDs[request.FrameStepIndex] != request.StepID {
		return session.SourceOccurrence{}, errors.New("session coordinator: handoff frame identity is unavailable")
	}
	occurrence.BranchLabel = frame.BranchLabel
	occurrence.IterationIndex = frame.IterationIndex
	seen := make(map[string]bool)
	for current := frame; current != nil; current = state.ExecutionFrames[current.ParentFrameID] {
		if current.FrameID == "" || seen[current.FrameID] {
			return session.SourceOccurrence{}, errors.New("session coordinator: handoff frame ancestry is invalid")
		}
		seen[current.FrameID] = true
		occurrence.FrameStack = append(occurrence.FrameStack, current.FrameID)
		if current.ParentFrameID != "" && state.ExecutionFrames[current.ParentFrameID] == nil {
			return session.SourceOccurrence{}, errors.New("session coordinator: handoff parent frame is unavailable")
		}
	}
	for left, right := 0, len(occurrence.FrameStack)-1; left < right; left, right = left+1, right-1 {
		occurrence.FrameStack[left], occurrence.FrameStack[right] =
			occurrence.FrameStack[right], occurrence.FrameStack[left]
	}
	return occurrence, nil
}

func validateHandoffInputs(plan *engine.ExecutionPlan, supplied map[string]any) (map[string]any, error) {
	if err := validateHandoffTargetDeclarations(plan); err != nil {
		return nil, err
	}
	result := make(map[string]any, len(supplied)+len(plan.Inputs))
	validatedValues := make(map[string]bool, len(supplied)+len(plan.Inputs))
	for name, value := range supplied {
		declaration, found := plan.Inputs[name]
		if !found || declaration == nil {
			return nil, fmt.Errorf("session coordinator: handoff target input %q is not declared", name)
		}
		if declaration.Type == "secret" {
			return nil, fmt.Errorf("session coordinator: handoff target input %q is secret", name)
		}
		result[name] = value
		validatedValues[name] = true
	}
	names := make([]string, 0, len(plan.Inputs))
	for name := range plan.Inputs {
		names = append(names, name)
	}
	sort.Strings(names)
	pendingDefaults := make(map[string]any)
	for _, name := range names {
		declaration := plan.Inputs[name]
		if declaration == nil {
			continue
		}
		if _, found := result[name]; found {
			continue
		}
		if declaration.Type == "secret" {
			if declaration.Default != nil {
				return nil, fmt.Errorf("session coordinator: handoff target input %q has a secret default", name)
			}
			if declaration.Required {
				return nil, fmt.Errorf("session coordinator: required handoff target input %q is secret", name)
			}
			continue
		}
		if declaration.Default != nil {
			pendingDefaults[name] = declaration.Default
			continue
		}
		if declaration.Required {
			return nil, fmt.Errorf("session coordinator: required handoff target input %q is missing", name)
		}
	}
	for len(pendingDefaults) > 0 {
		progress := false
		for _, name := range names {
			value, pending := pendingDefaults[name]
			if !pending {
				continue
			}
			resolved, ready, resolveErr := resolveHandoffDefault(value, result)
			if resolveErr != nil {
				return nil, fmt.Errorf("session coordinator: handoff target input %q has an unresolved default", name)
			}
			if !ready {
				continue
			}
			result[name] = resolved
			validatedValues[name] = true
			delete(pendingDefaults, name)
			progress = true
		}
		if !progress {
			return nil, errors.New("session coordinator: handoff target has unresolved or cyclic defaults")
		}
	}
	stringValues := make(map[string]string, len(validatedValues))
	for _, name := range names {
		if !validatedValues[name] {
			continue
		}
		declaration := plan.Inputs[name]
		value := result[name]
		if err := validateHandoffInputType(name, declaration, value); err != nil {
			return nil, err
		}
		if len(declaration.Enum) > 0 {
			stringValue, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("session coordinator: enum input %q is not a string", name)
			}
			stringValues[name] = stringValue
		}
	}
	if err := schema.CheckCallerInputBindings(plan.Inputs, stringValues); err != nil {
		return nil, fmt.Errorf("session coordinator: handoff target: %w", err)
	}
	for _, name := range names {
		declaration := plan.Inputs[name]
		if declaration != nil && !validatedValues[name] && (declaration.Type == "" || declaration.Type == "string") {
			result[name] = ""
		}
	}
	return result, nil
}

func resolveHandoffDefault(value any, available map[string]any) (any, bool, error) {
	switch typed := value.(type) {
	case string:
		if !strings.Contains(typed, "${") {
			return typed, true, nil
		}
		source, exact := schema.HandoffBindingSource(typed)
		if !exact {
			return nil, false, errors.New("interpolated defaults are not supported")
		}
		resolved, err := resolveHandoffSourceValue(available, source)
		if err != nil {
			return nil, false, nil
		}
		return resolved, true, nil
	case map[string]any:
		resolved := make(map[string]any, len(typed))
		for name, nested := range typed {
			value, ready, err := resolveHandoffDefault(nested, available)
			if err != nil || !ready {
				return nil, ready, err
			}
			resolved[name] = value
		}
		return resolved, true, nil
	case []any:
		resolved := make([]any, len(typed))
		for index, nested := range typed {
			value, ready, err := resolveHandoffDefault(nested, available)
			if err != nil || !ready {
				return nil, ready, err
			}
			resolved[index] = value
		}
		return resolved, true, nil
	default:
		return value, true, nil
	}
}

func validateHandoffInputType(name string, declaration *schema.Input, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("session coordinator: handoff target input %q is not a JSON value", name)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var normalized any
	if err := decoder.Decode(&normalized); err != nil {
		return fmt.Errorf("session coordinator: handoff target input %q is not a JSON value", name)
	}
	inputType := declaration.Type
	if inputType == "" {
		inputType = "string"
	}
	valid := false
	switch inputType {
	case "string":
		_, valid = normalized.(string)
	case "boolean":
		_, valid = normalized.(bool)
	case "number":
		_, valid = normalized.(json.Number)
	case "integer":
		number, ok := normalized.(json.Number)
		if ok {
			_, integerErr := number.Int64()
			valid = integerErr == nil
		}
	case "array":
		_, valid = normalized.([]any)
	case "object":
		_, valid = normalized.(map[string]any)
	case "secret":
		return fmt.Errorf("session coordinator: handoff target input %q is secret", name)
	default:
		return fmt.Errorf("session coordinator: handoff target input %q has unsupported type %q", name, inputType)
	}
	if !valid {
		return fmt.Errorf("session coordinator: handoff target input %q must be %s", name, inputType)
	}
	return nil
}

func validateHandoffTargetArtifacts(
	sourceProtection engine.DebugProtection,
	plan *engine.ExecutionPlan,
	vars map[string]any,
	encodedPlan json.RawMessage,
	encodedGraph json.RawMessage,
) error {
	if plan == nil || !json.Valid(encodedPlan) || !json.Valid(encodedGraph) {
		return errors.New("session coordinator: handoff target artifacts are invalid")
	}
	if err := validateHandoffTargetDeclarations(plan); err != nil {
		return err
	}
	for name, declaration := range plan.Inputs {
		if declaration != nil && declaration.Type == "secret" && declaration.Default != nil {
			return fmt.Errorf("session coordinator: handoff target input %q has a secret default", name)
		}
	}
	policy := plan.Governance
	if policy == nil && plan.GovernanceSource != nil {
		policy = internalgovernance.BuildPolicy(plan.GovernanceSource)
	}
	targetProtection := engine.ExtendDebugProtection(engine.DebugProtection{}, vars, plan.Inputs, policy)
	for name, declaration := range plan.Outputs {
		if declaration == nil || declaration.Type != "secret" {
			continue
		}
		targetProtection.ProtectedVars = append(targetProtection.ProtectedVars, name)
		if value, ok := vars[name].(string); ok && value != "" {
			targetProtection.SecretValues = append(targetProtection.SecretValues, value)
		}
	}
	protection := engine.MergeDebugProtection(sourceProtection, targetProtection)
	canonicalPlan, err := plansnapshot.CanonicalProtectionArtifact(encodedPlan)
	if err != nil {
		return errors.New("session coordinator: immutable target plan cannot be scanned")
	}
	var graphArtifact any
	if err := decodeStrictDocument(encodedGraph, &graphArtifact); err != nil {
		return errors.New("session coordinator: immutable target GraphJSON cannot be scanned")
	}
	canonicalGraph, err := json.Marshal(graphArtifact)
	if err != nil {
		return errors.New("session coordinator: immutable target GraphJSON cannot be scanned")
	}
	if err := internaldebugprotect.ValidateHandoffJSON(protection, canonicalPlan, canonicalGraph); err != nil {
		return fmt.Errorf("session coordinator: handoff target artifacts contain protected content: %w", err)
	}
	return nil
}

func validateHandoffTargetDeclarations(plan *engine.ExecutionPlan) error {
	if plan == nil {
		return errors.New("session coordinator: handoff target plan is required")
	}
	for name, declaration := range plan.Inputs {
		if declaration != nil && declaration.Default != nil && hasSensitiveJSONField(declaration.Default, 0) {
			return fmt.Errorf("session coordinator: handoff target input %q has a sensitive default", name)
		}
	}
	for name, declaration := range plan.Outputs {
		if declaration != nil && declaration.Type != "secret" && sensitive.Name(name) {
			return fmt.Errorf("session coordinator: handoff target output %q is sensitive but not secret", name)
		}
	}
	return nil
}

func hasSensitiveJSONField(value any, depth int) bool {
	if depth > 64 {
		return true
	}
	switch typed := value.(type) {
	case map[string]any:
		for name, item := range typed {
			if sensitive.Name(name) || hasSensitiveJSONField(item, depth+1) {
				return true
			}
		}
	case []any:
		for _, item := range typed {
			if hasSensitiveJSONField(item, depth+1) {
				return true
			}
		}
	default:
		if depth != 0 {
			return false
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return true
		}
		decoder := json.NewDecoder(bytes.NewReader(encoded))
		decoder.UseNumber()
		var normalized any
		if err := decoder.Decode(&normalized); err != nil {
			return true
		}
		return hasSensitiveJSONField(normalized, depth+1)
	}
	return false
}

type Handle struct {
	coordinator *Coordinator
	sessionID   string
	runID       string
	lease       session.Lease
	run         engine.RunHandle
	sessionRuns *sessionRunStore
	runOptions  engine.RunOptions
	execMu      sync.Mutex
	runMu       sync.RWMutex
	manifestMu  sync.RWMutex
	manifest    session.Manifest
	releaseOnce sync.Once
	releaseErr  error
}

func (handle *Handle) ConfigureStartupCommand(
	ctx context.Context,
	commandID string,
	clientCommandDigest string,
	vars map[string]string,
) error {
	handle.execMu.Lock()
	defer handle.execMu.Unlock()
	run := handle.currentRun()
	configurable, ok := run.(engine.StartupConfigurableRunHandle)
	if !ok {
		return errors.New("session coordinator: active run does not support startup configuration")
	}
	if err := configurable.ConfigureStartup(
		withClientCommandIdentity(ctx, commandID, clientCommandDigest), vars,
	); err != nil {
		return err
	}
	manifest := handle.sessionRuns.Manifest()
	handle.setManifest(manifest)
	receipt, found := manifest.AcceptedCommands[commandID]
	if !found || receipt.ClientCommandDigest != clientCommandDigest ||
		receipt.EventKind != session.EventExecutionCommitted {
		return errors.New("session coordinator: startup configuration has no durable command receipt")
	}
	return nil
}

func (handle *Handle) Next(ctx context.Context) (*engine.StepResult, error) {
	handle.execMu.Lock()
	defer handle.execMu.Unlock()
	run := handle.currentRun()
	if run == nil {
		return nil, io.EOF
	}
	result, runErr := run.Next(ctx)
	if handle.sessionRuns != nil {
		handle.setManifest(handle.sessionRuns.Manifest())
	}
	if request, ok := engine.HandoffRequestFromError(runErr); ok && handle.coordinator.handoffs != nil {
		if err := handle.commitHandoff(ctx, request); err != nil {
			return nil, err
		}
		return nil, nil
	}
	return result, runErr
}

func (handle *Handle) commitHandoff(ctx context.Context, request engine.HandoffRequest) error {
	state := handle.run.State()
	if state.Status != engine.RunStatusHandoffPending || state.PendingHandoff == nil ||
		session.DigestJSON(*state.PendingHandoff) != session.DigestJSON(request) {
		return errors.New("session coordinator: handoff signal does not match durable source state")
	}
	manifest := handle.currentManifest()
	committed := committedHandoff{}
	if err := handle.coordinator.prepareAndCommitHandoff(
		ctx, handle.sessionID, handle.lease.Epoch(), manifest, state.Plan, &state, request, &committed,
	); err != nil {
		return err
	}
	handle.setManifest(committed.manifest)
	return handle.startHandoffTarget(ctx, committed.manifest, committed.plan, committed.vars)
}

type committedHandoff struct {
	manifest session.Manifest
	plan     *engine.ExecutionPlan
	vars     map[string]any
}

func (coordinator *Coordinator) prepareAndCommitHandoff(
	ctx context.Context,
	sessionID string,
	writerEpoch uint64,
	manifest session.Manifest,
	sourcePlan *engine.ExecutionPlan,
	sourceState *engine.RunState,
	request engine.HandoffRequest,
	result *committedHandoff,
) error {
	sourceSegment, found := manifest.Segments[manifest.Session.ActiveSegmentID]
	sourceRunID := manifest.Session.ActiveRunID
	sourceAttempt := manifest.Attempts[sourceRunID]
	if !found || sourceRunID == "" || sourceAttempt.SegmentID != sourceSegment.SegmentID ||
		sourceAttempt.Status != session.AttemptStatusHandoffPending {
		return errors.New("session coordinator: handoff source is not the active segment")
	}
	if sourcePlan == nil {
		return errors.New("session coordinator: handoff source plan is unavailable")
	}
	if sourceState == nil {
		return errors.New("session coordinator: handoff source state is unavailable")
	}
	if err := validateHandoffRequestAgainstState(sourcePlan, *sourceState, request); err != nil {
		return err
	}
	expectedPath, err := staticHandoffPath(
		handoffDeclaringRunbookPath(sourcePlan.RunbookPath, request.CallPath), request.TargetRunbook,
	)
	if err != nil {
		return err
	}
	if coordinator.handoffs == nil {
		return errors.New("session coordinator: handoff resolver is not configured")
	}
	target, err := coordinator.handoffs.ResolveHandoff(ctx, HandoffResolveRequest{
		SessionID: sessionID, SourceSegment: sourceSegment, SourcePlan: sourcePlan, Handoff: request,
	})
	if err != nil {
		return fmt.Errorf("session coordinator: resolve handoff target: %w", err)
	}
	if target.Plan == nil || target.Plan.Validation == nil {
		return errors.New("session coordinator: handoff resolver returned an unvalidated plan")
	}
	if filepath.Clean(target.Plan.RunbookPath) != expectedPath {
		return errors.New("session coordinator: handoff resolver returned a different runbook")
	}
	targetVars, err := validateHandoffInputs(target.Plan, request.Context)
	if err != nil {
		return err
	}
	contextBindings, err := normalizedHandoffBindings(request.ContextBindings, request.Context)
	if err != nil {
		return err
	}
	factBindings, err := normalizedHandoffBindings(request.FactBindings, request.Facts)
	if err != nil {
		return err
	}
	contextData, err := json.Marshal(session.HandoffContext{Inputs: request.Context, Facts: request.Facts})
	if err != nil {
		return fmt.Errorf("session coordinator: encode handoff context: %w", err)
	}
	contextBlob := session.NewJSONBlob(contextData)
	sourceOccurrence, err := sourceOccurrenceFromHandoff(sourceRunID, *sourceState, request)
	if err != nil {
		return err
	}
	idempotencyKey := session.DigestJSON(struct {
		Source        session.SourceOccurrence `json:"source"`
		TargetRunbook string                   `json:"target_runbook"`
		ReasonCode    string                   `json:"reason_code"`
		ContextDigest string                   `json:"context_digest"`
	}{sourceOccurrence, request.TargetRunbook, request.ReasonCode, contextBlob.Digest})
	transitionID := deterministicHandoffID(sessionID, "transition", idempotencyKey)
	targetSegmentID := deterministicHandoffID(sessionID, "segment", idempotencyKey)
	targetRunID := deterministicHandoffID(sessionID, "run", idempotencyKey)
	targetPlan, err := clonePlanForRun(target.Plan, targetRunID)
	if err != nil {
		return err
	}
	if err := coordinator.runtime.PrepareDurablePlan(ctx, targetPlan); err != nil {
		return fmt.Errorf("session coordinator: prepare handoff target plan: %w", err)
	}
	targetOptions := engine.RunOptions{
		Mode: engine.RunMode(sourceAttempt.Mode), Actor: sourceAttempt.Actor, Client: sourceAttempt.Client,
		RuntimeVars: targetVars,
	}
	if err := coordinator.runtime.ValidateDurableHandoffPlan(ctx, targetPlan, targetOptions); err != nil {
		return fmt.Errorf("session coordinator: validate durable handoff target plan: %w", err)
	}
	canonicalGraph, err := finalizeExecutionPlanGraph(targetPlan, nil, target.Graph)
	if err != nil {
		return fmt.Errorf("session coordinator: validate finalized handoff target graph: %w", err)
	}
	planBlob, err := snapshotBlob(targetPlan)
	if err != nil {
		return err
	}
	boundGraph, err := bindHandoffGraph(targetPlan, planBlob.Digest, canonicalGraph)
	if err != nil {
		return err
	}
	if err := coordinator.runtime.ValidateDurableHandoffArtifacts(
		ctx, targetPlan, targetOptions, planBlob.Data, boundGraph,
	); err != nil {
		return fmt.Errorf("session coordinator: validate handoff target artifacts: %w", err)
	}
	sourceScope, err := immutableHandoffScope(sourcePlan, *sourceState, request)
	if err != nil {
		return err
	}
	if err := validateHandoffTargetArtifacts(
		sourceScope.Protection, targetPlan, targetVars, planBlob.Data, boundGraph,
	); err != nil {
		return err
	}
	graphBlob := session.NewJSONBlob(boundGraph)
	factNames := make([]string, 0, len(factBindings))
	for name := range factBindings {
		factNames = append(factNames, name)
	}
	sort.Strings(factNames)
	factRefs := make([]session.FactRef, 0, len(factNames))
	for _, name := range factNames {
		factRefs = append(factRefs, session.FactRef{Name: name, Source: factBindings[name]})
	}
	transition := session.TransitionRecord{
		TransitionID: transitionID, Status: session.TransitionStatusPrepared,
		SourceSegmentID: sourceSegment.SegmentID, SourceOccurrence: sourceOccurrence,
		TargetSegmentID: targetSegmentID, TargetRunID: targetRunID,
		TargetRunbookID: targetPlan.Metadata.RunbookID, TargetRunbookName: targetPlan.Metadata.RunbookName,
		TargetPlanHash: targetPlan.Metadata.PlanHash, TargetGraphHash: graphBlob.Digest,
		TargetExecutableSnapshotHash: planBlob.Digest,
		ReasonCode:                   request.ReasonCode, ReasonSummary: request.ReasonSummary,
		ContextBindings: contextBindings, FactRefs: factRefs,
		ContextDigest: contextBlob.Digest, IdempotencyKey: idempotencyKey,
	}
	targetSegment := session.SegmentRecord{
		SegmentID: targetSegmentID, Ordinal: sourceSegment.Ordinal + 1,
		RunbookID: targetPlan.Metadata.RunbookID, RunbookName: targetPlan.Metadata.RunbookName,
		EntrySelector: session.EntrySelector{Step: "$entry"}, Status: session.SegmentStatusPrepared,
		PlanHash: targetPlan.Metadata.PlanHash, GraphHash: graphBlob.Digest,
		ExecutableSnapshotHash: planBlob.Digest, CatalogDigest: targetPlan.Metadata.CatalogDigest,
		PackageLockDigest: session.DigestJSON(targetPlan.Metadata.PackageDigests),
		ProfileDigest:     session.DigestJSON(targetPlan.Metadata.Profile), AttemptRunIDs: []string{targetRunID},
		ExecutableRevision: 1, GraphRevision: 1,
	}
	targetAttempt := session.RunAttemptRecord{
		RunID: targetRunID, SegmentID: targetSegmentID, Ordinal: 1,
		Mode: sourceAttempt.Mode, Status: session.AttemptStatusStarting,
		Actor: sourceAttempt.Actor, Client: sourceAttempt.Client, PlanHash: targetPlan.Metadata.PlanHash,
	}
	durableCtx := context.WithoutCancel(ctx)
	prepareCommandID := deterministicHandoffID(sessionID, "prepare", transitionID)
	prepared, err := coordinator.sessions.PrepareTransition(durableCtx, session.PrepareTransitionRequest{
		SessionID: sessionID, CommandID: prepareCommandID, WriterEpoch: writerEpoch,
		ExpectedSequence: manifest.Session.Sequence, Transition: transition,
		TargetSegment: targetSegment, TargetAttempt: targetAttempt,
		Blobs: []session.JSONBlob{planBlob, graphBlob, contextBlob},
	})
	if err != nil {
		prepared, err = coordinator.recoverCommittedCommand(
			durableCtx, sessionID, prepareCommandID,
			session.TransitionPrepareDigest(transition, targetSegment, targetAttempt),
			session.EventTransitionPrepared, prepared, err,
		)
		if err != nil {
			return err
		}
	}
	commitCommandID := deterministicHandoffID(sessionID, "commit", transitionID)
	committed, err := coordinator.sessions.CommitTransition(durableCtx, session.CommitTransitionRequest{
		SessionID: sessionID, CommandID: commitCommandID, WriterEpoch: writerEpoch,
		ExpectedSequence: prepared.Session.Sequence, TransitionID: transitionID,
	})
	if err != nil {
		committed, err = coordinator.recoverCommittedCommand(
			durableCtx, sessionID, commitCommandID, session.TransitionCommitDigest(transitionID),
			session.EventTransitionCommitted, committed, err,
		)
		if err != nil {
			return err
		}
	}
	result.manifest = committed
	result.plan = targetPlan
	result.vars = targetVars
	return nil
}

func (handle *Handle) startHandoffTarget(
	ctx context.Context,
	manifest session.Manifest,
	plan *engine.ExecutionPlan,
	targetVars map[string]any,
) error {
	attempt, found := manifest.Attempts[plan.RunID]
	if !found || manifest.Session.ActiveRunID != attempt.RunID || attempt.Status != session.AttemptStatusStarting {
		return errors.New("session coordinator: committed handoff target is not startable")
	}
	options := handle.runOptions
	options.Mode = engine.RunMode(attempt.Mode)
	options.Actor = attempt.Actor
	options.Client = attempt.Client
	options.Vars = nil
	options.RuntimeVars = targetVars
	runHandle, sessionRuns, err := handle.coordinator.startOrResumeRun(
		ctx, handle.sessionID, handle.lease.Epoch(), manifest, attempt, plan, options,
	)
	if err != nil {
		return err
	}
	if attempt.ExecutionMutationHash == "" {
		if err := sessionRuns.SaveState(context.WithoutCancel(ctx), runHandle.State()); err != nil {
			_ = runHandle.Cancel(context.Background(), "initial handoff target execution commit failed")
			return err
		}
	}
	manifest = sessionRuns.Manifest()
	if handle.sessionRuns != nil {
		handle.sessionRuns.SetManifestObserver(nil)
	}
	handle.runMu.Lock()
	handle.runID = attempt.RunID
	handle.run = runHandle
	handle.sessionRuns = sessionRuns
	handle.runOptions = options
	handle.runMu.Unlock()
	handle.setManifest(manifest)
	sessionRuns.SetManifestObserver(handle.setManifest)
	return nil
}

func attemptStatusFromRun(status engine.RunStatus) session.AttemptStatus {
	switch status {
	case engine.RunStatusRunning:
		return session.AttemptStatusRunning
	case engine.RunStatusWaiting:
		return session.AttemptStatusWaiting
	case engine.RunStatusPausedAtBoundary:
		return session.AttemptStatusPausedAtBoundary
	case engine.RunStatusHandoffPending:
		return session.AttemptStatusHandoffPending
	case engine.RunStatusCompleted:
		return session.AttemptStatusCompleted
	case engine.RunStatusFailed:
		return session.AttemptStatusFailed
	case engine.RunStatusCancelled:
		return session.AttemptStatusCancelled
	case engine.RunStatusIndeterminate:
		return session.AttemptStatusIndeterminate
	default:
		return ""
	}
}

func (handle *Handle) Manifest() session.Manifest {
	return cloneManifest(handle.currentManifest())
}

func (handle *Handle) SessionID() string { return handle.sessionID }

func (handle *Handle) WriterEpoch() uint64 {
	if handle == nil || handle.lease == nil {
		return 0
	}
	return handle.lease.Epoch()
}

func (handle *Handle) Epoch() uint64 { return handle.WriterEpoch() }

func (handle *Handle) currentRun() engine.RunHandle {
	handle.runMu.RLock()
	defer handle.runMu.RUnlock()
	return handle.run
}

func (handle *Handle) Detach(
	ctx context.Context,
	commandID string,
	reason string,
) (session.Manifest, error) {
	return handle.DetachCommand(ctx, commandID, "", reason)
}

func (handle *Handle) DetachCommand(
	ctx context.Context,
	commandID string,
	clientCommandDigest string,
	reason string,
) (session.Manifest, error) {
	return handle.detachCommand(ctx, commandID, clientCommandDigest, reason, true)
}

func (handle *Handle) PauseAttached(
	ctx context.Context,
	commandID string,
	reason string,
) (session.Manifest, error) {
	return handle.detachCommand(ctx, commandID, "", reason, false)
}

func (handle *Handle) PauseAttachedCommand(
	ctx context.Context,
	commandID string,
	clientCommandDigest string,
	reason string,
) (session.Manifest, error) {
	return handle.detachCommand(ctx, commandID, clientCommandDigest, reason, false)
}

func (handle *Handle) detachCommand(
	ctx context.Context,
	commandID string,
	clientCommandDigest string,
	reason string,
	releaseSessionLease bool,
) (session.Manifest, error) {
	for {
		run := handle.currentRun()
		if run == nil {
			return session.Manifest{}, errors.New("session coordinator: active run handle is unavailable")
		}
		detachable, ok := run.(engine.DetachableRunHandle)
		if !ok {
			return session.Manifest{}, errors.New("session coordinator: active run handle cannot detach")
		}
		detachCtx := ctx
		if clientCommandDigest != "" {
			detachCtx = withClientCommandIdentity(ctx, commandID, clientCommandDigest)
		}
		detachErr := detachable.Detach(detachCtx, reason)

		handle.execMu.Lock()
		if handle.currentRun() != run {
			handle.execMu.Unlock()
			continue
		}
		if handle.sessionRuns != nil {
			handle.setManifest(handle.sessionRuns.Manifest())
		}
		manifest := handle.currentManifest()
		if errors.Is(detachErr, engine.ErrIndeterminate) {
			if clientCommandDigest != "" {
				receipt, found := manifest.AcceptedCommands[commandID]
				if found && receipt.ClientCommandDigest != clientCommandDigest {
					handle.execMu.Unlock()
					return session.Manifest{}, session.ErrCommandConflict
				}
				if !found {
					detached, recordErr := handle.coordinator.sessions.RecordDetach(
						context.WithoutCancel(ctx), session.DetachRequest{
							SessionID: handle.sessionID, CommandID: commandID, WriterEpoch: handle.lease.Epoch(),
							ExpectedSequence:    manifest.Session.Sequence,
							ClientCommandDigest: clientCommandDigest, Reason: reason,
						},
					)
					if recordErr != nil {
						detached, recordErr = handle.coordinator.recoverCommittedCommand(
							ctx, handle.sessionID, commandID, session.DetachDigest(reason),
							session.EventSessionDetached, detached, recordErr,
						)
					}
					if recordErr != nil {
						handle.execMu.Unlock()
						return session.Manifest{}, recordErr
					}
					manifest = detached
					handle.setManifest(manifest)
					if handle.sessionRuns != nil {
						handle.sessionRuns.SetManifest(manifest)
					}
				}
			}
			if releaseSessionLease {
				handle.releaseLeaseLocked()
			}
			handle.execMu.Unlock()
			return manifest, detachErr
		}
		if detachErr != nil {
			handle.execMu.Unlock()
			return session.Manifest{}, detachErr
		}
		paused, err := handle.coordinator.sessions.PauseSession(context.WithoutCancel(ctx), session.PauseRequest{
			SessionID: handle.sessionID, CommandID: commandID, WriterEpoch: handle.lease.Epoch(),
			ExpectedSequence: manifest.Session.Sequence, ClientCommandDigest: clientCommandDigest, Reason: reason,
		})
		if err != nil {
			paused, err = handle.coordinator.recoverCommittedCommand(
				ctx, handle.sessionID, commandID, session.PauseDigest(manifest.Session.ActiveRunID, reason),
				session.EventSessionPaused, paused, err,
			)
		}
		if err == nil {
			handle.setManifest(paused)
			if handle.sessionRuns != nil {
				handle.sessionRuns.SetManifest(paused)
			}
			if releaseSessionLease {
				handle.releaseLeaseLocked()
			}
		}
		handle.execMu.Unlock()
		return paused, err
	}
}

func (handle *Handle) CancelCommand(
	ctx context.Context,
	commandID string,
	clientCommandDigest string,
	reason string,
) (session.Manifest, error) {
	for {
		run := handle.currentRun()
		if run == nil {
			return session.Manifest{}, errors.New("session coordinator: active run handle is unavailable")
		}
		cancelErr := run.Cancel(
			withClientCommandIdentity(context.WithoutCancel(ctx), commandID, clientCommandDigest), reason,
		)

		handle.execMu.Lock()
		if handle.currentRun() != run {
			handle.execMu.Unlock()
			continue
		}
		if handle.sessionRuns != nil {
			handle.setManifest(handle.sessionRuns.Manifest())
		}
		manifest := handle.currentManifest()
		handle.execMu.Unlock()
		receipt, found := manifest.AcceptedCommands[commandID]
		if !found || receipt.ClientCommandDigest != clientCommandDigest ||
			receipt.EventKind != session.EventExecutionCommitted {
			if cancelErr != nil {
				return session.Manifest{}, cancelErr
			}
			return session.Manifest{}, errors.New("session coordinator: cancellation produced no durable command receipt")
		}
		return manifest, cancelErr
	}
}

func (handle *Handle) currentManifest() session.Manifest {
	handle.manifestMu.RLock()
	defer handle.manifestMu.RUnlock()
	return handle.manifest
}

func (handle *Handle) setManifest(manifest session.Manifest) {
	handle.manifestMu.Lock()
	handle.manifest = manifest
	handle.manifestMu.Unlock()
}

func cloneManifest(manifest session.Manifest) session.Manifest {
	encoded, _ := json.Marshal(manifest)
	var cloned session.Manifest
	_ = json.Unmarshal(encoded, &cloned)
	return cloned
}

func (handle *Handle) CloseInvestigation(
	ctx context.Context,
	commandID string,
	status session.Status,
) (session.Manifest, error) {
	return handle.CloseInvestigationCommand(ctx, commandID, "", status)
}

func (handle *Handle) CloseInvestigationCommand(
	ctx context.Context,
	commandID string,
	clientCommandDigest string,
	status session.Status,
) (session.Manifest, error) {
	handle.execMu.Lock()
	defer handle.execMu.Unlock()
	if run := handle.currentRun(); run != nil {
		runStatus := run.State().Status
		if runStatus == engine.RunStatusPending || runStatus == engine.RunStatusRunning || runStatus == engine.RunStatusWaiting {
			return session.Manifest{}, session.ErrAttemptActive
		}
	}
	current := handle.currentManifest()
	manifest, err := handle.coordinator.sessions.CloseSession(context.WithoutCancel(ctx), session.CloseRequest{
		SessionID: handle.sessionID, CommandID: commandID, WriterEpoch: handle.lease.Epoch(),
		ExpectedSequence: current.Session.Sequence, ClientCommandDigest: clientCommandDigest, Status: status,
	})
	if err != nil {
		manifest, err = handle.coordinator.recoverCommittedCommand(
			ctx, handle.sessionID, commandID, session.CloseDigest(status), session.EventSessionClosed, manifest, err,
		)
		if err != nil {
			return session.Manifest{}, err
		}
	}
	handle.setManifest(manifest)
	if handle.sessionRuns != nil {
		handle.sessionRuns.SetManifest(manifest)
	}
	return manifest, nil
}

func (handle *Handle) Release() error {
	handle.execMu.Lock()
	defer handle.execMu.Unlock()
	if run := handle.currentRun(); run != nil {
		status := run.State().Status
		if status == engine.RunStatusPending || status == engine.RunStatusRunning || status == engine.RunStatusWaiting {
			return session.ErrAttemptActive
		}
	}
	handle.releaseLeaseLocked()
	return handle.releaseErr
}

func (handle *Handle) releaseLeaseLocked() {
	handle.releaseOnce.Do(func() {
		handle.releaseErr = handle.lease.Release()
	})
}
