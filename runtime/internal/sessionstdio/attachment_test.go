package sessionstdio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ormasoftchile/yawr/runtime/internal/runstore"
	"github.com/ormasoftchile/yawr/runtime/internal/serve"
	"github.com/ormasoftchile/yawr/runtime/internal/sessioncoordinator"
	"github.com/ormasoftchile/yawr/runtime/internal/sessionstore"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/session"
)

type attachmentLease struct {
	epoch      uint64
	releases   int
	releaseErr error
}

func (lease *attachmentLease) Epoch() uint64 { return lease.epoch }
func (lease *attachmentLease) Advance() (uint64, error) {
	lease.epoch++
	return lease.epoch, nil
}
func (lease *attachmentLease) Release() error {
	lease.releases++
	return lease.releaseErr
}

type attachmentStore struct {
	frameStore
	lease       *attachmentLease
	acquireErr  error
	manifest    session.Manifest
	manifestErr error
	pauseCalls  int
	updates     chan struct{}
}

func (store *attachmentStore) SessionUpdates(string) <-chan struct{} {
	if store.updates == nil {
		store.updates = make(chan struct{}, 1)
	}
	return store.updates
}

type recordingCommandHandler struct {
	calls int
	err   error
}

func (handler *recordingCommandHandler) HandleSessionCommand(
	_ context.Context,
	_ *Attachment,
	_ session.StdioCommand,
	_ string,
) error {
	handler.calls++
	return handler.err
}

func (store *attachmentStore) AcquireSessionLease(context.Context, string) (session.Lease, error) {
	if store.acquireErr != nil {
		return nil, store.acquireErr
	}
	return store.lease, nil
}

func (store *attachmentStore) LoadManifest(context.Context, string) (session.Manifest, error) {
	return store.manifest, store.manifestErr
}

func (store *attachmentStore) ReadFrameEvents(
	ctx context.Context,
	sessionID string,
	afterSequence int64,
) ([]session.Event, map[int64]session.Manifest, int64, error) {
	events, manifests, headSequence, err := store.frameStore.ReadFrameEvents(ctx, sessionID, afterSequence)
	if err != nil {
		return nil, nil, 0, err
	}
	manifestSequence := store.manifest.Session.Sequence
	if manifestSequence > headSequence && afterSequence == manifestSequence {
		return nil, manifests, manifestSequence, nil
	}
	return events, manifests, headSequence, nil
}

func (store *attachmentStore) PauseSession(_ context.Context, request session.PauseRequest) (session.Manifest, error) {
	store.pauseCalls++
	if store.frameStore.manifests == nil {
		store.frameStore.manifests = make(map[int64]session.Manifest)
	}
	store.frameStore.manifests[store.manifest.Session.Sequence] = store.manifest
	store.manifest.Session.Status = session.StatusPaused
	store.manifest.Session.Sequence++
	store.manifest.AcceptedCommands[request.CommandID] = session.CommandReceipt{
		CommandID: request.CommandID, CommandDigest: session.PauseDigest(store.manifest.Session.ActiveRunID, request.Reason),
		ClientCommandDigest: request.ClientCommandDigest,
		EventKind:           session.EventSessionPaused, Sequence: store.manifest.Session.Sequence,
	}
	payload, _ := json.Marshal(map[string]any{
		"run_id": store.manifest.Session.ActiveRunID, "reason": request.Reason,
	})
	store.frameStore.events = append(store.frameStore.events, session.Event{
		EventID: request.CommandID, SessionID: request.SessionID, Sequence: store.manifest.Session.Sequence,
		WriterEpoch: request.WriterEpoch, CommandID: request.CommandID,
		Kind: session.EventSessionPaused, Payload: payload,
	})
	store.frameStore.manifests[store.manifest.Session.Sequence] = store.manifest
	select {
	case store.updates <- struct{}{}:
	default:
	}
	return store.manifest, nil
}

func (store *attachmentStore) RecordDetach(
	_ context.Context,
	request session.DetachRequest,
) (session.Manifest, error) {
	if receipt, found := store.manifest.AcceptedCommands[request.CommandID]; found {
		if receipt.ClientCommandDigest != request.ClientCommandDigest ||
			receipt.CommandDigest != session.DetachDigest(request.Reason) {
			return session.Manifest{}, session.ErrCommandConflict
		}
		return store.manifest, nil
	}
	store.frameStore.manifests[store.manifest.Session.Sequence] = store.manifest
	store.manifest.Session.Sequence++
	store.manifest.AcceptedCommands[request.CommandID] = session.CommandReceipt{
		CommandID: request.CommandID, CommandDigest: session.DetachDigest(request.Reason),
		ClientCommandDigest: request.ClientCommandDigest,
		EventKind:           session.EventSessionDetached, Sequence: store.manifest.Session.Sequence,
	}
	payload, _ := json.Marshal(map[string]any{"reason": request.Reason})
	store.frameStore.events = append(store.frameStore.events, session.Event{
		EventID: request.CommandID, SessionID: request.SessionID, Sequence: store.manifest.Session.Sequence,
		WriterEpoch: request.WriterEpoch, CommandID: request.CommandID,
		Kind: session.EventSessionDetached, Payload: payload,
	})
	store.frameStore.manifests[store.manifest.Session.Sequence] = store.manifest
	select {
	case store.updates <- struct{}{}:
	default:
	}
	return store.manifest, nil
}

type failingFrameWriter struct{}

func (failingFrameWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

type failAfterFrameWriter struct {
	successfulWrites int
	writes           int
	output           bytes.Buffer
}

func (writer *failAfterFrameWriter) Write(data []byte) (int, error) {
	if writer.writes >= writer.successfulWrites {
		return 0, io.ErrClosedPipe
	}
	writer.writes++
	return writer.output.Write(data)
}

func TestAttachOwnsLeaseWhileStreamingJournalFrames(t *testing.T) {
	const sessionID = "11111111-1111-4111-8111-111111111111"
	payload, _ := json.Marshal(struct {
		Session session.SessionRecord    `json:"session"`
		Segment session.SegmentRecord    `json:"segment"`
		Attempt session.RunAttemptRecord `json:"attempt"`
	}{
		Session: session.SessionRecord{SessionID: sessionID, Status: session.StatusActive, Sequence: 1},
		Segment: session.SegmentRecord{SegmentID: "segment", GraphHash: session.DigestBytes([]byte(`{}`)), GraphRevision: 1},
		Attempt: session.RunAttemptRecord{RunID: "run", SegmentID: "segment", Status: session.AttemptStatusStarting},
	})
	lease := &attachmentLease{epoch: 7}
	store := &attachmentStore{
		frameStore: frameStore{
			events: []session.Event{{
				EventID: "event", SessionID: sessionID, Sequence: 1, WriterEpoch: 7,
				Kind: session.EventSessionCreated, Payload: payload,
			}},
			blobs: map[string]json.RawMessage{session.DigestBytes([]byte(`{}`)): json.RawMessage(`{}`)},
			manifests: map[int64]session.Manifest{1: {
				SchemaVersion:    session.ManifestSchemaV1,
				Session:          session.SessionRecord{SessionID: sessionID, Status: session.StatusActive, Sequence: 1},
				AcceptedCommands: make(map[string]session.CommandReceipt),
			}},
		},
		lease:    lease,
		manifest: pausedAttachmentManifest(sessionID, 1),
	}
	var output bytes.Buffer
	attachment, err := Attach(context.Background(), store, sessionID, 0, &output)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if attachment.WriterEpoch() != 7 || lease.releases != 0 || output.Len() == 0 {
		t.Fatalf("attachment epoch/releases/output = %d/%d/%q", attachment.WriterEpoch(), lease.releases, output.String())
	}
	decoder := json.NewDecoder(&output)
	count := 0
	for {
		var frame session.StdioFrame
		if err := decoder.Decode(&frame); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if frame.WriterEpoch != 7 {
			t.Fatalf("frame writer epoch = %d", frame.WriterEpoch)
		}
		count++
	}
	if count == 0 {
		t.Fatal("attach emitted no frames")
	}
	if err := attachment.Release(); err != nil || lease.releases != 1 {
		t.Fatalf("Release = %v, count %d", err, lease.releases)
	}
	if err := attachment.Release(); err != nil || lease.releases != 1 {
		t.Fatalf("second Release = %v, count %d", err, lease.releases)
	}
}

func TestAttachReturnsStructuredActiveWriterError(t *testing.T) {
	store := &attachmentStore{acquireErr: session.ErrSessionLeaseHeld}
	_, err := Attach(context.Background(), store, "11111111-1111-4111-8111-111111111111", 0, io.Discard)
	var protocolErr *ProtocolError
	if !errors.As(err, &protocolErr) || protocolErr.Code != ErrorSessionAlreadyActive {
		t.Fatalf("Attach error = %#v", err)
	}
}

func TestAttachJoinsManifestLoadAndLeaseReleaseErrors(t *testing.T) {
	manifestErr := errors.New("manifest load failed")
	releaseErr := errors.New("lease release failed")
	lease := &attachmentLease{epoch: 8, releaseErr: releaseErr}
	store := &attachmentStore{lease: lease, manifestErr: manifestErr}
	_, err := Attach(
		context.Background(), store, "11111111-1111-4111-8111-111111111111", 0, io.Discard,
	)
	if !errors.Is(err, manifestErr) || !errors.Is(err, releaseErr) {
		t.Fatalf("Attach error = %v, want manifest and release failures", err)
	}
	if lease.releases != 1 {
		t.Fatalf("lease release count = %d, want 1", lease.releases)
	}
}

func TestAttachAllowsOrphanedActiveAndCompletedUnclosedSessions(t *testing.T) {
	const sessionID = "11111111-1111-4111-8111-111111111111"
	for _, test := range []struct {
		name     string
		manifest session.Manifest
	}{
		{
			name: "orphaned active",
			manifest: session.Manifest{
				SchemaVersion: session.ManifestSchemaV1,
				Session: session.SessionRecord{
					SessionID: sessionID, Status: session.StatusActive,
					ActiveSegmentID: "segment", ActiveRunID: "run", Sequence: 1,
				},
				Segments: map[string]session.SegmentRecord{
					"segment": {SegmentID: "segment", Status: session.SegmentStatusActive},
				},
				Attempts: map[string]session.RunAttemptRecord{
					"run": {RunID: "run", SegmentID: "segment", Status: session.AttemptStatusWaiting},
				},
				AcceptedCommands: make(map[string]session.CommandReceipt),
			},
		},
		{
			name: "completed unclosed",
			manifest: session.Manifest{
				SchemaVersion: session.ManifestSchemaV1,
				Session: session.SessionRecord{
					SessionID: sessionID, Status: session.StatusPaused, Sequence: 1,
				},
				Segments: map[string]session.SegmentRecord{
					"segment": {SegmentID: "segment", Status: session.SegmentStatusCompleted},
				},
				Attempts: map[string]session.RunAttemptRecord{
					"run": {RunID: "run", SegmentID: "segment", Status: session.AttemptStatusCompleted},
				},
				AcceptedCommands: make(map[string]session.CommandReceipt),
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			lease := &attachmentLease{epoch: 20}
			store := &attachmentStore{lease: lease, manifest: test.manifest}
			attachment, err := Attach(context.Background(), store, sessionID, 1, io.Discard)
			if err != nil {
				t.Fatalf("Attach: %v", err)
			}
			if err := attachment.Release(); err != nil || lease.releases != 1 {
				t.Fatalf("Release = %v, count %d", err, lease.releases)
			}
		})
	}
}

func TestAttachAtHeadEmitsCurrentWriterEpochSnapshot(t *testing.T) {
	const sessionID = "11111111-1111-4111-8111-111111111111"
	lease := &attachmentLease{epoch: 22}
	manifest := pausedAttachmentManifest(sessionID, 4)
	store := &attachmentStore{lease: lease, manifest: manifest}
	var output bytes.Buffer
	attachment, err := Attach(context.Background(), store, sessionID, 4, &output)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer attachment.Release()
	var frame session.StdioFrame
	if err := json.NewDecoder(&output).Decode(&frame); err != nil {
		t.Fatalf("decode attachment handshake: %v", err)
	}
	if frame.Type != session.FrameSessionSnapshot || frame.SessionID != sessionID ||
		frame.SessionSequence != 4 || frame.WriterEpoch != 22 ||
		frame.SequenceIndex != 0 || frame.SequenceCount != 1 || frame.FrameID == "" {
		t.Fatalf("attachment handshake = %#v", frame)
	}
	var snapshot session.Manifest
	if err := json.Unmarshal(frame.Payload, &snapshot); err != nil || snapshot.Session.Sequence != 4 {
		t.Fatalf("attachment snapshot = %#v, %v", snapshot, err)
	}
}

func TestAttachmentGraphSegmentIDUsesActiveOrLatestSegment(t *testing.T) {
	manifest := session.Manifest{
		Session: session.SessionRecord{ActiveSegmentID: "active"},
		Segments: map[string]session.SegmentRecord{
			"source": {SegmentID: "source", Ordinal: 1},
			"active": {SegmentID: "active", Ordinal: 2},
		},
	}
	if got := attachmentGraphSegmentID(manifest); got != "active" {
		t.Fatalf("active graph segment = %q", got)
	}
	manifest.Session.ActiveSegmentID = ""
	manifest.Segments["latest"] = session.SegmentRecord{SegmentID: "latest", Ordinal: 3}
	if got := attachmentGraphSegmentID(manifest); got != "latest" {
		t.Fatalf("closed graph segment = %q", got)
	}
}

func TestAttachReconcilesBeforeAdvertisingWriterEpoch(t *testing.T) {
	const sessionID = "11111111-1111-4111-8111-111111111111"
	lease := &attachmentLease{epoch: 22}
	store := &attachmentStore{lease: lease, manifest: pausedAttachmentManifest(sessionID, 4)}
	var output bytes.Buffer
	attachment, err := AttachReconciled(
		context.Background(), store, sessionID, 4, &output,
		func(attachment *Attachment) error {
			if output.Len() != 0 {
				return errors.New("attachment advertised a writer epoch before reconciliation")
			}
			return attachment.advanceWriterEpoch()
		},
	)
	if err != nil {
		t.Fatalf("AttachReconciled: %v", err)
	}
	defer attachment.Release()
	var frame session.StdioFrame
	if err := json.NewDecoder(&output).Decode(&frame); err != nil {
		t.Fatalf("decode attachment handshake: %v", err)
	}
	if frame.WriterEpoch != 23 || attachment.WriterEpoch() != 23 {
		t.Fatalf("advertised/final writer epoch = %d/%d, want 23/23", frame.WriterEpoch, attachment.WriterEpoch())
	}
}

func TestAttachAtHeadBoundsAcceptedCommandProjection(t *testing.T) {
	const (
		sessionID    = "11111111-1111-4111-8111-111111111111"
		receiptCount = 5_000
	)
	manifest := pausedAttachmentManifest(sessionID, receiptCount)
	for index := 0; index < receiptCount; index++ {
		commandID := fmt.Sprintf("command-%05d", index)
		manifest.AcceptedCommands[commandID] = session.CommandReceipt{
			CommandID: commandID, CommandDigest: session.DigestJSON(index),
			ClientCommandDigest: session.DigestJSON(struct{ Index int }{index}),
			EventKind:           session.EventSessionDetached, Sequence: int64(index + 1),
		}
	}
	store := &attachmentStore{lease: &attachmentLease{epoch: 24}, manifest: manifest}
	var output bytes.Buffer
	attachment, err := Attach(context.Background(), store, sessionID, receiptCount, &output)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer attachment.Release()
	var frame session.StdioFrame
	if err := json.NewDecoder(&output).Decode(&frame); err != nil {
		t.Fatalf("decode attachment handshake: %v", err)
	}
	var projected session.Manifest
	if err := json.Unmarshal(frame.Payload, &projected); err != nil {
		t.Fatalf("decode manifest projection: %v", err)
	}
	if len(projected.AcceptedCommands) > 256 {
		t.Fatalf("wire receipt count = %d, want at most 256", len(projected.AcceptedCommands))
	}
	if len(store.manifest.AcceptedCommands) != receiptCount {
		t.Fatalf("authoritative receipt count changed to %d", len(store.manifest.AcceptedCommands))
	}
}

func TestAttachmentAcceptedResumeRetryReentersRuntimeHandler(t *testing.T) {
	const sessionID = "11111111-1111-4111-8111-111111111111"
	const commandID = "22222222-2222-4222-8222-222222222222"
	command := session.StdioCommand{
		Version: session.StdioProtocolV1, Type: session.CommandSessionResume,
		CommandID: commandID, SessionID: sessionID, WriterEpoch: 21, ExpectedSequence: 3,
	}
	digest, err := session.StdioCommandDigest(command)
	if err != nil {
		t.Fatalf("StdioCommandDigest: %v", err)
	}
	manifest := pausedAttachmentManifest(sessionID, 4)
	manifest.AcceptedCommands[commandID] = session.CommandReceipt{
		CommandID: commandID, CommandDigest: session.ResumeDigest("run"), ClientCommandDigest: digest,
		EventKind: session.EventSessionResumed, Sequence: 3,
	}
	lease := &attachmentLease{epoch: 21}
	store := &attachmentStore{lease: lease, manifest: manifest}
	attachment, err := Attach(context.Background(), store, sessionID, 4, io.Discard)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer attachment.Release()
	handler := &recordingCommandHandler{}
	attachment.WithCommandHandler(handler)
	if detached, err := attachment.HandleCommand(context.Background(), command); err != nil || detached {
		t.Fatalf("resume retry detached/error = %v/%v", detached, err)
	}
	if handler.calls != 1 {
		t.Fatalf("runtime handler calls = %d, want 1", handler.calls)
	}
}

func TestAttachmentAcceptedResumeRetryAfterInternalCompletionDoesNotReenterRuntime(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	root := t.TempDir()
	sessions := sessionstore.NewDirStore(filepath.Join(root, "sessions"))
	runs := runstore.NewDirRunStore(filepath.Join(root, "runs"))
	t.Cleanup(func() {
		_ = runs.Close()
		_ = sessions.Close()
	})
	sessionID, runID := uuid.NewString(), uuid.NewString()
	broker := serve.NewPromptBroker(16)
	broker.Register(runID)
	coordinator := newRuntimeCoordinator(t, sessions, runs, broker)
	plan := engine.ValidatedForTest(&engine.ExecutionPlan{
		RunID: runID, RunbookPath: "completed-resume-retry.runbook.yaml",
		Steps:    []engine.ResolvedStep{{ID: "done", Kind: "noop", Spec: &schema.NoopSpec{}}},
		Metadata: engine.PlanMetadata{RunbookID: "completed-resume-retry", RunbookName: "Completed resume retry"},
	})
	handle, err := coordinator.Start(ctx, sessioncoordinator.StartRequest{
		SessionID: sessionID, CommandID: uuid.NewString(), SegmentID: uuid.NewString(), RunID: runID,
		Plan: plan, Graph: runtimeGraph(t, plan), RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	paused, err := handle.Detach(ctx, uuid.NewString(), "pause before resume")
	if err != nil {
		t.Fatalf("Detach: %v", err)
	}
	broker.Unregister(runID)

	var output bytes.Buffer
	attachment, err := Attach(ctx, sessions, sessionID, 0, &output)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	runtime, err := NewCoordinatorRuntime(coordinator, broker, engine.RunOptions{Mode: engine.RunModeReal})
	if err != nil {
		t.Fatalf("NewCoordinatorRuntime: %v", err)
	}
	attachment.WithCommandHandler(runtime)
	resumeCommand := session.StdioCommand{
		Version: session.StdioProtocolV1, Type: session.CommandSessionResume,
		CommandID: uuid.NewString(), SessionID: sessionID, WriterEpoch: attachment.WriterEpoch(),
		ExpectedSequence: paused.Session.Sequence,
	}
	if detached, err := attachment.HandleCommand(ctx, resumeCommand); err != nil || detached {
		t.Fatalf("resume detached/error = %v/%v", detached, err)
	}
	select {
	case driveErr := <-runtime.Done():
		if driveErr != nil {
			t.Fatalf("runtime drive: %v", driveErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("resumed run did not complete")
	}
	completed, err := sessions.LoadManifest(ctx, sessionID)
	if err != nil {
		t.Fatalf("LoadManifest completed: %v", err)
	}
	receipt, found := completed.AcceptedCommands[resumeCommand.CommandID]
	if !found || receipt.EventKind != session.EventSessionResumed ||
		completed.Session.ActiveRunID != "" || completed.Attempts[runID].Status != session.AttemptStatusCompleted ||
		completed.Session.Sequence <= receipt.Sequence {
		t.Fatalf("completed manifest/receipt = %#v/%#v", completed, receipt)
	}
	for _, later := range completed.AcceptedCommands {
		if later.Sequence > receipt.Sequence && later.ClientCommandDigest != "" {
			t.Fatalf("completion unexpectedly has later client receipt %#v", later)
		}
	}
	eventsBeforeRetry, err := sessions.ReadEvents(ctx, sessionID, 0)
	if err != nil {
		t.Fatalf("ReadEvents before retry: %v", err)
	}
	if err := attachment.Release(); err != nil {
		t.Fatalf("Release completed attachment: %v", err)
	}

	reconnected, err := Attach(ctx, sessions, sessionID, completed.Session.Sequence, io.Discard)
	if err != nil {
		t.Fatalf("Attach reconnect: %v", err)
	}
	defer reconnected.Release()
	handler := &recordingCommandHandler{err: errors.New("completed resume retried runtime")}
	reconnected.WithCommandHandler(handler)
	resumeCommand.WriterEpoch = reconnected.WriterEpoch()
	if detached, err := reconnected.HandleCommand(ctx, resumeCommand); err != nil || detached {
		t.Fatalf("completed resume retry detached/error = %v/%v", detached, err)
	}
	if handler.calls != 0 {
		t.Fatalf("runtime handler calls = %d, want 0", handler.calls)
	}
	eventsAfterRetry, err := sessions.ReadEvents(ctx, sessionID, 0)
	if err != nil || len(eventsAfterRetry) != len(eventsBeforeRetry) {
		t.Fatalf("resume retry events = %d/%d, %v", len(eventsAfterRetry), len(eventsBeforeRetry), err)
	}
}

func TestAttachmentAcceptedResumeRetryAfterLaterDetachDoesNotReenterRuntime(t *testing.T) {
	const sessionID = "11111111-1111-4111-8111-111111111111"
	const resumeCommandID = "22222222-2222-4222-8222-222222222222"
	const detachCommandID = "33333333-3333-4333-8333-333333333333"
	command := session.StdioCommand{
		Version: session.StdioProtocolV1, Type: session.CommandSessionResume,
		CommandID: resumeCommandID, SessionID: sessionID, WriterEpoch: 23, ExpectedSequence: 3,
	}
	resumeDigest, err := session.StdioCommandDigest(command)
	if err != nil {
		t.Fatalf("StdioCommandDigest: %v", err)
	}
	manifest := pausedAttachmentManifest(sessionID, 5)
	manifest.AcceptedCommands[resumeCommandID] = session.CommandReceipt{
		CommandID: resumeCommandID, CommandDigest: session.ResumeDigest("run"),
		ClientCommandDigest: resumeDigest, EventKind: session.EventSessionResumed, Sequence: 3,
	}
	manifest.AcceptedCommands[detachCommandID] = session.CommandReceipt{
		CommandID: detachCommandID, CommandDigest: session.PauseDigest("run", "session.detach"),
		ClientCommandDigest: session.DigestJSON("later detach"),
		EventKind:           session.EventSessionPaused, Sequence: 5,
	}
	lease := &attachmentLease{epoch: 23}
	store := &attachmentStore{lease: lease, manifest: manifest}
	attachment, err := Attach(context.Background(), store, sessionID, 5, io.Discard)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer attachment.Release()
	handler := &recordingCommandHandler{}
	attachment.WithCommandHandler(handler)
	if detached, err := attachment.HandleCommand(context.Background(), command); err != nil || detached {
		t.Fatalf("superseded resume detached/error = %v/%v", detached, err)
	}
	if handler.calls != 0 {
		t.Fatalf("runtime handler calls = %d, want 0", handler.calls)
	}
}

func TestAttachmentAcceptedDetachRetryAfterLaterResumeKeepsAttachmentActive(t *testing.T) {
	const sessionID = "11111111-1111-4111-8111-111111111111"
	const detachCommandID = "22222222-2222-4222-8222-222222222222"
	const resumeCommandID = "33333333-3333-4333-8333-333333333333"
	command := session.StdioCommand{
		Version: session.StdioProtocolV1, Type: session.CommandSessionDetach,
		CommandID: detachCommandID, SessionID: sessionID, WriterEpoch: 26, ExpectedSequence: 3,
	}
	detachDigest, err := session.StdioCommandDigest(command)
	if err != nil {
		t.Fatalf("StdioCommandDigest: %v", err)
	}
	manifest := pausedAttachmentManifest(sessionID, 5)
	manifest.Session.Status = session.StatusActive
	manifest.AcceptedCommands[detachCommandID] = session.CommandReceipt{
		CommandID: detachCommandID, CommandDigest: session.PauseDigest("run", "session.detach"),
		ClientCommandDigest: detachDigest, EventKind: session.EventSessionPaused, Sequence: 3,
	}
	manifest.AcceptedCommands[resumeCommandID] = session.CommandReceipt{
		CommandID: resumeCommandID, CommandDigest: session.ResumeDigest("run"),
		ClientCommandDigest: session.DigestJSON("later resume"),
		EventKind:           session.EventSessionResumed, Sequence: 5,
	}
	lease := &attachmentLease{epoch: 26}
	store := &attachmentStore{lease: lease, manifest: manifest}
	attachment, err := Attach(context.Background(), store, sessionID, 5, io.Discard)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer attachment.Release()
	if detached, err := attachment.HandleCommand(context.Background(), command); err != nil || detached {
		t.Fatalf("superseded detach detached/error = %v/%v", detached, err)
	}
	if lease.releases != 0 {
		t.Fatalf("superseded detach released active attachment %d times", lease.releases)
	}
}

func TestAttachReleasesLeaseWhenFrameWriteFails(t *testing.T) {
	const sessionID = "11111111-1111-4111-8111-111111111111"
	lease := &attachmentLease{epoch: 9}
	store := &attachmentStore{
		frameStore: frameStore{events: []session.Event{{
			EventID: "event", SessionID: sessionID, Sequence: 1, WriterEpoch: 9,
			Kind: session.EventSessionClosed, Payload: json.RawMessage(`{"status":"resolved"}`),
		}}},
		lease: lease, manifest: pausedAttachmentManifest(sessionID, 1),
	}
	if _, err := Attach(context.Background(), store, sessionID, 0, failingFrameWriter{}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("Attach error = %v", err)
	}
	if lease.releases != 1 {
		t.Fatalf("lease releases = %d, want 1", lease.releases)
	}
}

func TestAdoptRetainsLeaseWhenInitialFrameWriteFails(t *testing.T) {
	const sessionID = "11111111-1111-4111-8111-111111111111"
	lease := &attachmentLease{epoch: 10}
	store := &attachmentStore{
		frameStore: frameStore{events: []session.Event{{
			EventID: "event", SessionID: sessionID, Sequence: 1, WriterEpoch: 10,
			Kind: session.EventSessionClosed, Payload: json.RawMessage(`{"status":"resolved"}`),
		}}},
		lease: lease, manifest: pausedAttachmentManifest(sessionID, 1),
	}
	attachment, err := Adopt(context.Background(), store, sessionID, 0, failingFrameWriter{}, lease)
	if !errors.Is(err, io.ErrClosedPipe) || attachment == nil {
		t.Fatalf("Adopt attachment/error = %#v/%v", attachment, err)
	}
	if lease.releases != 0 {
		t.Fatalf("Adopt released lease before durable detach: %d", lease.releases)
	}
	if err := attachment.Release(); err != nil || lease.releases != 1 {
		t.Fatalf("Release = %v, count %d", err, lease.releases)
	}
}

func TestAttachmentReplayDoesNotRepeatAnAlreadyAdvancedSequence(t *testing.T) {
	const sessionID = "11111111-1111-4111-8111-111111111111"
	lease := &attachmentLease{epoch: 10}
	store := &attachmentStore{
		frameStore: frameStore{events: []session.Event{{
			EventID: "event", SessionID: sessionID, Sequence: 1, WriterEpoch: 10,
			Kind: session.EventSessionClosed, Payload: json.RawMessage(`{"status":"resolved"}`),
		}}},
		lease: lease, manifest: pausedAttachmentManifest(sessionID, 1),
	}
	var output bytes.Buffer
	attachment, err := Attach(context.Background(), store, sessionID, 0, &output)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer attachment.Release()
	written := output.Len()
	if err := attachment.Replay(context.Background(), 0); err != nil {
		t.Fatalf("Replay stale sequence: %v", err)
	}
	if output.Len() != written {
		t.Fatalf("stale replay duplicated %d bytes", output.Len()-written)
	}
}

func TestAttachmentReplayCannotSkipPastInternalContiguousCursor(t *testing.T) {
	const sessionID = "11111111-1111-4111-8111-111111111111"
	lease := &attachmentLease{epoch: 24}
	store := &attachmentStore{frameStore: frameStore{
		events: []session.Event{
			{
				EventID: "22222222-2222-4222-8222-222222222222", SessionID: sessionID,
				Sequence: 1, WriterEpoch: 24, Kind: session.EventSessionClosed,
				Payload: json.RawMessage(`{"status":"resolved"}`),
			},
			{
				EventID: "33333333-3333-4333-8333-333333333333", SessionID: sessionID,
				Sequence: 2, WriterEpoch: 24, Kind: session.EventSessionClosed,
				Payload: json.RawMessage(`{"status":"resolved"}`),
			},
		},
	}}
	var output bytes.Buffer
	attachment := &Attachment{
		store: store, sessionID: sessionID, lease: lease,
		projector: NewProjector(store, 0), output: &output, lastSequence: 1,
	}
	if err := attachment.Replay(context.Background(), 2); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	var frame session.StdioFrame
	if err := json.NewDecoder(&output).Decode(&frame); err != nil {
		t.Fatalf("decode replay frame: %v", err)
	}
	if frame.SessionSequence != 2 || attachment.LastSequence() != 2 {
		t.Fatalf("replayed frame/cursor = %#v/%d", frame, attachment.LastSequence())
	}
}

func TestAttachmentProtocolErrorUsesLastContiguousSequence(t *testing.T) {
	const sessionID = "11111111-1111-4111-8111-111111111111"
	lease := &attachmentLease{epoch: 25}
	manifest := pausedAttachmentManifest(sessionID, 11)
	store := &attachmentStore{lease: lease, manifest: manifest}
	var output bytes.Buffer
	attachment := &Attachment{
		store: store, sessionID: sessionID, lease: lease,
		projector: NewProjector(store, 0), output: &output, lastSequence: 10,
	}
	if err := attachment.SendProtocolError(context.Background(), protocolError(
		ErrorInvalidCommand, "bad command", nil,
	)); err != nil {
		t.Fatalf("SendProtocolError: %v", err)
	}
	var frame session.StdioFrame
	if err := json.NewDecoder(&output).Decode(&frame); err != nil {
		t.Fatalf("decode protocol error: %v", err)
	}
	if frame.Type != session.FrameProtocolError || frame.SessionSequence != 10 ||
		attachment.LastSequence() != 10 {
		t.Fatalf("protocol error frame/cursor = %#v/%d", frame, attachment.LastSequence())
	}
}

func TestAttachmentPartialSequenceWriteDoesNotAdvanceReplayCursor(t *testing.T) {
	const sessionID = "11111111-1111-4111-8111-111111111111"
	graph := json.RawMessage(`{"nodes":[],"edges":[]}`)
	graphBlob := session.NewJSONBlob(graph)
	segment := session.SegmentRecord{
		SegmentID: "22222222-2222-4222-8222-222222222222",
		GraphHash: graphBlob.Digest, GraphRevision: 1,
	}
	attempt := session.RunAttemptRecord{
		RunID: "33333333-3333-4333-8333-333333333333", SegmentID: segment.SegmentID,
		Status: session.AttemptStatusStarting,
	}
	record := session.SessionRecord{SessionID: sessionID, Status: session.StatusActive, Sequence: 1}
	payload, _ := json.Marshal(struct {
		Session session.SessionRecord    `json:"session"`
		Segment session.SegmentRecord    `json:"segment"`
		Attempt session.RunAttemptRecord `json:"attempt"`
	}{record, segment, attempt})
	store := &attachmentStore{frameStore: frameStore{
		events: []session.Event{{
			EventID: "44444444-4444-4444-8444-444444444444", SessionID: sessionID,
			Sequence: 1, WriterEpoch: 15, Kind: session.EventSessionCreated, Payload: payload,
		}},
		blobs: map[string]json.RawMessage{graphBlob.Digest: graph},
		manifests: map[int64]session.Manifest{1: {
			SchemaVersion: session.ManifestSchemaV1, Session: record,
			Segments:         map[string]session.SegmentRecord{segment.SegmentID: segment},
			Attempts:         map[string]session.RunAttemptRecord{attempt.RunID: attempt},
			AcceptedCommands: make(map[string]session.CommandReceipt),
		}},
	}, manifest: session.Manifest{
		SchemaVersion: session.ManifestSchemaV1, Session: record,
		Segments:         map[string]session.SegmentRecord{segment.SegmentID: segment},
		Attempts:         map[string]session.RunAttemptRecord{attempt.RunID: attempt},
		AcceptedCommands: make(map[string]session.CommandReceipt),
	}}
	firstLease := &attachmentLease{epoch: 15}
	partial := &failAfterFrameWriter{successfulWrites: 2}
	first, err := Adopt(context.Background(), store, sessionID, 0, partial, firstLease)
	if !errors.Is(err, io.ErrClosedPipe) || first == nil {
		t.Fatalf("partial Adopt attachment/error = %#v/%v", first, err)
	}
	if first.LastSequence() != 0 || firstLease.releases != 0 {
		t.Fatalf("partial cursor/releases = %d/%d", first.LastSequence(), firstLease.releases)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("release partial attachment: %v", err)
	}

	secondLease := &attachmentLease{epoch: 16}
	var replay bytes.Buffer
	second, err := Adopt(context.Background(), store, sessionID, 0, &replay, secondLease)
	if err != nil {
		t.Fatalf("replay Adopt: %v", err)
	}
	defer second.Release()
	decoder := json.NewDecoder(&replay)
	var frames []session.StdioFrame
	for {
		var frame session.StdioFrame
		if err := decoder.Decode(&frame); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("decode replay frame: %v", err)
		}
		frames = append(frames, frame)
	}
	if len(frames) < 6 || second.LastSequence() != 1 {
		t.Fatalf("complete replay frames/cursor = %d/%d", len(frames), second.LastSequence())
	}
	replayed, handshake := frames[:len(frames)-1], frames[len(frames)-1]
	for index, frame := range replayed {
		if frame.SessionSequence != 1 || frame.SequenceIndex != index ||
			frame.SequenceCount != len(replayed) || frame.WriterEpoch != 16 {
			t.Fatalf("replayed frame %d = %#v", index, frame)
		}
	}
	if handshake.Type != session.FrameSessionSnapshot || handshake.SessionSequence != 1 ||
		handshake.SequenceIndex != 0 || handshake.SequenceCount != 1 || handshake.WriterEpoch != 16 {
		t.Fatalf("attachment head handshake = %#v", handshake)
	}
}

func TestAttachmentStreamAdvancesOnlyThroughLastCompleteSequence(t *testing.T) {
	const sessionID = "11111111-1111-4111-8111-111111111111"
	store := &attachmentStore{frameStore: frameStore{
		events: []session.Event{
			{
				EventID: "22222222-2222-4222-8222-222222222222", SessionID: sessionID,
				Sequence: 1, WriterEpoch: 30, Kind: session.EventSessionClosed,
				Payload: json.RawMessage(`{"status":"resolved"}`),
			},
			{
				EventID: "33333333-3333-4333-8333-333333333333", SessionID: sessionID,
				Sequence: 2, WriterEpoch: 30, Kind: session.EventSessionClosed,
				Payload: json.RawMessage(`{"status":"resolved"}`),
			},
		},
	}}
	lease := &attachmentLease{epoch: 30}
	writer := &failAfterFrameWriter{successfulWrites: 1}
	attachment, err := Adopt(context.Background(), store, sessionID, 0, writer, lease)
	if !errors.Is(err, io.ErrClosedPipe) || attachment == nil {
		t.Fatalf("Adopt attachment/error = %#v/%v", attachment, err)
	}
	defer attachment.Release()
	if attachment.LastSequence() != 1 {
		t.Fatalf("stream cursor = %d, want 1", attachment.LastSequence())
	}
	decoder := json.NewDecoder(&writer.output)
	var first session.StdioFrame
	if err := decoder.Decode(&first); err != nil || first.SessionSequence != 1 {
		t.Fatalf("first complete frame = %#v, %v", first, err)
	}
}

func TestAttachmentRejectsReceiptWithoutClientCommandDigest(t *testing.T) {
	const sessionID = "11111111-1111-4111-8111-111111111111"
	const commandID = "22222222-2222-4222-8222-222222222222"
	lease := &attachmentLease{epoch: 17}
	manifest := pausedAttachmentManifest(sessionID, 4)
	manifest.AcceptedCommands[commandID] = session.CommandReceipt{
		CommandID: commandID, CommandDigest: session.DigestJSON("internal-command"),
		EventKind: session.EventSessionCreated, Sequence: 1,
	}
	store := &attachmentStore{lease: lease, manifest: manifest}
	attachment, err := Attach(context.Background(), store, sessionID, 4, io.Discard)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer attachment.Release()
	detached, err := attachment.HandleCommand(context.Background(), session.StdioCommand{
		Version: session.StdioProtocolV1, Type: session.CommandSessionDetach,
		CommandID: commandID, SessionID: sessionID, WriterEpoch: 17, ExpectedSequence: 4,
	})
	var protocolErr *ProtocolError
	if detached || !errors.As(err, &protocolErr) || protocolErr.Code != ErrorInvalidCommand ||
		!errors.Is(err, session.ErrCommandConflict) || store.pauseCalls != 0 {
		t.Fatalf("digestless receipt detached/error/calls = %v/%#v/%d", detached, err, store.pauseCalls)
	}
}

func pausedAttachmentManifest(sessionID string, sequence int64) session.Manifest {
	return session.Manifest{
		SchemaVersion: session.ManifestSchemaV1,
		Session: session.SessionRecord{
			SessionID: sessionID, Status: session.StatusPaused,
			ActiveSegmentID: "segment", ActiveRunID: "run", Sequence: sequence,
		},
		Segments: map[string]session.SegmentRecord{
			"segment": {SegmentID: "segment", Status: session.SegmentStatusPaused},
		},
		Attempts: map[string]session.RunAttemptRecord{
			"run": {RunID: "run", SegmentID: "segment", Status: session.AttemptStatusPausedAtBoundary},
		},
		AcceptedCommands: make(map[string]session.CommandReceipt),
	}
}

func TestAttachmentDetachPausesDurablyBeforeReleasingLease(t *testing.T) {
	const sessionID = "11111111-1111-4111-8111-111111111111"
	const commandID = "22222222-2222-4222-8222-222222222222"
	lease := &attachmentLease{epoch: 11}
	store := &attachmentStore{
		lease: lease, manifest: pausedAttachmentManifest(sessionID, 4),
	}
	attachment, err := Attach(context.Background(), store, sessionID, 4, io.Discard)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	manifest, err := attachment.Detach(context.Background(), commandID, "session.detach")
	if err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if manifest.Session.Status != session.StatusPaused || store.pauseCalls != 1 || lease.releases != 1 {
		t.Fatalf("detach status/calls/releases = %s/%d/%d", manifest.Session.Status, store.pauseCalls, lease.releases)
	}
	if _, err := attachment.Detach(context.Background(), commandID, "session.detach"); err != nil ||
		store.pauseCalls != 1 || lease.releases != 1 {
		t.Fatalf("idempotent detach error/calls/releases = %v/%d/%d", err, store.pauseCalls, lease.releases)
	}
}

func TestAttachmentServeHandlesIdempotentDetachCommand(t *testing.T) {
	const sessionID = "11111111-1111-4111-8111-111111111111"
	const commandID = "22222222-2222-4222-8222-222222222222"
	lease := &attachmentLease{epoch: 12}
	store := &attachmentStore{
		lease: lease, manifest: pausedAttachmentManifest(sessionID, 4),
	}
	attachment, err := Attach(context.Background(), store, sessionID, 4, io.Discard)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	command, _ := json.Marshal(session.StdioCommand{
		Version: session.StdioProtocolV1, Type: session.CommandSessionDetach,
		CommandID: commandID, SessionID: sessionID, WriterEpoch: 12, ExpectedSequence: 4,
	})
	result, err := attachment.Serve(context.Background(), bytes.NewReader(append(command, '\n')))
	if err != nil || !result.Detached || store.pauseCalls != 1 || lease.releases != 1 {
		t.Fatalf("Serve result/error/calls/releases = %#v/%v/%d/%d", result, err, store.pauseCalls, lease.releases)
	}
	if detached, err := attachment.HandleCommand(context.Background(), session.StdioCommand{
		Version: session.StdioProtocolV1, Type: session.CommandSessionDetach,
		CommandID: commandID, SessionID: sessionID, WriterEpoch: 12, ExpectedSequence: 4,
	}); err != nil || !detached || store.pauseCalls != 1 {
		t.Fatalf("retry detach/error/calls = %v/%v/%d", detached, err, store.pauseCalls)
	}
}

func TestAttachmentRejectsStaleWriterAndOversizedCommand(t *testing.T) {
	const sessionID = "11111111-1111-4111-8111-111111111111"
	lease := &attachmentLease{epoch: 13}
	store := &attachmentStore{
		lease: lease, manifest: pausedAttachmentManifest(sessionID, 4),
	}
	attachment, err := Attach(context.Background(), store, sessionID, 4, io.Discard)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer attachment.Release()
	_, err = attachment.HandleCommand(context.Background(), session.StdioCommand{
		Version: session.StdioProtocolV1, Type: session.CommandSessionDetach,
		CommandID: "22222222-2222-4222-8222-222222222222", SessionID: sessionID,
		WriterEpoch: 12, ExpectedSequence: 4,
	})
	var protocolErr *ProtocolError
	if !errors.As(err, &protocolErr) || protocolErr.Code != ErrorStaleWriter || store.pauseCalls != 0 {
		t.Fatalf("stale writer error/calls = %#v/%d", err, store.pauseCalls)
	}
	_, err = attachment.Serve(context.Background(), strings.NewReader(strings.Repeat("x", maxCommandBytes+1)+"\n"))
	if !errors.As(err, &protocolErr) || protocolErr.Code != ErrorInvalidCommand || store.pauseCalls != 0 {
		t.Fatalf("oversized command error/calls = %#v/%d", err, store.pauseCalls)
	}
}
