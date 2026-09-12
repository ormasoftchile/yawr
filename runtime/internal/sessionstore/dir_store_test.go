package sessionstore_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	"github.com/ormasoftchile/yawr/runtime/internal/sessionstore"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/session"
)

func TestDirStoreSingleSegmentSessionSurvivesRestart(t *testing.T) {
	base := t.TempDir()
	sessionID := uuid.NewString()
	segmentID := uuid.NewString()
	runID := uuid.NewString()
	commandID := uuid.NewString()
	planBlob := session.NewJSONBlob(json.RawMessage(`{"schema_version":"execution-plan/v3"}`))
	graphBlob := session.NewJSONBlob(json.RawMessage(`{"schema_version":"yawr.graph-json/v1","nodes":[],"edges":[]}`))

	store := sessionstore.NewDirStore(base)
	lease, err := store.AcquireSessionLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("AcquireSessionLease: %v", err)
	}
	request := session.CreateRequest{
		SessionID: sessionID, CommandID: commandID,
		Session: session.SessionRecord{Status: session.StatusActive},
		Segment: session.SegmentRecord{
			SegmentID: segmentID, Ordinal: 1, RunbookID: "root", RunbookName: "Root",
			Status: session.SegmentStatusActive, PlanHash: planBlob.Digest, GraphHash: graphBlob.Digest,
			ExecutableSnapshotHash: planBlob.Digest, AttemptRunIDs: []string{runID},
		},
		Attempt: session.RunAttemptRecord{
			RunID: runID, SegmentID: segmentID, Ordinal: 1, Mode: "real",
			Status: session.AttemptStatusRunning,
		},
		Blobs: []session.JSONBlob{planBlob, graphBlob},
	}
	request = bindCreationIdentity(request)
	created, err := store.CreateSession(context.Background(), lease.Epoch(), request)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if created.Session.SessionID != sessionID || created.Session.Sequence != 1 ||
		created.Session.ActiveSegmentID != segmentID || created.Session.ActiveRunID != runID {
		t.Fatalf("created manifest = %#v", created)
	}
	if err := lease.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened := sessionstore.NewDirStore(base)
	t.Cleanup(func() { _ = reopened.Close() })
	manifest, err := reopened.LoadManifest(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if manifest.Segments[segmentID].PlanHash != planBlob.Digest ||
		manifest.Attempts[runID].SegmentID != segmentID || manifest.Session.Sequence != 1 {
		t.Fatalf("restored manifest = %#v", manifest)
	}
	if _, err := reopened.ReadBlob(context.Background(), sessionID, planBlob.Digest); err != nil {
		t.Fatalf("ReadBlob: %v", err)
	}

	secondLease, err := reopened.AcquireSessionLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("AcquireSessionLease reopened: %v", err)
	}
	defer secondLease.Release()
	idempotent, err := reopened.CreateSession(context.Background(), secondLease.Epoch(), request)
	if err != nil {
		t.Fatalf("idempotent CreateSession: %v", err)
	}
	if idempotent.Session.Sequence != 1 {
		t.Fatalf("idempotent creation appended sequence %d", idempotent.Session.Sequence)
	}
	changed := request
	changed.EntryDigest = session.DigestJSON(map[string]any{"runbook": "different.runbook.yaml"})
	if _, err := reopened.CreateSession(context.Background(), secondLease.Epoch(), changed); !errors.Is(err, session.ErrCreationConflict) {
		t.Fatalf("changed creation error = %v, want ErrCreationConflict", err)
	}
	changedCommand := request
	changedCommand.CommandID = uuid.NewString()
	if _, err := reopened.CreateSession(context.Background(), secondLease.Epoch(), changedCommand); !errors.Is(err, session.ErrCreationConflict) {
		t.Fatalf("changed creation command error = %v, want ErrCreationConflict", err)
	}
	completed, err := reopened.UpdateAttempt(context.Background(), session.AttemptUpdateRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), WriterEpoch: secondLease.Epoch(),
		ExpectedSequence: 1, RunID: runID, Status: session.AttemptStatusCompleted,
	})
	if err != nil || completed.Session.Sequence != 2 {
		t.Fatalf("complete attempt = %#v, %v", completed, err)
	}

	closed, err := reopened.CloseSession(context.Background(), session.CloseRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), WriterEpoch: secondLease.Epoch(),
		ExpectedSequence: 2, Status: session.StatusResolved,
	})
	if err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	if closed.Session.Status != session.StatusResolved || closed.Session.Sequence != 3 {
		t.Fatalf("closed manifest = %#v", closed)
	}
	if closed.Segments[segmentID].Status != session.SegmentStatusCompleted ||
		closed.Attempts[runID].Status != session.AttemptStatusCompleted {
		t.Fatalf("closed topology = segment %#v attempt %#v", closed.Segments[segmentID], closed.Attempts[runID])
	}
	atCreation, err := reopened.LoadManifestAtSequence(context.Background(), sessionID, 1)
	if err != nil || atCreation.Session.Status != session.StatusActive ||
		atCreation.Session.ActiveRunID != runID || atCreation.Attempts[runID].Status != session.AttemptStatusRunning {
		t.Fatalf("creation manifest = %#v, %v", atCreation, err)
	}
	atCompletion, err := reopened.LoadManifestAtSequence(context.Background(), sessionID, 2)
	if err != nil || atCompletion.Session.Status != session.StatusPaused || atCompletion.Session.ActiveRunID != "" ||
		atCompletion.Attempts[runID].Status != session.AttemptStatusCompleted {
		t.Fatalf("completion manifest = %#v, %v", atCompletion, err)
	}
	if _, err := reopened.LoadManifestAtSequence(context.Background(), sessionID, 4); !errors.Is(err, session.ErrSequenceConflict) {
		t.Fatalf("future manifest error = %v, want ErrSequenceConflict", err)
	}
}

func TestDirStoreRejectsTamperedInitialGraphBlob(t *testing.T) {
	base := t.TempDir()
	sessionID := uuid.NewString()
	request := createRequest(sessionID)
	store := sessionstore.NewDirStore(base)
	t.Cleanup(func() { _ = store.Close() })
	lease, err := store.AcquireSessionLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("AcquireSessionLease: %v", err)
	}
	defer lease.Release()
	if _, err := store.CreateSession(context.Background(), lease.Epoch(), request); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	graphPath := filepath.Join(
		base, sessionID, "blobs", strings.TrimPrefix(request.Segment.GraphHash, "sha256:")+".json",
	)
	if err := os.WriteFile(graphPath, []byte(`{"tampered":true}`), 0o600); err != nil {
		t.Fatalf("tamper initial graph: %v", err)
	}
	if _, err := store.LoadManifest(context.Background(), sessionID); err == nil {
		t.Fatal("LoadManifest accepted a tampered initial graph blob")
	}
}

func TestDirStoreRejectsInvalidCreationBeforeDurableWrites(t *testing.T) {
	base := t.TempDir()
	store := sessionstore.NewDirStore(base)
	t.Cleanup(func() { _ = store.Close() })
	request := createRequest(uuid.NewString())
	request.Segment.ExecutableRevision = 2
	request.Segment.GraphRevision = 2
	request = bindCreationIdentity(request)
	lease, err := store.AcquireSessionLease(context.Background(), request.SessionID)
	if err != nil {
		t.Fatalf("AcquireSessionLease: %v", err)
	}
	defer lease.Release()
	if _, err := store.CreateSession(context.Background(), lease.Epoch(), request); err == nil {
		t.Fatal("CreateSession accepted a non-initial revision")
	}
	if _, err := store.LoadManifest(context.Background(), request.SessionID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("LoadManifest after rejected creation = %v, want not exist", err)
	}
	reservation := filepath.Join(base, ".creation-commands", request.CommandID+".json")
	if _, err := os.Stat(reservation); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("creation reservation exists after rejection: %v", err)
	}
}

func TestDirStoreRebuildsCreationJournalWithoutClientCommandDigest(t *testing.T) {
	base := t.TempDir()
	sessionID := uuid.NewString()
	store := sessionstore.NewDirStore(base)
	lease, err := store.AcquireSessionLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("AcquireSessionLease: %v", err)
	}
	if _, err := store.CreateSession(context.Background(), lease.Epoch(), createRequest(sessionID)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := lease.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	type creationEvent struct {
		SchemaVersion  string            `json:"schema_version"`
		EventID        string            `json:"event_id"`
		SessionID      string            `json:"session_id"`
		Sequence       int64             `json:"sequence"`
		WriterEpoch    uint64            `json:"writer_epoch"`
		CommandID      string            `json:"command_id"`
		CommandDigest  string            `json:"command_digest"`
		Kind           session.EventKind `json:"kind"`
		Timestamp      string            `json:"timestamp"`
		PreviousDigest string            `json:"previous_digest,omitempty"`
		Digest         string            `json:"digest"`
		Payload        json.RawMessage   `json:"payload"`
	}
	eventsPath := filepath.Join(base, sessionID, "events.jsonl")
	encoded, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatalf("ReadFile events: %v", err)
	}
	var event creationEvent
	if err := json.Unmarshal(bytes.TrimSpace(encoded), &event); err != nil {
		t.Fatalf("decode creation event: %v", err)
	}
	event.Digest = ""
	unsigned, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("encode unsigned creation event: %v", err)
	}
	event.Digest = session.DigestBytes(unsigned)
	encoded, err = json.Marshal(event)
	if err != nil {
		t.Fatalf("encode creation event: %v", err)
	}
	if err := os.WriteFile(eventsPath, append(encoded, '\n'), 0o600); err != nil {
		t.Fatalf("write creation journal: %v", err)
	}
	headPath := filepath.Join(base, sessionID, "heads", "head-00000000000000000001.json")
	headData, err := os.ReadFile(headPath)
	if err != nil {
		t.Fatalf("read head: %v", err)
	}
	var head map[string]any
	if err := json.Unmarshal(headData, &head); err != nil {
		t.Fatalf("decode head: %v", err)
	}
	head["event_digest"] = event.Digest
	headData, err = json.Marshal(head)
	if err != nil {
		t.Fatalf("encode head: %v", err)
	}
	if err := os.WriteFile(headPath, headData, 0o600); err != nil {
		t.Fatalf("write head: %v", err)
	}
	if err := os.Remove(filepath.Join(base, sessionID, "manifest.json")); err != nil {
		t.Fatalf("remove manifest: %v", err)
	}

	reopened := sessionstore.NewDirStore(base)
	t.Cleanup(func() { _ = reopened.Close() })
	manifest, err := reopened.LoadManifest(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("LoadManifest creation journal: %v", err)
	}
	if manifest.Session.SessionID != sessionID || manifest.Session.Sequence != 1 {
		t.Fatalf("rebuilt creation manifest = %#v", manifest.Session)
	}
}

func TestDirStorePauseAndResumeSessionAreIdempotentAndFenced(t *testing.T) {
	base := t.TempDir()
	store := sessionstore.NewDirStore(base)
	t.Cleanup(func() { _ = store.Close() })
	request := createRequest(uuid.NewString())
	lease, err := store.AcquireSessionLease(context.Background(), request.SessionID)
	if err != nil {
		t.Fatalf("AcquireSessionLease: %v", err)
	}
	manifest, err := store.CreateSession(context.Background(), lease.Epoch(), request)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	manifest, err = store.UpdateAttempt(context.Background(), session.AttemptUpdateRequest{
		SessionID: request.SessionID, CommandID: uuid.NewString(), WriterEpoch: lease.Epoch(),
		ExpectedSequence: manifest.Session.Sequence, RunID: manifest.Session.ActiveRunID,
		Status: session.AttemptStatusPausedAtBoundary,
	})
	if err != nil {
		t.Fatalf("pause attempt boundary: %v", err)
	}
	pauseCommandID := uuid.NewString()
	paused, err := store.PauseSession(context.Background(), session.PauseRequest{
		SessionID: request.SessionID, CommandID: pauseCommandID, WriterEpoch: lease.Epoch(),
		ExpectedSequence: manifest.Session.Sequence, Reason: "transport detached",
	})
	if err != nil {
		t.Fatalf("PauseSession: %v", err)
	}
	if paused.Session.Status != session.StatusPaused || paused.Session.ActiveRunID == "" ||
		paused.Attempts[paused.Session.ActiveRunID].Status != session.AttemptStatusPausedAtBoundary {
		t.Fatalf("paused manifest = %#v", paused)
	}
	idempotent, err := store.PauseSession(context.Background(), session.PauseRequest{
		SessionID: request.SessionID, CommandID: pauseCommandID, WriterEpoch: lease.Epoch(),
		ExpectedSequence: manifest.Session.Sequence, Reason: "transport detached",
	})
	if err != nil || idempotent.Session.Sequence != paused.Session.Sequence {
		t.Fatalf("idempotent pause = %#v, %v", idempotent, err)
	}
	resumeCommandID := uuid.NewString()
	resumed, err := store.ResumeSession(context.Background(), session.ResumeSessionRequest{
		SessionID: request.SessionID, CommandID: resumeCommandID, WriterEpoch: lease.Epoch(),
		ExpectedSequence: paused.Session.Sequence,
	})
	if err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}
	if resumed.Session.Status != session.StatusActive ||
		resumed.Attempts[resumed.Session.ActiveRunID].Status != session.AttemptStatusRunning {
		t.Fatalf("resumed manifest = %#v", resumed)
	}
	if err := lease.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := store.PauseSession(context.Background(), session.PauseRequest{
		SessionID: request.SessionID, CommandID: uuid.NewString(), WriterEpoch: lease.Epoch(),
		ExpectedSequence: resumed.Session.Sequence, Reason: "stale writer",
	}); !errors.Is(err, session.ErrSessionLeaseStale) {
		t.Fatalf("stale PauseSession error = %v", err)
	}
}

func TestDirStoreClosePausedSessionCancelsUnfinishedAttempt(t *testing.T) {
	store := sessionstore.NewDirStore(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	request := createRequest(uuid.NewString())
	lease, err := store.AcquireSessionLease(context.Background(), request.SessionID)
	if err != nil {
		t.Fatalf("AcquireSessionLease: %v", err)
	}
	manifest, err := store.CreateSession(context.Background(), lease.Epoch(), request)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	manifest, err = store.UpdateAttempt(context.Background(), session.AttemptUpdateRequest{
		SessionID: request.SessionID, CommandID: uuid.NewString(), WriterEpoch: lease.Epoch(),
		ExpectedSequence: manifest.Session.Sequence, RunID: manifest.Session.ActiveRunID,
		Status: session.AttemptStatusPausedAtBoundary,
	})
	if err != nil {
		t.Fatalf("pause attempt boundary: %v", err)
	}
	manifest, err = store.PauseSession(context.Background(), session.PauseRequest{
		SessionID: request.SessionID, CommandID: uuid.NewString(), WriterEpoch: lease.Epoch(),
		ExpectedSequence: manifest.Session.Sequence, Reason: "operator paused",
	})
	if err != nil {
		t.Fatalf("PauseSession: %v", err)
	}
	closed, err := store.CloseSession(context.Background(), session.CloseRequest{
		SessionID: request.SessionID, CommandID: uuid.NewString(), WriterEpoch: lease.Epoch(),
		ExpectedSequence: manifest.Session.Sequence, Status: session.StatusEscalated,
	})
	if err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	segmentID := request.Segment.SegmentID
	runID := request.Attempt.RunID
	if closed.Session.Status != session.StatusEscalated || closed.Session.ActiveRunID != "" ||
		closed.Session.ActiveSegmentID != "" || closed.Attempts[runID].Status != session.AttemptStatusCancelled ||
		closed.Segments[segmentID].Status != session.SegmentStatusCancelled {
		t.Fatalf("closed paused topology = session %#v segment %#v attempt %#v",
			closed.Session, closed.Segments[segmentID], closed.Attempts[runID])
	}
	events, err := store.ReadEvents(context.Background(), request.SessionID, closed.Session.Sequence-1)
	if err != nil || len(events) != 1 {
		t.Fatalf("close events = %#v, %v", events, err)
	}
	var payload map[string]any
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil ||
		payload["segment_id"] != segmentID || payload["run_id"] != runID {
		t.Fatalf("close payload = %#v, %v", payload, err)
	}
}

func TestDirStoreNoOpDetachReceiptIsIdempotentAndContentBound(t *testing.T) {
	base := t.TempDir()
	sessionID := uuid.NewString()
	store := sessionstore.NewDirStore(base)
	t.Cleanup(func() { _ = store.Close() })
	lease, err := store.AcquireSessionLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("AcquireSessionLease: %v", err)
	}
	defer lease.Release()
	created, err := store.CreateSession(context.Background(), lease.Epoch(), createRequest(sessionID))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	indeterminate, err := store.UpdateAttempt(context.Background(), session.AttemptUpdateRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), WriterEpoch: lease.Epoch(),
		ExpectedSequence: created.Session.Sequence, RunID: created.Session.ActiveRunID,
		Status: session.AttemptStatusIndeterminate,
	})
	if err != nil {
		t.Fatalf("UpdateAttempt: %v", err)
	}
	commandID := uuid.NewString()
	clientDigest := session.DigestJSON(map[string]any{"type": "session.detach", "command": commandID})
	request := session.DetachRequest{
		SessionID: sessionID, CommandID: commandID, WriterEpoch: lease.Epoch(),
		ExpectedSequence: indeterminate.Session.Sequence, ClientCommandDigest: clientDigest,
		Reason: "session.detach",
	}
	detached, err := store.RecordDetach(context.Background(), request)
	if err != nil {
		t.Fatalf("RecordDetach: %v", err)
	}
	receipt := detached.AcceptedCommands[commandID]
	if detached.Session.Status != session.StatusIndeterminate ||
		detached.Attempts[created.Session.ActiveRunID].Status != session.AttemptStatusIndeterminate ||
		detached.Session.Sequence != indeterminate.Session.Sequence+1 ||
		receipt.ClientCommandDigest != clientDigest || receipt.EventKind != session.EventSessionDetached {
		t.Fatalf("detached manifest/receipt = %#v/%#v", detached, receipt)
	}
	idempotent, err := store.RecordDetach(context.Background(), request)
	if err != nil || idempotent.Session.Sequence != detached.Session.Sequence {
		t.Fatalf("idempotent RecordDetach = %#v, %v", idempotent, err)
	}
	changed := request
	changed.ClientCommandDigest = session.DigestJSON(map[string]any{"type": "session.close", "command": commandID})
	if _, err := store.RecordDetach(context.Background(), changed); !errors.Is(err, session.ErrCommandConflict) {
		t.Fatalf("changed RecordDetach error = %v, want ErrCommandConflict", err)
	}
}

func TestDirStoreFencesWritersAndSerializesDuplicateCommands(t *testing.T) {
	base := t.TempDir()
	sessionID := uuid.NewString()
	first := sessionstore.NewDirStore(base)
	second := sessionstore.NewDirStore(base)
	t.Cleanup(func() {
		_ = first.Close()
		_ = second.Close()
	})
	firstLease, err := first.AcquireSessionLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("first lease: %v", err)
	}
	if _, err := second.AcquireSessionLease(context.Background(), sessionID); !errors.Is(err, session.ErrSessionLeaseHeld) {
		t.Fatalf("competing lease error = %v", err)
	}
	request := createRequest(sessionID)
	if _, err := first.CreateSession(context.Background(), firstLease.Epoch(), request); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := firstLease.Release(); err != nil {
		t.Fatalf("release first: %v", err)
	}
	secondLease, err := second.AcquireSessionLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("second lease: %v", err)
	}
	defer secondLease.Release()
	if secondLease.Epoch() <= firstLease.Epoch() {
		t.Fatalf("epochs did not increase: first=%d second=%d", firstLease.Epoch(), secondLease.Epoch())
	}
	if _, err := first.CloseSession(context.Background(), session.CloseRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), WriterEpoch: firstLease.Epoch(),
		ExpectedSequence: 1, Status: session.StatusResolved,
	}); !errors.Is(err, session.ErrSessionLeaseStale) {
		t.Fatalf("stale close error = %v", err)
	}
	manifest, err := second.UpdateAttempt(context.Background(), session.AttemptUpdateRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), WriterEpoch: secondLease.Epoch(),
		ExpectedSequence: 1, RunID: request.Attempt.RunID, Status: session.AttemptStatusCompleted,
	})
	if err != nil {
		t.Fatalf("complete attempt: %v", err)
	}

	commandID := uuid.NewString()
	closeRequest := session.CloseRequest{
		SessionID: sessionID, CommandID: commandID, WriterEpoch: secondLease.Epoch(),
		ExpectedSequence: manifest.Session.Sequence, Status: session.StatusResolved,
	}
	results := make(chan error, 2)
	var start sync.WaitGroup
	start.Add(1)
	for range 2 {
		go func() {
			start.Wait()
			_, commitErr := second.CloseSession(context.Background(), closeRequest)
			results <- commitErr
		}()
	}
	start.Done()
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("duplicate command: %v", err)
		}
	}
	events, err := second.ReadEvents(context.Background(), sessionID, 0)
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("duplicate command appended %d events, want 3 total", len(events))
	}
	if _, err := second.CloseSession(context.Background(), session.CloseRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), WriterEpoch: secondLease.Epoch(),
		ExpectedSequence: 1, Status: session.StatusCancelled,
	}); !errors.Is(err, session.ErrSessionClosed) {
		t.Fatalf("closed session mutation error = %v", err)
	}
}

func TestDirStoreCommitsHashChainedExecutionMutations(t *testing.T) {
	base := t.TempDir()
	sessionID := uuid.NewString()
	store := sessionstore.NewDirStore(base)
	t.Cleanup(func() { _ = store.Close() })
	lease, err := store.AcquireSessionLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("AcquireSessionLease: %v", err)
	}
	defer lease.Release()
	created, err := store.CreateSession(context.Background(), lease.Epoch(), createRequest(sessionID))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	runID := created.Session.ActiveRunID
	firstProjection := executionProjectionBlob(t, runID, 0)
	firstMutation := session.ExecutionMutation{
		SchemaVersion: session.ExecutionMutationSchemaV1, RunID: runID,
		CheckpointSequence: 0, RunStatus: "pending", AttemptStatus: session.AttemptStatusRunning,
		RunWriterEpoch: 1, StateProjectionHash: firstProjection.Digest,
	}
	wrongEpoch := firstMutation
	wrongEpoch.RunWriterEpoch = lease.Epoch() + 1
	if _, err := store.CommitExecutionMutation(context.Background(), session.ExecutionMutationRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), WriterEpoch: lease.Epoch(), ExpectedSequence: 1,
		Mutation: wrongEpoch, StateProjection: firstProjection,
	}); !errors.Is(err, session.ErrMutationConflict) {
		t.Fatalf("mismatched mutation epoch error = %v, want ErrMutationConflict", err)
	}
	firstMutationBlob, err := session.NewExecutionMutationBlob(firstMutation)
	if err != nil {
		t.Fatalf("NewExecutionMutationBlob: %v", err)
	}
	firstCommandID := uuid.NewString()
	clientCommandDigest := session.DigestJSON(map[string]any{"type": "interaction.answer", "selected": "one"})
	committed, err := store.CommitExecutionMutation(context.Background(), session.ExecutionMutationRequest{
		SessionID: sessionID, CommandID: firstCommandID, WriterEpoch: lease.Epoch(), ExpectedSequence: 1,
		ClientCommandDigest: clientCommandDigest, Mutation: firstMutation, StateProjection: firstProjection,
	})
	if err != nil {
		t.Fatalf("CommitExecutionMutation: %v", err)
	}
	firstAttempt := committed.Attempts[runID]
	if committed.Session.Sequence != 2 || firstAttempt.ExecutionMutationHash != firstMutationBlob.Digest ||
		firstAttempt.RunProjectionHash != firstProjection.Digest || firstAttempt.CheckpointSequence != 0 {
		t.Fatalf("committed mutation manifest = %#v", committed)
	}
	if _, err := store.ReadBlob(context.Background(), sessionID, firstMutationBlob.Digest); err != nil {
		t.Fatalf("read mutation blob: %v", err)
	}
	idempotent, err := store.CommitExecutionMutation(context.Background(), session.ExecutionMutationRequest{
		SessionID: sessionID, CommandID: firstCommandID, WriterEpoch: lease.Epoch(), ExpectedSequence: 1,
		ClientCommandDigest: clientCommandDigest, Mutation: firstMutation, StateProjection: firstProjection,
	})
	if err != nil || idempotent.Session.Sequence != 2 {
		t.Fatalf("idempotent mutation = %#v, %v", idempotent, err)
	}
	if receipt := idempotent.AcceptedCommands[firstCommandID]; receipt.ClientCommandDigest != clientCommandDigest {
		t.Fatalf("client command receipt = %#v", receipt)
	}
	if _, err := store.CommitExecutionMutation(context.Background(), session.ExecutionMutationRequest{
		SessionID: sessionID, CommandID: firstCommandID, WriterEpoch: lease.Epoch(), ExpectedSequence: 1,
		ClientCommandDigest: session.DigestJSON(map[string]any{"type": "interaction.answer", "selected": "two"}),
		Mutation:            firstMutation, StateProjection: firstProjection,
	}); !errors.Is(err, session.ErrCommandConflict) {
		t.Fatalf("changed client command digest error = %v, want ErrCommandConflict", err)
	}

	secondProjection := executionProjectionBlob(t, runID, 1)
	conflicting := session.ExecutionMutation{
		SchemaVersion: session.ExecutionMutationSchemaV1, RunID: runID,
		CheckpointSequence: 1, RunStatus: "running", AttemptStatus: session.AttemptStatusRunning,
		RunWriterEpoch: 1, PreviousMutationHash: session.DigestJSON("wrong"),
		StateProjectionHash: secondProjection.Digest, CommittedTraceSequence: 2,
		TraceSequenceStart: 1, TraceSequenceEnd: 2,
	}
	if _, err := store.CommitExecutionMutation(context.Background(), session.ExecutionMutationRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), WriterEpoch: lease.Epoch(), ExpectedSequence: 2,
		Mutation: conflicting, StateProjection: secondProjection,
	}); !errors.Is(err, session.ErrMutationConflict) {
		t.Fatalf("conflicting previous mutation error = %v", err)
	}
	conflicting.PreviousMutationHash = firstMutationBlob.Digest
	conflicting.StateProjectionHash = firstProjection.Digest
	if _, err := store.CommitExecutionMutation(context.Background(), session.ExecutionMutationRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), WriterEpoch: lease.Epoch(), ExpectedSequence: 2,
		Mutation: conflicting, StateProjection: firstProjection,
	}); !errors.Is(err, session.ErrMutationConflict) {
		t.Fatalf("stale projection error = %v, want ErrMutationConflict", err)
	}
	conflicting.StateProjectionHash = secondProjection.Digest
	secondMutationBlob, err := session.NewExecutionMutationBlob(conflicting)
	if err != nil {
		t.Fatalf("NewExecutionMutationBlob second: %v", err)
	}
	committed, err = store.CommitExecutionMutation(context.Background(), session.ExecutionMutationRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), WriterEpoch: lease.Epoch(), ExpectedSequence: 2,
		Mutation: conflicting, StateProjection: secondProjection,
	})
	if err != nil {
		t.Fatalf("CommitExecutionMutation second: %v", err)
	}
	secondAttempt := committed.Attempts[runID]
	if committed.Session.Sequence != 3 || secondAttempt.ExecutionMutationHash != secondMutationBlob.Digest ||
		secondAttempt.CheckpointSequence != 1 || secondAttempt.CommittedTraceSequence != 2 {
		t.Fatalf("second mutation manifest = %#v", committed)
	}
	events, err := store.ReadEvents(context.Background(), sessionID, 0)
	if err != nil || len(events) != 3 || events[1].Kind != session.EventExecutionCommitted ||
		events[2].Kind != session.EventExecutionCommitted {
		t.Fatalf("execution mutation events = %#v, %v", events, err)
	}
	forgedMutation := conflicting
	forgedMutation.PreviousMutationHash = session.DigestJSON("forged-previous")
	forgedBlob, err := session.NewExecutionMutationBlob(forgedMutation)
	if err != nil {
		t.Fatalf("NewExecutionMutationBlob forged: %v", err)
	}
	forgedPath := filepath.Join(
		base, sessionID, "blobs", strings.TrimPrefix(forgedBlob.Digest, "sha256:")+".json",
	)
	if err := os.WriteFile(forgedPath, forgedBlob.Data, 0o600); err != nil {
		t.Fatalf("write forged mutation blob: %v", err)
	}
	rewriteLatestJournalEvent(t, base, sessionID, func(event *session.Event) {
		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("decode execution payload: %v", err)
		}
		payload["mutation_hash"] = forgedBlob.Digest
		event.Payload, err = json.Marshal(payload)
		if err != nil {
			t.Fatalf("encode execution payload: %v", err)
		}
		event.CommandDigest = session.ExecutionMutationDigest(runID, forgedBlob.Digest)
	})
	if _, err := store.LoadManifest(context.Background(), sessionID); err == nil {
		t.Fatal("LoadManifest accepted an execution event detached from its mutation blob")
	}
}

func TestDirStoreRejectsTamperedExecutionFrameProjection(t *testing.T) {
	base := t.TempDir()
	sessionID := uuid.NewString()
	store := sessionstore.NewDirStore(base)
	t.Cleanup(func() { _ = store.Close() })
	lease, err := store.AcquireSessionLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("AcquireSessionLease: %v", err)
	}
	defer lease.Release()
	created, err := store.CreateSession(context.Background(), lease.Epoch(), createRequest(sessionID))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	runID := created.Session.ActiveRunID
	projection := executionProjectionBlob(t, runID, 0)
	frameContentData, err := json.Marshal(session.ExecutionFrameProjectionContent{})
	if err != nil {
		t.Fatalf("marshal frame projection content: %v", err)
	}
	frameChunkData, err := json.Marshal(session.ExecutionFrameProjectionChunk{
		SchemaVersion: session.ExecutionFrameProjectionChunkSchemaV1, Data: frameContentData,
	})
	if err != nil {
		t.Fatalf("marshal frame projection chunk: %v", err)
	}
	frameChunk := session.NewJSONBlob(frameChunkData)
	frameData, err := json.Marshal(session.ExecutionFrameProjection{
		SchemaVersion: session.ExecutionFrameProjectionSchemaV1, RunID: runID,
		CheckpointSequence: 0, CommittedTraceSequence: 0,
		ContentDigest: session.DigestBytes(frameContentData), ContentSize: int64(len(frameContentData)),
		ChunkDigests: []string{frameChunk.Digest},
	})
	if err != nil {
		t.Fatalf("marshal frame projection: %v", err)
	}
	frameProjection := session.NewJSONBlob(frameData)
	mutation := session.ExecutionMutation{
		SchemaVersion: session.ExecutionMutationSchemaV1, RunID: runID,
		CheckpointSequence: 0, RunStatus: engine.RunStatusPending, AttemptStatus: session.AttemptStatusRunning,
		RunWriterEpoch: lease.Epoch(), StateProjectionHash: projection.Digest,
		FrameProjectionHash: frameProjection.Digest,
	}
	if _, err := store.CommitExecutionMutation(context.Background(), session.ExecutionMutationRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), WriterEpoch: lease.Epoch(), ExpectedSequence: 1,
		Mutation: mutation, StateProjection: projection, FrameProjection: frameProjection,
		ProjectionBlobs: []session.JSONBlob{frameChunk},
	}); err != nil {
		t.Fatalf("CommitExecutionMutation: %v", err)
	}
	framePath := filepath.Join(
		base, sessionID, "blobs", strings.TrimPrefix(frameChunk.Digest, "sha256:")+".json",
	)
	if err := os.WriteFile(framePath, []byte(`{"tampered":true}`), 0o600); err != nil {
		t.Fatalf("tamper frame projection: %v", err)
	}
	if _, err := store.LoadManifest(context.Background(), sessionID); err == nil {
		t.Fatal("LoadManifest accepted a tampered execution frame projection")
	}
}

func TestDirStoreCommitsOrderedDirectTraceEvents(t *testing.T) {
	base := t.TempDir()
	sessionID := uuid.NewString()
	store := sessionstore.NewDirStore(base)
	t.Cleanup(func() { _ = store.Close() })
	lease, err := store.AcquireSessionLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("AcquireSessionLease: %v", err)
	}
	defer lease.Release()
	created, err := store.CreateSession(context.Background(), lease.Epoch(), createRequest(sessionID))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	runID := created.Session.ActiveRunID
	projection := executionProjectionBlob(t, runID, 0)
	mutation := session.ExecutionMutation{
		SchemaVersion: session.ExecutionMutationSchemaV1, RunID: runID,
		CheckpointSequence: 0, RunStatus: engine.RunStatusPending, AttemptStatus: session.AttemptStatusRunning,
		RunWriterEpoch: lease.Epoch(), StateProjectionHash: projection.Digest,
	}
	committed, err := store.CommitExecutionMutation(context.Background(), session.ExecutionMutationRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), WriterEpoch: lease.Epoch(), ExpectedSequence: 1,
		Mutation: mutation, StateProjection: projection,
	})
	if err != nil {
		t.Fatalf("CommitExecutionMutation: %v", err)
	}
	direct := engine.Event{
		EventID: uuid.NewString(), RunID: runID, RunbookID: "root",
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano), Kind: "step/started", Sequence: 1,
		Payload: map[string]any{"step_id": "work"},
	}
	encodedTrace, err := json.Marshal(direct)
	if err != nil {
		t.Fatalf("encode direct trace: %v", err)
	}
	traceBlob := session.NewJSONBlob(encodedTrace)
	commandID := uuid.NewString()
	committed, err = store.CommitTraceEvent(context.Background(), session.TraceCommitRequest{
		SessionID: sessionID, CommandID: commandID, WriterEpoch: lease.Epoch(),
		ExpectedSequence: committed.Session.Sequence, RunID: runID, Event: direct,
	})
	if err != nil {
		t.Fatalf("CommitTraceEvent: %v", err)
	}
	if committed.Attempts[runID].JournaledTraceSequence != 1 || committed.Session.Sequence != 3 {
		t.Fatalf("trace manifest = %#v", committed)
	}
	idempotent, err := store.CommitTraceEvent(context.Background(), session.TraceCommitRequest{
		SessionID: sessionID, CommandID: commandID, WriterEpoch: lease.Epoch(),
		ExpectedSequence: 2, RunID: runID, Event: direct,
	})
	if err != nil || idempotent.Session.Sequence != 3 {
		t.Fatalf("idempotent trace = %#v, %v", idempotent, err)
	}
	direct.EventID = uuid.NewString()
	direct.Sequence = 3
	if _, err := store.CommitTraceEvent(context.Background(), session.TraceCommitRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), WriterEpoch: lease.Epoch(),
		ExpectedSequence: 3, RunID: runID, Event: direct,
	}); !errors.Is(err, session.ErrMutationConflict) {
		t.Fatalf("out-of-order trace error = %v, want ErrMutationConflict", err)
	}
	events, err := store.ReadEvents(context.Background(), sessionID, 0)
	if err != nil || len(events) != 3 || events[2].Kind != session.EventTraceCommitted {
		t.Fatalf("trace journal events = %#v, %v", events, err)
	}
	tracePath := filepath.Join(base, sessionID, "blobs", strings.TrimPrefix(traceBlob.Digest, "sha256:")+".json")
	if err := os.WriteFile(tracePath, []byte(`{"tampered":true}`), 0o600); err != nil {
		t.Fatalf("tamper trace blob: %v", err)
	}
	if _, err := store.LoadManifest(context.Background(), sessionID); err == nil {
		t.Fatal("LoadManifest accepted a trace event detached from its immutable blob")
	}
}

func TestDirStoreTerminalMutationAllowsOnlyItsCheckpointTrace(t *testing.T) {
	base := t.TempDir()
	sessionID := uuid.NewString()
	store := sessionstore.NewDirStore(base)
	t.Cleanup(func() { _ = store.Close() })
	lease, err := store.AcquireSessionLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("AcquireSessionLease: %v", err)
	}
	defer lease.Release()
	created, err := store.CreateSession(context.Background(), lease.Epoch(), createRequest(sessionID))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	runID := created.Session.ActiveRunID
	initialProjection := executionProjectionBlob(t, runID, 0)
	running, err := store.CommitExecutionMutation(context.Background(), session.ExecutionMutationRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), WriterEpoch: lease.Epoch(), ExpectedSequence: 1,
		Mutation: session.ExecutionMutation{
			SchemaVersion: session.ExecutionMutationSchemaV1, RunID: runID,
			CheckpointSequence: 0, RunStatus: engine.RunStatusPending, AttemptStatus: session.AttemptStatusRunning,
			RunWriterEpoch: lease.Epoch(), StateProjectionHash: initialProjection.Digest,
		},
		StateProjection: initialProjection,
	})
	if err != nil {
		t.Fatalf("commit running mutation: %v", err)
	}
	terminalProjection := executionProjectionBlob(t, runID, 1)
	terminal, err := store.CommitExecutionMutation(context.Background(), session.ExecutionMutationRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), WriterEpoch: lease.Epoch(),
		ExpectedSequence: running.Session.Sequence,
		Mutation: session.ExecutionMutation{
			SchemaVersion: session.ExecutionMutationSchemaV1, RunID: runID,
			CheckpointSequence: 1, RunStatus: engine.RunStatusCancelled, AttemptStatus: session.AttemptStatusCancelled,
			RunWriterEpoch: lease.Epoch(), PreviousMutationHash: running.Attempts[runID].ExecutionMutationHash,
			StateProjectionHash: terminalProjection.Digest, CommittedTraceSequence: 1,
			TraceSequenceStart: 1, TraceSequenceEnd: 1,
		},
		StateProjection: terminalProjection,
	})
	if err != nil {
		t.Fatalf("commit terminal mutation: %v", err)
	}
	trace := engine.Event{
		EventID: uuid.NewString(), RunID: runID, RunbookID: "root",
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano), Sequence: 2,
		Kind: "step/output", Payload: map[string]any{"snapshot_file": "checkpoint-00000000000000000001.json"},
	}
	if _, err := store.CommitTraceEvent(context.Background(), session.TraceCommitRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), WriterEpoch: lease.Epoch(),
		ExpectedSequence: terminal.Session.Sequence, RunID: runID, Event: trace,
	}); !errors.Is(err, session.ErrSessionClosed) {
		t.Fatalf("arbitrary terminal trace error = %v, want ErrSessionClosed", err)
	}
	trace.EventID = uuid.NewString()
	trace.Kind = "checkpoint"
	trace.Payload["snapshot_file"] = "checkpoint-wrong.json"
	if _, err := store.CommitTraceEvent(context.Background(), session.TraceCommitRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), WriterEpoch: lease.Epoch(),
		ExpectedSequence: terminal.Session.Sequence, RunID: runID, Event: trace,
	}); !errors.Is(err, session.ErrSessionClosed) {
		t.Fatalf("unbound terminal checkpoint error = %v, want ErrSessionClosed", err)
	}
	trace.EventID = uuid.NewString()
	trace.Payload["snapshot_file"] = "checkpoint-00000000000000000001.json"
	withCheckpoint, err := store.CommitTraceEvent(context.Background(), session.TraceCommitRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), WriterEpoch: lease.Epoch(),
		ExpectedSequence: terminal.Session.Sequence, RunID: runID, Event: trace,
	})
	if err != nil {
		t.Fatalf("commit terminal checkpoint: %v", err)
	}
	trace.EventID = uuid.NewString()
	trace.Sequence++
	if _, err := store.CommitTraceEvent(context.Background(), session.TraceCommitRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), WriterEpoch: lease.Epoch(),
		ExpectedSequence: withCheckpoint.Session.Sequence, RunID: runID, Event: trace,
	}); !errors.Is(err, session.ErrSessionClosed) {
		t.Fatalf("second terminal checkpoint error = %v, want ErrSessionClosed", err)
	}
}

func TestDirStorePreparesAndCommitsStaticHandoffAtomically(t *testing.T) {
	base := t.TempDir()
	sessionID := uuid.NewString()
	store := sessionstore.NewDirStore(base)
	t.Cleanup(func() { _ = store.Close() })
	lease, err := store.AcquireSessionLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("AcquireSessionLease: %v", err)
	}
	defer lease.Release()
	created, err := store.CreateSession(context.Background(), lease.Epoch(), createRequest(sessionID))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sourceRunID := created.Session.ActiveRunID
	sourceProjection := executionProjectionBlob(t, sourceRunID, 0)
	sourceMutation := session.ExecutionMutation{
		SchemaVersion: session.ExecutionMutationSchemaV1, RunID: sourceRunID,
		CheckpointSequence: 0, RunStatus: engine.RunStatusHandoffPending,
		AttemptStatus: session.AttemptStatusHandoffPending, RunWriterEpoch: lease.Epoch(),
		StateProjectionHash: sourceProjection.Digest,
	}
	sourceReady, err := store.CommitExecutionMutation(context.Background(), session.ExecutionMutationRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), WriterEpoch: lease.Epoch(),
		ExpectedSequence: created.Session.Sequence, Mutation: sourceMutation, StateProjection: sourceProjection,
	})
	if err != nil {
		t.Fatalf("commit handoff-pending source mutation: %v", err)
	}
	transitionID, targetSegmentID, targetRunID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	planBlob := session.NewJSONBlob(json.RawMessage(`{"schema_version":"execution-plan/v3","target":true}`))
	graphBlob := session.NewJSONBlob(json.RawMessage(`{"schema_version":"yawr.graph-json/v1","target":true,"nodes":[],"edges":[]}`))
	contextBlob := session.NewJSONBlob(json.RawMessage(`{"inputs":{"server":"db01"},"facts":{"health":"degraded"}}`))
	transition := session.TransitionRecord{
		TransitionID: transitionID, Status: session.TransitionStatusPrepared,
		SourceSegmentID: sourceReady.Session.ActiveSegmentID,
		SourceOccurrence: session.SourceOccurrence{
			RunID: sourceRunID, QualifiedNodeID: "continue", StepID: "continue",
			Phase: "execute", Invocation: 1, RetryAttempt: 1, OccurrenceSequence: 1,
		},
		TargetSegmentID: targetSegmentID, TargetRunID: targetRunID,
		TargetRunbookID: "target", TargetRunbookName: "Target",
		TargetPlanHash: session.DigestJSON("target-plan"), TargetGraphHash: graphBlob.Digest,
		TargetExecutableSnapshotHash: planBlob.Digest,
		ReasonCode:                   "continue", ReasonSummary: "Continue investigation",
		ContextBindings: map[string]string{"server": "server"},
		FactRefs:        []session.FactRef{{Name: "health", Source: "health"}},
		ContextDigest:   contextBlob.Digest, IdempotencyKey: session.DigestJSON("handoff-key"),
	}
	targetSegment := session.SegmentRecord{
		SegmentID: targetSegmentID, Ordinal: 2, RunbookID: "target", RunbookName: "Target",
		EntrySelector: session.EntrySelector{Step: "$entry"}, Status: session.SegmentStatusPrepared,
		PlanHash: transition.TargetPlanHash, GraphHash: graphBlob.Digest,
		ExecutableSnapshotHash: planBlob.Digest, AttemptRunIDs: []string{targetRunID},
		ExecutableRevision: 1, GraphRevision: 1,
	}
	targetAttempt := session.RunAttemptRecord{
		RunID: targetRunID, SegmentID: targetSegmentID, Ordinal: 1,
		Mode: "real", Status: session.AttemptStatusStarting, PlanHash: transition.TargetPlanHash,
	}
	conflictingGraphPath := filepath.Join(
		base, sessionID, "blobs", strings.TrimPrefix(graphBlob.Digest, "sha256:")+".json",
	)
	if err := os.MkdirAll(filepath.Dir(conflictingGraphPath), 0o700); err != nil {
		t.Fatalf("create blob directory: %v", err)
	}
	if err := os.WriteFile(conflictingGraphPath, []byte(`{"conflict":true}`), 0o600); err != nil {
		t.Fatalf("write conflicting graph blob: %v", err)
	}
	if _, err := store.PrepareTransition(context.Background(), session.PrepareTransitionRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), WriterEpoch: lease.Epoch(),
		ExpectedSequence: sourceReady.Session.Sequence, Transition: transition,
		TargetSegment: targetSegment, TargetAttempt: targetAttempt,
		Blobs: []session.JSONBlob{planBlob, graphBlob, contextBlob},
	}); err == nil {
		t.Fatal("transition with conflicting later blob was prepared")
	}
	for _, blob := range []session.JSONBlob{planBlob, contextBlob} {
		path := filepath.Join(base, sessionID, "blobs", strings.TrimPrefix(blob.Digest, "sha256:")+".json")
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed preparation persisted earlier blob %s: %v", blob.Digest, err)
		}
	}
	if err := os.Remove(conflictingGraphPath); err != nil {
		t.Fatalf("remove conflicting graph blob: %v", err)
	}
	sensitiveContextBlob := session.NewJSONBlob(json.RawMessage(
		`{"inputs":{"labels":{"password":"p@ss"}},"facts":{"health":"degraded"}}`,
	))
	rejectedTransition := transition
	rejectedTransition.ContextBindings = map[string]string{"labels": "labels"}
	rejectedTransition.ContextDigest = sensitiveContextBlob.Digest
	if _, err := store.PrepareTransition(context.Background(), session.PrepareTransitionRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), WriterEpoch: lease.Epoch(),
		ExpectedSequence: sourceReady.Session.Sequence, Transition: rejectedTransition,
		TargetSegment: targetSegment, TargetAttempt: targetAttempt,
		Blobs: []session.JSONBlob{planBlob, graphBlob, sensitiveContextBlob},
	}); err == nil || strings.Contains(err.Error(), "p@ss") {
		t.Fatalf("sensitive context error = %v, want value-free refusal", err)
	}
	for _, blob := range []session.JSONBlob{planBlob, graphBlob, sensitiveContextBlob} {
		path := filepath.Join(base, sessionID, "blobs", strings.TrimPrefix(blob.Digest, "sha256:")+".json")
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("rejected transition persisted blob %s: %v", blob.Digest, err)
		}
	}
	prepareCommandID := uuid.NewString()
	prepared, err := store.PrepareTransition(context.Background(), session.PrepareTransitionRequest{
		SessionID: sessionID, CommandID: prepareCommandID, WriterEpoch: lease.Epoch(),
		ExpectedSequence: sourceReady.Session.Sequence, Transition: transition,
		TargetSegment: targetSegment, TargetAttempt: targetAttempt,
		Blobs: []session.JSONBlob{planBlob, graphBlob, contextBlob},
	})
	if err != nil {
		t.Fatalf("PrepareTransition: %v", err)
	}
	if prepared.Session.ActiveRunID != sourceRunID || prepared.Transitions[transitionID].Status != session.TransitionStatusPrepared ||
		prepared.Segments[targetSegmentID].SegmentID != "" {
		t.Fatalf("prepared manifest = %#v", prepared)
	}
	indeterminateProjection := executionProjectionBlob(t, sourceRunID, 1)
	indeterminateMutation := session.ExecutionMutation{
		SchemaVersion: session.ExecutionMutationSchemaV1, RunID: sourceRunID,
		CheckpointSequence: 1, RunStatus: engine.RunStatusIndeterminate,
		AttemptStatus: session.AttemptStatusIndeterminate, RunWriterEpoch: lease.Epoch(),
		PreviousMutationHash: sourceReady.Attempts[sourceRunID].ExecutionMutationHash,
		StateProjectionHash:  indeterminateProjection.Digest,
	}
	if _, err := store.CommitExecutionMutation(context.Background(), session.ExecutionMutationRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), WriterEpoch: lease.Epoch(),
		ExpectedSequence: prepared.Session.Sequence, Mutation: indeterminateMutation,
		StateProjection: indeterminateProjection,
	}); err == nil {
		t.Fatal("prepared transition source accepted a later indeterminate mutation")
	}
	stillFrozen, err := store.LoadManifest(context.Background(), sessionID)
	if err != nil || stillFrozen.Session.Sequence != prepared.Session.Sequence ||
		stillFrozen.Transitions[transitionID].Status != session.TransitionStatusPrepared ||
		stillFrozen.Attempts[sourceRunID].Status != session.AttemptStatusHandoffPending {
		t.Fatalf("prepared source changed after rejected mutation = %#v, %v", stillFrozen, err)
	}
	if _, err := store.AbortTransition(context.Background(), session.AbortTransitionRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), WriterEpoch: lease.Epoch(),
		ExpectedSequence: prepared.Session.Sequence, TransitionID: transitionID,
		Reason: "must retain committed source handoff",
	}); err == nil {
		t.Fatal("prepared transition with authoritative source handoff was abortable")
	}
	stillPrepared, err := store.LoadManifest(context.Background(), sessionID)
	if err != nil || stillPrepared.Transitions[transitionID].Status != session.TransitionStatusPrepared ||
		stillPrepared.Session.Sequence != prepared.Session.Sequence {
		t.Fatalf("manifest after forbidden prepared abort = %#v, %v", stillPrepared, err)
	}
	conflictingTransition := transition
	conflictingTransition.TransitionID = uuid.NewString()
	conflictingTransition.TargetSegmentID = uuid.NewString()
	conflictingTransition.TargetRunID = uuid.NewString()
	conflictingTransition.IdempotencyKey = session.DigestJSON("conflicting-handoff-key")
	conflictingSegment := targetSegment
	conflictingSegment.SegmentID = conflictingTransition.TargetSegmentID
	conflictingSegment.AttemptRunIDs = []string{conflictingTransition.TargetRunID}
	conflictingAttempt := targetAttempt
	conflictingAttempt.RunID = conflictingTransition.TargetRunID
	conflictingAttempt.SegmentID = conflictingTransition.TargetSegmentID
	if _, err := store.PrepareTransition(context.Background(), session.PrepareTransitionRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), WriterEpoch: lease.Epoch(),
		ExpectedSequence: prepared.Session.Sequence, Transition: conflictingTransition,
		TargetSegment: conflictingSegment, TargetAttempt: conflictingAttempt,
		Blobs: []session.JSONBlob{planBlob, graphBlob, contextBlob},
	}); err == nil {
		t.Fatal("second transition from one source was prepared")
	}
	afterConflict, err := store.LoadManifest(context.Background(), sessionID)
	if err != nil || afterConflict.Session.Sequence != prepared.Session.Sequence || len(afterConflict.Transitions) != 1 {
		t.Fatalf("journal after rejected preparation = %#v, %v", afterConflict, err)
	}
	idempotent, err := store.PrepareTransition(context.Background(), session.PrepareTransitionRequest{
		SessionID: sessionID, CommandID: prepareCommandID, WriterEpoch: lease.Epoch(),
		ExpectedSequence: sourceReady.Session.Sequence, Transition: transition,
		TargetSegment: targetSegment, TargetAttempt: targetAttempt,
		Blobs: []session.JSONBlob{planBlob, graphBlob, contextBlob},
	})
	if err != nil || idempotent.Session.Sequence != prepared.Session.Sequence {
		t.Fatalf("idempotent prepare = %#v, %v", idempotent, err)
	}
	commitCommandID := uuid.NewString()
	committed, err := store.CommitTransition(context.Background(), session.CommitTransitionRequest{
		SessionID: sessionID, CommandID: commitCommandID, WriterEpoch: lease.Epoch(),
		ExpectedSequence: prepared.Session.Sequence, TransitionID: transitionID,
	})
	if err != nil {
		t.Fatalf("CommitTransition: %v", err)
	}
	if committed.Session.ActiveSegmentID != targetSegmentID || committed.Session.ActiveRunID != targetRunID ||
		committed.Transitions[transitionID].Status != session.TransitionStatusCommitted ||
		committed.Segments[sourceReady.Session.ActiveSegmentID].Status != session.SegmentStatusHandedOff ||
		committed.Attempts[sourceRunID].Status != session.AttemptStatusCompleted ||
		committed.Segments[targetSegmentID].Status != session.SegmentStatusPrepared ||
		committed.Attempts[targetRunID].Status != session.AttemptStatusStarting {
		t.Fatalf("committed manifest = %#v", committed)
	}
	idempotent, err = store.CommitTransition(context.Background(), session.CommitTransitionRequest{
		SessionID: sessionID, CommandID: commitCommandID, WriterEpoch: lease.Epoch(),
		ExpectedSequence: prepared.Session.Sequence, TransitionID: transitionID,
	})
	if err != nil || idempotent.Session.Sequence != committed.Session.Sequence || len(idempotent.Segments) != 2 {
		t.Fatalf("idempotent commit = %#v, %v", idempotent, err)
	}
	if _, err := store.AbortTransition(context.Background(), session.AbortTransitionRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), WriterEpoch: lease.Epoch(),
		ExpectedSequence: committed.Session.Sequence, TransitionID: transitionID, Reason: "must not roll back",
	}); err == nil {
		t.Fatal("committed transition was abortable")
	}
	unchanged, err := store.LoadManifest(context.Background(), sessionID)
	if err != nil || unchanged.Session.Sequence != committed.Session.Sequence ||
		unchanged.Transitions[transitionID].Status != session.TransitionStatusCommitted ||
		unchanged.Session.ActiveRunID != targetRunID {
		t.Fatalf("manifest after forbidden abort = %#v, %v", unchanged, err)
	}
	events, err := store.ReadEvents(context.Background(), sessionID, 0)
	if err != nil || len(events) != 4 || events[2].Kind != session.EventTransitionPrepared ||
		events[3].Kind != session.EventTransitionCommitted {
		t.Fatalf("transition events = %#v, %v", events, err)
	}
	rewriteLatestJournalEvent(t, base, sessionID, func(event *session.Event) {
		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("decode transition commit payload: %v", err)
		}
		payload["target_attempt"].(map[string]any)["mode"] = "replay"
		payload["target_segment"].(map[string]any)["executable_snapshot_hash"] = session.DigestJSON("forged-snapshot")
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("encode transition commit payload: %v", err)
		}
		event.Payload = encoded
		event.CommandDigest = session.TransitionCommitDigest(transitionID)
	})
	if _, err := store.LoadManifest(context.Background(), sessionID); err == nil {
		t.Fatal("LoadManifest accepted a rehashed transition commit target")
	}
}

func TestDirStoreCommitsSegmentRevisionAtomically(t *testing.T) {
	base := t.TempDir()
	sessionID := uuid.NewString()
	store := sessionstore.NewDirStore(base)
	t.Cleanup(func() { _ = store.Close() })
	lease, err := store.AcquireSessionLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("AcquireSessionLease: %v", err)
	}
	defer lease.Release()
	created, err := store.CreateSession(context.Background(), lease.Epoch(), createRequest(sessionID))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	segmentID := created.Session.ActiveSegmentID
	runID := created.Session.ActiveRunID
	planBlob := session.NewJSONBlob(json.RawMessage(`{"schema_version":"execution-plan/v3","revision":2}`))
	graphBlob := session.NewJSONBlob(json.RawMessage(`{"schema_version":"1","revision":2}`))
	planHash := strings.Repeat("b", 64)
	closure, err := plansnapshot.EncodeFlowClosure([]schema.FlowNode{{Step: &schema.Step{
		ID: "child", Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{},
	}}})
	if err != nil {
		t.Fatalf("EncodeFlowClosure: %v", err)
	}
	resolution := engine.DynamicIncludeResolutionState{
		SchemaVersion: engine.DynamicIncludeResolutionStateSchemaV1,
		ResolutionID:  engine.InteractionPayloadDigest([]byte("resolution")), WriterEpoch: lease.Epoch(),
		QualifiedNodeID: "dynamic", StepID: "dynamic", Invocation: 1, Revision: 1,
		Pin: schema.LockedDynamicInclude{
			StepID: "dynamic", QualifiedNodeID: "dynamic", Invocation: 1, Revision: 1,
			RenderedRef: "pkg/child", QualifiedID: "pkg/child",
			RunbookID: "child", RunbookName: "Child", RunbookContentHash: strings.Repeat("a", 64),
			AbsPath: "C:/runbooks/child.runbook.yaml", PackageName: "pkg", PackageVersion: "1.0.0",
			FileDigest:    engine.InteractionPayloadDigest([]byte("file")),
			PackageDigest: engine.InteractionPayloadDigest([]byte("package")), ExecutableClosure: closure,
		},
		Status: engine.DynamicIncludeResolutionStatusActive, CommittedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	encodedPin, err := json.Marshal(resolution.Pin)
	if err != nil {
		t.Fatalf("marshal resolution pin: %v", err)
	}
	dispatch := engine.DispatchState{
		SchemaVersion: engine.DispatchStateSchemaV1,
		OccurrenceID:  engine.InteractionPayloadDigest([]byte("dispatch")), WriterEpoch: lease.Epoch(),
		QualifiedNodeID: "dynamic", StepID: "dynamic", Phase: engine.ExecutionPhaseExecute,
		Invocation: 1, RetryAttempt: 1, OccurrenceSequence: 1,
		Classification: "read-only", EndpointIdentity: "dynamic-include-resolver",
		RequestDigest:  engine.InteractionPayloadDigest([]byte("request")),
		IdempotencyKey: engine.InteractionPayloadDigest([]byte("idempotency")),
		Status:         engine.DispatchStatusSettled, ResultDigest: engine.InteractionPayloadDigest(encodedPin),
		PreparedAt: time.Now().UTC().Format(time.RFC3339Nano),
		SettledAt:  time.Now().UTC().Format(time.RFC3339Nano),
	}
	commandID := uuid.NewString()
	request := session.SegmentRevisionRequest{
		SessionID: sessionID, CommandID: commandID, WriterEpoch: lease.Epoch(),
		ExpectedSequence: created.Session.Sequence, SegmentID: segmentID, RunID: runID,
		ExecutableRevision: 2, GraphRevision: 2,
		PlanHash:           planHash,
		ExecutableSnapshot: planBlob, Graph: graphBlob, Resolution: resolution, Dispatch: dispatch,
	}
	for _, test := range []struct {
		name   string
		mutate func(*session.SegmentRevisionRequest)
	}{
		{name: "wrong invocation", mutate: func(request *session.SegmentRevisionRequest) {
			request.Dispatch.Invocation++
		}},
		{name: "wrong phase", mutate: func(request *session.SegmentRevisionRequest) {
			request.Dispatch.Phase = engine.ExecutionPhaseBefore
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			invalid := request
			invalid.CommandID = uuid.NewString()
			test.mutate(&invalid)
			if _, err := store.CommitSegmentRevision(context.Background(), invalid); err == nil {
				t.Fatal("CommitSegmentRevision accepted mismatched resolver dispatch")
			}
		})
	}
	revised, err := store.CommitSegmentRevision(context.Background(), request)
	if err != nil {
		t.Fatalf("CommitSegmentRevision: %v", err)
	}
	segment := revised.Segments[segmentID]
	if segment.ExecutableRevision != 2 || segment.GraphRevision != 2 ||
		segment.PlanHash != planHash ||
		segment.ExecutableSnapshotHash != planBlob.Digest || segment.GraphHash != graphBlob.Digest {
		t.Fatalf("revised segment = %#v", segment)
	}
	originalSegment, originalGraph, err := store.LoadSegmentGraphRevision(
		context.Background(), sessionID, segmentID, 1,
	)
	if err != nil || originalSegment.GraphRevision != 1 ||
		originalSegment.GraphHash != session.DigestBytes(originalGraph) {
		t.Fatalf("original graph revision = %#v/%s, %v", originalSegment, originalGraph, err)
	}
	revisedSegment, revisedGraph, err := store.LoadSegmentGraphRevision(
		context.Background(), sessionID, segmentID, 2,
	)
	if err != nil || revisedSegment.GraphRevision != 2 || revisedSegment.PlanHash != planHash ||
		revisedSegment.GraphHash != session.DigestBytes(revisedGraph) {
		t.Fatalf("revised graph lookup = %#v/%s, %v", revisedSegment, revisedGraph, err)
	}
	idempotent, err := store.CommitSegmentRevision(context.Background(), request)
	if err != nil || idempotent.Session.Sequence != revised.Session.Sequence {
		t.Fatalf("idempotent segment revision = %#v, %v", idempotent, err)
	}
	events, err := store.ReadEvents(context.Background(), sessionID, 0)
	if err != nil || len(events) != 2 || events[1].Kind != session.EventSegmentRevised {
		t.Fatalf("segment revision events = %#v, %v", events, err)
	}
}

func executionProjectionBlob(t *testing.T, runID string, checkpointSequence int64) session.JSONBlob {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{
		"schema_version":      session.RunProjectionSchemaV1,
		"run_id":              runID,
		"checkpoint_sequence": checkpointSequence,
		"files":               []any{map[string]any{"path": "snapshot", "digest": session.DigestJSON(checkpointSequence), "size": 1}},
	})
	if err != nil {
		t.Fatalf("marshal execution projection: %v", err)
	}
	return session.NewJSONBlob(encoded)
}

func TestDirStoreWriterEpochNeverResetsAfterSidecarDeletion(t *testing.T) {
	base := t.TempDir()
	sessionID := uuid.NewString()
	store := sessionstore.NewDirStore(base)
	t.Cleanup(func() { _ = store.Close() })
	first, err := store.AcquireSessionLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("first lease: %v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("release first: %v", err)
	}
	second, err := store.AcquireSessionLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("second lease: %v", err)
	}
	secondEpoch := second.Epoch()
	if err := second.Release(); err != nil {
		t.Fatalf("release second: %v", err)
	}
	_ = os.Remove(filepath.Join(base, "."+sessionID+".writer.epoch"))
	third, err := store.AcquireSessionLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("third lease: %v", err)
	}
	defer third.Release()
	if third.Epoch() <= secondEpoch {
		t.Fatalf("writer epoch reset: second=%d third=%d", secondEpoch, third.Epoch())
	}
}

func TestDirStoreCreationCommandIsGloballyUniqueAcrossSessions(t *testing.T) {
	base := t.TempDir()
	storeA := sessionstore.NewDirStore(base)
	storeB := sessionstore.NewDirStore(base)
	t.Cleanup(func() {
		_ = storeA.Close()
		_ = storeB.Close()
	})
	commandID := uuid.NewString()
	sessionA, sessionB := uuid.NewString(), uuid.NewString()
	requestA := createRequest(sessionA)
	requestA.CommandID = commandID
	requestB := createRequest(sessionB)
	requestB.CommandID = commandID
	leaseA, err := storeA.AcquireSessionLease(context.Background(), sessionA)
	if err != nil {
		t.Fatalf("lease A: %v", err)
	}
	defer leaseA.Release()
	leaseB, err := storeB.AcquireSessionLease(context.Background(), sessionB)
	if err != nil {
		t.Fatalf("lease B: %v", err)
	}
	defer leaseB.Release()
	if _, err := storeA.CreateSession(context.Background(), leaseA.Epoch(), requestA); err != nil {
		t.Fatalf("create A: %v", err)
	}
	if _, err := storeB.CreateSession(context.Background(), leaseB.Epoch(), requestB); !errors.Is(err, session.ErrCreationConflict) {
		t.Fatalf("create B error = %v, want ErrCreationConflict", err)
	}
	if _, err := storeB.LoadManifest(context.Background(), sessionB); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("conflicting session B was published: %v", err)
	}
}

func TestDirStoreRebuildsProjectionAndRejectsCommittedJournalTamper(t *testing.T) {
	base := t.TempDir()
	sessionID := uuid.NewString()
	store := sessionstore.NewDirStore(base)
	lease, err := store.AcquireSessionLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	request := createRequest(sessionID)
	if _, err := store.CreateSession(context.Background(), lease.Epoch(), request); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := lease.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	manifestPath := filepath.Join(base, sessionID, "manifest.json")
	if err := os.WriteFile(manifestPath, []byte("not-json"), 0o600); err != nil {
		t.Fatalf("corrupt manifest: %v", err)
	}
	manifest, err := store.LoadManifest(context.Background(), sessionID)
	if err != nil || manifest.Session.Sequence != 1 {
		t.Fatalf("journal rebuild = %#v, %v", manifest, err)
	}
	repaired, err := os.ReadFile(manifestPath)
	if err != nil || !json.Valid(repaired) {
		t.Fatalf("manifest projection was not repaired: %q, %v", repaired, err)
	}
	eventsPath := filepath.Join(base, sessionID, "events.jsonl")
	file, err := os.OpenFile(eventsPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	if _, err := file.WriteString(`{"partial":`); err != nil {
		t.Fatalf("append partial: %v", err)
	}
	_ = file.Close()
	if manifest, err := store.LoadManifest(context.Background(), sessionID); err != nil || manifest.Session.Sequence != 1 {
		t.Fatalf("truncated tail recovery = %#v, %v", manifest, err)
	}
	contents, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	newline := bytes.IndexByte(contents, '\n')
	if newline < 0 {
		t.Fatal("journal has no committed line")
	}
	committed := append([]byte(nil), contents[:newline]...)
	committed = bytes.Replace(committed, []byte(`"status":"active"`), []byte(`"status":"paused"`), 1)
	if err := os.WriteFile(eventsPath, append(committed, '\n'), 0o600); err != nil {
		t.Fatalf("tamper journal: %v", err)
	}
	if _, err := store.LoadManifest(context.Background(), sessionID); err == nil {
		t.Fatal("LoadManifest accepted a tampered committed event")
	}
}

func TestDirStoreRejectsCommittedJournalSuffixLoss(t *testing.T) {
	base := t.TempDir()
	sessionID := uuid.NewString()
	store := sessionstore.NewDirStore(base)
	t.Cleanup(func() { _ = store.Close() })
	lease, err := store.AcquireSessionLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	defer lease.Release()
	request := createRequest(sessionID)
	if _, err := store.CreateSession(context.Background(), lease.Epoch(), request); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := store.UpdateAttempt(context.Background(), session.AttemptUpdateRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), WriterEpoch: lease.Epoch(),
		ExpectedSequence: 1, RunID: request.Attempt.RunID, Status: session.AttemptStatusCompleted,
	}); err != nil {
		t.Fatalf("UpdateAttempt: %v", err)
	}
	eventsPath := filepath.Join(base, sessionID, "events.jsonl")
	contents, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	firstLineEnd := bytes.IndexByte(contents, '\n')
	if firstLineEnd < 0 {
		t.Fatal("journal has no first line")
	}
	if err := os.WriteFile(eventsPath, contents[:firstLineEnd+1], 0o600); err != nil {
		t.Fatalf("truncate committed suffix: %v", err)
	}
	if manifest, err := store.LoadManifest(context.Background(), sessionID); err == nil {
		t.Fatalf("suffix loss rolled manifest back to sequence %d", manifest.Session.Sequence)
	}
}

func TestDirStoreJournalSuffixSurvivesLatestHeadDeletion(t *testing.T) {
	base := t.TempDir()
	sessionID := uuid.NewString()
	store := sessionstore.NewDirStore(base)
	t.Cleanup(func() { _ = store.Close() })
	lease, err := store.AcquireSessionLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	defer lease.Release()
	request := createRequest(sessionID)
	if _, err := store.CreateSession(context.Background(), lease.Epoch(), request); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := store.UpdateAttempt(context.Background(), session.AttemptUpdateRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), WriterEpoch: lease.Epoch(),
		ExpectedSequence: 1, RunID: request.Attempt.RunID, Status: session.AttemptStatusCompleted,
	}); err != nil {
		t.Fatalf("UpdateAttempt: %v", err)
	}
	latestHead := filepath.Join(base, sessionID, "heads", "head-00000000000000000002.json")
	if err := os.Remove(latestHead); err != nil {
		t.Fatalf("remove latest head: %v", err)
	}
	frameEvents, _, frameHead, err := store.ReadFrameEvents(context.Background(), sessionID, 1)
	if err != nil || frameHead != 2 || len(frameEvents) != 1 || frameEvents[0].Sequence != 2 {
		t.Fatalf("frame suffix after head deletion = %#v head=%d, %v", frameEvents, frameHead, err)
	}
	manifest, err := store.LoadManifest(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("LoadManifest after head deletion: %v", err)
	}
	if manifest.Session.Sequence != 2 || manifest.Attempts[request.Attempt.RunID].Status != session.AttemptStatusCompleted {
		t.Fatalf("head deletion rolled journal back: %#v", manifest)
	}
	closed, err := store.CloseSession(context.Background(), session.CloseRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), WriterEpoch: lease.Epoch(),
		ExpectedSequence: 2, Status: session.StatusResolved,
	})
	if err != nil {
		t.Fatalf("CloseSession after head deletion: %v", err)
	}
	if closed.Session.Sequence != 3 || closed.Session.Status != session.StatusResolved {
		t.Fatalf("valid suffix was truncated before append: %#v", closed)
	}
	events, err := store.ReadEvents(context.Background(), sessionID, 0)
	if err != nil || len(events) != 3 || events[1].Kind != session.EventAttemptUpdated {
		t.Fatalf("journal after repaired append = %#v, %v", events, err)
	}
}

func TestDirStoreRejectsTornLatestHead(t *testing.T) {
	base := t.TempDir()
	sessionID := uuid.NewString()
	store := sessionstore.NewDirStore(base)
	t.Cleanup(func() { _ = store.Close() })
	lease, err := store.AcquireSessionLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	defer lease.Release()
	request := createRequest(sessionID)
	if _, err := store.CreateSession(context.Background(), lease.Epoch(), request); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := store.UpdateAttempt(context.Background(), session.AttemptUpdateRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), WriterEpoch: lease.Epoch(),
		ExpectedSequence: 1, RunID: request.Attempt.RunID, Status: session.AttemptStatusWaiting,
	}); err != nil {
		t.Fatalf("UpdateAttempt: %v", err)
	}
	latestHead := filepath.Join(base, sessionID, "heads", "head-00000000000000000002.json")
	if err := os.WriteFile(latestHead, nil, 0o600); err != nil {
		t.Fatalf("tear latest head: %v", err)
	}
	manifest, err := store.LoadManifest(context.Background(), sessionID)
	if err == nil || !strings.Contains(err.Error(), "invalid latest journal head") {
		t.Fatalf("LoadManifest after torn head = %#v, %v", manifest, err)
	}
}

func TestDirStoreRepairsTornCreationReservationOnRetry(t *testing.T) {
	base := t.TempDir()
	sessionID := uuid.NewString()
	request := createRequest(sessionID)
	reservationDir := filepath.Join(base, ".creation-commands")
	if err := os.MkdirAll(reservationDir, 0o700); err != nil {
		t.Fatalf("create reservation directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(reservationDir, request.CommandID+".json"), nil, 0o600); err != nil {
		t.Fatalf("create torn reservation: %v", err)
	}
	store := sessionstore.NewDirStore(base)
	t.Cleanup(func() { _ = store.Close() })
	lease, err := store.AcquireSessionLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	defer lease.Release()
	manifest, err := store.CreateSession(context.Background(), lease.Epoch(), request)
	if err != nil || manifest.Session.Sequence != 1 {
		t.Fatalf("CreateSession after torn reservation = %#v, %v", manifest, err)
	}
}

func TestDirStoreRepairsTornImmutableBlobOnRetry(t *testing.T) {
	base := t.TempDir()
	sessionID := uuid.NewString()
	request := createRequest(sessionID)
	tornBlob := request.Blobs[0]
	blobDir := filepath.Join(base, sessionID, "blobs")
	if err := os.MkdirAll(blobDir, 0o700); err != nil {
		t.Fatalf("create blob directory: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(blobDir, strings.TrimPrefix(tornBlob.Digest, "sha256:")+".json"), nil, 0o600,
	); err != nil {
		t.Fatalf("create torn blob: %v", err)
	}
	store := sessionstore.NewDirStore(base)
	t.Cleanup(func() { _ = store.Close() })
	lease, err := store.AcquireSessionLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	defer lease.Release()
	manifest, err := store.CreateSession(context.Background(), lease.Epoch(), request)
	if err != nil || manifest.Session.Sequence != 1 {
		t.Fatalf("CreateSession after torn blob = %#v, %v", manifest, err)
	}
}

func TestDirStoreTreatsTornEpochFilenameAsConsumed(t *testing.T) {
	base := t.TempDir()
	sessionID := uuid.NewString()
	epochDir := filepath.Join(base, "."+sessionID+".writer-epochs")
	if err := os.MkdirAll(epochDir, 0o700); err != nil {
		t.Fatalf("create epoch directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(epochDir, "epoch-00000000000000000001"), nil, 0o600); err != nil {
		t.Fatalf("create torn epoch: %v", err)
	}
	store := sessionstore.NewDirStore(base)
	t.Cleanup(func() { _ = store.Close() })
	lease, err := store.AcquireSessionLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("AcquireSessionLease after torn epoch: %v", err)
	}
	defer lease.Release()
	if lease.Epoch() != 2 {
		t.Fatalf("lease epoch = %d, want 2", lease.Epoch())
	}
}

func TestDirStoreRejectsRehashedInvalidJournalEvents(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*session.Event)
	}{
		{name: "different session", mutate: func(event *session.Event) { event.SessionID = uuid.NewString() }},
		{name: "regressed writer epoch", mutate: func(event *session.Event) { event.WriterEpoch-- }},
		{name: "unbound command digest", mutate: func(event *session.Event) {
			event.CommandDigest = session.DigestJSON("forged")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := t.TempDir()
			sessionID := uuid.NewString()
			store := sessionstore.NewDirStore(base)
			t.Cleanup(func() { _ = store.Close() })
			firstLease, err := store.AcquireSessionLease(context.Background(), sessionID)
			if err != nil {
				t.Fatalf("first lease: %v", err)
			}
			request := createRequest(sessionID)
			if _, err := store.CreateSession(context.Background(), firstLease.Epoch(), request); err != nil {
				t.Fatalf("CreateSession: %v", err)
			}
			if err := firstLease.Release(); err != nil {
				t.Fatalf("release first lease: %v", err)
			}
			secondLease, err := store.AcquireSessionLease(context.Background(), sessionID)
			if err != nil {
				t.Fatalf("second lease: %v", err)
			}
			defer secondLease.Release()
			if _, err := store.UpdateAttempt(context.Background(), session.AttemptUpdateRequest{
				SessionID: sessionID, CommandID: uuid.NewString(), WriterEpoch: secondLease.Epoch(),
				ExpectedSequence: 1, RunID: request.Attempt.RunID, Status: session.AttemptStatusWaiting,
			}); err != nil {
				t.Fatalf("UpdateAttempt: %v", err)
			}
			if _, err := store.UpdateAttempt(context.Background(), session.AttemptUpdateRequest{
				SessionID: sessionID, CommandID: uuid.NewString(), WriterEpoch: secondLease.Epoch(),
				ExpectedSequence: 2, RunID: request.Attempt.RunID, Status: session.AttemptStatusRunning,
			}); err != nil {
				t.Fatalf("second UpdateAttempt: %v", err)
			}
			rewriteLatestJournalEvent(t, base, sessionID, test.mutate)
			if _, err := store.LoadManifest(context.Background(), sessionID); err == nil {
				t.Fatal("LoadManifest accepted a rehashed invalid journal event")
			}
		})
	}
}

func TestDirStoreRejectsJournalRehashedForDifferentDirectorySession(t *testing.T) {
	base := t.TempDir()
	sessionID := uuid.NewString()
	store := sessionstore.NewDirStore(base)
	t.Cleanup(func() { _ = store.Close() })
	lease, err := store.AcquireSessionLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	defer lease.Release()
	if _, err := store.CreateSession(context.Background(), lease.Epoch(), createRequest(sessionID)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	otherSessionID := uuid.NewString()
	rewriteSingleCreationEvent(t, base, sessionID, func(event *session.Event, payload map[string]any) {
		event.SessionID = otherSessionID
		payload["session"].(map[string]any)["session_id"] = otherSessionID
	})
	if _, err := store.LoadManifest(context.Background(), sessionID); err == nil {
		t.Fatal("LoadManifest accepted a journal belonging to a different directory session")
	}
}

func TestDirStoreRejectsCreationPayloadRehashedWithoutCommandIdentity(t *testing.T) {
	base := t.TempDir()
	sessionID := uuid.NewString()
	store := sessionstore.NewDirStore(base)
	t.Cleanup(func() { _ = store.Close() })
	lease, err := store.AcquireSessionLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	defer lease.Release()
	if _, err := store.CreateSession(context.Background(), lease.Epoch(), createRequest(sessionID)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	rewriteSingleCreationEvent(t, base, sessionID, func(_ *session.Event, payload map[string]any) {
		payload["attempt"].(map[string]any)["actor"] = "forged-actor"
	})
	if _, err := store.LoadManifest(context.Background(), sessionID); err == nil {
		t.Fatal("LoadManifest accepted a creation payload detached from its command identity")
	}
}

func TestDirStoreRejectsForgedGeneratedCreationTopology(t *testing.T) {
	base := t.TempDir()
	sessionID := uuid.NewString()
	store := sessionstore.NewDirStore(base)
	t.Cleanup(func() { _ = store.Close() })
	lease, err := store.AcquireSessionLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	defer lease.Release()
	if _, err := store.CreateSession(context.Background(), lease.Epoch(), createRequest(sessionID)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	rewriteSingleCreationEvent(t, base, sessionID, func(_ *session.Event, payload map[string]any) {
		payload["session"].(map[string]any)["active_run_id"] = uuid.NewString()
		payload["attempt"].(map[string]any)["started_at"] = "2020-01-01T00:00:00Z"
	})
	if _, err := store.LoadManifest(context.Background(), sessionID); err == nil {
		t.Fatal("LoadManifest accepted forged generated creation topology")
	}
}

func TestDirStoreRejectsRehashedCreationIdentityThatConflictsWithReservation(t *testing.T) {
	base := t.TempDir()
	sessionID := uuid.NewString()
	store := sessionstore.NewDirStore(base)
	t.Cleanup(func() { _ = store.Close() })
	lease, err := store.AcquireSessionLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	defer lease.Release()
	if _, err := store.CreateSession(context.Background(), lease.Epoch(), createRequest(sessionID)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	rewriteSingleCreationEvent(t, base, sessionID, func(event *session.Event, payload map[string]any) {
		payload["attempt"].(map[string]any)["actor"] = "forged-actor"
		var sessionRecord session.SessionRecord
		var segment session.SegmentRecord
		var attempt session.RunAttemptRecord
		decodeFixtureValue(t, payload["session"], &sessionRecord)
		decodeFixtureValue(t, payload["segment"], &segment)
		decodeFixtureValue(t, payload["attempt"], &attempt)
		identity, err := session.NewCreationIdentity(
			sessionID, sessionRecord, segment, attempt, nil, nil, "",
		)
		if err != nil {
			t.Fatalf("NewCreationIdentity: %v", err)
		}
		identityData, err := json.Marshal(identity)
		if err != nil {
			t.Fatalf("encode identity: %v", err)
		}
		var identityValue any
		if err := json.Unmarshal(identityData, &identityValue); err != nil {
			t.Fatalf("decode identity map: %v", err)
		}
		payload["identity"] = identityValue
		event.CommandDigest = session.CreationDigest(identity)
		payload["session"].(map[string]any)["creation_digest"] = event.CommandDigest
	})
	if _, err := store.LoadManifest(context.Background(), sessionID); err == nil {
		t.Fatal("LoadManifest accepted a rewritten creation identity that conflicts with its reservation")
	}
}

func TestDirStoreConcurrentProjectionReadersDoNotRaceWriter(t *testing.T) {
	base := t.TempDir()
	sessionID := uuid.NewString()
	writer := sessionstore.NewDirStore(base)
	reader := sessionstore.NewDirStore(base)
	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})
	lease, err := writer.AcquireSessionLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("AcquireSessionLease: %v", err)
	}
	defer lease.Release()
	created, err := writer.CreateSession(context.Background(), lease.Epoch(), createRequest(sessionID))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	runID := created.Session.ActiveRunID
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	readerErrors := make(chan error, 1)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for ctx.Err() == nil {
			if _, loadErr := reader.LoadManifest(ctx, sessionID); loadErr != nil && !errors.Is(loadErr, context.Canceled) {
				select {
				case readerErrors <- loadErr:
				default:
				}
				return
			}
		}
	}()
	manifest := created
	for sequence := int64(1); sequence <= 100; sequence++ {
		direct := engine.Event{
			EventID: uuid.NewString(), RunID: runID, RunbookID: "root",
			Timestamp: time.Now().UTC().Format(time.RFC3339Nano), Kind: "step/output", Sequence: sequence,
			Payload: map[string]any{"line": sequence},
		}
		manifest, err = writer.CommitTraceEvent(context.Background(), session.TraceCommitRequest{
			SessionID: sessionID, CommandID: uuid.NewString(), WriterEpoch: lease.Epoch(),
			ExpectedSequence: manifest.Session.Sequence, RunID: runID, Event: direct,
		})
		if err != nil {
			t.Fatalf("CommitTraceEvent %d: %v", sequence, err)
		}
	}
	cancel()
	<-readerDone
	select {
	case readerErr := <-readerErrors:
		t.Fatalf("concurrent LoadManifest: %v", readerErr)
	default:
	}
	if manifest.Session.Sequence != 101 {
		t.Fatalf("final sequence = %d", manifest.Session.Sequence)
	}
}

func decodeFixtureValue(t *testing.T, value any, target any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode fixture value: %v", err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatalf("decode fixture value: %v", err)
	}
}

func rewriteSingleCreationEvent(
	t *testing.T,
	base string,
	sessionID string,
	mutate func(*session.Event, map[string]any),
) {
	t.Helper()
	eventsPath := filepath.Join(base, sessionID, "events.jsonl")
	data, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatalf("read creation journal: %v", err)
	}
	var event session.Event
	if err := json.Unmarshal(bytes.TrimSpace(data), &event); err != nil {
		t.Fatalf("decode creation event: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatalf("decode creation payload: %v", err)
	}
	mutate(&event, payload)
	event.Payload, err = json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode creation payload: %v", err)
	}
	event.Digest = ""
	unsigned, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("encode unsigned creation event: %v", err)
	}
	event.Digest = session.DigestBytes(unsigned)
	eventData, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("encode creation event: %v", err)
	}
	if err := os.WriteFile(eventsPath, append(eventData, '\n'), 0o600); err != nil {
		t.Fatalf("rewrite creation journal: %v", err)
	}
	headPath := filepath.Join(base, sessionID, "heads", "head-00000000000000000001.json")
	headData, err := os.ReadFile(headPath)
	if err != nil {
		t.Fatalf("read creation head: %v", err)
	}
	var head map[string]any
	if err := json.Unmarshal(headData, &head); err != nil {
		t.Fatalf("decode creation head: %v", err)
	}
	head["event_digest"] = event.Digest
	headData, err = json.Marshal(head)
	if err != nil {
		t.Fatalf("encode creation head: %v", err)
	}
	if err := os.WriteFile(headPath, headData, 0o600); err != nil {
		t.Fatalf("rewrite creation head: %v", err)
	}
}

func rewriteLatestJournalEvent(t *testing.T, base, sessionID string, mutate func(*session.Event)) {
	t.Helper()
	eventsPath := filepath.Join(base, sessionID, "events.jsonl")
	contents, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	lines := bytes.Split(bytes.TrimSuffix(contents, []byte{'\n'}), []byte{'\n'})
	if len(lines) < 2 {
		t.Fatalf("journal has %d lines", len(lines))
	}
	var event session.Event
	if err := json.Unmarshal(lines[len(lines)-1], &event); err != nil {
		t.Fatalf("decode latest event: %v", err)
	}
	mutate(&event)
	event.Digest = ""
	unsigned, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("encode unsigned event: %v", err)
	}
	event.Digest = session.DigestBytes(unsigned)
	lines[len(lines)-1], err = json.Marshal(event)
	if err != nil {
		t.Fatalf("encode latest event: %v", err)
	}
	if err := os.WriteFile(eventsPath, append(bytes.Join(lines, []byte{'\n'}), '\n'), 0o600); err != nil {
		t.Fatalf("rewrite events: %v", err)
	}
	headPath := filepath.Join(base, sessionID, "heads", fmt.Sprintf("head-%020d.json", event.Sequence))
	headData, err := os.ReadFile(headPath)
	if err != nil {
		t.Fatalf("read latest head: %v", err)
	}
	var head struct {
		SchemaVersion string `json:"schema_version"`
		SessionID     string `json:"session_id"`
		Sequence      int64  `json:"sequence"`
		EventDigest   string `json:"event_digest"`
		WriterEpoch   uint64 `json:"writer_epoch"`
	}
	if err := json.Unmarshal(headData, &head); err != nil {
		t.Fatalf("decode latest head: %v", err)
	}
	head.EventDigest = event.Digest
	head.WriterEpoch = event.WriterEpoch
	headData, err = json.Marshal(head)
	if err != nil {
		t.Fatalf("encode latest head: %v", err)
	}
	if err := os.WriteFile(headPath, headData, 0o600); err != nil {
		t.Fatalf("rewrite latest head: %v", err)
	}
}

func TestDirStoreTruncatesIncompleteJournalTailBeforeNextCommit(t *testing.T) {
	base := t.TempDir()
	sessionID := uuid.NewString()
	store := sessionstore.NewDirStore(base)
	t.Cleanup(func() { _ = store.Close() })
	lease, err := store.AcquireSessionLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	request := createRequest(sessionID)
	if _, err := store.CreateSession(context.Background(), lease.Epoch(), request); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := lease.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	eventsPath := filepath.Join(base, sessionID, "events.jsonl")
	file, err := os.OpenFile(eventsPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	if _, err := file.WriteString(`{"sequence":2`); err != nil {
		t.Fatalf("append partial: %v", err)
	}
	_ = file.Close()

	lease, err = store.AcquireSessionLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("second lease: %v", err)
	}
	defer lease.Release()
	manifest, err := store.UpdateAttempt(context.Background(), session.AttemptUpdateRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), WriterEpoch: lease.Epoch(),
		ExpectedSequence: 1, RunID: request.Attempt.RunID, Status: session.AttemptStatusCompleted,
	})
	if err != nil {
		t.Fatalf("complete after partial tail: %v", err)
	}
	manifest, err = store.CloseSession(context.Background(), session.CloseRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), WriterEpoch: lease.Epoch(),
		ExpectedSequence: manifest.Session.Sequence, Status: session.StatusResolved,
	})
	if err != nil {
		t.Fatalf("CloseSession after partial tail: %v", err)
	}
	if manifest.Session.Sequence != 3 || manifest.Session.Status != session.StatusResolved {
		t.Fatalf("manifest after tail recovery = %#v", manifest)
	}
	events, err := store.ReadEvents(context.Background(), sessionID, 0)
	if err != nil || len(events) != 3 {
		t.Fatalf("events after tail recovery = %#v, %v", events, err)
	}
}

func createRequest(sessionID string) session.CreateRequest {
	segmentID := uuid.NewString()
	runID := uuid.NewString()
	planBlob := session.NewJSONBlob(json.RawMessage(`{"schema_version":"execution-plan/v3"}`))
	graphBlob := session.NewJSONBlob(json.RawMessage(`{"schema_version":"yawr.graph-json/v1","nodes":[],"edges":[]}`))
	return bindCreationIdentity(session.CreateRequest{
		SessionID: sessionID, CommandID: uuid.NewString(),
		Session: session.SessionRecord{Status: session.StatusActive},
		Segment: session.SegmentRecord{
			SegmentID: segmentID, Ordinal: 1, RunbookID: "root", RunbookName: "Root",
			Status: session.SegmentStatusActive, PlanHash: planBlob.Digest, GraphHash: graphBlob.Digest,
			ExecutableSnapshotHash: planBlob.Digest, AttemptRunIDs: []string{runID},
		},
		Attempt: session.RunAttemptRecord{
			RunID: runID, SegmentID: segmentID, Ordinal: 1, Mode: "real", Status: session.AttemptStatusRunning,
		},
		Blobs: []session.JSONBlob{planBlob, graphBlob},
	})
}

func bindCreationIdentity(request session.CreateRequest) session.CreateRequest {
	identity, err := session.NewCreationIdentity(
		request.SessionID, request.Session, request.Segment, request.Attempt, nil, nil, "",
	)
	if err != nil {
		panic(err)
	}
	request.Identity = identity
	request.EntryDigest = session.CreationDigest(identity)
	return request
}
