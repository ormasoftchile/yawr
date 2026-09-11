package sessionstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ormasoftchile/yawr/runtime/pkg/session"
)

func TestWriteManifestBoundsAcceptedCommandProjection(t *testing.T) {
	const receiptCount = 20_000
	sessionID := uuid.NewString()
	manifest := session.Manifest{
		SchemaVersion:    session.ManifestSchemaV1,
		Session:          session.SessionRecord{SessionID: sessionID, Sequence: receiptCount},
		Segments:         make(map[string]session.SegmentRecord),
		Attempts:         make(map[string]session.RunAttemptRecord),
		AcceptedCommands: make(map[string]session.CommandReceipt, receiptCount),
	}
	for index := 0; index < receiptCount; index++ {
		commandID := fmt.Sprintf("command-%05d-xxxxxxxxxxxxxxxxxxxx", index)
		manifest.AcceptedCommands[commandID] = session.CommandReceipt{
			CommandID: commandID, CommandDigest: session.DigestJSON(index),
			ClientCommandDigest: session.DigestJSON(struct{ Index int }{index}),
			EventKind:           session.EventSessionDetached, Sequence: int64(index + 1),
		}
	}
	store := NewDirStore(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	if err := store.writeManifest(manifest); err != nil {
		t.Fatalf("writeManifest: %v", err)
	}
	encoded, err := readFileBounded(store.manifestPath(sessionID), maxJournalEventBytes)
	if err != nil {
		t.Fatalf("bounded manifest projection: %v", err)
	}
	var projected session.Manifest
	if err := json.Unmarshal(encoded, &projected); err != nil {
		t.Fatalf("decode manifest projection: %v", err)
	}
	if len(projected.AcceptedCommands) > 256 {
		t.Fatalf("projected receipt count = %d, want at most 256", len(projected.AcceptedCommands))
	}
	if _, found := projected.AcceptedCommands["command-19999-xxxxxxxxxxxxxxxxxxxx"]; !found {
		t.Fatal("manifest projection omitted newest receipt")
	}
	if _, found := projected.AcceptedCommands["command-00000-xxxxxxxxxxxxxxxxxxxx"]; found {
		t.Fatal("manifest projection retained oldest receipt")
	}
	if len(manifest.AcceptedCommands) != receiptCount {
		t.Fatalf("authoritative receipt count changed to %d", len(manifest.AcceptedCommands))
	}
}

func TestLoadManifestIgnoresOversizedOptionalProjection(t *testing.T) {
	store := NewDirStore(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	sessionID := createManifestProjectionTestSession(t, store)
	if err := os.WriteFile(
		store.manifestPath(sessionID), bytes.Repeat([]byte("x"), maxJournalEventBytes+1), 0o600,
	); err != nil {
		t.Fatalf("write oversized manifest projection: %v", err)
	}
	manifest, err := store.LoadManifest(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("LoadManifest rejected valid journal: %v", err)
	}
	if manifest.Session.SessionID != sessionID || len(manifest.AcceptedCommands) != 1 {
		t.Fatalf("journal manifest = %#v", manifest)
	}
}

func TestOversizedManifestProjectionStillPublishesFrameUpdate(t *testing.T) {
	store := NewDirStore(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	sessionID := uuid.NewString()
	commandID := uuid.NewString()
	updates := store.SessionUpdates(sessionID)
	event := session.Event{
		SessionID: sessionID, Sequence: 1, CommandID: commandID,
		Kind: session.EventSessionDetached, Payload: json.RawMessage(`{"reason":"transport detached"}`),
	}
	store.cacheCommittedFrameEvent(event)
	manifest := session.Manifest{
		SchemaVersion: session.ManifestSchemaV1,
		Session: session.SessionRecord{
			SessionID: sessionID, Status: session.StatusIndeterminate, Sequence: 1,
		},
		Segments: map[string]session.SegmentRecord{
			"oversized": {SegmentID: "oversized", RunbookName: strings.Repeat("x", maxJournalEventBytes)},
		},
		Attempts: make(map[string]session.RunAttemptRecord),
		AcceptedCommands: map[string]session.CommandReceipt{
			commandID: {
				CommandID: commandID, EventKind: event.Kind, Sequence: event.Sequence,
			},
		},
	}
	if err := store.writeManifest(manifest); err == nil {
		t.Fatal("writeManifest accepted an oversized projection")
	}
	select {
	case <-updates:
	default:
		t.Fatal("committed frame update was not published")
	}
	replayed, states, headSequence, ok := store.cachedFrameEvents(sessionID, 0)
	if !ok || len(replayed) != 1 || len(states) != 1 || headSequence != 1 {
		t.Fatalf("cached frame events = %d/%d/%d/%v", len(replayed), len(states), headSequence, ok)
	}
}

func TestLoadManifestPublishesRecoveredEventAfterJournalHeadConflict(t *testing.T) {
	ctx := context.Background()
	store := NewDirStore(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	sessionID := createManifestProjectionTestSession(t, store)
	lease, err := store.AcquireSessionLease(ctx, sessionID)
	if err != nil {
		t.Fatalf("AcquireSessionLease: %v", err)
	}
	defer lease.Release()

	initial, _, headSequence, err := store.ReadFrameEvents(ctx, sessionID, 0)
	if err != nil || len(initial) != 1 || headSequence != 1 {
		t.Fatalf("prime frame cache = %d/%d, %v", len(initial), headSequence, err)
	}
	updates := store.SessionUpdates(sessionID)
	conflictingHead := store.headPath(sessionID, 2)
	if err := os.WriteFile(conflictingHead, []byte(`{"conflict":true}`), 0o600); err != nil {
		t.Fatalf("write conflicting head: %v", err)
	}
	request := session.AttemptUpdateRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), WriterEpoch: lease.Epoch(),
		ExpectedSequence: 1, Status: session.AttemptStatusWaiting,
	}
	manifest, err := store.LoadManifest(ctx, sessionID)
	if err != nil {
		t.Fatalf("LoadManifest before failed publication: %v", err)
	}
	request.RunID = manifest.Session.ActiveRunID
	if _, err := store.UpdateAttempt(ctx, request); !errors.Is(err, session.ErrProjection) {
		t.Fatalf("UpdateAttempt error = %v, want ErrProjection", err)
	}
	recovered, err := store.LoadManifest(ctx, sessionID)
	if err != nil {
		t.Fatalf("LoadManifest recovery: %v", err)
	}
	if recovered.Session.Sequence != 2 ||
		recovered.Attempts[request.RunID].Status != session.AttemptStatusWaiting {
		t.Fatalf("recovered manifest = %#v", recovered)
	}
	select {
	case <-updates:
	default:
		t.Fatal("journal recovery did not publish a session update")
	}
	replayed, states, headSequence, err := store.ReadFrameEvents(ctx, sessionID, 1)
	if err != nil || headSequence != 2 || len(replayed) != 1 || replayed[0].Sequence != 2 || len(states) != 1 {
		t.Fatalf("recovered frame suffix = %#v states=%d head=%d, %v", replayed, len(states), headSequence, err)
	}
	if _, err := store.LoadManifest(ctx, sessionID); err != nil {
		t.Fatalf("second LoadManifest: %v", err)
	}
	select {
	case <-updates:
		t.Fatal("journal recovery republished an already-cached event")
	default:
	}
}

func TestFrameManifestProjectionStaysLinearAcrossThousandsOfEvents(t *testing.T) {
	const eventCount = 5_000
	sessionID := uuid.NewString()
	manifest := session.Manifest{
		SchemaVersion: session.ManifestSchemaV1,
		Session: session.SessionRecord{
			SessionID: sessionID, Status: session.StatusIndeterminate,
		},
		Segments: make(map[string]session.SegmentRecord), Attempts: make(map[string]session.RunAttemptRecord),
		Transitions: make(map[string]session.TransitionRecord), Occurrences: make(map[string]session.OccurrenceRecord),
		AcceptedCommands: make(map[string]session.CommandReceipt, eventCount),
	}
	projectedReceipts := 0
	projectedBytes := 0
	for index := 1; index <= eventCount; index++ {
		commandID := fmt.Sprintf("command-%05d", index)
		event := session.Event{
			SessionID: sessionID, Sequence: int64(index), CommandID: commandID,
			Kind: session.EventSessionDetached, Payload: json.RawMessage(`{"reason":"transport detached"}`),
		}
		manifest.Session.Sequence = event.Sequence
		manifest.AcceptedCommands[commandID] = session.CommandReceipt{
			CommandID: commandID, CommandDigest: session.DetachDigest("transport detached"),
			ClientCommandDigest: session.DigestJSON(commandID),
			EventKind:           event.Kind, Sequence: event.Sequence,
		}
		projected, err := frameManifestProjection(event, manifest)
		if err != nil {
			t.Fatalf("frameManifestProjection sequence %d: %v", index, err)
		}
		projectedReceipts += len(projected.AcceptedCommands)
		encoded, err := json.Marshal(projected)
		if err != nil {
			t.Fatalf("marshal projected sequence %d: %v", index, err)
		}
		projectedBytes += len(encoded)
	}
	if projectedReceipts != eventCount {
		t.Fatalf("projected receipt operations = %d, want %d", projectedReceipts, eventCount)
	}
	if projectedBytes > eventCount*2_048 {
		t.Fatalf("projected bytes = %d, want at most %d", projectedBytes, eventCount*2_048)
	}
	if len(manifest.AcceptedCommands) != eventCount {
		t.Fatalf("authoritative receipt count = %d, want %d", len(manifest.AcceptedCommands), eventCount)
	}
}

func TestLeaseHeldCommitsDoNotRescanFullJournal(t *testing.T) {
	const commitCount = 100
	store := NewDirStore(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	sessionID := createManifestProjectionTestSession(t, store)
	lease, err := store.AcquireSessionLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("AcquireSessionLease: %v", err)
	}
	defer lease.Release()
	manifest, err := store.LoadManifest(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	manifest, err = store.UpdateAttempt(context.Background(), session.AttemptUpdateRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), WriterEpoch: lease.Epoch(),
		ExpectedSequence: manifest.Session.Sequence, RunID: manifest.Session.ActiveRunID,
		Status: session.AttemptStatusIndeterminate,
	})
	if err != nil {
		t.Fatalf("UpdateAttempt: %v", err)
	}
	store.journalScans.Store(0)
	for index := 0; index < commitCount; index++ {
		manifest, err = store.RecordDetach(context.Background(), session.DetachRequest{
			SessionID:   sessionID,
			CommandID:   uuid.NewSHA1(uuid.MustParse(sessionID), []byte(fmt.Sprintf("detach-%03d", index))).String(),
			WriterEpoch: lease.Epoch(), ExpectedSequence: manifest.Session.Sequence,
			Reason: "transport detached", ClientCommandDigest: session.DigestJSON(index),
		})
		if err != nil {
			t.Fatalf("RecordDetach %d: %v", index, err)
		}
	}
	if scans := store.journalScans.Load(); scans > 1 {
		t.Fatalf("full journal scans = %d for %d commits, want at most 1", scans, commitCount)
	}
}

func TestReadFrameEventsColdReplayIsBoundedAndRebuildsAllReceipts(t *testing.T) {
	const receiptCount = 5_000
	base := t.TempDir()
	store := NewDirStore(base)
	sessionID := createManifestProjectionTestSession(t, store)
	lease, err := store.AcquireSessionLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("AcquireSessionLease: %v", err)
	}
	manifest, err := store.LoadManifest(context.Background(), sessionID)
	if err != nil {
		_ = lease.Release()
		t.Fatalf("LoadManifest: %v", err)
	}
	manifest, err = store.UpdateAttempt(context.Background(), session.AttemptUpdateRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), WriterEpoch: lease.Epoch(),
		ExpectedSequence: manifest.Session.Sequence, RunID: manifest.Session.ActiveRunID,
		Status: session.AttemptStatusIndeterminate,
	})
	if err != nil {
		_ = lease.Release()
		t.Fatalf("UpdateAttempt indeterminate: %v", err)
	}
	events, err := store.loadEvents(context.Background(), sessionID)
	if err != nil {
		_ = lease.Release()
		t.Fatalf("loadEvents: %v", err)
	}
	previousDigest := events[len(events)-1].Digest
	payload := json.RawMessage(`{"reason":"transport detached"}`)
	timestamp := time.Now().UTC().Format(time.RFC3339Nano)
	var suffix bytes.Buffer
	var finalEvent session.Event
	firstCommandID := ""
	for index := 0; index < receiptCount; index++ {
		commandID := uuid.NewSHA1(uuid.MustParse(sessionID), []byte(fmt.Sprintf("bulk-detach-%05d", index))).String()
		if index == 0 {
			firstCommandID = commandID
		}
		finalEvent, err = newEventWithClientCommand(
			sessionID, lease.Epoch(), manifest.Session.Sequence+int64(index)+1, previousDigest,
			commandID, session.DetachDigest("transport detached"), session.DigestJSON(commandID),
			session.EventSessionDetached, timestamp, payload,
		)
		if err != nil {
			_ = lease.Release()
			t.Fatalf("newClientCommandEvent %d: %v", index, err)
		}
		encoded, marshalErr := json.Marshal(finalEvent)
		if marshalErr != nil {
			_ = lease.Release()
			t.Fatalf("marshal event %d: %v", index, marshalErr)
		}
		suffix.Write(encoded)
		suffix.WriteByte('\n')
		previousDigest = finalEvent.Digest
	}
	journal, err := os.OpenFile(store.eventsPath(sessionID), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		_ = lease.Release()
		t.Fatalf("open journal: %v", err)
	}
	if _, err := journal.Write(suffix.Bytes()); err != nil {
		_ = journal.Close()
		_ = lease.Release()
		t.Fatalf("append journal fixture: %v", err)
	}
	if err := journal.Sync(); err != nil {
		_ = journal.Close()
		_ = lease.Release()
		t.Fatalf("sync journal fixture: %v", err)
	}
	if err := journal.Close(); err != nil {
		_ = lease.Release()
		t.Fatalf("close journal fixture: %v", err)
	}
	if err := store.writeJournalHead(journalHead{
		SchemaVersion: journalHeadSchemaV1, SessionID: sessionID, Sequence: finalEvent.Sequence,
		EventDigest: finalEvent.Digest, WriterEpoch: finalEvent.WriterEpoch,
	}); err != nil {
		_ = lease.Release()
		t.Fatalf("write final journal head: %v", err)
	}
	if err := lease.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened := NewDirStore(base)
	t.Cleanup(func() { _ = reopened.Close() })
	replayed, states, headSequence, err := reopened.ReadFrameEvents(context.Background(), sessionID, 0)
	if err != nil {
		t.Fatalf("ReadFrameEvents: %v", err)
	}
	wantEvents := receiptCount + 2
	if len(replayed) != wantEvents || len(states) != wantEvents || headSequence != int64(wantEvents) {
		t.Fatalf("replayed events/states/head = %d/%d/%d, want %d/%d/%d",
			len(replayed), len(states), headSequence, wantEvents, wantEvents, wantEvents)
	}
	projectedReceipts := 0
	projectedBytes := 0
	for sequence, state := range states {
		projectedReceipts += len(state.AcceptedCommands)
		encoded, err := json.Marshal(state)
		if err != nil {
			t.Fatalf("marshal frame state %d: %v", sequence, err)
		}
		projectedBytes += len(encoded)
	}
	if projectedReceipts != wantEvents {
		t.Fatalf("projected receipt operations = %d, want %d", projectedReceipts, wantEvents)
	}
	if projectedBytes > wantEvents*4_096 {
		t.Fatalf("projected bytes = %d, want at most %d", projectedBytes, wantEvents*4_096)
	}
	authoritative, err := reopened.LoadManifest(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("LoadManifest authoritative: %v", err)
	}
	if len(authoritative.AcceptedCommands) != wantEvents {
		t.Fatalf("authoritative receipt count = %d, want %d", len(authoritative.AcceptedCommands), wantEvents)
	}
	if _, found := authoritative.AcceptedCommands[firstCommandID]; !found {
		t.Fatal("journal rebuild omitted oldest synthetic receipt")
	}
}

func createManifestProjectionTestSession(t *testing.T, store *DirStore) string {
	t.Helper()
	sessionID, segmentID, runID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	planBlob := session.NewJSONBlob(json.RawMessage(`{"schema_version":"yawr.execution-plan/v1"}`))
	graphBlob := session.NewJSONBlob(json.RawMessage(`{"schema_version":"yawr.graph-json/v1","nodes":[],"edges":[]}`))
	request := session.CreateRequest{
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
	}
	identity, err := session.NewCreationIdentity(
		sessionID, request.Session, request.Segment, request.Attempt, nil, nil, "",
	)
	if err != nil {
		t.Fatalf("NewCreationIdentity: %v", err)
	}
	request.Identity = identity
	request.EntryDigest = session.CreationDigest(identity)
	lease, err := store.AcquireSessionLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("AcquireSessionLease: %v", err)
	}
	if _, err := store.CreateSession(context.Background(), lease.Epoch(), request); err != nil {
		_ = lease.Release()
		t.Fatalf("CreateSession: %v", err)
	}
	if err := lease.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	return sessionID
}
