package sessionstore

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/sensitive"
	"github.com/ormasoftchile/yawr/runtime/pkg/session"
)

const (
	maxJournalEventBytes      = 4 << 20
	maxBlobBytes              = 64 << 20
	maxProjectionBytes        = 512 << 20
	frameProjectionChunkBytes = 8 << 20
	maxJournalEvents          = 1_000_000
	maxProjectionBlobs        = 20_000
)

const journalHeadSchemaV1 = "yawr.investigation-session-head/v1"

var errFileExceedsLimit = errors.New("sessionstore: file exceeds limit")

type journalHead struct {
	SchemaVersion string `json:"schema_version"`
	SessionID     string `json:"session_id"`
	Sequence      int64  `json:"sequence"`
	EventDigest   string `json:"event_digest"`
	WriterEpoch   uint64 `json:"writer_epoch"`
}

type DirStore struct {
	baseDir       string
	mutationMu    sync.RWMutex
	commitMu      sync.Mutex
	mu            sync.Mutex
	leases        map[*fileSessionLease]struct{}
	leaseEpochs   map[string]uint64
	updates       map[string]chan struct{}
	closed        bool
	frameMu       sync.Mutex
	frameCaches   map[string]*frameEventCache
	journalMu     sync.Mutex
	journalCaches map[string]*journalStateCache
	journalScans  atomic.Uint64
}

type journalStateCache struct {
	writerEpoch     uint64
	events          []session.Event
	rebuild         *manifestRebuildState
	committedOffset int64
}

type frameEventCache struct {
	baseSequence            int64
	headSequence            int64
	events                  []session.Event
	manifests               map[int64]session.Manifest
	pendingManifestSequence int64
}

func NewDirStore(baseDir string) *DirStore {
	if baseDir == "" {
		baseDir = ".runbook/sessions"
	}
	return &DirStore{
		baseDir: baseDir, leases: make(map[*fileSessionLease]struct{}),
		leaseEpochs: make(map[string]uint64), updates: make(map[string]chan struct{}),
		frameCaches: make(map[string]*frameEventCache), journalCaches: make(map[string]*journalStateCache),
	}
}

func (store *DirStore) SessionUpdates(sessionID string) <-chan struct{} {
	store.mu.Lock()
	defer store.mu.Unlock()
	if updates := store.updates[sessionID]; updates != nil {
		return updates
	}
	updates := make(chan struct{}, 1)
	if store.closed {
		close(updates)
		return updates
	}
	store.updates[sessionID] = updates
	return updates
}

func (store *DirStore) notifySessionUpdate(sessionID string) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return
	}
	if updates := store.updates[sessionID]; updates != nil {
		select {
		case updates <- struct{}{}:
		default:
		}
	}
}

func (store *DirStore) SessionDir(sessionID string) string {
	return filepath.Join(store.baseDir, sessionID)
}

func (store *DirStore) eventsPath(sessionID string) string {
	return filepath.Join(store.SessionDir(sessionID), "events.jsonl")
}

func (store *DirStore) manifestPath(sessionID string) string {
	return filepath.Join(store.SessionDir(sessionID), "manifest.json")
}

func (store *DirStore) headsDir(sessionID string) string {
	return filepath.Join(store.SessionDir(sessionID), "heads")
}

func (store *DirStore) headPath(sessionID string, sequence int64) string {
	return filepath.Join(store.headsDir(sessionID), fmt.Sprintf("head-%020d.json", sequence))
}

func (store *DirStore) blobPath(sessionID, digest string) string {
	return filepath.Join(store.SessionDir(sessionID), "blobs", strings.TrimPrefix(digest, "sha256:")+".json")
}

type sessionCreatedPayload struct {
	Identity session.CreationIdentity `json:"identity"`
	Session  session.SessionRecord    `json:"session"`
	Segment  session.SegmentRecord    `json:"segment"`
	Attempt  session.RunAttemptRecord `json:"attempt"`
}

type sessionClosedPayload struct {
	Status    session.Status `json:"status"`
	SegmentID string         `json:"segment_id,omitempty"`
	RunID     string         `json:"run_id,omitempty"`
}

type sessionPausedPayload struct {
	RunID  string `json:"run_id"`
	Reason string `json:"reason"`
}

type sessionResumedPayload struct {
	RunID string `json:"run_id"`
}

type sessionDetachedPayload struct {
	Reason string `json:"reason"`
}

type attemptUpdatedPayload struct {
	RunID  string                `json:"run_id"`
	Status session.AttemptStatus `json:"status"`
}

type executionCommittedPayload struct {
	RunID                  string                              `json:"run_id"`
	Status                 session.AttemptStatus               `json:"status"`
	MutationHash           string                              `json:"mutation_hash"`
	PreviousMutationHash   string                              `json:"previous_mutation_hash,omitempty"`
	ProjectionHash         string                              `json:"projection_hash"`
	FrameProjectionHash    string                              `json:"frame_projection_hash,omitempty"`
	CheckpointSequence     int64                               `json:"checkpoint_sequence"`
	CommittedTraceSequence int64                               `json:"committed_trace_sequence"`
	Occurrences            map[string]session.OccurrenceRecord `json:"occurrences,omitempty"`
}

type traceCommittedPayload struct {
	RunID         string                    `json:"run_id"`
	TraceHash     string                    `json:"trace_hash"`
	TraceSequence int64                     `json:"trace_sequence"`
	Occurrence    *session.OccurrenceRecord `json:"occurrence,omitempty"`
}

type segmentRevisedPayload struct {
	SegmentID              string                               `json:"segment_id"`
	RunID                  string                               `json:"run_id"`
	ExecutableRevision     int64                                `json:"executable_revision"`
	GraphRevision          int64                                `json:"graph_revision"`
	PlanHash               string                               `json:"plan_hash"`
	ExecutableSnapshotHash string                               `json:"executable_snapshot_hash"`
	GraphHash              string                               `json:"graph_hash"`
	Resolution             engine.DynamicIncludeResolutionState `json:"resolution"`
	Dispatch               engine.DispatchState                 `json:"dispatch"`
}

type transitionPreparedPayload struct {
	Transition    session.TransitionRecord `json:"transition"`
	TargetSegment session.SegmentRecord    `json:"target_segment"`
	TargetAttempt session.RunAttemptRecord `json:"target_attempt"`
}

type transitionCommittedPayload struct {
	TransitionID  string                   `json:"transition_id"`
	TargetSegment session.SegmentRecord    `json:"target_segment"`
	TargetAttempt session.RunAttemptRecord `json:"target_attempt"`
}

type transitionAbortedPayload struct {
	TransitionID string `json:"transition_id"`
	Reason       string `json:"reason"`
}

type creationReservation struct {
	SchemaVersion  string `json:"schema_version"`
	CommandID      string `json:"command_id"`
	SessionID      string `json:"session_id"`
	CreationDigest string `json:"creation_digest"`
}

const creationReservationSchemaV1 = "yawr.investigation-session-creation/v1"

func (store *DirStore) CreateSession(
	ctx context.Context,
	writerEpoch uint64,
	request session.CreateRequest,
) (session.Manifest, error) {
	store.commitMu.Lock()
	defer store.commitMu.Unlock()
	store.mutationMu.RLock()
	defer store.mutationMu.RUnlock()
	if err := ctx.Err(); err != nil {
		return session.Manifest{}, err
	}
	if err := validateUUID("session id", request.SessionID); err != nil {
		return session.Manifest{}, err
	}
	if err := validateUUID("command id", request.CommandID); err != nil {
		return session.Manifest{}, err
	}
	if err := store.validateWriterEpoch(request.SessionID, writerEpoch); err != nil {
		return session.Manifest{}, err
	}
	if events, err := store.loadEvents(ctx, request.SessionID); err == nil {
		manifest, rebuildErr := store.manifestForEvents(request.SessionID, events)
		if rebuildErr != nil {
			return session.Manifest{}, rebuildErr
		}
		if manifest.Session.CreationCommandID == request.CommandID && manifest.Session.CreationDigest == request.EntryDigest {
			return manifest, nil
		}
		return session.Manifest{}, session.ErrCreationConflict
	} else if !errors.Is(err, os.ErrNotExist) {
		return session.Manifest{}, err
	}
	request = normalizeCreateRequest(request)
	if err := validateCreateRequest(request); err != nil {
		return session.Manifest{}, err
	}
	if err := store.reserveCreationCommand(request.CommandID, request.SessionID, request.EntryDigest); err != nil {
		return session.Manifest{}, err
	}
	for _, blob := range request.Blobs {
		if err := store.writeBlob(request.SessionID, blob); err != nil {
			return session.Manifest{}, err
		}
	}
	for _, digest := range []string{request.Segment.GraphHash, request.Segment.ExecutableSnapshotHash} {
		if _, err := store.ReadBlob(ctx, request.SessionID, digest); err != nil {
			return session.Manifest{}, fmt.Errorf("sessionstore: referenced blob %s: %w", digest, err)
		}
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	record := request.Session
	record.SessionID = request.SessionID
	record.RootSegmentID = request.Segment.SegmentID
	record.ActiveSegmentID = request.Segment.SegmentID
	record.ActiveRunID = request.Attempt.RunID
	record.Sequence = 1
	record.ManifestRevision = 1
	record.WriterEpoch = writerEpoch
	record.CreationCommandID = request.CommandID
	record.CreationDigest = request.EntryDigest
	record.CreatedAt = now
	record.UpdatedAt = now
	segment := request.Segment
	attempt := request.Attempt
	attempt.StartedAt = now
	attempt.UpdatedAt = now
	payload, err := json.Marshal(sessionCreatedPayload{
		Identity: request.Identity, Session: record, Segment: segment, Attempt: attempt,
	})
	if err != nil {
		return session.Manifest{}, err
	}
	event, err := newEvent(request.SessionID, writerEpoch, 1, "", request.CommandID, request.EntryDigest, session.EventSessionCreated, now, payload)
	if err != nil {
		return session.Manifest{}, err
	}
	if err := store.appendEvent(event); err != nil {
		return session.Manifest{}, err
	}
	manifest, err := rebuildManifest([]session.Event{event})
	if err != nil {
		return session.Manifest{}, err
	}
	if err := store.writeManifest(manifest); err != nil {
		return manifest, fmt.Errorf("%w: %v", session.ErrProjection, err)
	}
	return manifest, nil
}

func normalizeCreateRequest(request session.CreateRequest) session.CreateRequest {
	request.Segment.AttemptRunIDs = append([]string(nil), request.Segment.AttemptRunIDs...)
	if request.Segment.EntrySelector.Step == "" {
		request.Segment.EntrySelector.Step = "$entry"
	}
	if request.Segment.ExecutableRevision == 0 {
		request.Segment.ExecutableRevision = 1
	}
	if request.Segment.GraphRevision == 0 {
		request.Segment.GraphRevision = 1
	}
	if request.Attempt.PlanHash == "" {
		request.Attempt.PlanHash = request.Segment.PlanHash
	}
	return request
}

func (store *DirStore) reserveCreationCommand(commandID, sessionID, digest string) error {
	directory := filepath.Join(store.baseDir, ".creation-commands")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	reservation := creationReservation{
		SchemaVersion: creationReservationSchemaV1, CommandID: commandID,
		SessionID: sessionID, CreationDigest: digest,
	}
	encoded, err := json.Marshal(reservation)
	if err != nil {
		return err
	}
	path := filepath.Join(directory, commandID+".json")
	if err := publishImmutableFile(path, encoded, 0o600, 4096); err != nil {
		if errors.Is(err, errImmutableFileConflict) {
			return session.ErrCreationConflict
		}
		return err
	}
	return nil
}

func (store *DirStore) validateCreationReservation(event session.Event) error {
	path := filepath.Join(store.baseDir, ".creation-commands", event.CommandID+".json")
	data, err := readFileBounded(path, 4096)
	if err != nil {
		return fmt.Errorf("sessionstore: creation reservation: %w", err)
	}
	var reservation creationReservation
	if err := decodeStrictJSON(data, &reservation); err != nil ||
		reservation.SchemaVersion != creationReservationSchemaV1 ||
		reservation.CommandID != event.CommandID || reservation.SessionID != event.SessionID ||
		reservation.CreationDigest != event.CommandDigest {
		return errors.New("sessionstore: journal conflicts with creation reservation")
	}
	return nil
}

func (store *DirStore) CloseSession(ctx context.Context, request session.CloseRequest) (session.Manifest, error) {
	store.commitMu.Lock()
	defer store.commitMu.Unlock()
	store.mutationMu.RLock()
	defer store.mutationMu.RUnlock()
	if err := ctx.Err(); err != nil {
		return session.Manifest{}, err
	}
	if err := validateUUID("session id", request.SessionID); err != nil {
		return session.Manifest{}, err
	}
	if err := validateUUID("command id", request.CommandID); err != nil {
		return session.Manifest{}, err
	}
	if !isCloseStatus(request.Status) {
		return session.Manifest{}, fmt.Errorf("sessionstore: unsupported close status %q", request.Status)
	}
	if err := store.validateWriterEpoch(request.SessionID, request.WriterEpoch); err != nil {
		return session.Manifest{}, err
	}
	events, err := store.loadEvents(ctx, request.SessionID)
	if err != nil {
		return session.Manifest{}, err
	}
	manifest, err := store.manifestForEvents(request.SessionID, events)
	if err != nil {
		return session.Manifest{}, err
	}
	commandDigest := session.CloseDigest(request.Status)
	if request.ClientCommandDigest != "" && !validDigest(request.ClientCommandDigest) {
		return session.Manifest{}, errors.New("sessionstore: invalid client command digest")
	}
	if receipt, found := manifest.AcceptedCommands[request.CommandID]; found {
		if receipt.CommandDigest != commandDigest || receipt.ClientCommandDigest != request.ClientCommandDigest ||
			receipt.EventKind != session.EventSessionClosed {
			return session.Manifest{}, session.ErrCommandConflict
		}
		return manifest, nil
	}
	if isTerminalSessionStatus(manifest.Session.Status) {
		return session.Manifest{}, session.ErrSessionClosed
	}
	if activeRunID := manifest.Session.ActiveRunID; activeRunID != "" {
		attempt := manifest.Attempts[activeRunID]
		if !isTerminalAttemptStatus(attempt.Status) &&
			!(manifest.Session.Status == session.StatusPaused && isPausedAttemptStatus(attempt.Status)) {
			return session.Manifest{}, session.ErrAttemptActive
		}
	}
	if request.ExpectedSequence != manifest.Session.Sequence {
		return session.Manifest{}, session.ErrSequenceConflict
	}
	closePayload := sessionClosedPayload{Status: request.Status}
	if activeRunID := manifest.Session.ActiveRunID; activeRunID != "" &&
		isPausedAttemptStatus(manifest.Attempts[activeRunID].Status) {
		closePayload.SegmentID = manifest.Session.ActiveSegmentID
		closePayload.RunID = activeRunID
	}
	payload, err := json.Marshal(closePayload)
	if err != nil {
		return session.Manifest{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	previousDigest := events[len(events)-1].Digest
	event, err := newEventWithClientCommand(
		request.SessionID, request.WriterEpoch, manifest.Session.Sequence+1, previousDigest,
		request.CommandID, commandDigest, request.ClientCommandDigest, session.EventSessionClosed, now, payload,
	)
	if err != nil {
		return session.Manifest{}, err
	}
	if err := store.appendEvent(event); err != nil {
		return session.Manifest{}, err
	}
	events = append(events, event)
	manifest, err = store.manifestForEvents(request.SessionID, events)
	if err != nil {
		return session.Manifest{}, err
	}
	if err := store.writeManifest(manifest); err != nil {
		return manifest, fmt.Errorf("%w: %v", session.ErrProjection, err)
	}
	return manifest, nil
}

func (store *DirStore) PauseSession(ctx context.Context, request session.PauseRequest) (session.Manifest, error) {
	store.commitMu.Lock()
	defer store.commitMu.Unlock()
	store.mutationMu.RLock()
	defer store.mutationMu.RUnlock()
	if err := ctx.Err(); err != nil {
		return session.Manifest{}, err
	}
	if err := validateUUID("session id", request.SessionID); err != nil {
		return session.Manifest{}, err
	}
	if err := validateUUID("command id", request.CommandID); err != nil {
		return session.Manifest{}, err
	}
	if request.Reason == "" || len(request.Reason) > 1024 || strings.ContainsAny(request.Reason, "\r\n\x00") {
		return session.Manifest{}, errors.New("sessionstore: invalid pause reason")
	}
	if err := store.validateWriterEpoch(request.SessionID, request.WriterEpoch); err != nil {
		return session.Manifest{}, err
	}
	events, err := store.loadEvents(ctx, request.SessionID)
	if err != nil {
		return session.Manifest{}, err
	}
	manifest, err := store.manifestForEvents(request.SessionID, events)
	if err != nil {
		return session.Manifest{}, err
	}
	runID := manifest.Session.ActiveRunID
	commandDigest := session.PauseDigest(runID, request.Reason)
	if request.ClientCommandDigest != "" && !validDigest(request.ClientCommandDigest) {
		return session.Manifest{}, errors.New("sessionstore: invalid client command digest")
	}
	if receipt, found := manifest.AcceptedCommands[request.CommandID]; found {
		if receipt.CommandDigest != commandDigest || receipt.ClientCommandDigest != request.ClientCommandDigest ||
			receipt.EventKind != session.EventSessionPaused {
			return session.Manifest{}, session.ErrCommandConflict
		}
		return manifest, nil
	}
	if isTerminalSessionStatus(manifest.Session.Status) {
		return session.Manifest{}, session.ErrSessionClosed
	}
	if runID == "" {
		return session.Manifest{}, errors.New("sessionstore: session has no active attempt to pause")
	}
	attempt := manifest.Attempts[runID]
	if attempt.Status != session.AttemptStatusPausedAtBoundary && attempt.Status != session.AttemptStatusHandoffPending {
		return session.Manifest{}, fmt.Errorf("sessionstore: attempt status %q cannot pause", attempt.Status)
	}
	if request.ExpectedSequence != manifest.Session.Sequence {
		return session.Manifest{}, session.ErrSequenceConflict
	}
	payload, err := json.Marshal(sessionPausedPayload{RunID: runID, Reason: request.Reason})
	if err != nil {
		return session.Manifest{}, err
	}
	event, err := newEventWithClientCommand(
		request.SessionID, request.WriterEpoch, manifest.Session.Sequence+1, events[len(events)-1].Digest,
		request.CommandID, commandDigest, request.ClientCommandDigest, session.EventSessionPaused,
		time.Now().UTC().Format(time.RFC3339Nano), payload,
	)
	if err != nil {
		return session.Manifest{}, err
	}
	if err := store.appendEvent(event); err != nil {
		return session.Manifest{}, err
	}
	manifest, err = store.manifestForEvents(request.SessionID, append(events, event))
	if err != nil {
		return session.Manifest{}, err
	}
	if err := store.writeManifest(manifest); err != nil {
		return manifest, fmt.Errorf("%w: %v", session.ErrProjection, err)
	}
	return manifest, nil
}

func (store *DirStore) ResumeSession(ctx context.Context, request session.ResumeSessionRequest) (session.Manifest, error) {
	store.commitMu.Lock()
	defer store.commitMu.Unlock()
	store.mutationMu.RLock()
	defer store.mutationMu.RUnlock()
	if err := ctx.Err(); err != nil {
		return session.Manifest{}, err
	}
	if err := validateUUID("session id", request.SessionID); err != nil {
		return session.Manifest{}, err
	}
	if err := validateUUID("command id", request.CommandID); err != nil {
		return session.Manifest{}, err
	}
	if err := store.validateWriterEpoch(request.SessionID, request.WriterEpoch); err != nil {
		return session.Manifest{}, err
	}
	events, err := store.loadEvents(ctx, request.SessionID)
	if err != nil {
		return session.Manifest{}, err
	}
	manifest, err := store.manifestForEvents(request.SessionID, events)
	if err != nil {
		return session.Manifest{}, err
	}
	runID := manifest.Session.ActiveRunID
	commandDigest := session.ResumeDigest(runID)
	if request.ClientCommandDigest != "" && !validDigest(request.ClientCommandDigest) {
		return session.Manifest{}, errors.New("sessionstore: invalid client command digest")
	}
	if receipt, found := manifest.AcceptedCommands[request.CommandID]; found {
		if receipt.CommandDigest != commandDigest || receipt.ClientCommandDigest != request.ClientCommandDigest ||
			receipt.EventKind != session.EventSessionResumed {
			return session.Manifest{}, session.ErrCommandConflict
		}
		return manifest, nil
	}
	if manifest.Session.Status != session.StatusPaused || runID == "" {
		return session.Manifest{}, errors.New("sessionstore: session is not resumable from pause")
	}
	if manifest.Attempts[runID].Status != session.AttemptStatusPausedAtBoundary {
		return session.Manifest{}, errors.New("sessionstore: active attempt is not paused at a boundary")
	}
	if request.ExpectedSequence != manifest.Session.Sequence {
		return session.Manifest{}, session.ErrSequenceConflict
	}
	payload, err := json.Marshal(sessionResumedPayload{RunID: runID})
	if err != nil {
		return session.Manifest{}, err
	}
	event, err := newEventWithClientCommand(
		request.SessionID, request.WriterEpoch, manifest.Session.Sequence+1, events[len(events)-1].Digest,
		request.CommandID, commandDigest, request.ClientCommandDigest, session.EventSessionResumed,
		time.Now().UTC().Format(time.RFC3339Nano), payload,
	)
	if err != nil {
		return session.Manifest{}, err
	}
	if err := store.appendEvent(event); err != nil {
		return session.Manifest{}, err
	}
	manifest, err = store.manifestForEvents(request.SessionID, append(events, event))
	if err != nil {
		return session.Manifest{}, err
	}
	if err := store.writeManifest(manifest); err != nil {
		return manifest, fmt.Errorf("%w: %v", session.ErrProjection, err)
	}
	return manifest, nil
}

func (store *DirStore) RecordDetach(
	ctx context.Context,
	request session.DetachRequest,
) (session.Manifest, error) {
	store.commitMu.Lock()
	defer store.commitMu.Unlock()
	store.mutationMu.RLock()
	defer store.mutationMu.RUnlock()
	if err := ctx.Err(); err != nil {
		return session.Manifest{}, err
	}
	if err := validateUUID("session id", request.SessionID); err != nil {
		return session.Manifest{}, err
	}
	if err := validateUUID("command id", request.CommandID); err != nil {
		return session.Manifest{}, err
	}
	if request.Reason == "" || len(request.Reason) > 1024 || strings.ContainsAny(request.Reason, "\r\n\x00") {
		return session.Manifest{}, errors.New("sessionstore: invalid detach reason")
	}
	if !validDigest(request.ClientCommandDigest) {
		return session.Manifest{}, errors.New("sessionstore: client command digest is required")
	}
	if err := store.validateWriterEpoch(request.SessionID, request.WriterEpoch); err != nil {
		return session.Manifest{}, err
	}
	events, err := store.loadEvents(ctx, request.SessionID)
	if err != nil {
		return session.Manifest{}, err
	}
	manifest, err := store.manifestForEvents(request.SessionID, events)
	if err != nil {
		return session.Manifest{}, err
	}
	commandDigest := session.DetachDigest(request.Reason)
	if receipt, found := manifest.AcceptedCommands[request.CommandID]; found {
		if receipt.CommandDigest != commandDigest || receipt.ClientCommandDigest != request.ClientCommandDigest ||
			receipt.EventKind != session.EventSessionDetached {
			return session.Manifest{}, session.ErrCommandConflict
		}
		return manifest, nil
	}
	if manifest.Session.Status != session.StatusIndeterminate && manifest.Session.ActiveRunID != "" {
		return session.Manifest{}, session.ErrAttemptActive
	}
	if request.ExpectedSequence != manifest.Session.Sequence {
		return session.Manifest{}, session.ErrSequenceConflict
	}
	payload, err := json.Marshal(sessionDetachedPayload{Reason: request.Reason})
	if err != nil {
		return session.Manifest{}, err
	}
	event, err := newEventWithClientCommand(
		request.SessionID, request.WriterEpoch, manifest.Session.Sequence+1, events[len(events)-1].Digest,
		request.CommandID, commandDigest, request.ClientCommandDigest, session.EventSessionDetached,
		time.Now().UTC().Format(time.RFC3339Nano), payload,
	)
	if err != nil {
		return session.Manifest{}, err
	}
	if err := store.appendEvent(event); err != nil {
		return session.Manifest{}, err
	}
	manifest, err = store.manifestForEvents(request.SessionID, append(events, event))
	if err != nil {
		return session.Manifest{}, err
	}
	if err := store.writeManifest(manifest); err != nil {
		return manifest, fmt.Errorf("%w: %v", session.ErrProjection, err)
	}
	return manifest, nil
}

func (store *DirStore) UpdateAttempt(
	ctx context.Context,
	request session.AttemptUpdateRequest,
) (session.Manifest, error) {
	store.commitMu.Lock()
	defer store.commitMu.Unlock()
	store.mutationMu.RLock()
	defer store.mutationMu.RUnlock()
	if err := ctx.Err(); err != nil {
		return session.Manifest{}, err
	}
	if err := validateUUID("session id", request.SessionID); err != nil {
		return session.Manifest{}, err
	}
	if err := validateUUID("command id", request.CommandID); err != nil {
		return session.Manifest{}, err
	}
	if err := validateUUID("run id", request.RunID); err != nil {
		return session.Manifest{}, err
	}
	if !validAttemptStatus(request.Status) {
		return session.Manifest{}, fmt.Errorf("sessionstore: invalid attempt status %q", request.Status)
	}
	if err := store.validateWriterEpoch(request.SessionID, request.WriterEpoch); err != nil {
		return session.Manifest{}, err
	}
	events, err := store.loadEvents(ctx, request.SessionID)
	if err != nil {
		return session.Manifest{}, err
	}
	manifest, err := store.manifestForEvents(request.SessionID, events)
	if err != nil {
		return session.Manifest{}, err
	}
	if _, found := manifest.Attempts[request.RunID]; !found {
		return session.Manifest{}, errors.New("sessionstore: attempt does not belong to session")
	}
	payloadValue := attemptUpdatedPayload{RunID: request.RunID, Status: request.Status}
	commandDigest := session.AttemptUpdateDigest(request.RunID, request.Status)
	if receipt, found := manifest.AcceptedCommands[request.CommandID]; found {
		if receipt.CommandDigest != commandDigest || receipt.EventKind != session.EventAttemptUpdated {
			return session.Manifest{}, session.ErrCommandConflict
		}
		return manifest, nil
	}
	if isTerminalSessionStatus(manifest.Session.Status) {
		return session.Manifest{}, session.ErrSessionClosed
	}
	currentAttempt := manifest.Attempts[request.RunID]
	if !validAttemptTransition(currentAttempt.Status, request.Status) {
		return session.Manifest{}, fmt.Errorf("sessionstore: attempt transition %q to %q is invalid", currentAttempt.Status, request.Status)
	}
	if request.ExpectedSequence != manifest.Session.Sequence {
		return session.Manifest{}, session.ErrSequenceConflict
	}
	payload, err := json.Marshal(payloadValue)
	if err != nil {
		return session.Manifest{}, err
	}
	event, err := newEvent(
		request.SessionID, request.WriterEpoch, manifest.Session.Sequence+1, events[len(events)-1].Digest,
		request.CommandID, commandDigest, session.EventAttemptUpdated,
		time.Now().UTC().Format(time.RFC3339Nano), payload,
	)
	if err != nil {
		return session.Manifest{}, err
	}
	if err := store.appendEvent(event); err != nil {
		return session.Manifest{}, err
	}
	manifest, err = store.manifestForEvents(request.SessionID, append(events, event))
	if err != nil {
		return session.Manifest{}, err
	}
	if err := store.writeManifest(manifest); err != nil {
		return manifest, fmt.Errorf("%w: %v", session.ErrProjection, err)
	}
	return manifest, nil
}

func (store *DirStore) CommitExecutionMutation(
	ctx context.Context,
	request session.ExecutionMutationRequest,
) (session.Manifest, error) {
	store.commitMu.Lock()
	defer store.commitMu.Unlock()
	store.mutationMu.RLock()
	defer store.mutationMu.RUnlock()
	if err := ctx.Err(); err != nil {
		return session.Manifest{}, err
	}
	if err := validateUUID("session id", request.SessionID); err != nil {
		return session.Manifest{}, err
	}
	if err := validateUUID("command id", request.CommandID); err != nil {
		return session.Manifest{}, err
	}
	mutation := request.Mutation
	if err := validateUUID("run id", mutation.RunID); err != nil {
		return session.Manifest{}, err
	}
	if len(request.ProjectionBlobs) > maxProjectionBlobs {
		return session.Manifest{}, errors.New("sessionstore: execution mutation has too many projection blobs")
	}
	if err := validateExecutionMutation(mutation, request.StateProjection); err != nil {
		return session.Manifest{}, err
	}
	var projectedFrame session.ExecutionFrameProjectionContent
	if err := validateExecutionFrameProjection(
		mutation, request.FrameProjection, frameProjectionBlobLoader(request.ProjectionBlobs), &projectedFrame,
	); err != nil {
		return session.Manifest{}, err
	}
	if err := store.validateWriterEpoch(request.SessionID, request.WriterEpoch); err != nil {
		return session.Manifest{}, err
	}
	if mutation.RunWriterEpoch != request.WriterEpoch {
		return session.Manifest{}, fmt.Errorf("%w: run writer epoch does not match session writer epoch", session.ErrMutationConflict)
	}
	events, err := store.loadEvents(ctx, request.SessionID)
	if err != nil {
		return session.Manifest{}, err
	}
	manifest, err := store.manifestForEvents(request.SessionID, events)
	if err != nil {
		return session.Manifest{}, err
	}
	attempt, found := manifest.Attempts[mutation.RunID]
	if !found {
		return session.Manifest{}, errors.New("sessionstore: attempt does not belong to session")
	}
	if err := validateOccurrenceRecords(
		manifest, mutation.RunID, request.Occurrences, projectedFrame.PendingTraceEvents,
	); err != nil {
		return session.Manifest{}, err
	}
	if preparedTransitionUsesRun(manifest, mutation.RunID) {
		return session.Manifest{}, errors.New("sessionstore: prepared transition freezes its source mutation")
	}
	mutationBlob, err := session.NewExecutionMutationBlob(mutation)
	if err != nil {
		return session.Manifest{}, err
	}
	commandDigest := session.ExecutionMutationDigest(mutation.RunID, mutationBlob.Digest)
	if request.ClientCommandDigest != "" && !validDigest(request.ClientCommandDigest) {
		return session.Manifest{}, errors.New("sessionstore: invalid client command digest")
	}
	if receipt, found := manifest.AcceptedCommands[request.CommandID]; found {
		if receipt.CommandDigest != commandDigest || receipt.ClientCommandDigest != request.ClientCommandDigest ||
			receipt.EventKind != session.EventExecutionCommitted {
			return session.Manifest{}, session.ErrCommandConflict
		}
		return manifest, nil
	}
	if isTerminalSessionStatus(manifest.Session.Status) {
		return session.Manifest{}, session.ErrSessionClosed
	}
	if !validAttemptTransition(attempt.Status, mutation.AttemptStatus) {
		return session.Manifest{}, fmt.Errorf(
			"sessionstore: attempt transition %q to %q is invalid", attempt.Status, mutation.AttemptStatus,
		)
	}
	latestTraceSequence := attempt.CommittedTraceSequence
	if attempt.JournaledTraceSequence > latestTraceSequence {
		latestTraceSequence = attempt.JournaledTraceSequence
	}
	if mutation.PreviousMutationHash != attempt.ExecutionMutationHash ||
		attempt.ExecutionMutationHash == "" && mutation.CheckpointSequence != 0 ||
		attempt.ExecutionMutationHash != "" && mutation.CheckpointSequence <= attempt.CheckpointSequence ||
		mutation.CommittedTraceSequence < latestTraceSequence {
		return session.Manifest{}, session.ErrMutationConflict
	}
	if mutation.TraceSequenceStart == 0 && mutation.CommittedTraceSequence != latestTraceSequence ||
		mutation.TraceSequenceStart != 0 &&
			(mutation.TraceSequenceStart != latestTraceSequence+1 ||
				mutation.TraceSequenceEnd != mutation.CommittedTraceSequence) {
		return session.Manifest{}, session.ErrMutationConflict
	}
	if request.ExpectedSequence != manifest.Session.Sequence {
		return session.Manifest{}, session.ErrSequenceConflict
	}
	for _, blob := range request.ProjectionBlobs {
		if err := store.writeBlob(request.SessionID, blob); err != nil {
			return session.Manifest{}, err
		}
	}
	if err := store.writeBlob(request.SessionID, request.StateProjection); err != nil {
		return session.Manifest{}, err
	}
	if mutation.FrameProjectionHash != "" {
		if err := store.writeBlob(request.SessionID, request.FrameProjection); err != nil {
			return session.Manifest{}, err
		}
	}
	if err := store.writeBlob(request.SessionID, mutationBlob); err != nil {
		return session.Manifest{}, err
	}
	payloadValue := executionCommittedPayload{
		RunID: mutation.RunID, Status: mutation.AttemptStatus, MutationHash: mutationBlob.Digest,
		PreviousMutationHash: mutation.PreviousMutationHash, ProjectionHash: mutation.StateProjectionHash,
		FrameProjectionHash: mutation.FrameProjectionHash,
		CheckpointSequence:  mutation.CheckpointSequence, CommittedTraceSequence: mutation.CommittedTraceSequence,
		Occurrences: request.Occurrences,
	}
	payload, err := json.Marshal(payloadValue)
	if err != nil {
		return session.Manifest{}, err
	}
	event, err := newEventWithClientCommand(
		request.SessionID, request.WriterEpoch, manifest.Session.Sequence+1, events[len(events)-1].Digest,
		request.CommandID, commandDigest, request.ClientCommandDigest, session.EventExecutionCommitted,
		time.Now().UTC().Format(time.RFC3339Nano), payload,
	)
	if err != nil {
		return session.Manifest{}, err
	}
	if err := store.appendEvent(event); err != nil {
		return session.Manifest{}, err
	}
	manifest, err = store.manifestForEvents(request.SessionID, append(events, event))
	if err != nil {
		return session.Manifest{}, err
	}
	if err := store.writeManifest(manifest); err != nil {
		return manifest, fmt.Errorf("%w: %v", session.ErrProjection, err)
	}
	return manifest, nil
}

func (store *DirStore) CommitTraceEvent(
	ctx context.Context,
	request session.TraceCommitRequest,
) (session.Manifest, error) {
	store.commitMu.Lock()
	defer store.commitMu.Unlock()
	store.mutationMu.RLock()
	defer store.mutationMu.RUnlock()
	if err := ctx.Err(); err != nil {
		return session.Manifest{}, err
	}
	if err := validateUUID("session id", request.SessionID); err != nil {
		return session.Manifest{}, err
	}
	if err := validateUUID("command id", request.CommandID); err != nil {
		return session.Manifest{}, err
	}
	if err := validateUUID("run id", request.RunID); err != nil {
		return session.Manifest{}, err
	}
	if request.Event.RunID != request.RunID || request.Event.Sequence < 1 || request.Event.Kind == "" ||
		validateUUID("trace event id", request.Event.EventID) != nil || session.ParseTime(request.Event.Timestamp) != nil {
		return session.Manifest{}, errors.New("sessionstore: invalid direct trace event")
	}
	encodedTrace, err := json.Marshal(request.Event)
	if err != nil || !json.Valid(encodedTrace) {
		return session.Manifest{}, errors.New("sessionstore: invalid direct trace payload")
	}
	if len(encodedTrace) > session.MaxStdioFrameProjectionBytes {
		return session.Manifest{}, errors.New("sessionstore: direct trace payload exceeds stdio frame limits")
	}
	traceBlob := session.NewJSONBlob(encodedTrace)
	commandDigest := session.TraceCommitDigest(request.RunID, traceBlob.Digest)
	if err := store.validateWriterEpoch(request.SessionID, request.WriterEpoch); err != nil {
		return session.Manifest{}, err
	}
	events, err := store.loadEvents(ctx, request.SessionID)
	if err != nil {
		return session.Manifest{}, err
	}
	manifest, err := store.manifestForEvents(request.SessionID, events)
	if err != nil {
		return session.Manifest{}, err
	}
	if receipt, found := manifest.AcceptedCommands[request.CommandID]; found {
		if receipt.CommandDigest != commandDigest || receipt.EventKind != session.EventTraceCommitted {
			return session.Manifest{}, session.ErrCommandConflict
		}
		return manifest, nil
	}
	attempt, found := manifest.Attempts[request.RunID]
	if !found {
		return session.Manifest{}, errors.New("sessionstore: attempt does not belong to session")
	}
	var occurrence *session.OccurrenceRecord
	if occurrenceID, record, terminal, err := session.OccurrenceFromTraceEvent(
		request.SessionID, attempt.SegmentID, executionSourceForAttempt(attempt), request.Event,
	); err != nil {
		return session.Manifest{}, err
	} else if terminal {
		if occurrenceID != session.OccurrenceID(record) {
			return session.Manifest{}, errors.New("sessionstore: invalid direct trace occurrence identity")
		}
		occurrence = &record
	}
	if isTerminalSessionStatus(manifest.Session.Status) && !isTerminalCheckpointTrace(attempt, request.Event) {
		return session.Manifest{}, session.ErrSessionClosed
	}
	latestTraceSequence := attempt.CommittedTraceSequence
	if attempt.JournaledTraceSequence > latestTraceSequence {
		latestTraceSequence = attempt.JournaledTraceSequence
	}
	if request.Event.Sequence != latestTraceSequence+1 {
		return session.Manifest{}, session.ErrMutationConflict
	}
	if request.ExpectedSequence != manifest.Session.Sequence {
		return session.Manifest{}, session.ErrSequenceConflict
	}
	if err := store.writeBlob(request.SessionID, traceBlob); err != nil {
		return session.Manifest{}, err
	}
	payload, err := json.Marshal(traceCommittedPayload{
		RunID: request.RunID, TraceHash: traceBlob.Digest, TraceSequence: request.Event.Sequence,
		Occurrence: occurrence,
	})
	if err != nil {
		return session.Manifest{}, err
	}
	event, err := newEvent(
		request.SessionID, request.WriterEpoch, manifest.Session.Sequence+1, events[len(events)-1].Digest,
		request.CommandID, commandDigest, session.EventTraceCommitted,
		time.Now().UTC().Format(time.RFC3339Nano), payload,
	)
	if err != nil {
		return session.Manifest{}, err
	}
	if err := store.appendEvent(event); err != nil {
		return session.Manifest{}, err
	}
	manifest, err = store.manifestForEvents(request.SessionID, append(events, event))
	if err != nil {
		return session.Manifest{}, err
	}
	if err := store.writeManifest(manifest); err != nil {
		return manifest, fmt.Errorf("%w: %v", session.ErrProjection, err)
	}
	return manifest, nil
}

func isTerminalCheckpointTrace(attempt session.RunAttemptRecord, event engine.Event) bool {
	if event.Kind != "checkpoint" || event.Sequence != attempt.CommittedTraceSequence+1 ||
		attempt.JournaledTraceSequence >= event.Sequence {
		return false
	}
	snapshotFile, ok := event.Payload["snapshot_file"].(string)
	return ok && snapshotFile == fmt.Sprintf("checkpoint-%020d.json", attempt.CheckpointSequence)
}

func (store *DirStore) CommitSegmentRevision(
	ctx context.Context,
	request session.SegmentRevisionRequest,
) (session.Manifest, error) {
	store.commitMu.Lock()
	defer store.commitMu.Unlock()
	store.mutationMu.RLock()
	defer store.mutationMu.RUnlock()
	if err := ctx.Err(); err != nil {
		return session.Manifest{}, err
	}
	for label, value := range map[string]string{
		"session id": request.SessionID, "command id": request.CommandID,
		"segment id": request.SegmentID, "run id": request.RunID,
	} {
		if err := validateUUID(label, value); err != nil {
			return session.Manifest{}, err
		}
	}
	if err := store.validateWriterEpoch(request.SessionID, request.WriterEpoch); err != nil {
		return session.Manifest{}, err
	}
	if err := validateBlob(request.ExecutableSnapshot); err != nil {
		return session.Manifest{}, err
	}
	if err := validateBlob(request.Graph); err != nil {
		return session.Manifest{}, err
	}
	events, err := store.loadEvents(ctx, request.SessionID)
	if err != nil {
		return session.Manifest{}, err
	}
	manifest, err := store.manifestForEvents(request.SessionID, events)
	if err != nil {
		return session.Manifest{}, err
	}
	commandDigest := session.SegmentRevisionDigest(
		request.SegmentID, request.RunID, request.ExecutableRevision, request.GraphRevision,
		request.PlanHash, request.ExecutableSnapshot.Digest, request.Graph.Digest, request.Resolution, request.Dispatch,
	)
	if receipt, found := manifest.AcceptedCommands[request.CommandID]; found {
		if receipt.CommandDigest != commandDigest || receipt.EventKind != session.EventSegmentRevised {
			return session.Manifest{}, session.ErrCommandConflict
		}
		return manifest, nil
	}
	segment, found := manifest.Segments[request.SegmentID]
	attempt := manifest.Attempts[request.RunID]
	if !found || manifest.Session.ActiveSegmentID != request.SegmentID ||
		manifest.Session.ActiveRunID != request.RunID || attempt.SegmentID != request.SegmentID {
		return session.Manifest{}, errors.New("sessionstore: segment revision is not for the active attempt")
	}
	if request.ExpectedSequence != manifest.Session.Sequence {
		return session.Manifest{}, session.ErrSequenceConflict
	}
	if request.ExecutableRevision != segment.ExecutableRevision+1 ||
		request.GraphRevision != segment.GraphRevision+1 || request.ExecutableRevision != request.GraphRevision {
		return session.Manifest{}, errors.New("sessionstore: segment revision is not contiguous")
	}
	if !validHash(request.PlanHash) {
		return session.Manifest{}, errors.New("sessionstore: segment revision plan hash is invalid")
	}
	if err := validateSegmentResolution(request.Resolution, request.WriterEpoch); err != nil ||
		request.Resolution.Revision != request.ExecutableRevision-1 {
		if err == nil {
			err = errors.New("sessionstore: segment resolution revision does not match segment revision")
		}
		return session.Manifest{}, err
	}
	if err := validateSegmentRevisionDispatch(request.Dispatch, request.Resolution, request.WriterEpoch); err != nil {
		return session.Manifest{}, err
	}
	payload := segmentRevisedPayload{
		SegmentID: request.SegmentID, RunID: request.RunID,
		ExecutableRevision: request.ExecutableRevision, GraphRevision: request.GraphRevision,
		PlanHash:               request.PlanHash,
		ExecutableSnapshotHash: request.ExecutableSnapshot.Digest, GraphHash: request.Graph.Digest,
		Resolution: request.Resolution, Dispatch: request.Dispatch,
	}
	encodedPayload, err := json.Marshal(payload)
	if err != nil {
		return session.Manifest{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	event, err := newEvent(
		request.SessionID, request.WriterEpoch, manifest.Session.Sequence+1, events[len(events)-1].Digest,
		request.CommandID, commandDigest, session.EventSegmentRevised, now, encodedPayload,
	)
	if err != nil {
		return session.Manifest{}, err
	}
	nextManifest, err := store.manifestForEvents(request.SessionID, append(events, event))
	if err != nil {
		return session.Manifest{}, err
	}
	blobs := []session.JSONBlob{request.ExecutableSnapshot, request.Graph}
	newBlobPaths, err := store.preflightBlobWrites(request.SessionID, blobs)
	if err != nil {
		return session.Manifest{}, err
	}
	rollbackBlobs := true
	defer func() {
		if rollbackBlobs {
			store.removeNewBlobs(newBlobPaths)
		}
	}()
	for _, blob := range blobs {
		if err := store.writeBlob(request.SessionID, blob); err != nil {
			return session.Manifest{}, err
		}
	}
	rollbackBlobs = false
	if err := store.appendEvent(event); err != nil {
		return session.Manifest{}, err
	}
	manifest = nextManifest
	if err := store.writeManifest(manifest); err != nil {
		return manifest, fmt.Errorf("%w: %v", session.ErrProjection, err)
	}
	return manifest, nil
}

func (store *DirStore) PrepareTransition(
	ctx context.Context,
	request session.PrepareTransitionRequest,
) (session.Manifest, error) {
	store.commitMu.Lock()
	defer store.commitMu.Unlock()
	store.mutationMu.RLock()
	defer store.mutationMu.RUnlock()
	if err := ctx.Err(); err != nil {
		return session.Manifest{}, err
	}
	if err := validateUUID("session id", request.SessionID); err != nil {
		return session.Manifest{}, err
	}
	if err := validateUUID("command id", request.CommandID); err != nil {
		return session.Manifest{}, err
	}
	if err := store.validateWriterEpoch(request.SessionID, request.WriterEpoch); err != nil {
		return session.Manifest{}, err
	}
	if err := validateTransitionPreparation(request); err != nil {
		return session.Manifest{}, err
	}
	events, err := store.loadEvents(ctx, request.SessionID)
	if err != nil {
		return session.Manifest{}, err
	}
	manifest, err := store.manifestForEvents(request.SessionID, events)
	if err != nil {
		return session.Manifest{}, err
	}
	commandDigest := session.TransitionPrepareDigest(request.Transition, request.TargetSegment, request.TargetAttempt)
	if receipt, found := manifest.AcceptedCommands[request.CommandID]; found {
		if receipt.CommandDigest != commandDigest || receipt.EventKind != session.EventTransitionPrepared {
			return session.Manifest{}, session.ErrCommandConflict
		}
		return manifest, nil
	}
	if isTerminalSessionStatus(manifest.Session.Status) {
		return session.Manifest{}, session.ErrSessionClosed
	}
	if request.ExpectedSequence != manifest.Session.Sequence {
		return session.Manifest{}, session.ErrSequenceConflict
	}
	if _, found := manifest.Transitions[request.Transition.TransitionID]; found {
		return session.Manifest{}, errors.New("sessionstore: transition already exists")
	}
	for _, existing := range manifest.Transitions {
		if existing.SourceSegmentID == request.Transition.SourceSegmentID {
			return session.Manifest{}, errors.New("sessionstore: transition source already has a transition")
		}
	}
	if manifest.Session.ActiveRunID != request.Transition.SourceOccurrence.RunID ||
		manifest.Session.ActiveSegmentID != request.Transition.SourceSegmentID {
		return session.Manifest{}, errors.New("sessionstore: transition source is not active")
	}
	sourceAttempt := manifest.Attempts[manifest.Session.ActiveRunID]
	if sourceAttempt.Status != session.AttemptStatusHandoffPending || sourceAttempt.SegmentID != request.Transition.SourceSegmentID {
		return session.Manifest{}, errors.New("sessionstore: transition source is not paused at handoff")
	}
	sourceSegment := manifest.Segments[request.Transition.SourceSegmentID]
	if request.TargetSegment.Ordinal != sourceSegment.Ordinal+1 {
		return session.Manifest{}, errors.New("sessionstore: target segment ordinal does not follow its source")
	}
	if _, found := manifest.Segments[request.TargetSegment.SegmentID]; found {
		return session.Manifest{}, errors.New("sessionstore: target segment already exists")
	}
	if _, found := manifest.Attempts[request.TargetAttempt.RunID]; found {
		return session.Manifest{}, errors.New("sessionstore: target attempt already exists")
	}
	requestedBlobs := make(map[string]json.RawMessage, len(request.Blobs))
	for _, blob := range request.Blobs {
		if err := validateBlob(blob); err != nil {
			return session.Manifest{}, err
		}
		requestedBlobs[blob.Digest] = blob.Data
	}
	var contextData json.RawMessage
	for _, digest := range []string{
		request.Transition.TargetGraphHash,
		request.Transition.TargetExecutableSnapshotHash,
		request.Transition.ContextDigest,
	} {
		data, found := requestedBlobs[digest]
		if !found {
			var readErr error
			data, readErr = store.ReadBlob(ctx, request.SessionID, digest)
			if readErr != nil {
				return session.Manifest{}, fmt.Errorf("sessionstore: transition blob %s: %w", digest, readErr)
			}
		}
		if digest == request.Transition.ContextDigest {
			contextData = data
		}
	}
	if err := validateTransitionContext(request.Transition, contextData); err != nil {
		return session.Manifest{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	transition := request.Transition
	transition.PreparedAt = now
	payload, err := json.Marshal(transitionPreparedPayload{
		Transition: transition, TargetSegment: request.TargetSegment, TargetAttempt: request.TargetAttempt,
	})
	if err != nil {
		return session.Manifest{}, err
	}
	event, err := newEvent(
		request.SessionID, request.WriterEpoch, manifest.Session.Sequence+1, events[len(events)-1].Digest,
		request.CommandID, commandDigest, session.EventTransitionPrepared, now, payload,
	)
	if err != nil {
		return session.Manifest{}, err
	}
	nextManifest, err := store.manifestForEvents(request.SessionID, append(events, event))
	if err != nil {
		return session.Manifest{}, err
	}
	newBlobPaths, err := store.preflightBlobWrites(request.SessionID, request.Blobs)
	if err != nil {
		return session.Manifest{}, err
	}
	rollbackBlobs := true
	defer func() {
		if rollbackBlobs {
			store.removeNewBlobs(newBlobPaths)
		}
	}()
	for _, blob := range request.Blobs {
		if err := store.writeBlob(request.SessionID, blob); err != nil {
			return session.Manifest{}, err
		}
	}
	rollbackBlobs = false
	if err := store.appendEvent(event); err != nil {
		return session.Manifest{}, err
	}
	manifest = nextManifest
	if err := store.writeManifest(manifest); err != nil {
		return manifest, fmt.Errorf("%w: %v", session.ErrProjection, err)
	}
	return manifest, nil
}

func (store *DirStore) CommitTransition(
	ctx context.Context,
	request session.CommitTransitionRequest,
) (session.Manifest, error) {
	store.commitMu.Lock()
	defer store.commitMu.Unlock()
	store.mutationMu.RLock()
	defer store.mutationMu.RUnlock()
	if err := ctx.Err(); err != nil {
		return session.Manifest{}, err
	}
	for label, value := range map[string]string{
		"session id": request.SessionID, "command id": request.CommandID, "transition id": request.TransitionID,
	} {
		if err := validateUUID(label, value); err != nil {
			return session.Manifest{}, err
		}
	}
	if err := store.validateWriterEpoch(request.SessionID, request.WriterEpoch); err != nil {
		return session.Manifest{}, err
	}
	events, err := store.loadEvents(ctx, request.SessionID)
	if err != nil {
		return session.Manifest{}, err
	}
	manifest, err := store.manifestForEvents(request.SessionID, events)
	if err != nil {
		return session.Manifest{}, err
	}
	commandDigest := session.TransitionCommitDigest(request.TransitionID)
	if receipt, found := manifest.AcceptedCommands[request.CommandID]; found {
		if receipt.CommandDigest != commandDigest || receipt.EventKind != session.EventTransitionCommitted {
			return session.Manifest{}, session.ErrCommandConflict
		}
		return manifest, nil
	}
	transition, found := manifest.Transitions[request.TransitionID]
	if !found || transition.Status != session.TransitionStatusPrepared {
		return session.Manifest{}, errors.New("sessionstore: transition is not prepared")
	}
	sourceAttempt := manifest.Attempts[transition.SourceOccurrence.RunID]
	if sourceAttempt.Status != session.AttemptStatusHandoffPending || sourceAttempt.ExecutionMutationHash == "" {
		return session.Manifest{}, errors.New("sessionstore: prepared transition source is no longer handoff-pending")
	}
	if request.ExpectedSequence != manifest.Session.Sequence {
		return session.Manifest{}, session.ErrSequenceConflict
	}
	prepared, found, err := findPreparedTransition(events, request.TransitionID)
	if err != nil || !found {
		return session.Manifest{}, errors.New("sessionstore: prepared transition payload is unavailable")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	payload, err := json.Marshal(transitionCommittedPayload{
		TransitionID:  request.TransitionID,
		TargetSegment: prepared.TargetSegment,
		TargetAttempt: prepared.TargetAttempt,
	})
	if err != nil {
		return session.Manifest{}, err
	}
	event, err := newEvent(
		request.SessionID, request.WriterEpoch, manifest.Session.Sequence+1, events[len(events)-1].Digest,
		request.CommandID, commandDigest, session.EventTransitionCommitted, now, payload,
	)
	if err != nil {
		return session.Manifest{}, err
	}
	nextManifest, err := store.manifestForEvents(request.SessionID, append(events, event))
	if err != nil {
		return session.Manifest{}, err
	}
	if err := store.appendEvent(event); err != nil {
		return session.Manifest{}, err
	}
	manifest = nextManifest
	if err := store.writeManifest(manifest); err != nil {
		return manifest, fmt.Errorf("%w: %v", session.ErrProjection, err)
	}
	return manifest, nil
}

func (store *DirStore) AbortTransition(
	ctx context.Context,
	request session.AbortTransitionRequest,
) (session.Manifest, error) {
	store.commitMu.Lock()
	defer store.commitMu.Unlock()
	store.mutationMu.RLock()
	defer store.mutationMu.RUnlock()
	if err := ctx.Err(); err != nil {
		return session.Manifest{}, err
	}
	for label, value := range map[string]string{
		"session id": request.SessionID, "command id": request.CommandID, "transition id": request.TransitionID,
	} {
		if err := validateUUID(label, value); err != nil {
			return session.Manifest{}, err
		}
	}
	if strings.TrimSpace(request.Reason) == "" {
		return session.Manifest{}, errors.New("sessionstore: transition abort reason is required")
	}
	if err := store.validateWriterEpoch(request.SessionID, request.WriterEpoch); err != nil {
		return session.Manifest{}, err
	}
	events, err := store.loadEvents(ctx, request.SessionID)
	if err != nil {
		return session.Manifest{}, err
	}
	manifest, err := store.manifestForEvents(request.SessionID, events)
	if err != nil {
		return session.Manifest{}, err
	}
	commandDigest := session.TransitionAbortDigest(request.TransitionID, request.Reason)
	if receipt, found := manifest.AcceptedCommands[request.CommandID]; found {
		if receipt.CommandDigest != commandDigest || receipt.EventKind != session.EventTransitionAborted {
			return session.Manifest{}, session.ErrCommandConflict
		}
		return manifest, nil
	}
	transition, found := manifest.Transitions[request.TransitionID]
	if !found || transition.Status != session.TransitionStatusPrepared {
		return session.Manifest{}, errors.New("sessionstore: only a prepared transition can be aborted")
	}
	sourceAttempt := manifest.Attempts[transition.SourceOccurrence.RunID]
	if sourceAttempt.ExecutionMutationHash != "" {
		return session.Manifest{}, errors.New("sessionstore: authoritative source handoff cannot be aborted")
	}
	if request.ExpectedSequence != manifest.Session.Sequence {
		return session.Manifest{}, session.ErrSequenceConflict
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	payload, err := json.Marshal(transitionAbortedPayload{TransitionID: request.TransitionID, Reason: request.Reason})
	if err != nil {
		return session.Manifest{}, err
	}
	event, err := newEvent(
		request.SessionID, request.WriterEpoch, manifest.Session.Sequence+1, events[len(events)-1].Digest,
		request.CommandID, commandDigest, session.EventTransitionAborted, now, payload,
	)
	if err != nil {
		return session.Manifest{}, err
	}
	nextManifest, err := store.manifestForEvents(request.SessionID, append(events, event))
	if err != nil {
		return session.Manifest{}, err
	}
	if err := store.appendEvent(event); err != nil {
		return session.Manifest{}, err
	}
	manifest = nextManifest
	if err := store.writeManifest(manifest); err != nil {
		return manifest, fmt.Errorf("%w: %v", session.ErrProjection, err)
	}
	return manifest, nil
}

func validateExecutionMutation(mutation session.ExecutionMutation, projection session.JSONBlob) error {
	if mutation.SchemaVersion != session.ExecutionMutationSchemaV1 || mutation.CheckpointSequence < 0 ||
		mutation.RunWriterEpoch == 0 || !validAttemptStatus(mutation.AttemptStatus) ||
		!validDigest(mutation.StateProjectionHash) || mutation.StateProjectionHash != projection.Digest ||
		len(projection.Data) == 0 || mutation.CommittedTraceSequence < 0 || mutation.TraceSequenceStart < 0 ||
		mutation.TraceSequenceEnd < mutation.TraceSequenceStart || mutation.TraceSequenceEnd > mutation.CommittedTraceSequence {
		return errors.New("sessionstore: invalid execution mutation")
	}
	if mutation.FrameProjectionHash != "" && !validDigest(mutation.FrameProjectionHash) {
		return errors.New("sessionstore: invalid execution frame projection hash")
	}
	expectedStatus := session.AttemptStatus("")
	switch mutation.RunStatus {
	case "pending", "running":
		expectedStatus = session.AttemptStatusRunning
	case "waiting":
		expectedStatus = session.AttemptStatusWaiting
	case "paused_at_boundary":
		expectedStatus = session.AttemptStatusPausedAtBoundary
	case "handoff_pending":
		expectedStatus = session.AttemptStatusHandoffPending
	case "completed":
		expectedStatus = session.AttemptStatusCompleted
	case "failed":
		expectedStatus = session.AttemptStatusFailed
	case "cancelled":
		expectedStatus = session.AttemptStatusCancelled
	case "indeterminate":
		expectedStatus = session.AttemptStatusIndeterminate
	}
	if expectedStatus == "" || mutation.AttemptStatus != expectedStatus {
		return errors.New("sessionstore: execution mutation statuses do not match")
	}
	var projectionHeader struct {
		SchemaVersion      string `json:"schema_version"`
		RunID              string `json:"run_id"`
		CheckpointSequence int64  `json:"checkpoint_sequence"`
	}
	if err := json.Unmarshal(projection.Data, &projectionHeader); err != nil ||
		projectionHeader.SchemaVersion != session.RunProjectionSchemaV1 ||
		projectionHeader.RunID != mutation.RunID ||
		projectionHeader.CheckpointSequence != mutation.CheckpointSequence {
		return fmt.Errorf("%w: state projection metadata does not match mutation", session.ErrMutationConflict)
	}
	if mutation.PreviousMutationHash != "" && !validDigest(mutation.PreviousMutationHash) {
		return errors.New("sessionstore: invalid previous execution mutation")
	}
	if mutation.TraceSequenceStart == 0 && mutation.TraceSequenceEnd != 0 {
		return errors.New("sessionstore: invalid execution mutation trace range")
	}
	return nil
}

func frameProjectionBlobLoader(blobs []session.JSONBlob) func(string) (json.RawMessage, error) {
	byDigest := make(map[string]session.JSONBlob, len(blobs))
	for _, blob := range blobs {
		byDigest[blob.Digest] = blob
	}
	return func(digest string) (json.RawMessage, error) {
		blob, found := byDigest[digest]
		if !found || validateBlob(blob) != nil {
			return nil, errors.New("sessionstore: execution frame projection chunk is unavailable")
		}
		return blob.Data, nil
	}
}

func validateExecutionFrameProjection(
	mutation session.ExecutionMutation,
	blob session.JSONBlob,
	loadChunk func(string) (json.RawMessage, error),
	projectedOutput ...*session.ExecutionFrameProjectionContent,
) error {
	if mutation.FrameProjectionHash == "" {
		if blob.Digest != "" || len(blob.Data) != 0 {
			return errors.New("sessionstore: legacy execution mutation has an unexpected frame projection")
		}
		return nil
	}
	if mutation.FrameProjectionHash != blob.Digest || validateBlob(blob) != nil {
		return errors.New("sessionstore: invalid execution frame projection blob")
	}
	var projection session.ExecutionFrameProjection
	if err := decodeStrictJSON(blob.Data, &projection); err != nil ||
		projection.SchemaVersion != session.ExecutionFrameProjectionSchemaV1 ||
		projection.RunID != mutation.RunID || projection.CheckpointSequence != mutation.CheckpointSequence ||
		projection.CommittedTraceSequence != mutation.CommittedTraceSequence ||
		!validDigest(projection.ContentDigest) || projection.ContentSize < 1 ||
		projection.ContentSize > session.MaxStdioFrameProjectionBytes ||
		len(projection.ChunkDigests) == 0 || loadChunk == nil {
		return fmt.Errorf("%w: execution frame projection metadata does not match mutation", session.ErrMutationConflict)
	}
	expectedChunkCount := int((projection.ContentSize + frameProjectionChunkBytes - 1) / frameProjectionChunkBytes)
	if len(projection.ChunkDigests) != expectedChunkCount {
		return errors.New("sessionstore: execution frame projection chunk count mismatch")
	}
	content := make([]byte, 0, projection.ContentSize)
	for index, digest := range projection.ChunkDigests {
		if !validDigest(digest) {
			return errors.New("sessionstore: invalid execution frame projection chunk digest")
		}
		chunkData, err := loadChunk(digest)
		if err != nil || session.DigestBytes(chunkData) != digest {
			return errors.New("sessionstore: execution frame projection chunk digest mismatch")
		}
		var chunk session.ExecutionFrameProjectionChunk
		expectedChunkSize := frameProjectionChunkBytes
		if index == len(projection.ChunkDigests)-1 {
			expectedChunkSize = int(projection.ContentSize) - len(content)
		}
		if err := decodeStrictJSON(chunkData, &chunk); err != nil ||
			chunk.SchemaVersion != session.ExecutionFrameProjectionChunkSchemaV1 ||
			len(chunk.Data) != expectedChunkSize ||
			int64(len(chunk.Data)) > projection.ContentSize-int64(len(content)) {
			return errors.New("sessionstore: invalid execution frame projection chunk")
		}
		content = append(content, chunk.Data...)
	}
	if int64(len(content)) != projection.ContentSize || session.DigestBytes(content) != projection.ContentDigest {
		return errors.New("sessionstore: execution frame projection content digest mismatch")
	}
	var projected session.ExecutionFrameProjectionContent
	if err := decodeStrictJSON(content, &projected); err != nil || len(projected.Interactions) > 128 {
		return errors.New("sessionstore: invalid execution frame projection content")
	}
	for turnID, interaction := range projected.Interactions {
		if interaction == nil || turnID == "" || interaction.TurnID != turnID ||
			interaction.SchemaVersion != engine.InteractionStateSchemaV1 || interaction.StepID == "" ||
			interaction.Kind == "" || interaction.RequestDigest != engine.InteractionPayloadDigest(interaction.Request) ||
			(interaction.Status != engine.InteractionStatusPending && interaction.Status != engine.InteractionStatusAnswered) {
			return errors.New("sessionstore: invalid execution frame interaction")
		}
		if interaction.Status == engine.InteractionStatusAnswered &&
			interaction.AnswerDigest != engine.InteractionPayloadDigest(interaction.Answer) {
			return errors.New("sessionstore: invalid answered execution frame interaction")
		}
	}
	previousTraceSequence := int64(0)
	for index, traceEvent := range projected.PendingTraceEvents {
		if traceEvent.RunID != mutation.RunID || traceEvent.EventID == "" || traceEvent.Kind == "" ||
			traceEvent.Sequence < 1 || traceEvent.Sequence > mutation.CommittedTraceSequence ||
			session.ParseTime(traceEvent.Timestamp) != nil ||
			index > 0 && traceEvent.Sequence != previousTraceSequence+1 {
			return errors.New("sessionstore: invalid execution frame trace event")
		}
		previousTraceSequence = traceEvent.Sequence
	}
	if len(projected.PendingTraceEvents) == 0 {
		if mutation.TraceSequenceStart != 0 || mutation.TraceSequenceEnd != 0 {
			return errors.New("sessionstore: execution frame projection omits committed trace events")
		}
	} else if projected.PendingTraceEvents[0].Sequence != mutation.TraceSequenceStart ||
		previousTraceSequence != mutation.TraceSequenceEnd || previousTraceSequence != mutation.CommittedTraceSequence {
		return errors.New("sessionstore: execution frame projection trace range does not match mutation")
	}
	if len(projectedOutput) > 0 && projectedOutput[0] != nil {
		*projectedOutput[0] = projected
	}
	return nil
}

func (store *DirStore) LoadManifest(ctx context.Context, sessionID string) (session.Manifest, error) {
	store.commitMu.Lock()
	defer store.commitMu.Unlock()
	if err := ctx.Err(); err != nil {
		return session.Manifest{}, err
	}
	if err := validateUUID("session id", sessionID); err != nil {
		return session.Manifest{}, err
	}
	store.invalidateJournalCache(sessionID)
	events, committedOffset, err := store.scanJournal(ctx, sessionID)
	if err != nil {
		return session.Manifest{}, err
	}
	cacheHead, reconcileCache := store.committedFrameCacheHead(sessionID)
	recoveredEvents := make([]session.Event, 0)
	recoveredManifests := make(map[int64]session.Manifest)
	manifest, err := rebuildManifestWithObserver(events, func(event session.Event, current session.Manifest) error {
		if !reconcileCache || event.Sequence <= cacheHead {
			return nil
		}
		recoveredEvents = append(recoveredEvents, event)
		if !frameEventNeedsManifest(event.Kind) {
			return nil
		}
		projected, err := frameManifestProjection(event, current)
		if err != nil {
			return err
		}
		recoveredManifests[event.Sequence] = projected
		return nil
	})
	if err != nil {
		return session.Manifest{}, err
	}
	if store.cacheRecoveredFrameEvents(sessionID, cacheHead, recoveredEvents, recoveredManifests) {
		store.notifySessionUpdate(sessionID)
	}
	store.primeJournalCache(sessionID, events, committedOffset)
	_ = store.writeManifest(manifest)
	return manifest, nil
}

func (store *DirStore) LoadManifestAtSequence(
	ctx context.Context,
	sessionID string,
	sequence int64,
) (session.Manifest, error) {
	store.commitMu.Lock()
	defer store.commitMu.Unlock()
	if err := ctx.Err(); err != nil {
		return session.Manifest{}, err
	}
	if err := validateUUID("session id", sessionID); err != nil {
		return session.Manifest{}, err
	}
	if sequence < 1 {
		return session.Manifest{}, session.ErrSequenceConflict
	}
	events, err := store.loadEvents(ctx, sessionID)
	if err != nil {
		return session.Manifest{}, err
	}
	if sequence > int64(len(events)) || events[sequence-1].Sequence != sequence {
		return session.Manifest{}, session.ErrSequenceConflict
	}
	return rebuildManifest(events[:sequence])
}

func (store *DirStore) ReadFrameEvents(
	ctx context.Context,
	sessionID string,
	afterSequence int64,
) ([]session.Event, map[int64]session.Manifest, int64, error) {
	store.commitMu.Lock()
	defer store.commitMu.Unlock()
	if afterSequence < 0 {
		return nil, nil, 0, errors.New("sessionstore: after sequence cannot be negative")
	}
	if events, manifests, headSequence, ok := store.cachedFrameEvents(sessionID, afterSequence); ok {
		return events, manifests, headSequence, nil
	}
	manifests := make(map[int64]session.Manifest)
	events, _, err := store.scanJournalWithObserver(ctx, sessionID, func(event session.Event, manifest session.Manifest) error {
		if event.Sequence <= afterSequence || !frameEventNeedsManifest(event.Kind) {
			return nil
		}
		projected, err := frameManifestProjection(event, manifest)
		if err != nil {
			return err
		}
		manifests[event.Sequence] = projected
		return nil
	})
	if err != nil {
		return nil, nil, 0, err
	}
	if len(events) == 0 {
		return nil, nil, 0, os.ErrNotExist
	}
	headSequence := events[len(events)-1].Sequence
	if afterSequence > headSequence {
		return nil, nil, headSequence, session.ErrSequenceConflict
	}
	index := 0
	for index < len(events) && events[index].Sequence <= afterSequence {
		index++
	}
	suffix := append([]session.Event(nil), events[index:]...)
	store.replaceFrameEventCache(sessionID, afterSequence, headSequence, suffix, manifests)
	return suffix, manifests, headSequence, nil
}

func (store *DirStore) cachedFrameEvents(
	sessionID string,
	afterSequence int64,
) ([]session.Event, map[int64]session.Manifest, int64, bool) {
	store.frameMu.Lock()
	defer store.frameMu.Unlock()
	cache := store.frameCaches[sessionID]
	if cache == nil || afterSequence < cache.baseSequence || afterSequence > cache.headSequence ||
		cache.pendingManifestSequence != 0 {
		return nil, nil, 0, false
	}
	trim := int(afterSequence - cache.baseSequence)
	if trim > len(cache.events) {
		return nil, nil, 0, false
	}
	if trim > 0 {
		cache.events = append([]session.Event(nil), cache.events[trim:]...)
		for sequence := range cache.manifests {
			if sequence <= afterSequence {
				delete(cache.manifests, sequence)
			}
		}
		cache.baseSequence = afterSequence
	}
	events := append([]session.Event(nil), cache.events...)
	manifests := make(map[int64]session.Manifest, len(cache.manifests))
	for sequence, manifest := range cache.manifests {
		manifests[sequence] = manifest
	}
	return events, manifests, cache.headSequence, true
}

func (store *DirStore) replaceFrameEventCache(
	sessionID string,
	baseSequence int64,
	headSequence int64,
	events []session.Event,
	manifests map[int64]session.Manifest,
) {
	store.frameMu.Lock()
	defer store.frameMu.Unlock()
	store.frameCaches[sessionID] = &frameEventCache{
		baseSequence: baseSequence, headSequence: headSequence,
		events: append([]session.Event(nil), events...), manifests: manifests,
	}
}

func (store *DirStore) committedFrameCacheHead(sessionID string) (int64, bool) {
	store.frameMu.Lock()
	defer store.frameMu.Unlock()
	cache := store.frameCaches[sessionID]
	if cache == nil || cache.pendingManifestSequence != 0 {
		return 0, false
	}
	return cache.headSequence, true
}

func (store *DirStore) cacheRecoveredFrameEvents(
	sessionID string,
	expectedHead int64,
	events []session.Event,
	manifests map[int64]session.Manifest,
) bool {
	if len(events) == 0 || events[0].Sequence != expectedHead+1 {
		return false
	}
	store.frameMu.Lock()
	defer store.frameMu.Unlock()
	cache := store.frameCaches[sessionID]
	if cache == nil || cache.headSequence != expectedHead || cache.pendingManifestSequence != 0 {
		return false
	}
	for index, event := range events {
		if event.SessionID != sessionID || event.Sequence != expectedHead+int64(index)+1 {
			return false
		}
	}
	cache.events = append(cache.events, events...)
	for sequence, manifest := range manifests {
		cache.manifests[sequence] = manifest
	}
	cache.headSequence = events[len(events)-1].Sequence
	return true
}

func (store *DirStore) cacheCommittedFrameEvent(event session.Event) {
	store.frameMu.Lock()
	defer store.frameMu.Unlock()
	cache := store.frameCaches[event.SessionID]
	if cache == nil || cache.headSequence+1 != event.Sequence {
		cache = &frameEventCache{
			baseSequence: event.Sequence - 1, headSequence: event.Sequence - 1,
			manifests: make(map[int64]session.Manifest),
		}
		store.frameCaches[event.SessionID] = cache
	}
	cache.events = append(cache.events, event)
	cache.headSequence = event.Sequence
	cache.pendingManifestSequence = event.Sequence
}

func (store *DirStore) cacheCommittedFrameManifest(manifest session.Manifest) bool {
	store.frameMu.Lock()
	defer store.frameMu.Unlock()
	cache := store.frameCaches[manifest.Session.SessionID]
	if cache == nil || cache.pendingManifestSequence != manifest.Session.Sequence {
		return false
	}
	if len(cache.events) == 0 || cache.events[len(cache.events)-1].Sequence != manifest.Session.Sequence {
		return false
	}
	if frameEventNeedsManifest(cache.events[len(cache.events)-1].Kind) {
		event := cache.events[len(cache.events)-1]
		projected, err := frameManifestProjection(event, manifest)
		if err != nil {
			return false
		}
		cache.manifests[manifest.Session.Sequence] = projected
	}
	cache.pendingManifestSequence = 0
	return true
}

func frameEventNeedsManifest(kind session.EventKind) bool {
	switch kind {
	case session.EventSessionCreated, session.EventSessionClosed, session.EventSessionPaused, session.EventSessionResumed,
		session.EventSessionDetached, session.EventAttemptUpdated,
		session.EventExecutionCommitted, session.EventSegmentRevised, session.EventTransitionAborted:
		return true
	default:
		return false
	}
}

func frameManifestProjection(event session.Event, manifest session.Manifest) (session.Manifest, error) {
	if manifest.SchemaVersion != session.ManifestSchemaV1 || manifest.Session.SessionID != event.SessionID ||
		manifest.Session.Sequence != event.Sequence {
		return session.Manifest{}, errors.New("sessionstore: frame manifest does not match journal event")
	}
	receipt, found := manifest.AcceptedCommands[event.CommandID]
	if !found || receipt.Sequence != event.Sequence || receipt.EventKind != event.Kind {
		return session.Manifest{}, errors.New("sessionstore: frame event receipt is unavailable")
	}
	projected := session.Manifest{
		SchemaVersion: manifest.SchemaVersion, Session: manifest.Session,
		Segments: make(map[string]session.SegmentRecord), Attempts: make(map[string]session.RunAttemptRecord),
		Transitions: make(map[string]session.TransitionRecord), Occurrences: make(map[string]session.OccurrenceRecord),
		AcceptedCommands: map[string]session.CommandReceipt{event.CommandID: receipt},
	}
	projectAttempt := func(runID string) error {
		attempt, found := manifest.Attempts[runID]
		if !found || attempt.SegmentID == "" {
			return errors.New("sessionstore: frame event attempt is unavailable")
		}
		segment, found := manifest.Segments[attempt.SegmentID]
		if !found {
			return errors.New("sessionstore: frame event segment is unavailable")
		}
		segment.AttemptRunIDs = append([]string(nil), segment.AttemptRunIDs...)
		projected.Attempts[runID] = attempt
		projected.Segments[segment.SegmentID] = segment
		return nil
	}
	switch event.Kind {
	case session.EventSessionCreated:
		var payload sessionCreatedPayload
		if err := decodeStrictJSON(event.Payload, &payload); err != nil {
			return session.Manifest{}, err
		}
		if err := projectAttempt(payload.Attempt.RunID); err != nil {
			return session.Manifest{}, err
		}
	case session.EventSessionClosed:
		var payload sessionClosedPayload
		if err := decodeStrictJSON(event.Payload, &payload); err != nil {
			return session.Manifest{}, err
		}
		if payload.RunID != "" {
			if err := projectAttempt(payload.RunID); err != nil {
				return session.Manifest{}, err
			}
		}
	case session.EventAttemptUpdated:
		var payload attemptUpdatedPayload
		if err := decodeStrictJSON(event.Payload, &payload); err != nil {
			return session.Manifest{}, err
		}
		if err := projectAttempt(payload.RunID); err != nil {
			return session.Manifest{}, err
		}
	case session.EventExecutionCommitted:
		var payload executionCommittedPayload
		if err := decodeStrictJSON(event.Payload, &payload); err != nil {
			return session.Manifest{}, err
		}
		if err := projectAttempt(payload.RunID); err != nil {
			return session.Manifest{}, err
		}
	case session.EventSegmentRevised:
		var payload segmentRevisedPayload
		if err := decodeStrictJSON(event.Payload, &payload); err != nil {
			return session.Manifest{}, err
		}
		if err := projectAttempt(payload.RunID); err != nil {
			return session.Manifest{}, err
		}
	case session.EventSessionPaused:
		var payload sessionPausedPayload
		if err := decodeStrictJSON(event.Payload, &payload); err != nil {
			return session.Manifest{}, err
		}
		if err := projectAttempt(payload.RunID); err != nil {
			return session.Manifest{}, err
		}
	case session.EventSessionResumed:
		var payload sessionResumedPayload
		if err := decodeStrictJSON(event.Payload, &payload); err != nil {
			return session.Manifest{}, err
		}
		if err := projectAttempt(payload.RunID); err != nil {
			return session.Manifest{}, err
		}
	case session.EventSessionDetached:
		if manifest.Session.ActiveRunID != "" {
			if err := projectAttempt(manifest.Session.ActiveRunID); err != nil {
				return session.Manifest{}, err
			}
		}
	case session.EventTransitionAborted:
		var payload transitionAbortedPayload
		if err := decodeStrictJSON(event.Payload, &payload); err != nil {
			return session.Manifest{}, err
		}
		transition, found := manifest.Transitions[payload.TransitionID]
		if !found {
			return session.Manifest{}, errors.New("sessionstore: frame event transition is unavailable")
		}
		transition.SourceOccurrence.CallPath = append(
			[]engine.DebugCallFrame(nil), transition.SourceOccurrence.CallPath...,
		)
		transition.SourceOccurrence.FrameStack = append([]string(nil), transition.SourceOccurrence.FrameStack...)
		transition.FactRefs = append([]session.FactRef(nil), transition.FactRefs...)
		if transition.ContextBindings != nil {
			transition.ContextBindings = make(map[string]string, len(transition.ContextBindings))
			for name, value := range manifest.Transitions[payload.TransitionID].ContextBindings {
				transition.ContextBindings[name] = value
			}
		}
		projected.Transitions[payload.TransitionID] = transition
	default:
		return session.Manifest{}, fmt.Errorf("sessionstore: event %q has no frame manifest projection", event.Kind)
	}
	return projected, nil
}

func (store *DirStore) ReadEvents(ctx context.Context, sessionID string, afterSequence int64) ([]session.Event, error) {
	store.commitMu.Lock()
	defer store.commitMu.Unlock()
	if afterSequence < 0 {
		return nil, errors.New("sessionstore: after sequence cannot be negative")
	}
	events, err := store.loadEvents(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	index := 0
	for index < len(events) && events[index].Sequence <= afterSequence {
		index++
	}
	return append([]session.Event(nil), events[index:]...), nil
}

func (store *DirStore) ReadBlob(ctx context.Context, sessionID, digest string) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateUUID("session id", sessionID); err != nil {
		return nil, err
	}
	if !validDigest(digest) {
		return nil, errors.New("sessionstore: invalid blob digest")
	}
	data, err := readFileBounded(store.blobPath(sessionID, digest), maxBlobBytes)
	if err != nil {
		return nil, err
	}
	if !json.Valid(data) || session.DigestBytes(data) != digest {
		return nil, errors.New("sessionstore: blob digest mismatch")
	}
	return append(json.RawMessage(nil), data...), nil
}

func (store *DirStore) LoadSegmentGraphRevision(
	ctx context.Context,
	sessionID string,
	segmentID string,
	revision int64,
) (session.SegmentRecord, json.RawMessage, error) {
	store.commitMu.Lock()
	defer store.commitMu.Unlock()
	if err := ctx.Err(); err != nil {
		return session.SegmentRecord{}, nil, err
	}
	if err := validateUUID("session id", sessionID); err != nil {
		return session.SegmentRecord{}, nil, err
	}
	if err := validateUUID("segment id", segmentID); err != nil || revision < 1 {
		return session.SegmentRecord{}, nil, errors.New("sessionstore: invalid segment graph revision identity")
	}
	events, err := store.loadEvents(ctx, sessionID)
	if err != nil {
		return session.SegmentRecord{}, nil, err
	}
	if _, err := store.manifestForEvents(sessionID, events); err != nil {
		return session.SegmentRecord{}, nil, err
	}
	var segment session.SegmentRecord
	for _, event := range events {
		switch event.Kind {
		case session.EventSessionCreated:
			var payload sessionCreatedPayload
			if err := decodeStrictJSON(event.Payload, &payload); err != nil {
				return session.SegmentRecord{}, nil, err
			}
			if payload.Segment.SegmentID == segmentID {
				segment = payload.Segment
			}
		case session.EventTransitionCommitted:
			var payload transitionCommittedPayload
			if err := decodeStrictJSON(event.Payload, &payload); err != nil {
				return session.SegmentRecord{}, nil, err
			}
			if payload.TargetSegment.SegmentID == segmentID {
				segment = payload.TargetSegment
			}
		case session.EventSegmentRevised:
			var payload segmentRevisedPayload
			if err := decodeStrictJSON(event.Payload, &payload); err != nil {
				return session.SegmentRecord{}, nil, err
			}
			if payload.SegmentID == segmentID && segment.SegmentID == segmentID {
				segment.ExecutableRevision = payload.ExecutableRevision
				segment.GraphRevision = payload.GraphRevision
				segment.PlanHash = payload.PlanHash
				segment.ExecutableSnapshotHash = payload.ExecutableSnapshotHash
				segment.GraphHash = payload.GraphHash
			}
		}
		if segment.SegmentID == segmentID && segment.GraphRevision == revision {
			data, err := store.ReadBlob(ctx, sessionID, segment.GraphHash)
			if err != nil {
				return session.SegmentRecord{}, nil, err
			}
			return segment, data, nil
		}
	}
	return session.SegmentRecord{}, nil, os.ErrNotExist
}

func validateCreateRequest(request session.CreateRequest) error {
	if request.EntryDigest == "" || !validDigest(request.EntryDigest) {
		return errors.New("sessionstore: entry digest is required")
	}
	if session.CreationDigest(request.Identity) != request.EntryDigest {
		return errors.New("sessionstore: creation digest does not match identity")
	}
	if err := session.ValidateCreationIdentity(
		request.Identity, request.SessionID, request.Session, request.Segment, request.Attempt,
	); err != nil {
		return fmt.Errorf("sessionstore: %w", err)
	}
	if err := validateUUID("segment id", request.Segment.SegmentID); err != nil {
		return err
	}
	if err := validateUUID("run id", request.Attempt.RunID); err != nil {
		return err
	}
	validStartingTopology := request.Segment.Status == session.SegmentStatusPrepared && request.Attempt.Status == session.AttemptStatusStarting
	validRunningTopology := request.Segment.Status == session.SegmentStatusActive && request.Attempt.Status == session.AttemptStatusRunning
	if request.Session.Status != session.StatusActive || !validStartingTopology && !validRunningTopology ||
		request.Segment.Ordinal != 1 || request.Attempt.Ordinal != 1 ||
		request.Attempt.SegmentID != request.Segment.SegmentID || request.Segment.RunbookID == "" ||
		request.Segment.EntrySelector.Step != "$entry" || request.Segment.ExecutableRevision != 1 ||
		request.Segment.GraphRevision != 1 || request.Attempt.PlanHash != request.Segment.PlanHash ||
		!validAttemptMode(request.Attempt.Mode) ||
		len(request.Segment.AttemptRunIDs) != 1 || request.Segment.AttemptRunIDs[0] != request.Attempt.RunID {
		return errors.New("sessionstore: invalid root session topology")
	}
	if !validHash(request.Segment.PlanHash) {
		return errors.New("sessionstore: invalid segment plan hash")
	}
	for _, digest := range []string{request.Segment.GraphHash, request.Segment.ExecutableSnapshotHash} {
		if !validDigest(digest) {
			return errors.New("sessionstore: invalid segment blob reference")
		}
	}
	return nil
}

func validateTransitionPreparation(request session.PrepareTransitionRequest) error {
	payload := transitionPreparedPayload{
		Transition: request.Transition, TargetSegment: request.TargetSegment, TargetAttempt: request.TargetAttempt,
	}
	if err := validatePreparedTransitionPayload(payload, ""); err != nil {
		return err
	}
	if len(request.Blobs) == 0 || len(request.Blobs) > maxProjectionBlobs {
		return errors.New("sessionstore: transition blobs are required")
	}
	return nil
}

func validatePreparedTransitionPayload(payload transitionPreparedPayload, preparedAt string) error {
	transition := payload.Transition
	segment := payload.TargetSegment
	attempt := payload.TargetAttempt
	for label, value := range map[string]string{
		"transition id":     transition.TransitionID,
		"source segment id": transition.SourceSegmentID,
		"source run id":     transition.SourceOccurrence.RunID,
		"target segment id": transition.TargetSegmentID,
		"target run id":     transition.TargetRunID,
	} {
		if err := validateUUID(label, value); err != nil {
			return err
		}
	}
	if transition.Status != session.TransitionStatusPrepared || transition.PreparedAt != preparedAt ||
		transition.CommittedAt != "" || transition.AbortedAt != "" ||
		transition.TargetSegmentID != segment.SegmentID || transition.TargetRunID != attempt.RunID ||
		transition.TargetRunbookID == "" || transition.TargetRunbookName == "" ||
		transition.TargetRunbookID != segment.RunbookID || transition.TargetRunbookName != segment.RunbookName ||
		strings.TrimSpace(transition.ReasonCode) == "" || strings.TrimSpace(transition.ReasonSummary) == "" ||
		transition.SourceOccurrence.QualifiedNodeID == "" || transition.SourceOccurrence.StepID == "" ||
		transition.SourceOccurrence.Phase != string(engine.ExecutionPhaseExecute) ||
		transition.SourceOccurrence.Invocation < 1 || transition.SourceOccurrence.RetryAttempt < 1 ||
		transition.SourceOccurrence.OccurrenceSequence < 1 ||
		transition.SourceOccurrence.QualifiedNodeID != engine.DebugNodeID(
			transition.SourceOccurrence.CallPath, transition.SourceOccurrence.StepID,
		) || transition.SourceOccurrence.FrameID == "" &&
		(transition.SourceOccurrence.FrameStepIndex != 0 || len(transition.SourceOccurrence.FrameStack) != 0 ||
			transition.SourceOccurrence.BranchLabel != "" || transition.SourceOccurrence.IterationIndex != 0) ||
		transition.SourceOccurrence.FrameID != "" &&
			(transition.SourceOccurrence.FrameStepIndex < 0 || len(transition.SourceOccurrence.FrameStack) == 0 ||
				transition.SourceOccurrence.FrameStack[len(transition.SourceOccurrence.FrameStack)-1] !=
					transition.SourceOccurrence.FrameID) {
		return errors.New("sessionstore: invalid transition identity")
	}
	if transition.SourceSegmentID == transition.TargetSegmentID ||
		transition.SourceOccurrence.RunID == transition.TargetRunID ||
		segment.Status != session.SegmentStatusPrepared || segment.Ordinal < 2 ||
		segment.EntrySelector.Step != "$entry" || segment.PlanHash != transition.TargetPlanHash ||
		segment.GraphHash != transition.TargetGraphHash ||
		segment.ExecutableSnapshotHash != transition.TargetExecutableSnapshotHash ||
		len(segment.AttemptRunIDs) != 1 || segment.AttemptRunIDs[0] != attempt.RunID ||
		segment.ExecutableRevision != 1 || segment.GraphRevision != 1 ||
		attempt.SegmentID != segment.SegmentID || attempt.Ordinal != 1 ||
		attempt.Status != session.AttemptStatusStarting || !validAttemptMode(attempt.Mode) ||
		attempt.PlanHash != transition.TargetPlanHash || attempt.StartedAt != "" || attempt.UpdatedAt != "" ||
		attempt.CompletedAt != "" || attempt.RunProjectionHash != "" || attempt.ExecutionMutationHash != "" ||
		attempt.CheckpointSequence != 0 || attempt.CommittedTraceSequence != 0 || attempt.JournaledTraceSequence != 0 {
		return errors.New("sessionstore: invalid transition target topology")
	}
	if !validHash(transition.TargetPlanHash) || !validDigest(transition.TargetGraphHash) ||
		!validDigest(transition.TargetExecutableSnapshotHash) || !validDigest(transition.ContextDigest) ||
		!validDigest(transition.IdempotencyKey) {
		return errors.New("sessionstore: invalid transition digest")
	}
	for _, digest := range []string{segment.CatalogDigest, segment.PackageLockDigest, segment.ProfileDigest} {
		if digest != "" && !validDigest(digest) {
			return errors.New("sessionstore: invalid transition dependency digest")
		}
	}
	if len(transition.ContextBindings) > 1024 || len(transition.FactRefs) > 1024 {
		return errors.New("sessionstore: transition context allowlist is too large")
	}
	for name, source := range transition.ContextBindings {
		if strings.TrimSpace(name) == "" || strings.TrimSpace(source) == "" || sensitive.Name(name) {
			return errors.New("sessionstore: invalid transition context binding")
		}
	}
	facts := make(map[string]bool, len(transition.FactRefs))
	for _, fact := range transition.FactRefs {
		if strings.TrimSpace(fact.Name) == "" || strings.TrimSpace(fact.Source) == "" ||
			sensitive.Name(fact.Name) || facts[fact.Name] {
			return errors.New("sessionstore: invalid transition fact reference")
		}
		facts[fact.Name] = true
	}
	return nil
}

func validateTransitionContext(transition session.TransitionRecord, data json.RawMessage) error {
	if len(data) == 0 || len(data) > 1<<20 {
		return errors.New("sessionstore: handoff context exceeds limits")
	}
	var handoffContext session.HandoffContext
	if err := decodeStrictJSON(data, &handoffContext); err != nil {
		return errors.New("sessionstore: invalid handoff context")
	}
	if len(handoffContext.Inputs) != len(transition.ContextBindings) ||
		len(handoffContext.Facts) != len(transition.FactRefs) ||
		invalidSensitiveValue(handoffContext.Inputs, 0) || invalidSensitiveValue(handoffContext.Facts, 0) {
		return errors.New("sessionstore: handoff context does not match its allowlist")
	}
	for name := range transition.ContextBindings {
		if _, found := handoffContext.Inputs[name]; !found {
			return errors.New("sessionstore: handoff input is missing")
		}
	}
	for _, fact := range transition.FactRefs {
		if _, found := handoffContext.Facts[fact.Name]; !found {
			return errors.New("sessionstore: handoff fact is missing")
		}
	}
	return nil
}

func invalidSensitiveValue(value any, depth int) bool {
	if depth > 64 {
		return true
	}
	switch typed := value.(type) {
	case map[string]any:
		for name, item := range typed {
			if sensitive.Name(name) || invalidSensitiveValue(item, depth+1) {
				return true
			}
		}
	case []any:
		for _, item := range typed {
			if invalidSensitiveValue(item, depth+1) {
				return true
			}
		}
	}
	return false
}

func findPreparedTransition(events []session.Event, transitionID string) (transitionPreparedPayload, bool, error) {
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.Kind != session.EventTransitionPrepared {
			continue
		}
		var payload transitionPreparedPayload
		if err := decodeStrictJSON(event.Payload, &payload); err != nil {
			return transitionPreparedPayload{}, false, err
		}
		if payload.Transition.TransitionID == transitionID {
			return payload, true, nil
		}
	}
	return transitionPreparedPayload{}, false, nil
}

func validAttemptMode(mode string) bool {
	switch mode {
	case "real", "dry-run", "replay", "route-test":
		return true
	default:
		return false
	}
}

func isCloseStatus(status session.Status) bool {
	switch status {
	case session.StatusResolved, session.StatusEscalated, session.StatusCancelled, session.StatusAbandoned:
		return true
	default:
		return false
	}
}

func validAttemptStatus(status session.AttemptStatus) bool {
	switch status {
	case session.AttemptStatusStarting, session.AttemptStatusRunning, session.AttemptStatusWaiting,
		session.AttemptStatusPausedAtBoundary, session.AttemptStatusHandoffPending,
		session.AttemptStatusCompleted, session.AttemptStatusFailed,
		session.AttemptStatusCancelled, session.AttemptStatusIndeterminate:
		return true
	default:
		return false
	}
}

func validAttemptTransition(from, to session.AttemptStatus) bool {
	if from == to {
		return from == session.AttemptStatusStarting || from == session.AttemptStatusRunning ||
			from == session.AttemptStatusWaiting || from == session.AttemptStatusPausedAtBoundary ||
			from == session.AttemptStatusHandoffPending
	}
	switch from {
	case session.AttemptStatusStarting:
		return to == session.AttemptStatusRunning || to == session.AttemptStatusFailed ||
			to == session.AttemptStatusCancelled || to == session.AttemptStatusIndeterminate
	case session.AttemptStatusRunning:
		return to == session.AttemptStatusWaiting || to == session.AttemptStatusPausedAtBoundary ||
			to == session.AttemptStatusHandoffPending ||
			to == session.AttemptStatusCompleted || to == session.AttemptStatusFailed ||
			to == session.AttemptStatusCancelled || to == session.AttemptStatusIndeterminate
	case session.AttemptStatusWaiting:
		return to == session.AttemptStatusRunning || to == session.AttemptStatusPausedAtBoundary ||
			to == session.AttemptStatusHandoffPending ||
			to == session.AttemptStatusCompleted || to == session.AttemptStatusFailed ||
			to == session.AttemptStatusCancelled || to == session.AttemptStatusIndeterminate
	case session.AttemptStatusPausedAtBoundary:
		return to == session.AttemptStatusRunning || to == session.AttemptStatusCancelled ||
			to == session.AttemptStatusIndeterminate
	case session.AttemptStatusHandoffPending:
		return to == session.AttemptStatusCancelled || to == session.AttemptStatusIndeterminate
	case session.AttemptStatusIndeterminate:
		return to == session.AttemptStatusRunning || to == session.AttemptStatusCancelled
	default:
		return false
	}
}

func isTerminalSessionStatus(status session.Status) bool {
	switch status {
	case session.StatusResolved, session.StatusEscalated, session.StatusCancelled, session.StatusAbandoned:
		return true
	default:
		return false
	}
}

func isTerminalAttemptStatus(status session.AttemptStatus) bool {
	switch status {
	case session.AttemptStatusCompleted, session.AttemptStatusFailed,
		session.AttemptStatusCancelled, session.AttemptStatusIndeterminate:
		return true
	default:
		return false
	}
}

func isPausedAttemptStatus(status session.AttemptStatus) bool {
	return status == session.AttemptStatusPausedAtBoundary || status == session.AttemptStatusHandoffPending
}

func newEvent(
	sessionID string,
	writerEpoch uint64,
	sequence int64,
	previousDigest string,
	commandID string,
	commandDigest string,
	kind session.EventKind,
	timestamp string,
	payload json.RawMessage,
) (session.Event, error) {
	event := session.Event{
		SchemaVersion: session.EventSchemaV1, EventID: uuid.NewString(), SessionID: sessionID,
		Sequence: sequence, WriterEpoch: writerEpoch, CommandID: commandID, CommandDigest: commandDigest,
		Kind: kind, Timestamp: timestamp, PreviousDigest: previousDigest,
		Payload: append(json.RawMessage(nil), payload...),
	}
	digest, err := eventDigest(event)
	if err != nil {
		return session.Event{}, err
	}
	event.Digest = digest
	return event, nil
}

func newEventWithClientCommand(
	sessionID string,
	writerEpoch uint64,
	sequence int64,
	previousDigest string,
	commandID string,
	commandDigest string,
	clientCommandDigest string,
	kind session.EventKind,
	timestamp string,
	payload json.RawMessage,
) (session.Event, error) {
	event, err := newEvent(
		sessionID, writerEpoch, sequence, previousDigest, commandID, commandDigest, kind, timestamp, payload,
	)
	if err != nil {
		return session.Event{}, err
	}
	event.ClientCommandDigest = clientCommandDigest
	event.Digest, err = eventDigest(event)
	return event, err
}

func eventDigest(event session.Event) (string, error) {
	event.Digest = ""
	encoded, err := json.Marshal(event)
	if err != nil {
		return "", err
	}
	return session.DigestBytes(encoded), nil
}

func (store *DirStore) appendEvent(event session.Event) error {
	cacheAdvanced := false
	defer func() {
		if !cacheAdvanced {
			store.invalidateJournalCache(event.SessionID)
		}
	}()
	directory := store.SessionDir(event.SessionID)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	if err := store.truncateJournalToCommittedHead(event.SessionID); err != nil {
		return err
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if len(encoded) > maxJournalEventBytes {
		return fmt.Errorf("sessionstore: journal event exceeds %d bytes", maxJournalEventBytes)
	}
	encoded = append(encoded, '\n')
	file, err := os.OpenFile(store.eventsPath(event.SessionID), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(encoded); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := syncDirectory(directory); err != nil {
		return err
	}
	if err := store.writeJournalHead(journalHead{
		SchemaVersion: journalHeadSchemaV1, SessionID: event.SessionID, Sequence: event.Sequence,
		EventDigest: event.Digest, WriterEpoch: event.WriterEpoch,
	}); err != nil {
		return fmt.Errorf("%w: journal head: %v", session.ErrProjection, err)
	}
	if err := store.advanceJournalCache(event, int64(len(encoded))); err != nil {
		return fmt.Errorf("%w: incremental manifest: %v", session.ErrProjection, err)
	}
	cacheAdvanced = true
	store.cacheCommittedFrameEvent(event)
	return nil
}

func (store *DirStore) truncateJournalToCommittedHead(sessionID string) error {
	path := store.eventsPath(sessionID)
	committedOffset, cached := store.cachedCommittedOffset(sessionID)
	if !cached {
		_, scannedOffset, scanErr := store.scanJournal(context.Background(), sessionID)
		if scanErr != nil && !errors.Is(scanErr, os.ErrNotExist) {
			return scanErr
		}
		committedOffset = scannedOffset
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() == committedOffset {
		return nil
	}
	if info.Size() < committedOffset {
		return errors.New("sessionstore: journal changed while truncating incomplete tail")
	}
	if err := file.Truncate(committedOffset); err != nil {
		return err
	}
	return file.Sync()
}

func (store *DirStore) writeJournalHead(head journalHead) error {
	encoded, err := json.Marshal(head)
	if err != nil {
		return err
	}
	directory := store.headsDir(head.SessionID)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	path := store.headPath(head.SessionID, head.Sequence)
	if err := publishImmutableFile(path, encoded, 0o600, 4096); err != nil {
		if errors.Is(err, errImmutableFileConflict) {
			return errors.New("sessionstore: committed journal head conflicts")
		}
		return err
	}
	return nil
}

func (store *DirStore) loadJournalHead(sessionID string) (journalHead, error) {
	entries, err := os.ReadDir(store.headsDir(sessionID))
	if err != nil {
		return journalHead{}, err
	}
	candidates := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "head-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		candidates = append(candidates, entry.Name())
	}
	sort.Sort(sort.Reverse(sort.StringSlice(candidates)))
	for _, candidate := range candidates {
		data, readErr := readFileBounded(filepath.Join(store.headsDir(sessionID), candidate), 4096)
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return journalHead{}, readErr
		}
		var head journalHead
		if decodeStrictJSON(data, &head) != nil || head.SchemaVersion != journalHeadSchemaV1 ||
			head.SessionID != sessionID || head.Sequence < 1 || !validDigest(head.EventDigest) ||
			head.WriterEpoch == 0 || candidate != fmt.Sprintf("head-%020d.json", head.Sequence) {
			continue
		}
		return head, nil
	}
	return journalHead{}, os.ErrNotExist
}

func (store *DirStore) loadEvents(ctx context.Context, sessionID string) ([]session.Event, error) {
	if cache := store.writerJournalCache(sessionID); cache != nil {
		return cache.events, nil
	}
	events, committedOffset, err := store.scanJournal(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if len(events) == 0 {
		return nil, os.ErrNotExist
	}
	store.primeJournalCache(sessionID, events, committedOffset)
	return events, nil
}

func (store *DirStore) manifestForEvents(sessionID string, events []session.Event) (session.Manifest, error) {
	if cache := store.writerJournalCache(sessionID); cache != nil &&
		len(cache.events) == len(events) && len(events) > 0 &&
		cache.events[len(cache.events)-1].Digest == events[len(events)-1].Digest {
		return cache.rebuild.manifest, nil
	}
	return rebuildManifest(events)
}

func (store *DirStore) activeWriterEpoch(sessionID string) uint64 {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.leaseEpochs[sessionID]
}

func (store *DirStore) writerJournalCache(sessionID string) *journalStateCache {
	epoch := store.activeWriterEpoch(sessionID)
	if epoch == 0 {
		return nil
	}
	store.journalMu.Lock()
	defer store.journalMu.Unlock()
	cache := store.journalCaches[sessionID]
	if cache == nil || cache.writerEpoch != epoch {
		return nil
	}
	return cache
}

func (store *DirStore) primeJournalCache(sessionID string, events []session.Event, committedOffset int64) {
	epoch := store.activeWriterEpoch(sessionID)
	if epoch == 0 || len(events) == 0 {
		return
	}
	state := newManifestRebuildState(len(events))
	if _, err := rebuildManifestFromState(state, events, nil); err != nil {
		return
	}
	copied := make([]session.Event, len(events))
	copy(copied, events)
	store.journalMu.Lock()
	store.journalCaches[sessionID] = &journalStateCache{
		writerEpoch: epoch, events: copied, rebuild: state, committedOffset: committedOffset,
	}
	store.journalMu.Unlock()
}

func (store *DirStore) advanceJournalCache(event session.Event, encodedBytes int64) error {
	epoch := store.activeWriterEpoch(event.SessionID)
	if epoch == 0 || epoch != event.WriterEpoch {
		return nil
	}
	store.journalMu.Lock()
	defer store.journalMu.Unlock()
	cache := store.journalCaches[event.SessionID]
	if cache == nil {
		if event.Sequence != 1 {
			return nil
		}
		cache = &journalStateCache{writerEpoch: epoch, rebuild: newManifestRebuildState(1)}
		store.journalCaches[event.SessionID] = cache
	}
	if cache.writerEpoch != epoch {
		delete(store.journalCaches, event.SessionID)
		return nil
	}
	if _, err := rebuildManifestFromState(cache.rebuild, []session.Event{event}, nil); err != nil {
		delete(store.journalCaches, event.SessionID)
		return err
	}
	next := make([]session.Event, len(cache.events)+1)
	copy(next, cache.events)
	next[len(cache.events)] = event
	cache.events = next
	cache.committedOffset += encodedBytes
	return nil
}

func (store *DirStore) cachedCommittedOffset(sessionID string) (int64, bool) {
	cache := store.writerJournalCache(sessionID)
	if cache == nil {
		return 0, false
	}
	return cache.committedOffset, true
}

func (store *DirStore) invalidateJournalCache(sessionID string) {
	store.journalMu.Lock()
	delete(store.journalCaches, sessionID)
	store.journalMu.Unlock()
}

func (store *DirStore) scanJournal(ctx context.Context, sessionID string) ([]session.Event, int64, error) {
	return store.scanJournalWithObserver(ctx, sessionID, nil)
}

func (store *DirStore) scanJournalWithObserver(
	ctx context.Context,
	sessionID string,
	observer func(session.Event, session.Manifest) error,
) ([]session.Event, int64, error) {
	store.journalScans.Add(1)
	if err := validateUUID("session id", sessionID); err != nil {
		return nil, 0, err
	}
	head, headErr := store.loadJournalHead(sessionID)
	if headErr != nil && !errors.Is(headErr, os.ErrNotExist) {
		return nil, 0, headErr
	}
	file, err := os.Open(store.eventsPath(sessionID))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && headErr == nil {
			return nil, 0, errors.New("sessionstore: committed journal head has no journal")
		}
		return nil, 0, err
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 64*1024)
	events := make([]session.Event, 0)
	committedOffset := int64(0)
	for {
		if err := ctx.Err(); err != nil {
			return nil, committedOffset, err
		}
		line, readErr := reader.ReadBytes('\n')
		if errors.Is(readErr, io.EOF) {
			if len(line) > maxJournalEventBytes {
				return nil, committedOffset, errors.New("sessionstore: incomplete journal tail is too large")
			}
			break
		}
		if readErr != nil {
			return nil, committedOffset, readErr
		}
		committedOffset += int64(len(line))
		line = bytes.TrimSpace(line)
		if len(line) == 0 || len(line) > maxJournalEventBytes {
			return nil, committedOffset, errors.New("sessionstore: invalid journal event size")
		}
		var event session.Event
		if err := decodeStrictJSON(line, &event); err != nil {
			return nil, committedOffset, fmt.Errorf("sessionstore: decode journal event %d: %w", len(events)+1, err)
		}
		if event.SessionID != sessionID {
			return nil, committedOffset, errors.New("sessionstore: journal event belongs to a different session directory")
		}
		events = append(events, event)
		if len(events) > maxJournalEvents {
			return nil, committedOffset, errors.New("sessionstore: journal event limit exceeded")
		}
	}
	if headErr == nil {
		if head.Sequence > int64(len(events)) {
			return nil, committedOffset, errors.New("sessionstore: journal is shorter than its committed head")
		}
		anchored := events[head.Sequence-1]
		if anchored.Digest != head.EventDigest || anchored.WriterEpoch != head.WriterEpoch {
			return nil, committedOffset, errors.New("sessionstore: journal does not match its committed head")
		}
	}
	if len(events) > 0 {
		rebuilt, err := rebuildManifestWithObserver(events, observer)
		if err != nil {
			return nil, committedOffset, err
		}
		if err := store.validateCreationReservation(events[0]); err != nil {
			return nil, committedOffset, err
		}
		if err := store.validateReferencedEventBlobs(ctx, sessionID, events, rebuilt); err != nil {
			return nil, committedOffset, err
		}
	}
	return events, committedOffset, nil
}

func (store *DirStore) validateReferencedEventBlobs(
	ctx context.Context,
	sessionID string,
	events []session.Event,
	manifest session.Manifest,
) error {
	for _, event := range events {
		switch event.Kind {
		case session.EventSessionCreated:
			var payload sessionCreatedPayload
			if err := decodeStrictJSON(event.Payload, &payload); err != nil {
				return err
			}
			for _, digest := range []string{payload.Segment.ExecutableSnapshotHash, payload.Segment.GraphHash} {
				if _, err := store.ReadBlob(ctx, sessionID, digest); err != nil {
					return fmt.Errorf("sessionstore: initial segment blob %s: %w", digest, err)
				}
			}
		case session.EventExecutionCommitted:
			var payload executionCommittedPayload
			if err := decodeStrictJSON(event.Payload, &payload); err != nil {
				return err
			}
			mutationData, err := store.ReadBlob(ctx, sessionID, payload.MutationHash)
			if err != nil {
				return fmt.Errorf("sessionstore: execution mutation blob: %w", err)
			}
			var mutation session.ExecutionMutation
			if err := decodeStrictJSON(mutationData, &mutation); err != nil {
				return fmt.Errorf("sessionstore: decode execution mutation blob: %w", err)
			}
			projectionData, err := store.ReadBlob(ctx, sessionID, payload.ProjectionHash)
			if err != nil {
				return fmt.Errorf("sessionstore: execution projection blob: %w", err)
			}
			if err := validateExecutionMutation(mutation, session.NewJSONBlob(projectionData)); err != nil ||
				mutation.RunID != payload.RunID || mutation.AttemptStatus != payload.Status ||
				mutation.PreviousMutationHash != payload.PreviousMutationHash ||
				mutation.StateProjectionHash != payload.ProjectionHash ||
				mutation.FrameProjectionHash != payload.FrameProjectionHash ||
				mutation.CheckpointSequence != payload.CheckpointSequence ||
				mutation.CommittedTraceSequence != payload.CommittedTraceSequence ||
				mutation.RunWriterEpoch != event.WriterEpoch {
				return errors.New("sessionstore: execution event does not match immutable mutation")
			}
			if payload.FrameProjectionHash != "" {
				frameProjectionData, err := store.ReadBlob(ctx, sessionID, payload.FrameProjectionHash)
				if err != nil {
					return fmt.Errorf("sessionstore: execution frame projection blob: %w", err)
				}
				var projected session.ExecutionFrameProjectionContent
				if err := validateExecutionFrameProjection(
					mutation, session.NewJSONBlob(frameProjectionData),
					func(digest string) (json.RawMessage, error) {
						return store.ReadBlob(ctx, sessionID, digest)
					},
					&projected,
				); err != nil {
					return err
				}
				if payload.Occurrences != nil {
					if err := validateOccurrenceRecords(
						manifest, payload.RunID, payload.Occurrences, projected.PendingTraceEvents,
					); err != nil {
						return err
					}
				}
			} else if len(payload.Occurrences) > 0 {
				return errors.New("sessionstore: legacy execution event has occurrence records")
			}
		case session.EventTraceCommitted:
			var payload traceCommittedPayload
			if err := decodeStrictJSON(event.Payload, &payload); err != nil {
				return err
			}
			traceData, err := store.ReadBlob(ctx, sessionID, payload.TraceHash)
			if err != nil {
				return fmt.Errorf("sessionstore: direct trace blob: %w", err)
			}
			var traceEvent engine.Event
			if err := decodeStrictJSON(traceData, &traceEvent); err != nil ||
				traceEvent.RunID != payload.RunID || traceEvent.Sequence != payload.TraceSequence ||
				validateUUID("trace event id", traceEvent.EventID) != nil || traceEvent.Kind == "" ||
				session.ParseTime(traceEvent.Timestamp) != nil {
				return errors.New("sessionstore: trace event does not match immutable trace blob")
			}
			if payload.Occurrence != nil {
				attempt, found := manifest.Attempts[payload.RunID]
				if !found {
					return errors.New("sessionstore: direct trace occurrence attempt is unavailable")
				}
				occurrenceID, expected, terminal, err := session.OccurrenceFromTraceEvent(
					sessionID, attempt.SegmentID, executionSourceForAttempt(attempt), traceEvent,
				)
				if err != nil || !terminal || occurrenceID != session.OccurrenceID(*payload.Occurrence) ||
					session.DigestJSON(expected) != session.DigestJSON(*payload.Occurrence) {
					return errors.New("sessionstore: direct trace occurrence does not match immutable trace blob")
				}
			}
		case session.EventSegmentRevised:
			var payload segmentRevisedPayload
			if err := decodeStrictJSON(event.Payload, &payload); err != nil {
				return err
			}
			for _, digest := range []string{payload.ExecutableSnapshotHash, payload.GraphHash} {
				if _, err := store.ReadBlob(ctx, sessionID, digest); err != nil {
					return fmt.Errorf("sessionstore: segment revision blob %s: %w", digest, err)
				}
			}
		case session.EventTransitionPrepared:
			var payload transitionPreparedPayload
			if err := decodeStrictJSON(event.Payload, &payload); err != nil {
				return err
			}
			for _, digest := range []string{
				payload.Transition.TargetGraphHash,
				payload.Transition.TargetExecutableSnapshotHash,
				payload.Transition.ContextDigest,
			} {
				if _, err := store.ReadBlob(ctx, sessionID, digest); err != nil {
					return fmt.Errorf("sessionstore: transition blob %s: %w", digest, err)
				}
			}
			contextData, err := store.ReadBlob(ctx, sessionID, payload.Transition.ContextDigest)
			if err != nil {
				return err
			}
			if err := validateTransitionContext(payload.Transition, contextData); err != nil {
				return err
			}
		}
	}
	return nil
}

func rebuildManifest(events []session.Event) (session.Manifest, error) {
	return rebuildManifestWithObserver(events, nil)
}

func rebuildManifestWithObserver(
	events []session.Event,
	observer func(session.Event, session.Manifest) error,
) (session.Manifest, error) {
	if len(events) == 0 {
		return session.Manifest{}, os.ErrNotExist
	}
	state := newManifestRebuildState(len(events))
	return rebuildManifestFromState(state, events, observer)
}

type manifestRebuildState struct {
	manifest            session.Manifest
	previousDigest      string
	previousWriterEpoch uint64
	eventIDs            map[string]bool
	preparedTransitions map[string]transitionPreparedPayload
}

func newManifestRebuildState(capacity int) *manifestRebuildState {
	return &manifestRebuildState{manifest: session.Manifest{
		SchemaVersion: session.ManifestSchemaV1,
		Segments:      make(map[string]session.SegmentRecord), Attempts: make(map[string]session.RunAttemptRecord),
		Transitions: make(map[string]session.TransitionRecord), Occurrences: make(map[string]session.OccurrenceRecord),
		AcceptedCommands: make(map[string]session.CommandReceipt),
	}, eventIDs: make(map[string]bool, capacity), preparedTransitions: make(map[string]transitionPreparedPayload)}
}

func rebuildManifestFromState(
	state *manifestRebuildState,
	events []session.Event,
	observer func(session.Event, session.Manifest) error,
) (session.Manifest, error) {
	if state == nil {
		return session.Manifest{}, errors.New("sessionstore: manifest rebuild state is required")
	}
	manifest := state.manifest
	previousDigest := state.previousDigest
	previousWriterEpoch := state.previousWriterEpoch
	eventIDs := state.eventIDs
	preparedTransitions := state.preparedTransitions
	for _, event := range events {
		index := int(manifest.Session.Sequence)
		if event.SchemaVersion != session.EventSchemaV1 || event.Sequence != int64(index+1) ||
			event.SessionID == "" || event.WriterEpoch == 0 || event.EventID == "" || event.CommandID == "" ||
			event.CommandDigest == "" || event.Kind == "" || !json.Valid(event.Payload) || event.PreviousDigest != previousDigest {
			return session.Manifest{}, fmt.Errorf("sessionstore: invalid journal event at sequence %d", index+1)
		}
		if validateUUID("event session id", event.SessionID) != nil || validateUUID("event id", event.EventID) != nil ||
			validateUUID("event command id", event.CommandID) != nil || eventIDs[event.EventID] ||
			index > 0 && event.SessionID != manifest.Session.SessionID || event.WriterEpoch < previousWriterEpoch {
			return session.Manifest{}, fmt.Errorf("sessionstore: invalid journal event identity at sequence %d", index+1)
		}
		eventIDs[event.EventID] = true
		if err := session.ParseTime(event.Timestamp); err != nil {
			return session.Manifest{}, fmt.Errorf("sessionstore: invalid journal timestamp at sequence %d", index+1)
		}
		digest, err := eventDigest(event)
		if err != nil || digest != event.Digest {
			return session.Manifest{}, fmt.Errorf("sessionstore: journal digest mismatch at sequence %d", index+1)
		}
		if event.ClientCommandDigest != "" && !validDigest(event.ClientCommandDigest) {
			return session.Manifest{}, fmt.Errorf("sessionstore: invalid client command digest at sequence %d", index+1)
		}
		if receipt, found := manifest.AcceptedCommands[event.CommandID]; found {
			if receipt.CommandDigest != event.CommandDigest ||
				receipt.ClientCommandDigest != event.ClientCommandDigest || receipt.EventKind != event.Kind {
				return session.Manifest{}, session.ErrCommandConflict
			}
			return session.Manifest{}, fmt.Errorf("sessionstore: duplicate command event at sequence %d", index+1)
		}
		switch event.Kind {
		case session.EventSessionCreated:
			if index != 0 {
				return session.Manifest{}, errors.New("sessionstore: session.created must be first")
			}
			var payload sessionCreatedPayload
			if err := decodeStrictJSON(event.Payload, &payload); err != nil {
				return session.Manifest{}, err
			}
			if payload.Session.SessionID != event.SessionID || payload.Session.CreationCommandID != event.CommandID ||
				payload.Session.CreationDigest != event.CommandDigest || payload.Segment.SegmentID == "" ||
				payload.Attempt.SegmentID != payload.Segment.SegmentID || payload.Session.WriterEpoch != event.WriterEpoch {
				return session.Manifest{}, errors.New("sessionstore: invalid session creation payload")
			}
			if payload.Session.RootSegmentID != payload.Segment.SegmentID ||
				payload.Session.ActiveSegmentID != payload.Segment.SegmentID ||
				payload.Session.ActiveRunID != payload.Attempt.RunID || payload.Session.Sequence != 1 ||
				payload.Session.ManifestRevision != 1 || payload.Session.CreatedAt != event.Timestamp ||
				payload.Session.UpdatedAt != event.Timestamp || payload.Attempt.StartedAt != event.Timestamp ||
				payload.Attempt.UpdatedAt != event.Timestamp || payload.Attempt.CompletedAt != "" ||
				payload.Attempt.RunProjectionHash != "" || payload.Attempt.ExecutionMutationHash != "" ||
				payload.Attempt.CheckpointSequence != 0 || payload.Attempt.CommittedTraceSequence != 0 ||
				payload.Attempt.JournaledTraceSequence != 0 || payload.Attempt.PlanHash != payload.Segment.PlanHash ||
				payload.Segment.EntrySelector.Step != "$entry" || payload.Segment.ExecutableRevision != 1 ||
				payload.Segment.GraphRevision != 1 {
				return session.Manifest{}, errors.New("sessionstore: invalid generated session creation topology")
			}
			if event.CommandDigest != session.CreationDigest(payload.Identity) ||
				session.ValidateCreationIdentity(
					payload.Identity, event.SessionID, payload.Session, payload.Segment, payload.Attempt,
				) != nil {
				return session.Manifest{}, errors.New("sessionstore: creation command digest does not match payload")
			}
			if err := validateCreateRequest(session.CreateRequest{
				SessionID: event.SessionID, CommandID: event.CommandID, EntryDigest: event.CommandDigest,
				Identity: payload.Identity, Session: payload.Session, Segment: payload.Segment, Attempt: payload.Attempt,
			}); err != nil {
				return session.Manifest{}, errors.New("sessionstore: invalid replayed session creation")
			}
			manifest.Session = payload.Session
			manifest.Segments[payload.Segment.SegmentID] = payload.Segment
			manifest.Attempts[payload.Attempt.RunID] = payload.Attempt
		case session.EventSessionClosed:
			if manifest.Session.SessionID == "" {
				return session.Manifest{}, errors.New("sessionstore: close before creation")
			}
			var payload sessionClosedPayload
			if err := decodeStrictJSON(event.Payload, &payload); err != nil || !isCloseStatus(payload.Status) {
				return session.Manifest{}, errors.New("sessionstore: invalid close payload")
			}
			if event.CommandDigest != session.CloseDigest(payload.Status) {
				return session.Manifest{}, errors.New("sessionstore: close command digest does not match payload")
			}
			if payload.RunID != "" || payload.SegmentID != "" {
				if payload.RunID == "" || payload.SegmentID == "" ||
					manifest.Session.ActiveRunID != payload.RunID ||
					manifest.Session.ActiveSegmentID != payload.SegmentID ||
					manifest.Attempts[payload.RunID].SegmentID != payload.SegmentID {
					return session.Manifest{}, errors.New("sessionstore: close payload active topology does not match manifest")
				}
			}
			if activeRunID := manifest.Session.ActiveRunID; activeRunID != "" {
				attempt := manifest.Attempts[activeRunID]
				if !isTerminalAttemptStatus(attempt.Status) &&
					!(manifest.Session.Status == session.StatusPaused && isPausedAttemptStatus(attempt.Status)) {
					return session.Manifest{}, errors.New("sessionstore: close event targets an active attempt")
				}
				if isPausedAttemptStatus(attempt.Status) {
					attempt.Status = session.AttemptStatusCancelled
					attempt.UpdatedAt = event.Timestamp
					attempt.CompletedAt = event.Timestamp
					manifest.Attempts[activeRunID] = attempt
					segment := manifest.Segments[attempt.SegmentID]
					segment.Status = session.SegmentStatusCancelled
					manifest.Segments[attempt.SegmentID] = segment
				}
			}
			manifest.Session.Status = payload.Status
			manifest.Session.ActiveSegmentID = ""
			manifest.Session.ActiveRunID = ""
		case session.EventSessionPaused:
			var payload sessionPausedPayload
			if err := decodeStrictJSON(event.Payload, &payload); err != nil || payload.RunID == "" ||
				payload.Reason == "" || event.CommandDigest != session.PauseDigest(payload.RunID, payload.Reason) {
				return session.Manifest{}, errors.New("sessionstore: invalid session pause payload")
			}
			if manifest.Session.ActiveRunID != payload.RunID {
				return session.Manifest{}, errors.New("sessionstore: pause targets a non-active attempt")
			}
			attempt := manifest.Attempts[payload.RunID]
			switch attempt.Status {
			case session.AttemptStatusPausedAtBoundary, session.AttemptStatusHandoffPending:
				manifest.Session.Status = session.StatusPaused
			default:
				return session.Manifest{}, errors.New("sessionstore: pause targets a non-pausable attempt")
			}
		case session.EventSessionResumed:
			var payload sessionResumedPayload
			if err := decodeStrictJSON(event.Payload, &payload); err != nil || payload.RunID == "" ||
				event.CommandDigest != session.ResumeDigest(payload.RunID) {
				return session.Manifest{}, errors.New("sessionstore: invalid session resume payload")
			}
			if manifest.Session.Status != session.StatusPaused || manifest.Session.ActiveRunID != payload.RunID ||
				manifest.Attempts[payload.RunID].Status != session.AttemptStatusPausedAtBoundary {
				return session.Manifest{}, errors.New("sessionstore: resume targets a non-paused attempt")
			}
			if err := applyAttemptStatus(&manifest, payload.RunID, session.AttemptStatusRunning, "", event.Timestamp); err != nil {
				return session.Manifest{}, err
			}
		case session.EventSessionDetached:
			var payload sessionDetachedPayload
			if err := decodeStrictJSON(event.Payload, &payload); err != nil || payload.Reason == "" ||
				len(payload.Reason) > 1024 || strings.ContainsAny(payload.Reason, "\r\n\x00") ||
				event.CommandDigest != session.DetachDigest(payload.Reason) || event.ClientCommandDigest == "" {
				return session.Manifest{}, errors.New("sessionstore: invalid session detach payload")
			}
			if manifest.Session.Status != session.StatusIndeterminate && manifest.Session.ActiveRunID != "" {
				return session.Manifest{}, errors.New("sessionstore: detach receipt targets active execution")
			}
		case session.EventAttemptUpdated:
			var payload attemptUpdatedPayload
			if err := decodeStrictJSON(event.Payload, &payload); err != nil || !validAttemptStatus(payload.Status) {
				return session.Manifest{}, errors.New("sessionstore: invalid attempt update payload")
			}
			if event.CommandDigest != session.AttemptUpdateDigest(payload.RunID, payload.Status) {
				return session.Manifest{}, errors.New("sessionstore: attempt command digest does not match payload")
			}
			if err := applyAttemptStatus(&manifest, payload.RunID, payload.Status, "", event.Timestamp); err != nil {
				return session.Manifest{}, err
			}
		case session.EventExecutionCommitted:
			var payload executionCommittedPayload
			if err := decodeStrictJSON(event.Payload, &payload); err != nil ||
				!validAttemptStatus(payload.Status) || !validDigest(payload.MutationHash) ||
				!validDigest(payload.ProjectionHash) || payload.CheckpointSequence < 0 ||
				payload.CommittedTraceSequence < 0 ||
				payload.PreviousMutationHash != "" && !validDigest(payload.PreviousMutationHash) {
				return session.Manifest{}, errors.New("sessionstore: invalid execution commit payload")
			}
			if event.CommandDigest != session.ExecutionMutationDigest(payload.RunID, payload.MutationHash) {
				return session.Manifest{}, errors.New("sessionstore: execution command digest does not match payload")
			}
			if preparedTransitionUsesRun(manifest, payload.RunID) {
				return session.Manifest{}, errors.New("sessionstore: execution mutation follows a prepared source transition")
			}
			if err := applyExecutionMutation(&manifest, payload, event.Timestamp); err != nil {
				return session.Manifest{}, err
			}
			if err := applyOccurrenceRecords(&manifest, payload.RunID, payload.Occurrences); err != nil {
				return session.Manifest{}, err
			}
		case session.EventTraceCommitted:
			var payload traceCommittedPayload
			if err := decodeStrictJSON(event.Payload, &payload); err != nil ||
				!validDigest(payload.TraceHash) || payload.TraceSequence < 1 {
				return session.Manifest{}, errors.New("sessionstore: invalid trace commit payload")
			}
			if event.CommandDigest != session.TraceCommitDigest(payload.RunID, payload.TraceHash) {
				return session.Manifest{}, errors.New("sessionstore: trace command digest does not match payload")
			}
			if err := applyTraceCommit(&manifest, payload); err != nil {
				return session.Manifest{}, err
			}
			if payload.Occurrence != nil {
				occurrenceID := session.OccurrenceID(*payload.Occurrence)
				if err := applyOccurrenceRecords(
					&manifest, payload.RunID, map[string]session.OccurrenceRecord{occurrenceID: *payload.Occurrence},
				); err != nil {
					return session.Manifest{}, err
				}
			}
		case session.EventSegmentRevised:
			var payload segmentRevisedPayload
			if err := decodeStrictJSON(event.Payload, &payload); err != nil ||
				validateUUID("segment id", payload.SegmentID) != nil || validateUUID("run id", payload.RunID) != nil ||
				payload.ExecutableRevision < 2 || payload.GraphRevision != payload.ExecutableRevision ||
				!validHash(payload.PlanHash) ||
				!validDigest(payload.ExecutableSnapshotHash) || !validDigest(payload.GraphHash) {
				return session.Manifest{}, errors.New("sessionstore: invalid segment revision payload")
			}
			if event.CommandDigest != session.SegmentRevisionDigest(
				payload.SegmentID, payload.RunID, payload.ExecutableRevision, payload.GraphRevision,
				payload.PlanHash, payload.ExecutableSnapshotHash, payload.GraphHash, payload.Resolution, payload.Dispatch,
			) {
				return session.Manifest{}, errors.New("sessionstore: segment revision command digest does not match payload")
			}
			segment, found := manifest.Segments[payload.SegmentID]
			attempt := manifest.Attempts[payload.RunID]
			if !found || manifest.Session.ActiveSegmentID != payload.SegmentID ||
				manifest.Session.ActiveRunID != payload.RunID || attempt.SegmentID != payload.SegmentID ||
				payload.ExecutableRevision != segment.ExecutableRevision+1 ||
				payload.GraphRevision != segment.GraphRevision+1 {
				return session.Manifest{}, errors.New("sessionstore: segment revision is not contiguous for the active attempt")
			}
			if err := validateSegmentResolution(payload.Resolution, event.WriterEpoch); err != nil ||
				payload.Resolution.Revision != payload.ExecutableRevision-1 {
				if err == nil {
					err = errors.New("sessionstore: segment resolution revision does not match segment revision")
				}
				return session.Manifest{}, err
			}
			if err := validateSegmentRevisionDispatch(payload.Dispatch, payload.Resolution, event.WriterEpoch); err != nil {
				return session.Manifest{}, err
			}
			segment.ExecutableRevision = payload.ExecutableRevision
			segment.GraphRevision = payload.GraphRevision
			segment.PlanHash = payload.PlanHash
			segment.ExecutableSnapshotHash = payload.ExecutableSnapshotHash
			segment.GraphHash = payload.GraphHash
			manifest.Segments[payload.SegmentID] = segment
		case session.EventTransitionPrepared:
			var payload transitionPreparedPayload
			if err := decodeStrictJSON(event.Payload, &payload); err != nil ||
				validatePreparedTransitionPayload(payload, event.Timestamp) != nil {
				return session.Manifest{}, errors.New("sessionstore: invalid transition preparation payload")
			}
			if event.CommandDigest != session.TransitionPrepareDigest(
				payload.Transition, payload.TargetSegment, payload.TargetAttempt,
			) {
				return session.Manifest{}, errors.New("sessionstore: transition prepare command digest does not match payload")
			}
			if _, found := manifest.Transitions[payload.Transition.TransitionID]; found {
				return session.Manifest{}, errors.New("sessionstore: duplicate transition id")
			}
			if manifest.Session.ActiveSegmentID != payload.Transition.SourceSegmentID ||
				manifest.Session.ActiveRunID != payload.Transition.SourceOccurrence.RunID {
				return session.Manifest{}, errors.New("sessionstore: transition source is not active")
			}
			sourceAttempt := manifest.Attempts[payload.Transition.SourceOccurrence.RunID]
			if sourceAttempt.SegmentID != payload.Transition.SourceSegmentID ||
				sourceAttempt.Status != session.AttemptStatusHandoffPending {
				return session.Manifest{}, errors.New("sessionstore: transition source is not paused at handoff")
			}
			sourceSegment := manifest.Segments[payload.Transition.SourceSegmentID]
			if payload.TargetSegment.Ordinal != sourceSegment.Ordinal+1 {
				return session.Manifest{}, errors.New("sessionstore: target segment ordinal does not follow its source")
			}
			if _, found := manifest.Segments[payload.TargetSegment.SegmentID]; found {
				return session.Manifest{}, errors.New("sessionstore: target segment already exists")
			}
			if _, found := manifest.Attempts[payload.TargetAttempt.RunID]; found {
				return session.Manifest{}, errors.New("sessionstore: target attempt already exists")
			}
			manifest.Transitions[payload.Transition.TransitionID] = payload.Transition
			preparedTransitions[payload.Transition.TransitionID] = payload
		case session.EventTransitionCommitted:
			var payload transitionCommittedPayload
			if err := decodeStrictJSON(event.Payload, &payload); err != nil ||
				validateUUID("transition id", payload.TransitionID) != nil {
				return session.Manifest{}, errors.New("sessionstore: invalid transition commit payload")
			}
			if event.CommandDigest != session.TransitionCommitDigest(payload.TransitionID) {
				return session.Manifest{}, errors.New("sessionstore: transition commit command digest does not match payload")
			}
			transition, found := manifest.Transitions[payload.TransitionID]
			if !found || transition.Status != session.TransitionStatusPrepared {
				return session.Manifest{}, errors.New("sessionstore: committed transition is not prepared")
			}
			prepared, found := preparedTransitions[payload.TransitionID]
			if !found || !reflect.DeepEqual(payload.TargetSegment, prepared.TargetSegment) ||
				!reflect.DeepEqual(payload.TargetAttempt, prepared.TargetAttempt) {
				return session.Manifest{}, errors.New("sessionstore: committed transition target does not match its preparation")
			}
			sourceAttempt := manifest.Attempts[transition.SourceOccurrence.RunID]
			if sourceAttempt.Status != session.AttemptStatusHandoffPending || sourceAttempt.ExecutionMutationHash == "" {
				return session.Manifest{}, errors.New("sessionstore: committed transition source is no longer handoff-pending")
			}
			if manifest.Session.ActiveSegmentID != transition.SourceSegmentID ||
				manifest.Session.ActiveRunID != transition.SourceOccurrence.RunID ||
				payload.TargetSegment.SegmentID != transition.TargetSegmentID ||
				payload.TargetAttempt.RunID != transition.TargetRunID ||
				payload.TargetAttempt.SegmentID != transition.TargetSegmentID {
				return session.Manifest{}, errors.New("sessionstore: committed transition topology mismatch")
			}
			if _, found := manifest.Segments[payload.TargetSegment.SegmentID]; found {
				return session.Manifest{}, errors.New("sessionstore: duplicate target segment")
			}
			if _, found := manifest.Attempts[payload.TargetAttempt.RunID]; found {
				return session.Manifest{}, errors.New("sessionstore: duplicate target attempt")
			}
			sourceSegment := manifest.Segments[transition.SourceSegmentID]
			sourceSegment.Status = session.SegmentStatusHandedOff
			manifest.Segments[transition.SourceSegmentID] = sourceSegment
			sourceAttempt.Status = session.AttemptStatusCompleted
			sourceAttempt.UpdatedAt = event.Timestamp
			sourceAttempt.CompletedAt = event.Timestamp
			manifest.Attempts[transition.SourceOccurrence.RunID] = sourceAttempt
			targetSegment := prepared.TargetSegment
			manifest.Segments[targetSegment.SegmentID] = targetSegment
			targetAttempt := prepared.TargetAttempt
			targetAttempt.StartedAt = event.Timestamp
			targetAttempt.UpdatedAt = event.Timestamp
			manifest.Attempts[targetAttempt.RunID] = targetAttempt
			transition.Status = session.TransitionStatusCommitted
			transition.CommittedAt = event.Timestamp
			manifest.Transitions[payload.TransitionID] = transition
			manifest.Session.Status = session.StatusActive
			manifest.Session.ActiveSegmentID = targetSegment.SegmentID
			manifest.Session.ActiveRunID = targetAttempt.RunID
		case session.EventTransitionAborted:
			var payload transitionAbortedPayload
			if err := decodeStrictJSON(event.Payload, &payload); err != nil ||
				validateUUID("transition id", payload.TransitionID) != nil || strings.TrimSpace(payload.Reason) == "" {
				return session.Manifest{}, errors.New("sessionstore: invalid transition abort payload")
			}
			if event.CommandDigest != session.TransitionAbortDigest(payload.TransitionID, payload.Reason) {
				return session.Manifest{}, errors.New("sessionstore: transition abort command digest does not match payload")
			}
			transition, found := manifest.Transitions[payload.TransitionID]
			if !found || transition.Status != session.TransitionStatusPrepared {
				return session.Manifest{}, errors.New("sessionstore: aborted transition is not prepared")
			}
			transition.Status = session.TransitionStatusAborted
			transition.AbortedAt = event.Timestamp
			manifest.Transitions[payload.TransitionID] = transition
		default:
			return session.Manifest{}, fmt.Errorf("sessionstore: unsupported event kind %q", event.Kind)
		}
		manifest.Session.Sequence = event.Sequence
		manifest.Session.ManifestRevision = event.Sequence
		manifest.Session.WriterEpoch = event.WriterEpoch
		manifest.Session.UpdatedAt = event.Timestamp
		manifest.AcceptedCommands[event.CommandID] = session.CommandReceipt{
			CommandID: event.CommandID, CommandDigest: event.CommandDigest,
			ClientCommandDigest: event.ClientCommandDigest, EventKind: event.Kind, Sequence: event.Sequence,
		}
		previousDigest = event.Digest
		previousWriterEpoch = event.WriterEpoch
		if observer != nil {
			if err := observer(event, manifest); err != nil {
				return session.Manifest{}, err
			}
		}
	}
	state.manifest = manifest
	state.previousDigest = previousDigest
	state.previousWriterEpoch = previousWriterEpoch
	state.eventIDs = eventIDs
	state.preparedTransitions = preparedTransitions
	return manifest, nil
}

func applyAttemptStatus(
	manifest *session.Manifest,
	runID string,
	status session.AttemptStatus,
	projectionHash string,
	timestamp string,
) error {
	attempt, found := manifest.Attempts[runID]
	if !found {
		return errors.New("sessionstore: updated attempt does not exist")
	}
	if !validAttemptTransition(attempt.Status, status) {
		return errors.New("sessionstore: invalid attempt status transition")
	}
	segment := manifest.Segments[attempt.SegmentID]
	attempt.Status = status
	attempt.UpdatedAt = timestamp
	if projectionHash != "" {
		attempt.RunProjectionHash = projectionHash
	}
	switch status {
	case session.AttemptStatusStarting, session.AttemptStatusRunning, session.AttemptStatusWaiting:
		manifest.Session.Status = session.StatusActive
		manifest.Session.ActiveSegmentID = segment.SegmentID
		manifest.Session.ActiveRunID = attempt.RunID
		segment.Status = session.SegmentStatusActive
		attempt.CompletedAt = ""
	case session.AttemptStatusPausedAtBoundary, session.AttemptStatusHandoffPending:
		manifest.Session.Status = session.StatusPaused
		segment.Status = session.SegmentStatusPaused
	case session.AttemptStatusCompleted:
		manifest.Session.Status = session.StatusPaused
		manifest.Session.ActiveRunID = ""
		segment.Status = session.SegmentStatusCompleted
		attempt.CompletedAt = timestamp
	case session.AttemptStatusFailed:
		manifest.Session.Status = session.StatusFailed
		manifest.Session.ActiveRunID = ""
		segment.Status = session.SegmentStatusFailed
		attempt.CompletedAt = timestamp
	case session.AttemptStatusCancelled:
		manifest.Session.Status = session.StatusCancelled
		manifest.Session.ActiveRunID = ""
		segment.Status = session.SegmentStatusCancelled
		attempt.CompletedAt = timestamp
	case session.AttemptStatusIndeterminate:
		manifest.Session.Status = session.StatusIndeterminate
		segment.Status = session.SegmentStatusIndeterminate
		attempt.CompletedAt = timestamp
	}
	manifest.Attempts[runID] = attempt
	manifest.Segments[segment.SegmentID] = segment
	return nil
}

func applyExecutionMutation(
	manifest *session.Manifest,
	payload executionCommittedPayload,
	timestamp string,
) error {
	attempt, found := manifest.Attempts[payload.RunID]
	latestTraceSequence := attempt.CommittedTraceSequence
	if attempt.JournaledTraceSequence > latestTraceSequence {
		latestTraceSequence = attempt.JournaledTraceSequence
	}
	if !found || payload.PreviousMutationHash != attempt.ExecutionMutationHash ||
		attempt.ExecutionMutationHash == "" && payload.CheckpointSequence != 0 ||
		attempt.ExecutionMutationHash != "" && payload.CheckpointSequence <= attempt.CheckpointSequence ||
		payload.CommittedTraceSequence < latestTraceSequence {
		return session.ErrMutationConflict
	}
	if err := applyAttemptStatus(manifest, payload.RunID, payload.Status, payload.ProjectionHash, timestamp); err != nil {
		return err
	}
	attempt = manifest.Attempts[payload.RunID]
	attempt.ExecutionMutationHash = payload.MutationHash
	attempt.CheckpointSequence = payload.CheckpointSequence
	attempt.CommittedTraceSequence = payload.CommittedTraceSequence
	manifest.Attempts[payload.RunID] = attempt
	return nil
}

func applyTraceCommit(manifest *session.Manifest, payload traceCommittedPayload) error {
	attempt, found := manifest.Attempts[payload.RunID]
	if !found {
		return errors.New("sessionstore: trace attempt does not exist")
	}
	latestTraceSequence := attempt.CommittedTraceSequence
	if attempt.JournaledTraceSequence > latestTraceSequence {
		latestTraceSequence = attempt.JournaledTraceSequence
	}
	if payload.TraceSequence != latestTraceSequence+1 {
		return session.ErrMutationConflict
	}
	attempt.JournaledTraceSequence = payload.TraceSequence
	manifest.Attempts[payload.RunID] = attempt
	return nil
}

func executionSourceForAttempt(attempt session.RunAttemptRecord) session.ExecutionSource {
	if attempt.Mode == string(engine.RunModeReplay) || attempt.Mode == string(engine.RunModeRouteTest) {
		return session.ExecutionSourceSaved
	}
	return session.ExecutionSourceLive
}

func validateOccurrenceRecords(
	manifest session.Manifest,
	runID string,
	records map[string]session.OccurrenceRecord,
	events []engine.Event,
) error {
	attempt, found := manifest.Attempts[runID]
	if !found || attempt.SegmentID == "" {
		return errors.New("sessionstore: occurrence attempt is unavailable")
	}
	expected := make(map[string]session.OccurrenceRecord)
	for _, event := range events {
		occurrenceID, record, terminal, err := session.OccurrenceFromTraceEvent(
			manifest.Session.SessionID, attempt.SegmentID, executionSourceForAttempt(attempt), event,
		)
		if err != nil {
			return err
		}
		if terminal {
			expected[occurrenceID] = record
		}
	}
	if len(expected) != len(records) {
		return errors.New("sessionstore: occurrence records do not match committed trace events")
	}
	for occurrenceID, expectedRecord := range expected {
		actual, found := records[occurrenceID]
		if !found || session.DigestJSON(actual) != session.DigestJSON(expectedRecord) {
			return errors.New("sessionstore: occurrence record does not match its committed trace event")
		}
	}
	return nil
}

func applyOccurrenceRecords(
	manifest *session.Manifest,
	runID string,
	records map[string]session.OccurrenceRecord,
) error {
	if len(records) == 0 {
		return nil
	}
	attempt, found := manifest.Attempts[runID]
	if !found || attempt.SegmentID == "" {
		return errors.New("sessionstore: occurrence attempt is unavailable")
	}
	latestTraceSequence := attempt.CommittedTraceSequence
	if attempt.JournaledTraceSequence > latestTraceSequence {
		latestTraceSequence = attempt.JournaledTraceSequence
	}
	for occurrenceID, record := range records {
		if occurrenceID != session.OccurrenceID(record) || record.SessionID != manifest.Session.SessionID ||
			record.SegmentID != attempt.SegmentID || record.RunID != runID || record.QualifiedNodeID == "" ||
			record.StepID == "" || record.Phase == "" || record.Invocation < 1 || record.RetryAttempt < 1 ||
			record.OccurrenceSequence < 1 || record.EventSequence < 1 || record.EventSequence > latestTraceSequence ||
			record.ExecutionSource != executionSourceForAttempt(attempt) ||
			(record.Status != "completed" && record.Status != "failed" && record.Status != "skipped" && record.Status != "indeterminate") {
			return errors.New("sessionstore: invalid occurrence record")
		}
		if existing, exists := manifest.Occurrences[occurrenceID]; exists {
			if session.DigestJSON(existing) != session.DigestJSON(record) {
				return errors.New("sessionstore: occurrence metadata is immutable")
			}
			continue
		}
		manifest.Occurrences[occurrenceID] = record
	}
	return nil
}

func preparedTransitionUsesRun(manifest session.Manifest, runID string) bool {
	for _, transition := range manifest.Transitions {
		if transition.Status == session.TransitionStatusPrepared && transition.SourceOccurrence.RunID == runID {
			return true
		}
	}
	return false
}

func (store *DirStore) writeBlob(sessionID string, blob session.JSONBlob) error {
	if err := validateBlob(blob); err != nil {
		return err
	}
	if err := publishImmutableFile(store.blobPath(sessionID, blob.Digest), blob.Data, 0o600, maxBlobBytes); err != nil {
		if errors.Is(err, errImmutableFileConflict) {
			return errors.New("sessionstore: immutable blob conflicts with existing content")
		}
		return err
	}
	return nil
}

func validateBlob(blob session.JSONBlob) error {
	if !validDigest(blob.Digest) || len(blob.Data) == 0 || len(blob.Data) > maxBlobBytes || !json.Valid(blob.Data) ||
		session.DigestBytes(blob.Data) != blob.Digest {
		return errors.New("sessionstore: invalid immutable blob")
	}
	return nil
}

func validateSegmentResolution(resolution engine.DynamicIncludeResolutionState, writerEpoch uint64) error {
	if resolution.SchemaVersion != engine.DynamicIncludeResolutionStateSchemaV1 ||
		!validDigest(resolution.ResolutionID) || resolution.WriterEpoch != writerEpoch ||
		resolution.QualifiedNodeID == "" || resolution.StepID == "" || resolution.Invocation < 1 ||
		resolution.Revision < 1 ||
		resolution.QualifiedNodeID != engine.DebugNodeID(resolution.CallPath, resolution.StepID) ||
		resolution.Status != engine.DynamicIncludeResolutionStatusActive || resolution.CompletedAt != "" ||
		resolution.Pin.StepID != resolution.StepID || resolution.Pin.QualifiedNodeID != resolution.QualifiedNodeID ||
		resolution.Pin.Invocation != resolution.Invocation || resolution.Pin.Revision != resolution.Revision ||
		strings.TrimSpace(resolution.Pin.PackageName) == "" ||
		strings.TrimSpace(resolution.Pin.PackageVersion) == "" {
		return errors.New("sessionstore: invalid segment revision resolution")
	}
	if resolution.FrameID != "" && (!validDigest(resolution.FrameID) || resolution.FrameStepIndex < 0) ||
		resolution.FrameID == "" && resolution.FrameStepIndex != 0 {
		return errors.New("sessionstore: invalid segment revision frame binding")
	}
	if _, err := time.Parse(time.RFC3339Nano, resolution.CommittedAt); err != nil {
		return errors.New("sessionstore: invalid segment revision commit time")
	}
	if err := plansnapshot.ValidateDynamicIncludePin(resolution.Pin); err != nil {
		return errors.New("sessionstore: invalid segment revision pin")
	}
	return nil
}

func validateSegmentRevisionDispatch(
	dispatch engine.DispatchState,
	resolution engine.DynamicIncludeResolutionState,
	writerEpoch uint64,
) error {
	encodedPin, err := json.Marshal(resolution.Pin)
	if err != nil {
		return errors.New("sessionstore: invalid segment revision dispatch result")
	}
	if dispatch.SchemaVersion != engine.DispatchStateSchemaV1 || !validDigest(dispatch.OccurrenceID) ||
		dispatch.WriterEpoch != writerEpoch || dispatch.QualifiedNodeID != resolution.QualifiedNodeID ||
		dispatch.StepID != resolution.StepID || dispatch.FrameID != resolution.FrameID ||
		dispatch.FrameStepIndex != resolution.FrameStepIndex ||
		session.DigestJSON(dispatch.CallPath) != session.DigestJSON(resolution.CallPath) ||
		dispatch.Phase != engine.ExecutionPhaseExecute || dispatch.Invocation != resolution.Invocation ||
		dispatch.Classification != "read-only" || dispatch.EndpointIdentity != "dynamic-include-resolver" ||
		dispatch.Status != engine.DispatchStatusSettled || !validDigest(dispatch.RequestDigest) ||
		!validDigest(dispatch.IdempotencyKey) || dispatch.ResultDigest != engine.InteractionPayloadDigest(encodedPin) ||
		dispatch.Invocation < 1 || dispatch.RetryAttempt < 1 || dispatch.OccurrenceSequence < 1 {
		return errors.New("sessionstore: invalid segment revision dispatch")
	}
	if _, err := time.Parse(time.RFC3339Nano, dispatch.PreparedAt); err != nil {
		return errors.New("sessionstore: invalid segment revision dispatch prepare time")
	}
	if _, err := time.Parse(time.RFC3339Nano, dispatch.SettledAt); err != nil {
		return errors.New("sessionstore: invalid segment revision dispatch settle time")
	}
	return nil
}

func (store *DirStore) preflightBlobWrites(sessionID string, blobs []session.JSONBlob) ([]string, error) {
	newPaths := make([]string, 0, len(blobs))
	seen := make(map[string]bool, len(blobs))
	for _, blob := range blobs {
		path := store.blobPath(sessionID, blob.Digest)
		if seen[path] {
			continue
		}
		seen[path] = true
		existing, err := readFileBounded(path, maxBlobBytes)
		switch {
		case err == nil:
			if len(existing) != 0 && !bytes.Equal(existing, blob.Data) {
				return nil, errors.New("sessionstore: immutable blob conflicts with existing content")
			}
		case errors.Is(err, os.ErrNotExist):
			newPaths = append(newPaths, path)
		default:
			return nil, err
		}
	}
	return newPaths, nil
}

func (store *DirStore) removeNewBlobs(paths []string) {
	directories := make(map[string]bool)
	for _, path := range paths {
		if err := os.Remove(path); err == nil {
			directories[filepath.Dir(path)] = true
		}
	}
	for directory := range directories {
		_ = syncDirectory(directory)
	}
}

func (store *DirStore) writeManifest(manifest session.Manifest) error {
	if store.cacheCommittedFrameManifest(manifest) {
		store.notifySessionUpdate(manifest.Session.SessionID)
	}
	encoded, err := json.Marshal(session.ProjectManifest(manifest))
	if err != nil {
		return err
	}
	if len(encoded) > maxJournalEventBytes {
		return fmt.Errorf("sessionstore: manifest projection exceeds %d bytes", maxJournalEventBytes)
	}
	directory := store.SessionDir(manifest.Session.SessionID)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	lockFile, err := os.OpenFile(filepath.Join(directory, ".manifest.projection.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err := lockSessionFile(lockFile); err != nil {
		_ = lockFile.Close()
		return err
	}
	defer func() {
		_ = unlockSessionFile(lockFile)
		_ = lockFile.Close()
	}()
	path := store.manifestPath(manifest.Session.SessionID)
	if currentData, readErr := readFileBounded(path, maxJournalEventBytes); readErr == nil {
		if bytes.Equal(currentData, encoded) {
			return nil
		}
		var current session.Manifest
		if decodeStrictJSON(currentData, &current) == nil && current.Session.Sequence > manifest.Session.Sequence {
			return nil
		}
	} else if !errors.Is(readErr, os.ErrNotExist) && !errors.Is(readErr, errFileExceedsLimit) {
		return readErr
	}
	temporary, err := os.CreateTemp(directory, ".manifest-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(encoded); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		if runtime.GOOS != "windows" {
			return err
		}
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return removeErr
		}
		if err := os.Rename(temporaryPath, path); err != nil {
			return err
		}
	}
	return syncDirectory(directory)
}

func validateUUID(label, value string) error {
	parsed, err := uuid.Parse(value)
	if err != nil || parsed.String() != value {
		return fmt.Errorf("sessionstore: %s must be a canonical UUID", label)
	}
	return nil
}

func validDigest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != 71 {
		return false
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil && len(decoded) == 32
}

func validHash(value string) bool {
	if validDigest(value) {
		return true
	}
	if len(value) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32
}

func decodeStrictJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("sessionstore: unexpected trailing JSON")
		}
		return err
	}
	return nil
}

func readFileBounded(path string, maximum int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, errFileExceedsLimit
	}
	return data, nil
}

func syncDirectory(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func (store *DirStore) Close() error {
	store.mutationMu.Lock()
	defer store.mutationMu.Unlock()
	store.mu.Lock()
	leases := make([]*fileSessionLease, 0, len(store.leases))
	for lease := range store.leases {
		leases = append(leases, lease)
		delete(store.leases, lease)
	}
	if !store.closed {
		store.closed = true
		for sessionID, updates := range store.updates {
			close(updates)
			delete(store.updates, sessionID)
		}
	}
	store.mu.Unlock()
	store.frameMu.Lock()
	clear(store.frameCaches)
	store.frameMu.Unlock()
	store.journalMu.Lock()
	clear(store.journalCaches)
	store.journalMu.Unlock()
	var firstErr error
	for _, lease := range leases {
		if err := lease.releaseLocked(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
