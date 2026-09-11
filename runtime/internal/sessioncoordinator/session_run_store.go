package sessioncoordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"
	internaltrace "github.com/ormasoftchile/yawr/runtime/internal/trace"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/session"
	tracepkg "github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

type sessionRunStore struct {
	coordinator            *Coordinator
	sessionID              string
	runID                  string
	writerEpoch            uint64
	mu                     sync.Mutex
	manifest               session.Manifest
	onManifest             func(session.Manifest)
	pendingTraceProjection []engine.Event
	configuredTraceFailed  bool
}

type clientCommandIdentity struct {
	commandID string
	digest    string
}

type clientCommandIdentityContextKey struct{}

func withClientCommandIdentity(ctx context.Context, commandID, digest string) context.Context {
	return context.WithValue(ctx, clientCommandIdentityContextKey{}, clientCommandIdentity{
		commandID: commandID, digest: digest,
	})
}

func clientCommandIdentityFromContext(ctx context.Context) (clientCommandIdentity, bool) {
	identity, ok := ctx.Value(clientCommandIdentityContextKey{}).(clientCommandIdentity)
	return identity, ok && identity.commandID != "" && identity.digest != ""
}

func newSessionRunStore(
	coordinator *Coordinator,
	sessionID string,
	runID string,
	writerEpoch uint64,
	manifest session.Manifest,
) *sessionRunStore {
	return &sessionRunStore{
		coordinator: coordinator, sessionID: sessionID, runID: runID,
		writerEpoch: writerEpoch, manifest: manifest,
	}
}

func (store *sessionRunStore) AcquireRunLease(ctx context.Context, runID string) (engine.RunLease, error) {
	if runID != store.runID {
		return nil, fmt.Errorf("session coordinator: run lease id %q does not match %q", runID, store.runID)
	}
	for {
		lease, err := store.coordinator.runs.AcquireRunLease(ctx, runID)
		if err != nil {
			return nil, err
		}
		if lease.Epoch() == store.writerEpoch {
			return lease, nil
		}
		if lease.Epoch() > store.writerEpoch {
			_ = lease.Release()
			return nil, fmt.Errorf(
				"session coordinator: run writer epoch %d exceeds session writer epoch %d",
				lease.Epoch(), store.writerEpoch,
			)
		}
		if err := lease.Release(); err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
}

func (store *sessionRunStore) SavePlan(ctx context.Context, runID string, plan *engine.ExecutionPlan) error {
	return store.coordinator.runs.SavePlan(ctx, runID, plan)
}

func (store *sessionRunStore) LoadPlan(ctx context.Context, runID string) (*engine.ExecutionPlan, error) {
	return store.coordinator.runs.LoadPlan(ctx, runID)
}

func (store *sessionRunStore) PlanDigest(runID string) (string, bool) {
	return store.coordinator.runs.PlanDigest(runID)
}

func (store *sessionRunStore) SaveState(ctx context.Context, state engine.RunState) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if state.RunID != store.runID {
		return fmt.Errorf("session coordinator: state run id %q does not match %q", state.RunID, store.runID)
	}
	if err := store.coordinator.runs.SaveState(ctx, state); err != nil {
		return err
	}
	revisedPlan, err := store.commitDynamicSegmentRevision(ctx, state)
	if err != nil {
		return err
	}
	archive, err := store.coordinator.projections.ExportRunProjection(ctx, store.runID)
	if err != nil {
		return fmt.Errorf("session coordinator: export execution mutation projection: %w", err)
	}
	if revisedPlan != nil {
		archive, err = alignRunProjectionWithPlan(archive, revisedPlan, state.DynamicIncludes, nil)
		if err != nil {
			return err
		}
	}
	if len(store.pendingTraceProjection) > 0 {
		archive, err = appendTraceEventsToRunProjection(archive, store.pendingTraceProjection)
		if err != nil {
			return fmt.Errorf("session coordinator: retain failed trace projection: %w", err)
		}
	}
	projection, projectionBlobs, err := encodeSessionRunProjection(archive, state)
	if err != nil {
		return err
	}
	frameProjection, frameProjectionChunks, err := encodeExecutionFrameProjection(state)
	if err != nil {
		return err
	}
	projectionBlobs = append(projectionBlobs, frameProjectionChunks...)
	attempt := store.manifest.Attempts[store.runID]
	status := attemptStatusFromRun(state.Status)
	if state.Status == engine.RunStatusPending {
		status = session.AttemptStatusRunning
	}
	if status == "" {
		return fmt.Errorf("session coordinator: unsupported run status %q", state.Status)
	}
	traceStart, traceEnd := int64(0), int64(0)
	if len(state.PendingTraceEvents) > 0 {
		traceStart = state.PendingTraceEvents[0].Sequence
		traceEnd = state.PendingTraceEvents[len(state.PendingTraceEvents)-1].Sequence
	}
	mutation := session.ExecutionMutation{
		SchemaVersion: session.ExecutionMutationSchemaV1, RunID: store.runID,
		CheckpointSequence: state.CheckpointSequence, RunStatus: state.Status, AttemptStatus: status,
		RunWriterEpoch: state.WriterEpoch, PreviousMutationHash: attempt.ExecutionMutationHash,
		StateProjectionHash: projection.Digest, FrameProjectionHash: frameProjection.Digest,
		CommittedTraceSequence: state.CommittedTraceSequence,
		TraceSequenceStart:     traceStart, TraceSequenceEnd: traceEnd,
	}
	mutationBlob, err := session.NewExecutionMutationBlob(mutation)
	if err != nil {
		return err
	}
	occurrences, err := occurrenceRecordsForEvents(store.manifest, store.runID, state.PendingTraceEvents)
	if err != nil {
		return err
	}
	commandID := uuid.NewSHA1(
		uuid.MustParse(store.sessionID), []byte("execution-mutation:"+store.runID+":"+mutationBlob.Digest),
	).String()
	answerCommandID := ""
	answerCommandDigest := ""
	for _, interaction := range state.Interactions {
		if interaction == nil || interaction.Status != engine.InteractionStatusAnswered || interaction.AnswerCommandID == "" {
			continue
		}
		if receipt, accepted := store.manifest.AcceptedCommands[interaction.AnswerCommandID]; accepted {
			if receipt.ClientCommandDigest != interaction.AnswerCommandDigest ||
				receipt.EventKind != session.EventExecutionCommitted {
				return session.ErrCommandConflict
			}
			continue
		}
		if answerCommandID != "" && answerCommandID != interaction.AnswerCommandID {
			return errors.New("session coordinator: one checkpoint contains multiple answer command ids")
		}
		answerCommandID = interaction.AnswerCommandID
		answerCommandDigest = interaction.AnswerCommandDigest
	}
	if identity, found := clientCommandIdentityFromContext(ctx); found &&
		(state.Status == engine.RunStatusPending || state.Status == engine.RunStatusCancelled ||
			state.Status == engine.RunStatusIndeterminate) {
		commandID = identity.commandID
		answerCommandDigest = identity.digest
	} else if answerCommandID != "" {
		commandID = answerCommandID
	}
	committed, commitErr := store.coordinator.sessions.CommitExecutionMutation(ctx, session.ExecutionMutationRequest{
		SessionID: store.sessionID, CommandID: commandID, WriterEpoch: store.writerEpoch,
		ExpectedSequence: store.manifest.Session.Sequence, ClientCommandDigest: answerCommandDigest,
		Mutation: mutation, StateProjection: projection, FrameProjection: frameProjection,
		ProjectionBlobs: projectionBlobs, Occurrences: occurrences,
	})
	if commitErr != nil {
		committed, commitErr = store.coordinator.recoverCommittedCommand(
			ctx, store.sessionID, commandID, session.ExecutionMutationDigest(store.runID, mutationBlob.Digest),
			session.EventExecutionCommitted, committed, commitErr,
		)
		if commitErr != nil {
			return commitErr
		}
	}
	store.manifest = committed
	store.pendingTraceProjection = nil
	if store.onManifest != nil {
		store.onManifest(committed)
	}
	return nil
}

func occurrenceRecordsForEvents(
	manifest session.Manifest,
	runID string,
	events []engine.Event,
) (map[string]session.OccurrenceRecord, error) {
	attempt, found := manifest.Attempts[runID]
	if !found || attempt.SegmentID == "" {
		return nil, errors.New("session coordinator: occurrence attempt is unavailable")
	}
	source := session.ExecutionSourceLive
	if attempt.Mode == string(engine.RunModeReplay) || attempt.Mode == string(engine.RunModeRouteTest) {
		source = session.ExecutionSourceSaved
	}
	records := make(map[string]session.OccurrenceRecord)
	for _, event := range events {
		occurrenceID, record, terminal, err := session.OccurrenceFromTraceEvent(
			manifest.Session.SessionID, attempt.SegmentID, source, event,
		)
		if err != nil {
			return nil, err
		}
		if !terminal {
			continue
		}
		if existing, duplicate := records[occurrenceID]; duplicate && session.DigestJSON(existing) != session.DigestJSON(record) {
			return nil, errors.New("session coordinator: checkpoint rewrites an occurrence")
		}
		records[occurrenceID] = record
	}
	if len(records) == 0 {
		return nil, nil
	}
	return records, nil
}

func (store *sessionRunStore) commitDynamicSegmentRevision(
	ctx context.Context,
	state engine.RunState,
) (*engine.ExecutionPlan, error) {
	attempt := store.manifest.Attempts[store.runID]
	segment := store.manifest.Segments[attempt.SegmentID]
	desiredRevision := int64(len(state.DynamicIncludes) + 1)
	if desiredRevision <= segment.ExecutableRevision && desiredRevision <= segment.GraphRevision {
		if desiredRevision == 1 {
			return nil, nil
		}
		planData, err := store.coordinator.sessions.ReadBlob(ctx, store.sessionID, segment.ExecutableSnapshotHash)
		if err != nil {
			return nil, err
		}
		plan, err := restorePlanBlob(planData)
		if err != nil {
			return nil, err
		}
		plan.RunID = store.runID
		return plan, nil
	}
	if desiredRevision != segment.ExecutableRevision+1 || desiredRevision != segment.GraphRevision+1 {
		return nil, errors.New("session coordinator: dynamic segment revision is not contiguous")
	}
	plan, planBlob, boundGraph, resolution, err := buildDynamicSegmentRevision(state)
	if err != nil {
		return nil, err
	}
	options := engine.RunOptions{
		Mode: engine.RunMode(attempt.Mode), Actor: attempt.Actor, Client: attempt.Client, RuntimeVars: state.Vars,
	}
	if err := store.coordinator.runtime.ValidateDurableHandoffPlan(ctx, plan, options); err != nil {
		return nil, fmt.Errorf("session coordinator: validate dynamic revision plan: %w", err)
	}
	if err := store.coordinator.runtime.ValidateDurableHandoffArtifacts(
		ctx, plan, options, planBlob.Data, boundGraph,
	); err != nil {
		return nil, fmt.Errorf("session coordinator: validate dynamic revision artifacts: %w", err)
	}
	graphBlob := session.NewJSONBlob(boundGraph)
	dispatch, err := settledDynamicRevisionDispatch(state, resolution)
	if err != nil {
		return nil, err
	}
	commandDigest := session.SegmentRevisionDigest(
		segment.SegmentID, store.runID, desiredRevision, desiredRevision, plan.Metadata.PlanHash,
		planBlob.Digest, graphBlob.Digest,
		resolution, dispatch,
	)
	commandID := uuid.NewSHA1(
		uuid.MustParse(store.sessionID), []byte("segment-revision:"+commandDigest),
	).String()
	committed, commitErr := store.coordinator.sessions.CommitSegmentRevision(ctx, session.SegmentRevisionRequest{
		SessionID: store.sessionID, CommandID: commandID, WriterEpoch: store.writerEpoch,
		ExpectedSequence: store.manifest.Session.Sequence, SegmentID: segment.SegmentID, RunID: store.runID,
		ExecutableRevision: desiredRevision, GraphRevision: desiredRevision,
		PlanHash:           plan.Metadata.PlanHash,
		ExecutableSnapshot: planBlob, Graph: graphBlob, Resolution: resolution, Dispatch: dispatch,
	})
	if commitErr != nil {
		committed, commitErr = store.coordinator.recoverCommittedCommand(
			ctx, store.sessionID, commandID, commandDigest, session.EventSegmentRevised, committed, commitErr,
		)
		if commitErr != nil {
			return nil, commitErr
		}
	}
	store.manifest = committed
	if store.onManifest != nil {
		store.onManifest(committed)
	}
	return plan, nil
}

func settledDynamicRevisionDispatch(
	state engine.RunState,
	resolution engine.DynamicIncludeResolutionState,
) (engine.DispatchState, error) {
	encodedPin, err := json.Marshal(resolution.Pin)
	if err != nil {
		return engine.DispatchState{}, errors.New("session coordinator: dynamic revision pin cannot be matched")
	}
	expectedResultDigest := engine.InteractionPayloadDigest(encodedPin)
	var matched *engine.DispatchState
	for _, dispatch := range state.Dispatches {
		if dispatch == nil || dispatch.Status != engine.DispatchStatusSettled ||
			dispatch.EndpointIdentity != "dynamic-include-resolver" ||
			dispatch.QualifiedNodeID != resolution.QualifiedNodeID || dispatch.StepID != resolution.StepID ||
			dispatch.FrameID != resolution.FrameID || dispatch.FrameStepIndex != resolution.FrameStepIndex ||
			dispatch.Invocation != resolution.Invocation || dispatch.ResultDigest != expectedResultDigest ||
			!sameHandoffCallPath(dispatch.CallPath, resolution.CallPath) {
			continue
		}
		if matched != nil {
			return engine.DispatchState{}, errors.New("session coordinator: dynamic revision resolver dispatch is ambiguous")
		}
		matched = dispatch
	}
	if matched == nil {
		return engine.DispatchState{}, errors.New("session coordinator: dynamic revision resolver dispatch is unavailable")
	}
	return *matched, nil
}

func (store *sessionRunStore) LoadState(ctx context.Context, runID string) (engine.RunState, error) {
	return store.coordinator.runs.LoadState(ctx, runID)
}

func (store *sessionRunStore) WriteTrace(ctx context.Context, runID string, event engine.Event) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if runID != store.runID || event.RunID != store.runID {
		return fmt.Errorf("session coordinator: trace run id does not match %q", store.runID)
	}
	attempt := store.manifest.Attempts[store.runID]
	latestTraceSequence := attempt.CommittedTraceSequence
	if attempt.JournaledTraceSequence > latestTraceSequence {
		latestTraceSequence = attempt.JournaledTraceSequence
	}
	if event.Sequence > latestTraceSequence {
		encoded, err := json.Marshal(event)
		if err != nil {
			return err
		}
		traceHash := session.NewJSONBlob(encoded).Digest
		commandID := uuid.NewSHA1(
			uuid.MustParse(store.sessionID), []byte("trace-event:"+store.runID+":"+event.EventID),
		).String()
		committed, commitErr := store.coordinator.sessions.CommitTraceEvent(ctx, session.TraceCommitRequest{
			SessionID: store.sessionID, CommandID: commandID, WriterEpoch: store.writerEpoch,
			ExpectedSequence: store.manifest.Session.Sequence, RunID: store.runID, Event: event,
		})
		if commitErr != nil {
			committed, commitErr = store.coordinator.recoverCommittedCommand(
				ctx, store.sessionID, commandID, session.TraceCommitDigest(store.runID, traceHash),
				session.EventTraceCommitted, committed, commitErr,
			)
			if commitErr != nil {
				return commitErr
			}
		}
		store.manifest = committed
		if store.onManifest != nil {
			store.onManifest(committed)
		}
	}
	if err := store.coordinator.runs.WriteTrace(ctx, runID, event); err != nil {
		store.pendingTraceProjection = append(store.pendingTraceProjection, event)
	}
	return nil
}

func (*sessionRunStore) TraceEventsAuthoritative() bool { return true }

func (store *sessionRunStore) RecoverTraceProjection(
	ctx context.Context,
	writer tracepkg.TraceWriter,
	state engine.RunState,
) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if state.RunID != store.runID {
		return errors.New("session coordinator: trace recovery state belongs to a different run")
	}
	events, err := internaltrace.NewJSONLReader(store.TracePath(store.runID)).ReadAll(ctx)
	if err != nil {
		return fmt.Errorf("session coordinator: read authoritative trace projection: %w", err)
	}
	attempt := store.manifest.Attempts[store.runID]
	lastSequence := attempt.CommittedTraceSequence
	if attempt.JournaledTraceSequence > lastSequence {
		lastSequence = attempt.JournaledTraceSequence
	}
	if len(events) != int(lastSequence) {
		return fmt.Errorf(
			"session coordinator: authoritative trace has %d events, want %d", len(events), lastSequence,
		)
	}
	projectedSequence := int64(0)
	if provider, ok := writer.(interface{ LastSequence(string) int64 }); ok {
		projectedSequence = provider.LastSequence(store.runID)
		if projectedSequence > lastSequence {
			store.configuredTraceFailed = true
			return nil
		}
	}
	for index, event := range events {
		if event.Sequence != int64(index+1) || event.RunID != store.runID || event.EventID == "" {
			return errors.New("session coordinator: authoritative trace projection is not contiguous")
		}
		if event.Sequence <= projectedSequence {
			continue
		}
		if err := writer.Append(event); err != nil {
			store.configuredTraceFailed = true
			return nil
		}
	}
	store.configuredTraceFailed = false
	return nil
}

func (store *sessionRunStore) TraceProjectionEnabled() bool {
	store.mu.Lock()
	defer store.mu.Unlock()
	return !store.configuredTraceFailed
}

func (store *sessionRunStore) MarkTraceProjectionFailed() {
	store.mu.Lock()
	store.configuredTraceFailed = true
	store.mu.Unlock()
}

func (store *sessionRunStore) Close() error {
	return store.coordinator.runs.Close()
}

func (store *sessionRunStore) RunDir(runID string) string {
	return store.coordinator.projections.RunDir(runID)
}

func (store *sessionRunStore) TracePath(runID string) string {
	return store.coordinator.projections.TracePath(runID)
}

func (store *sessionRunStore) Manifest() session.Manifest {
	store.mu.Lock()
	defer store.mu.Unlock()
	return cloneManifest(store.manifest)
}

func (store *sessionRunStore) SetManifest(manifest session.Manifest) {
	store.mu.Lock()
	store.manifest = manifest
	store.mu.Unlock()
}

func (store *sessionRunStore) SetManifestObserver(observer func(session.Manifest)) {
	store.mu.Lock()
	store.onManifest = observer
	store.mu.Unlock()
}

var _ engine.DurableRunStore = (*sessionRunStore)(nil)
var _ interface{ RunDir(string) string } = (*sessionRunStore)(nil)
var _ interface{ TracePath(string) string } = (*sessionRunStore)(nil)
