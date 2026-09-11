package sessionstdio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/runstate"
	"github.com/ormasoftchile/yawr/runtime/pkg/session"
)

const (
	defaultGraphChunkBytes             = 64 << 10
	executionFrameProjectionChunkBytes = 8 << 20
	maxExecutionFrameProjectionBytes   = session.MaxStdioFrameProjectionBytes
)

type FrameStore interface {
	ReadFrameEvents(context.Context, string, int64) ([]session.Event, map[int64]session.Manifest, int64, error)
	ReadBlob(context.Context, string, string) (json.RawMessage, error)
}

type Projector struct {
	store              FrameStore
	chunkBytes         int
	graphSegmentFilter func(string, int64) bool
}

func (projector *Projector) WithGraphSegmentFilter(filter func(string, int64) bool) *Projector {
	projector.graphSegmentFilter = filter
	return projector
}

type GraphChunkPayload = session.GraphChunkPayload

func NewProjector(store FrameStore, chunkBytes int) *Projector {
	if chunkBytes < 1 {
		chunkBytes = defaultGraphChunkBytes
	}
	return &Projector{store: store, chunkBytes: chunkBytes}
}

func (projector *Projector) Frames(
	ctx context.Context,
	sessionID string,
	afterSequence int64,
) ([]session.StdioFrame, error) {
	frames := make([]session.StdioFrame, 0)
	_, err := projector.Project(ctx, sessionID, afterSequence, func(group []session.StdioFrame) error {
		frames = append(frames, group...)
		return nil
	})
	return frames, err
}

func (projector *Projector) Project(
	ctx context.Context,
	sessionID string,
	afterSequence int64,
	emit func([]session.StdioFrame) error,
) (int64, error) {
	if projector == nil || projector.store == nil {
		return afterSequence, errors.New("session stdio: frame store is required")
	}
	if sessionID == "" || afterSequence < 0 || emit == nil {
		return afterSequence, errors.New("session stdio: invalid replay position")
	}
	events, manifests, batchHeadSequence, err := projector.store.ReadFrameEvents(ctx, sessionID, afterSequence)
	if err != nil {
		return afterSequence, err
	}
	if afterSequence > batchHeadSequence {
		return afterSequence, errors.New("session stdio: replay position is not a committed journal sequence")
	}
	if afterSequence == batchHeadSequence {
		if len(events) != 0 {
			return afterSequence, errors.New("session stdio: frame batch follows the committed journal head")
		}
		return afterSequence, nil
	}
	if len(events) == 0 || events[len(events)-1].Sequence != batchHeadSequence {
		return afterSequence, errors.New("session stdio: frame batch does not reach the committed journal head")
	}
	expectedSequence := afterSequence + 1
	lastSequence := afterSequence
	for _, event := range events {
		if event.SessionID != sessionID || event.EventID == "" || event.Sequence != expectedSequence ||
			event.Kind == "" || !json.Valid(event.Payload) {
			return lastSequence, errors.New("session stdio: invalid journal event identity")
		}
		group, err := projector.projectEvent(ctx, event, manifests[event.Sequence])
		if err != nil {
			return lastSequence, fmt.Errorf("session stdio: project sequence %d: %w", event.Sequence, err)
		}
		if len(group) == 0 {
			return lastSequence, fmt.Errorf("session stdio: sequence %d has no frame projection", event.Sequence)
		}
		for index := range group {
			group[index].Version = session.StdioProtocolV1
			group[index].SessionID = sessionID
			group[index].SessionSequence = event.Sequence
			group[index].SequenceIndex = index
			group[index].SequenceCount = len(group)
			group[index].WriterEpoch = event.WriterEpoch
			group[index].FrameID = session.DigestJSON(struct {
				EventID string            `json:"event_id"`
				Type    session.FrameType `json:"type"`
				Index   int               `json:"index"`
			}{event.EventID, group[index].Type, index})
			if err := validateStdioFrameSize(group[index]); err != nil {
				return lastSequence, fmt.Errorf("session stdio: project sequence %d: %w", event.Sequence, err)
			}
		}
		if err := emit(group); err != nil {
			return lastSequence, err
		}
		lastSequence = event.Sequence
		expectedSequence++
	}
	return lastSequence, nil
}

func validateStdioFrameSize(frame session.StdioFrame) error {
	_, err := marshalStdioFrame(frame)
	return err
}

type createdPayload struct {
	Session session.SessionRecord    `json:"session"`
	Segment session.SegmentRecord    `json:"segment"`
	Attempt session.RunAttemptRecord `json:"attempt"`
}

type attemptPayload struct {
	RunID  string                `json:"run_id"`
	Status session.AttemptStatus `json:"status"`
}

type runPayload struct {
	RunID string `json:"run_id"`
}

type closedPayload struct {
	Status    session.Status `json:"status"`
	SegmentID string         `json:"segment_id,omitempty"`
	RunID     string         `json:"run_id,omitempty"`
}

type tracePayload struct {
	RunID         string `json:"run_id"`
	TraceHash     string `json:"trace_hash"`
	TraceSequence int64  `json:"trace_sequence"`
}

type revisionPayload struct {
	SegmentID     string `json:"segment_id"`
	RunID         string `json:"run_id"`
	GraphRevision int64  `json:"graph_revision"`
	PlanHash      string `json:"plan_hash"`
	GraphHash     string `json:"graph_hash"`
}

type executionPayload struct {
	RunID                  string                `json:"run_id"`
	Status                 session.AttemptStatus `json:"status"`
	ProjectionHash         string                `json:"projection_hash"`
	FrameProjectionHash    string                `json:"frame_projection_hash,omitempty"`
	CheckpointSequence     int64                 `json:"checkpoint_sequence"`
	CommittedTraceSequence int64                 `json:"committed_trace_sequence"`
}

type committedTransitionPayload struct {
	TransitionID  string                   `json:"transition_id"`
	TargetSegment session.SegmentRecord    `json:"target_segment"`
	TargetAttempt session.RunAttemptRecord `json:"target_attempt"`
}

func (projector *Projector) projectEvent(
	ctx context.Context,
	event session.Event,
	manifest session.Manifest,
) ([]session.StdioFrame, error) {
	switch event.Kind {
	case session.EventSessionCreated:
		var payload createdPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil ||
			payload.Session.SessionID != event.SessionID || payload.Segment.SegmentID == "" || payload.Attempt.RunID == "" {
			return nil, errors.New("invalid session creation payload")
		}
		started, _ := json.Marshal(payload.Session)
		snapshotFrame, _, err := projector.manifestFrame(event, manifest)
		if err != nil {
			return nil, err
		}
		segmentData, _ := json.Marshal(payload.Segment)
		attemptData, _ := json.Marshal(payload.Attempt)
		frames := []session.StdioFrame{
			{Type: session.FrameSessionStarted, Payload: started},
			snapshotFrame,
			{Type: session.FrameSegmentAdded, SegmentID: payload.Segment.SegmentID, Payload: segmentData},
		}
		graphFrames, err := projector.graphFrames(
			ctx, event.SessionID, payload.Segment.SegmentID, payload.Attempt.RunID,
			payload.Segment.GraphRevision, payload.Segment.GraphHash,
		)
		if err != nil {
			return nil, err
		}
		frames = append(frames, graphFrames...)
		frames = append(frames, session.StdioFrame{
			Type: session.FrameAttemptStarted, SegmentID: payload.Segment.SegmentID,
			RunID: payload.Attempt.RunID, Payload: attemptData,
		})
		return frames, nil
	case session.EventAttemptUpdated:
		var payload attemptPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil || payload.RunID == "" {
			return nil, errors.New("invalid attempt update payload")
		}
		attempt, found := manifest.Attempts[payload.RunID]
		if !found || attempt.Status != payload.Status || attempt.SegmentID == "" {
			return nil, errors.New("attempt update does not match historical manifest")
		}
		segment, found := manifest.Segments[attempt.SegmentID]
		if !found {
			return nil, errors.New("attempt update segment is unavailable")
		}
		frameType := session.FrameAttemptStarted
		switch payload.Status {
		case session.AttemptStatusStarting, session.AttemptStatusRunning:
		case session.AttemptStatusWaiting, session.AttemptStatusPausedAtBoundary, session.AttemptStatusHandoffPending:
			frameType = session.FrameAttemptPaused
		case session.AttemptStatusCompleted, session.AttemptStatusFailed,
			session.AttemptStatusCancelled, session.AttemptStatusIndeterminate:
			segmentData, err := json.Marshal(segment)
			if err != nil {
				return nil, err
			}
			frames := []session.StdioFrame{
				{
					Type: session.FrameAttemptFinished, SegmentID: attempt.SegmentID,
					RunID: payload.RunID, Payload: event.Payload,
				},
				{
					Type: session.FrameSegmentFinished, SegmentID: attempt.SegmentID,
					RunID: payload.RunID, Payload: segmentData,
				},
			}
			if manifest.Session.Status == session.StatusCancelled {
				sessionData, err := json.Marshal(manifest.Session)
				if err != nil {
					return nil, err
				}
				frames = append(frames, session.StdioFrame{
					Type: session.FrameSessionFinished, SegmentID: attempt.SegmentID,
					RunID: payload.RunID, Payload: sessionData,
				})
			}
			return frames, nil
		default:
			return nil, errors.New("invalid attempt update status")
		}
		return []session.StdioFrame{{
			Type: frameType, SegmentID: attempt.SegmentID, RunID: payload.RunID, Payload: event.Payload,
		}}, nil
	case session.EventTraceCommitted:
		var payload tracePayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil || payload.RunID == "" ||
			payload.TraceHash == "" || payload.TraceSequence < 1 {
			return nil, errors.New("invalid trace commit payload")
		}
		traceData, err := projector.store.ReadBlob(ctx, event.SessionID, payload.TraceHash)
		if err != nil {
			return nil, err
		}
		var traceEvent engine.Event
		if session.DigestBytes(traceData) != payload.TraceHash || json.Unmarshal(traceData, &traceEvent) != nil ||
			traceEvent.RunID != payload.RunID || traceEvent.Sequence != payload.TraceSequence ||
			traceEvent.EventID == "" || traceEvent.Kind == "" || session.ParseTime(traceEvent.Timestamp) != nil {
			return nil, errors.New("trace commit does not match immutable trace event")
		}
		if traceEvent.Payload["code_presentation"] != nil {
			traceEvent.Payload = runstate.PreviewEventPayload(traceEvent.Payload)
			traceData, err = json.Marshal(traceEvent)
			if err != nil {
				return nil, err
			}
		}
		return []session.StdioFrame{{Type: session.FrameRunEvent, RunID: payload.RunID, Payload: traceData}}, nil
	case session.EventSegmentRevised:
		var payload revisionPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil || payload.SegmentID == "" ||
			payload.GraphRevision < 1 || payload.PlanHash == "" || payload.GraphHash == "" {
			return nil, errors.New("invalid segment revision payload")
		}
		snapshotFrame, projected, err := projector.manifestFrame(event, manifest)
		if err != nil {
			return nil, err
		}
		attempt, found := projected.Attempts[payload.RunID]
		segment, segmentFound := projected.Segments[payload.SegmentID]
		if !found || !segmentFound || attempt.SegmentID != payload.SegmentID ||
			segment.GraphRevision != payload.GraphRevision || segment.PlanHash != payload.PlanHash ||
			segment.GraphHash != payload.GraphHash {
			return nil, errors.New("segment revision does not match historical manifest")
		}
		graphFrames, err := projector.graphFrames(
			ctx, event.SessionID, payload.SegmentID, payload.RunID, payload.GraphRevision, payload.GraphHash,
		)
		if err != nil {
			return nil, err
		}
		snapshotFrame.SegmentID = payload.SegmentID
		snapshotFrame.RunID = payload.RunID
		return append([]session.StdioFrame{snapshotFrame}, graphFrames...), nil
	case session.EventTransitionPrepared:
		return []session.StdioFrame{{Type: session.FrameTransitionPrepared, Payload: event.Payload}}, nil
	case session.EventTransitionCommitted:
		var payload committedTransitionPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil || payload.TransitionID == "" ||
			payload.TargetSegment.SegmentID == "" || payload.TargetSegment.GraphHash == "" ||
			payload.TargetAttempt.RunID == "" || payload.TargetAttempt.SegmentID != payload.TargetSegment.SegmentID {
			return nil, errors.New("invalid committed transition payload")
		}
		segmentData, _ := json.Marshal(payload.TargetSegment)
		attemptData, _ := json.Marshal(payload.TargetAttempt)
		frames := []session.StdioFrame{{Type: session.FrameTransitionCommitted, Payload: event.Payload}, {
			Type: session.FrameSegmentAdded, SegmentID: payload.TargetSegment.SegmentID,
			RunID: payload.TargetAttempt.RunID, Payload: segmentData,
		}}
		graphFrames, err := projector.graphFrames(
			ctx, event.SessionID, payload.TargetSegment.SegmentID, payload.TargetAttempt.RunID,
			payload.TargetSegment.GraphRevision, payload.TargetSegment.GraphHash,
		)
		if err != nil {
			return nil, err
		}
		frames = append(frames, graphFrames...)
		frames = append(frames, session.StdioFrame{
			Type: session.FrameAttemptStarted, SegmentID: payload.TargetSegment.SegmentID,
			RunID: payload.TargetAttempt.RunID, Payload: attemptData,
		})
		return frames, nil
	case session.EventSessionClosed:
		var payload closedPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil || payload.Status == "" ||
			(payload.RunID == "") != (payload.SegmentID == "") {
			return nil, errors.New("invalid session close payload")
		}
		frames := make([]session.StdioFrame, 0, 3)
		if payload.RunID != "" {
			attempt, found := manifest.Attempts[payload.RunID]
			segment, segmentFound := manifest.Segments[payload.SegmentID]
			if !found || !segmentFound || attempt.SegmentID != payload.SegmentID ||
				attempt.Status != session.AttemptStatusCancelled || segment.Status != session.SegmentStatusCancelled {
				return nil, errors.New("session close does not match historical terminal topology")
			}
			attemptData, _ := json.Marshal(attempt)
			segmentData, _ := json.Marshal(segment)
			frames = append(frames,
				session.StdioFrame{Type: session.FrameAttemptFinished, SegmentID: payload.SegmentID, RunID: payload.RunID, Payload: attemptData},
				session.StdioFrame{Type: session.FrameSegmentFinished, SegmentID: payload.SegmentID, RunID: payload.RunID, Payload: segmentData},
			)
		}
		return append(frames, session.StdioFrame{
			Type: session.FrameSessionFinished, SegmentID: payload.SegmentID, RunID: payload.RunID, Payload: event.Payload,
		}), nil
	case session.EventExecutionCommitted:
		var payload executionPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil || payload.RunID == "" ||
			payload.ProjectionHash == "" || payload.CheckpointSequence < 0 || payload.CommittedTraceSequence < 0 {
			return nil, errors.New("invalid execution commit payload")
		}
		var state projectedRunState
		var err error
		if payload.FrameProjectionHash != "" {
			state, err = projector.readFrameProjectionState(
				ctx, event.SessionID, payload.RunID, payload.FrameProjectionHash,
				payload.CheckpointSequence, payload.CommittedTraceSequence,
			)
		} else {
			state, err = projector.readProjectionState(
				ctx, event.SessionID, payload.RunID, payload.ProjectionHash,
				payload.CheckpointSequence, payload.CommittedTraceSequence,
			)
		}
		if err != nil {
			return nil, err
		}
		snapshotFrame, manifest, err := projector.manifestFrame(event, manifest)
		if err != nil {
			return nil, err
		}
		attempt, found := manifest.Attempts[payload.RunID]
		if !found || attempt.Status != payload.Status || attempt.SegmentID == "" {
			return nil, errors.New("execution commit does not match historical attempt")
		}
		segment, found := manifest.Segments[attempt.SegmentID]
		if !found {
			return nil, errors.New("execution commit segment is unavailable")
		}
		snapshotFrame.RunID = payload.RunID
		frames := []session.StdioFrame{snapshotFrame}
		for _, traceEvent := range state.PendingTraceEvents {
			if traceEvent.Payload["code_presentation"] != nil {
				traceEvent.Payload = runstate.PreviewEventPayload(traceEvent.Payload)
			}
			encoded, err := json.Marshal(traceEvent)
			if err != nil {
				return nil, errors.New("invalid projected run event")
			}
			frames = append(frames, session.StdioFrame{
				Type: session.FrameRunEvent, SegmentID: attempt.SegmentID,
				RunID: payload.RunID, Payload: encoded,
			})
		}
		turnIDs := make([]string, 0, len(state.Interactions))
		for turnID := range state.Interactions {
			turnIDs = append(turnIDs, turnID)
		}
		sort.Strings(turnIDs)
		for _, turnID := range turnIDs {
			interaction := state.Interactions[turnID]
			frameType := session.FrameInteractionPending
			if interaction.Status == "answered" {
				frameType = session.FrameInteractionResolved
			}
			frames = append(frames, session.StdioFrame{
				Type: frameType, SegmentID: attempt.SegmentID,
				RunID: payload.RunID, Payload: interaction.Raw,
			})
		}
		switch payload.Status {
		case session.AttemptStatusWaiting, session.AttemptStatusPausedAtBoundary, session.AttemptStatusHandoffPending:
			frames = append(frames, session.StdioFrame{
				Type: session.FrameAttemptPaused, SegmentID: attempt.SegmentID,
				RunID: payload.RunID, Payload: event.Payload,
			})
		case session.AttemptStatusCompleted, session.AttemptStatusFailed,
			session.AttemptStatusCancelled, session.AttemptStatusIndeterminate:
			segmentData, err := json.Marshal(segment)
			if err != nil {
				return nil, err
			}
			frames = append(frames,
				session.StdioFrame{
					Type: session.FrameAttemptFinished, SegmentID: attempt.SegmentID,
					RunID: payload.RunID, Payload: event.Payload,
				},
				session.StdioFrame{
					Type: session.FrameSegmentFinished, SegmentID: attempt.SegmentID,
					RunID: payload.RunID, Payload: segmentData,
				},
			)
			if manifest.Session.Status == session.StatusCancelled {
				sessionData, err := json.Marshal(manifest.Session)
				if err != nil {
					return nil, err
				}
				frames = append(frames, session.StdioFrame{
					Type: session.FrameSessionFinished, SegmentID: attempt.SegmentID,
					RunID: payload.RunID, Payload: sessionData,
				})
			}
		}
		return frames, nil
	case session.EventTransitionAborted:
		snapshotFrame, _, err := projector.manifestFrame(event, manifest)
		if err != nil {
			return nil, err
		}
		return []session.StdioFrame{snapshotFrame}, nil
	case session.EventSessionPaused:
		segmentID, runID, err := projector.activeAttemptIdentity(event, manifest)
		if err != nil {
			return nil, err
		}
		return []session.StdioFrame{{
			Type: session.FrameAttemptPaused, SegmentID: segmentID, RunID: runID, Payload: event.Payload,
		}}, nil
	case session.EventSessionResumed:
		segmentID, runID, err := projector.activeAttemptIdentity(event, manifest)
		if err != nil {
			return nil, err
		}
		return []session.StdioFrame{{
			Type: session.FrameAttemptStarted, SegmentID: segmentID, RunID: runID, Payload: event.Payload,
		}}, nil
	case session.EventSessionDetached:
		snapshotFrame, _, err := projector.manifestFrame(event, manifest)
		if err != nil {
			return nil, err
		}
		return []session.StdioFrame{snapshotFrame}, nil
	default:
		return nil, fmt.Errorf("unsupported journal event kind %q", event.Kind)
	}
}

func (projector *Projector) activeAttemptIdentity(
	event session.Event,
	manifest session.Manifest,
) (string, string, error) {
	var payload runPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil || payload.RunID == "" {
		return "", "", errors.New("invalid active attempt payload")
	}
	attempt, found := manifest.Attempts[payload.RunID]
	if !found || manifest.Session.ActiveRunID != payload.RunID ||
		manifest.Session.ActiveSegmentID != attempt.SegmentID || attempt.SegmentID == "" {
		return "", "", errors.New("active attempt payload does not match historical manifest")
	}
	return attempt.SegmentID, payload.RunID, nil
}

func (projector *Projector) manifestFrame(
	event session.Event,
	manifest session.Manifest,
) (session.StdioFrame, session.Manifest, error) {
	if manifest.SchemaVersion != session.ManifestSchemaV1 || manifest.Session.SessionID != event.SessionID ||
		manifest.Session.Sequence != event.Sequence {
		return session.StdioFrame{}, session.Manifest{}, errors.New("historical manifest does not match journal event")
	}
	encoded, err := json.Marshal(session.ProjectManifest(manifest))
	if err != nil {
		return session.StdioFrame{}, session.Manifest{}, err
	}
	return session.StdioFrame{Type: session.FrameSessionSnapshot, Payload: encoded}, manifest, nil
}

type projectionRoot struct {
	SchemaVersion      string           `json:"schema_version"`
	RunID              string           `json:"run_id"`
	CheckpointSequence int64            `json:"checkpoint_sequence"`
	Files              []projectionFile `json:"files"`
}

type projectionFile struct {
	Path         string   `json:"path"`
	Digest       string   `json:"digest"`
	Size         int64    `json:"size"`
	ChunkDigests []string `json:"chunk_digests"`
}

type projectionChunk struct {
	SchemaVersion string `json:"schema_version"`
	Data          []byte `json:"data"`
}

type projectedInteraction struct {
	TurnID string
	Status string
	Raw    json.RawMessage
}

type projectedRunState struct {
	Interactions       map[string]projectedInteraction
	PendingTraceEvents []engine.Event
}

func (projector *Projector) readFrameProjectionState(
	ctx context.Context,
	sessionID string,
	runID string,
	projectionHash string,
	checkpointSequence int64,
	committedTraceSequence int64,
) (projectedRunState, error) {
	data, err := projector.store.ReadBlob(ctx, sessionID, projectionHash)
	if err != nil {
		return projectedRunState{}, err
	}
	if session.DigestBytes(data) != projectionHash {
		return projectedRunState{}, errors.New("execution frame projection digest mismatch")
	}
	var projection session.ExecutionFrameProjection
	if err := json.Unmarshal(data, &projection); err != nil ||
		projection.SchemaVersion != session.ExecutionFrameProjectionSchemaV1 || projection.RunID != runID ||
		projection.CheckpointSequence != checkpointSequence ||
		projection.CommittedTraceSequence != committedTraceSequence || projection.ContentSize < 1 ||
		projection.ContentSize > maxExecutionFrameProjectionBytes || len(projection.ChunkDigests) == 0 {
		return projectedRunState{}, errors.New("invalid execution frame projection")
	}
	expectedChunkCount := int((projection.ContentSize + executionFrameProjectionChunkBytes - 1) /
		executionFrameProjectionChunkBytes)
	if len(projection.ChunkDigests) != expectedChunkCount {
		return projectedRunState{}, errors.New("execution frame projection chunk count mismatch")
	}
	content := make([]byte, 0, projection.ContentSize)
	for index, digest := range projection.ChunkDigests {
		chunkData, err := projector.store.ReadBlob(ctx, sessionID, digest)
		if err != nil || session.DigestBytes(chunkData) != digest {
			return projectedRunState{}, errors.New("execution frame projection chunk digest mismatch")
		}
		var chunk session.ExecutionFrameProjectionChunk
		expectedChunkSize := executionFrameProjectionChunkBytes
		if index == len(projection.ChunkDigests)-1 {
			expectedChunkSize = int(projection.ContentSize) - len(content)
		}
		if err := json.Unmarshal(chunkData, &chunk); err != nil ||
			chunk.SchemaVersion != session.ExecutionFrameProjectionChunkSchemaV1 ||
			len(chunk.Data) != expectedChunkSize ||
			int64(len(chunk.Data)) > projection.ContentSize-int64(len(content)) {
			return projectedRunState{}, errors.New("invalid execution frame projection chunk")
		}
		content = append(content, chunk.Data...)
	}
	if int64(len(content)) != projection.ContentSize || session.DigestBytes(content) != projection.ContentDigest {
		return projectedRunState{}, errors.New("execution frame projection content digest mismatch")
	}
	var projected struct {
		Interactions       map[string]json.RawMessage `json:"interactions"`
		PendingTraceEvents []engine.Event             `json:"pending_trace_events"`
	}
	if err := json.Unmarshal(content, &projected); err != nil {
		return projectedRunState{}, errors.New("invalid execution frame projection content")
	}
	return validateProjectedRunState(
		runID, committedTraceSequence, projected.Interactions, projected.PendingTraceEvents,
	)
}

func (projector *Projector) readProjectionState(
	ctx context.Context,
	sessionID string,
	runID string,
	projectionHash string,
	checkpointSequence int64,
	committedTraceSequence int64,
) (projectedRunState, error) {
	encodedRoot, err := projector.store.ReadBlob(ctx, sessionID, projectionHash)
	if err != nil {
		return projectedRunState{}, err
	}
	if session.DigestBytes(encodedRoot) != projectionHash {
		return projectedRunState{}, errors.New("run projection digest mismatch")
	}
	var root projectionRoot
	if err := json.Unmarshal(encodedRoot, &root); err != nil || root.SchemaVersion != session.RunProjectionSchemaV1 ||
		root.RunID != runID || root.CheckpointSequence != checkpointSequence {
		return projectedRunState{}, errors.New("invalid run projection")
	}
	var snapshotFile *projectionFile
	for index := range root.Files {
		if strings.HasPrefix(root.Files[index].Path, "snapshots/") {
			if snapshotFile != nil {
				return projectedRunState{}, errors.New("run projection has multiple current snapshots")
			}
			snapshotFile = &root.Files[index]
		}
	}
	if snapshotFile == nil || snapshotFile.Size < 0 || snapshotFile.Size > 64<<20 ||
		len(snapshotFile.ChunkDigests) == 0 {
		return projectedRunState{}, errors.New("run projection has no current snapshot")
	}
	snapshot := make([]byte, 0, snapshotFile.Size)
	for _, digest := range snapshotFile.ChunkDigests {
		encodedChunk, err := projector.store.ReadBlob(ctx, sessionID, digest)
		if err != nil {
			return projectedRunState{}, err
		}
		if session.DigestBytes(encodedChunk) != digest {
			return projectedRunState{}, errors.New("run projection chunk digest mismatch")
		}
		var chunk projectionChunk
		if err := json.Unmarshal(encodedChunk, &chunk); err != nil ||
			chunk.SchemaVersion != "yawr.session-run-projection-chunk/v1" {
			return projectedRunState{}, errors.New("invalid run projection chunk")
		}
		snapshot = append(snapshot, chunk.Data...)
		if int64(len(snapshot)) > snapshotFile.Size {
			return projectedRunState{}, errors.New("run projection snapshot exceeds declared size")
		}
	}
	if int64(len(snapshot)) != snapshotFile.Size || session.DigestBytes(snapshot) != snapshotFile.Digest {
		return projectedRunState{}, errors.New("run projection snapshot digest mismatch")
	}
	var state struct {
		RunID              string                     `json:"RunID"`
		Interactions       map[string]json.RawMessage `json:"Interactions"`
		PendingTraceEvents []engine.Event             `json:"pending_trace_events"`
	}
	if err := json.Unmarshal(snapshot, &state); err != nil || state.RunID != runID {
		return projectedRunState{}, errors.New("invalid run state projection")
	}
	return validateProjectedRunState(runID, committedTraceSequence, state.Interactions, state.PendingTraceEvents)
}

func validateProjectedRunState(
	runID string,
	committedTraceSequence int64,
	interactions map[string]json.RawMessage,
	pendingTraceEvents []engine.Event,
) (projectedRunState, error) {
	result := projectedRunState{
		Interactions:       make(map[string]projectedInteraction, len(interactions)),
		PendingTraceEvents: append([]engine.Event(nil), pendingTraceEvents...),
	}
	for turnID, raw := range interactions {
		var header struct {
			TurnID string `json:"turn_id"`
			StepID string `json:"step_id"`
			Kind   string `json:"kind"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal(raw, &header); err != nil || header.TurnID != turnID ||
			header.StepID == "" || header.Kind == "" ||
			(header.Status != "pending" && header.Status != "answered") {
			return projectedRunState{}, errors.New("invalid projected interaction")
		}
		result.Interactions[turnID] = projectedInteraction{
			TurnID: turnID, Status: header.Status, Raw: append(json.RawMessage(nil), raw...),
		}
	}
	previousTraceSequence := int64(0)
	for index, traceEvent := range result.PendingTraceEvents {
		if traceEvent.RunID != runID || traceEvent.EventID == "" || traceEvent.Kind == "" ||
			traceEvent.Sequence < 1 || traceEvent.Sequence > committedTraceSequence ||
			session.ParseTime(traceEvent.Timestamp) != nil ||
			index > 0 && traceEvent.Sequence != previousTraceSequence+1 {
			return projectedRunState{}, errors.New("invalid projected run event")
		}
		previousTraceSequence = traceEvent.Sequence
	}
	if len(result.PendingTraceEvents) > 0 && previousTraceSequence != committedTraceSequence {
		return projectedRunState{}, errors.New("projected run event batch does not reach committed trace sequence")
	}
	return result, nil
}

func (projector *Projector) graphFrames(
	ctx context.Context,
	sessionID string,
	segmentID string,
	runID string,
	revision int64,
	digest string,
) ([]session.StdioFrame, error) {
	if projector.graphSegmentFilter != nil && !projector.graphSegmentFilter(segmentID, revision) {
		payload, err := json.Marshal(session.GraphAvailablePayload{
			GraphRevision: revision, WholeBlobHash: digest,
		})
		if err != nil {
			return nil, err
		}
		return []session.StdioFrame{{
			Type: session.FrameSegmentGraphAvailable, SegmentID: segmentID, RunID: runID, Payload: payload,
		}}, nil
	}
	graph, err := projector.store.ReadBlob(ctx, sessionID, digest)
	if err != nil {
		return nil, err
	}
	if !json.Valid(graph) || session.DigestBytes(graph) != digest {
		return nil, errors.New("graph blob digest mismatch")
	}
	chunkCount := (len(graph) + projector.chunkBytes - 1) / projector.chunkBytes
	if chunkCount == 0 {
		chunkCount = 1
	}
	frames := make([]session.StdioFrame, 0, chunkCount)
	for chunkIndex := 0; chunkIndex < chunkCount; chunkIndex++ {
		start := chunkIndex * projector.chunkBytes
		end := start + projector.chunkBytes
		if end > len(graph) {
			end = len(graph)
		}
		payload, err := json.Marshal(session.GraphChunkPayload{
			GraphRevision: revision, ChunkIndex: chunkIndex, ChunkCount: chunkCount,
			WholeBlobHash: digest, Data: append([]byte(nil), graph[start:end]...),
		})
		if err != nil {
			return nil, err
		}
		frames = append(frames, session.StdioFrame{
			Type: session.FrameSegmentGraph, SegmentID: segmentID, RunID: runID, Payload: payload,
		})
	}
	return frames, nil
}
