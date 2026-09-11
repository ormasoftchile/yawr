package sessionstdio

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/session"
)

type frameStore struct {
	events        []session.Event
	blobs         map[string]json.RawMessage
	manifests     map[int64]session.Manifest
	blobBytesRead int
}

func TestSerializedFrameLimitMatchesClientBoundary(t *testing.T) {
	valid := session.StdioFrame{
		Version: session.StdioProtocolV1, Type: session.FrameRunEvent,
		FrameID: "frame", SessionID: "session", SessionSequence: 1,
		SequenceCount: 1, WriterEpoch: 1,
		Payload: json.RawMessage(`{"value":"ok"}`),
	}
	if err := validateStdioFrameSize(valid); err != nil {
		t.Fatalf("valid frame: %v", err)
	}
	var output bytes.Buffer
	if err := WriteFrame(&output, valid); err != nil || output.Len() == 0 || output.Bytes()[output.Len()-1] != '\n' {
		t.Fatalf("WriteFrame valid output = %d, %v", output.Len(), err)
	}
	valid.Payload = json.RawMessage(`{"value":"` + string(bytes.Repeat([]byte{'x'}, session.MaxStdioFrameBytes)) + `"}`)
	if err := validateStdioFrameSize(valid); err == nil {
		t.Fatal("oversized serialized frame was accepted")
	}
	output.Reset()
	if err := WriteFrame(&output, valid); err == nil || output.Len() != 0 {
		t.Fatalf("WriteFrame oversized output = %d, %v", output.Len(), err)
	}
}

func (store *frameStore) ReadEvents(_ context.Context, _ string, afterSequence int64) ([]session.Event, error) {
	var events []session.Event
	for _, event := range store.events {
		if event.Sequence > afterSequence {
			events = append(events, event)
		}
	}
	return events, nil
}

func (store *frameStore) ReadFrameEvents(
	ctx context.Context,
	sessionID string,
	afterSequence int64,
) ([]session.Event, map[int64]session.Manifest, int64, error) {
	events, err := store.ReadEvents(ctx, sessionID, afterSequence)
	if err != nil {
		return nil, nil, 0, err
	}
	var headSequence int64
	for _, event := range store.events {
		if event.Sequence > headSequence {
			headSequence = event.Sequence
		}
	}
	for manifestSequence := range store.manifests {
		if manifestSequence > headSequence {
			headSequence = manifestSequence
		}
	}
	manifests := make(map[int64]session.Manifest)
	for sequence, manifest := range store.manifests {
		if sequence > afterSequence {
			manifests[sequence] = manifest
		}
	}
	return events, manifests, headSequence, nil
}

func (store *frameStore) ReadBlob(_ context.Context, _ string, digest string) (json.RawMessage, error) {
	data := store.blobs[digest]
	store.blobBytesRead += len(data)
	return append(json.RawMessage(nil), data...), nil
}

func TestProjectorColdExecutionReplayReadsProjectionBytesLinearly(t *testing.T) {
	const checkpoints = 24
	bytesForN := coldExecutionReplayProjectionBytes(t, checkpoints)
	bytesForTwoN := coldExecutionReplayProjectionBytes(t, checkpoints*2)
	ratio := float64(bytesForTwoN) / float64(bytesForN)
	t.Logf("projection bytes: n=%d 2n=%d ratio=%.2f", bytesForN, bytesForTwoN, ratio)
	if bytesForTwoN > bytesForN*5/2 {
		t.Fatalf("projection bytes scaled superlinearly: n=%d 2n=%d ratio=%.2f",
			bytesForN, bytesForTwoN, ratio)
	}
}

func coldExecutionReplayProjectionBytes(t *testing.T, checkpoints int) int {
	t.Helper()
	const sessionID = "11111111-1111-4111-8111-111111111111"
	const segmentID = "22222222-2222-4222-8222-222222222222"
	const runID = "33333333-3333-4333-8333-333333333333"
	store := &frameStore{
		blobs: make(map[string]json.RawMessage), manifests: make(map[int64]session.Manifest, checkpoints),
	}
	for checkpoint := 1; checkpoint <= checkpoints; checkpoint++ {
		snapshot, err := json.Marshal(map[string]any{
			"RunID": runID, "Interactions": map[string]any{}, "pending_trace_events": []any{},
			"History": string(bytes.Repeat([]byte{'x'}, checkpoint*4_096)),
		})
		if err != nil {
			t.Fatalf("marshal cumulative snapshot %d: %v", checkpoint, err)
		}
		chunkData, err := json.Marshal(map[string]any{
			"schema_version": "yawr.session-run-projection-chunk/v1", "data": snapshot,
		})
		if err != nil {
			t.Fatalf("marshal cumulative chunk %d: %v", checkpoint, err)
		}
		chunk := session.NewJSONBlob(chunkData)
		store.blobs[chunk.Digest] = chunk.Data
		rootData, err := json.Marshal(map[string]any{
			"schema_version": session.RunProjectionSchemaV1, "run_id": runID,
			"checkpoint_sequence": checkpoint,
			"files": []map[string]any{{
				"path":   fmt.Sprintf("snapshots/checkpoint-%020d.json", checkpoint),
				"digest": session.DigestBytes(snapshot), "size": len(snapshot),
				"chunk_digests": []string{chunk.Digest},
			}},
		})
		if err != nil {
			t.Fatalf("marshal cumulative root %d: %v", checkpoint, err)
		}
		root := session.NewJSONBlob(rootData)
		store.blobs[root.Digest] = root.Data
		frameContentData, err := json.Marshal(map[string]any{
			"interactions": map[string]any{}, "pending_trace_events": []any{},
		})
		if err != nil {
			t.Fatalf("marshal frame projection content %d: %v", checkpoint, err)
		}
		frameChunkData, err := json.Marshal(session.ExecutionFrameProjectionChunk{
			SchemaVersion: session.ExecutionFrameProjectionChunkSchemaV1, Data: frameContentData,
		})
		if err != nil {
			t.Fatalf("marshal frame projection chunk %d: %v", checkpoint, err)
		}
		frameChunk := session.NewJSONBlob(frameChunkData)
		store.blobs[frameChunk.Digest] = frameChunk.Data
		frameData, err := json.Marshal(session.ExecutionFrameProjection{
			SchemaVersion: session.ExecutionFrameProjectionSchemaV1, RunID: runID,
			CheckpointSequence: int64(checkpoint), CommittedTraceSequence: 0,
			ContentDigest: session.DigestBytes(frameContentData), ContentSize: int64(len(frameContentData)),
			ChunkDigests: []string{frameChunk.Digest},
		})
		if err != nil {
			t.Fatalf("marshal frame projection root %d: %v", checkpoint, err)
		}
		frameProjection := session.NewJSONBlob(frameData)
		store.blobs[frameProjection.Digest] = frameProjection.Data
		payload, err := json.Marshal(map[string]any{
			"run_id": runID, "status": session.AttemptStatusRunning,
			"mutation_hash":   session.DigestBytes([]byte(fmt.Sprintf("mutation-%d", checkpoint))),
			"projection_hash": root.Digest, "frame_projection_hash": frameProjection.Digest,
			"checkpoint_sequence": checkpoint, "committed_trace_sequence": 0,
		})
		if err != nil {
			t.Fatalf("marshal event payload %d: %v", checkpoint, err)
		}
		sequence := int64(checkpoint)
		store.events = append(store.events, session.Event{
			EventID: fmt.Sprintf("event-%d", checkpoint), SessionID: sessionID, Sequence: sequence,
			WriterEpoch: 1, Kind: session.EventExecutionCommitted, Payload: payload,
		})
		store.manifests[sequence] = session.Manifest{
			SchemaVersion: session.ManifestSchemaV1,
			Session: session.SessionRecord{
				SessionID: sessionID, Sequence: sequence, Status: session.StatusActive,
				ActiveSegmentID: segmentID, ActiveRunID: runID,
			},
			Segments: map[string]session.SegmentRecord{
				segmentID: {SegmentID: segmentID, Status: session.SegmentStatusActive},
			},
			Attempts: map[string]session.RunAttemptRecord{
				runID: {RunID: runID, SegmentID: segmentID, Status: session.AttemptStatusRunning},
			},
			AcceptedCommands: make(map[string]session.CommandReceipt),
		}
	}
	frames, err := NewProjector(store, 0).Frames(context.Background(), sessionID, 0)
	if err != nil {
		t.Fatalf("cold Frames(%d): %v", checkpoints, err)
	}
	if len(frames) != checkpoints {
		t.Fatalf("cold frame count = %d, want %d", len(frames), checkpoints)
	}
	return store.blobBytesRead
}

func TestProjectorReplaysCompleteCreationSequenceWithVerifiedGraphChunks(t *testing.T) {
	const sessionID = "11111111-1111-4111-8111-111111111111"
	graph := json.RawMessage(`{"nodes":[{"id":"one"},{"id":"two"}],"edges":[]}`)
	graphBlob := session.NewJSONBlob(graph)
	segment := session.SegmentRecord{
		SegmentID: "22222222-2222-4222-8222-222222222222", Ordinal: 1,
		RunbookID: "entry", RunbookName: "Entry", PlanHash: "plan",
		GraphHash: graphBlob.Digest, ExecutableSnapshotHash: session.DigestBytes([]byte("plan")),
		ExecutableRevision: 1, GraphRevision: 1,
	}
	attempt := session.RunAttemptRecord{
		RunID: "33333333-3333-4333-8333-333333333333", SegmentID: segment.SegmentID,
		Ordinal: 1, Mode: "real", Status: session.AttemptStatusStarting,
	}
	record := session.SessionRecord{
		SessionID: sessionID, Status: session.StatusActive, RootSegmentID: segment.SegmentID,
		ActiveSegmentID: segment.SegmentID, ActiveRunID: attempt.RunID, Sequence: 1,
	}
	creationCommandID := "55555555-5555-4555-8555-555555555555"
	creationManifest := session.Manifest{
		SchemaVersion: session.ManifestSchemaV1, Session: record,
		Segments:    map[string]session.SegmentRecord{segment.SegmentID: segment},
		Attempts:    map[string]session.RunAttemptRecord{attempt.RunID: attempt},
		Transitions: make(map[string]session.TransitionRecord), Occurrences: make(map[string]session.OccurrenceRecord),
		AcceptedCommands: map[string]session.CommandReceipt{
			creationCommandID: {CommandID: creationCommandID, EventKind: session.EventSessionCreated, Sequence: 1},
		},
	}
	payload, err := json.Marshal(struct {
		Session session.SessionRecord    `json:"session"`
		Segment session.SegmentRecord    `json:"segment"`
		Attempt session.RunAttemptRecord `json:"attempt"`
	}{record, segment, attempt})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	store := &frameStore{
		events: []session.Event{{
			EventID: "44444444-4444-4444-8444-444444444444", SessionID: sessionID,
			Sequence: 1, WriterEpoch: 1, CommandID: creationCommandID,
			Kind: session.EventSessionCreated, Payload: payload,
		}},
		blobs: map[string]json.RawMessage{graphBlob.Digest: graph}, manifests: map[int64]session.Manifest{1: creationManifest},
	}
	projector := NewProjector(store, 11)
	frames, err := projector.Frames(context.Background(), sessionID, 0)
	if err != nil {
		t.Fatalf("Frames: %v", err)
	}
	graphChunks := make([]GraphChunkPayload, 0)
	seenIDs := make(map[string]bool)
	for index, frame := range frames {
		if frame.Version != session.StdioProtocolV1 || frame.SessionID != sessionID ||
			frame.SessionSequence != 1 || frame.SequenceIndex != index || frame.SequenceCount != len(frames) ||
			frame.FrameID == "" || seenIDs[frame.FrameID] {
			t.Fatalf("frame %d = %#v", index, frame)
		}
		seenIDs[frame.FrameID] = true
		if frame.Type == session.FrameSegmentGraph {
			var chunk GraphChunkPayload
			if err := json.Unmarshal(frame.Payload, &chunk); err != nil {
				t.Fatalf("decode graph chunk: %v", err)
			}
			graphChunks = append(graphChunks, chunk)
		}
	}
	wantFixed := []session.FrameType{
		session.FrameSessionStarted, session.FrameSessionSnapshot,
		session.FrameSegmentAdded,
	}
	for index, want := range wantFixed {
		if frames[index].Type != want {
			t.Fatalf("frame %d type = %s, want %s", index, frames[index].Type, want)
		}
	}
	if frames[len(frames)-1].Type != session.FrameAttemptStarted || len(graphChunks) < 2 {
		t.Fatalf("creation frame types/chunks = %#v/%d", frames, len(graphChunks))
	}
	var snapshot session.Manifest
	if err := json.Unmarshal(frames[1].Payload, &snapshot); err != nil ||
		snapshot.Session.Sequence != 1 || snapshot.AcceptedCommands[creationCommandID].Sequence != 1 {
		t.Fatalf("creation snapshot = %#v, %v", snapshot, err)
	}
	var rebuilt []byte
	for index, chunk := range graphChunks {
		if chunk.ChunkIndex != index || chunk.ChunkCount != len(graphChunks) ||
			chunk.WholeBlobHash != graphBlob.Digest || chunk.GraphRevision != 1 {
			t.Fatalf("graph chunk %d = %#v", index, chunk)
		}
		rebuilt = append(rebuilt, chunk.Data...)
	}
	if !bytes.Equal(rebuilt, graph) || session.DigestBytes(rebuilt) != graphBlob.Digest {
		t.Fatalf("rebuilt graph digest/content = %s/%s", session.DigestBytes(rebuilt), rebuilt)
	}
	replayed, err := projector.Frames(context.Background(), sessionID, 1)
	if err != nil || len(replayed) != 0 {
		t.Fatalf("Frames after acknowledged sequence = %#v, %v", replayed, err)
	}
	metadataOnly, err := NewProjector(store, 11).
		WithGraphSegmentFilter(func(string, int64) bool { return false }).
		Frames(context.Background(), sessionID, 0)
	if err != nil {
		t.Fatalf("metadata-only Frames: %v", err)
	}
	for _, frame := range metadataOnly {
		if frame.Type == session.FrameSegmentGraph {
			t.Fatal("metadata-only replay emitted a historical graph blob")
		}
	}
	if len(metadataOnly) != 5 || metadataOnly[0].Type != session.FrameSessionStarted ||
		metadataOnly[3].Type != session.FrameSegmentGraphAvailable ||
		metadataOnly[len(metadataOnly)-1].Type != session.FrameAttemptStarted {
		t.Fatalf("metadata-only creation frames = %#v", metadataOnly)
	}
}

func TestProjectorReplaysCommittedHandoffAsOneCompleteSequence(t *testing.T) {
	const sessionID = "11111111-1111-4111-8111-111111111111"
	graph := json.RawMessage(`{"nodes":[{"id":"target"}],"edges":[]}`)
	graphBlob := session.NewJSONBlob(graph)
	segment := session.SegmentRecord{
		SegmentID: "22222222-2222-4222-8222-222222222222", Ordinal: 2,
		RunbookID: "target", RunbookName: "Target", PlanHash: "target-plan",
		GraphHash: graphBlob.Digest, ExecutableSnapshotHash: session.DigestBytes([]byte("target-plan")),
		ExecutableRevision: 1, GraphRevision: 1,
	}
	attempt := session.RunAttemptRecord{
		RunID: "33333333-3333-4333-8333-333333333333", SegmentID: segment.SegmentID,
		Ordinal: 1, Mode: "real", Status: session.AttemptStatusStarting,
	}
	payload, _ := json.Marshal(struct {
		TransitionID  string                   `json:"transition_id"`
		TargetSegment session.SegmentRecord    `json:"target_segment"`
		TargetAttempt session.RunAttemptRecord `json:"target_attempt"`
	}{"44444444-4444-4444-8444-444444444444", segment, attempt})
	store := &frameStore{
		events: []session.Event{{
			EventID: "55555555-5555-4555-8555-555555555555", SessionID: sessionID,
			Sequence: 2, WriterEpoch: 4, Kind: session.EventTransitionCommitted, Payload: payload,
		}},
		blobs: map[string]json.RawMessage{graphBlob.Digest: graph},
		manifests: map[int64]session.Manifest{1: {
			SchemaVersion: session.ManifestSchemaV1,
			Session:       session.SessionRecord{SessionID: sessionID, Sequence: 1, Status: session.StatusActive},
		}},
	}
	frames, err := NewProjector(store, 9).Frames(context.Background(), sessionID, 1)
	if err != nil {
		t.Fatalf("Frames: %v", err)
	}
	if len(frames) < 5 || frames[0].Type != session.FrameTransitionCommitted ||
		frames[1].Type != session.FrameSegmentAdded || frames[len(frames)-1].Type != session.FrameAttemptStarted {
		t.Fatalf("handoff frame group = %#v", frames)
	}
	for index, frame := range frames {
		if frame.SessionSequence != 2 || frame.SequenceIndex != index || frame.SequenceCount != len(frames) ||
			frame.SegmentID != "" && frame.SegmentID != segment.SegmentID ||
			frame.RunID != "" && frame.RunID != attempt.RunID {
			t.Fatalf("handoff frame %d = %#v", index, frame)
		}
	}
}

func TestProjectorRevisedSegmentEmitsSnapshotBeforeGraph(t *testing.T) {
	const sessionID = "11111111-1111-4111-8111-111111111111"
	const segmentID = "22222222-2222-4222-8222-222222222222"
	const runID = "33333333-3333-4333-8333-333333333333"
	graph := json.RawMessage(`{"schema_version":"1","nodes":[],"edges":[],"frames":[],"groups":[]}`)
	graphBlob := session.NewJSONBlob(graph)
	payload, err := json.Marshal(revisionPayload{
		SegmentID: segmentID, RunID: runID, GraphRevision: 2,
		PlanHash: strings.Repeat("a", 64), GraphHash: graphBlob.Digest,
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	store := &frameStore{
		events: []session.Event{{
			EventID: "event-revision", SessionID: sessionID, Sequence: 2, WriterEpoch: 1,
			Kind: session.EventSegmentRevised, Payload: payload,
		}},
		blobs: map[string]json.RawMessage{graphBlob.Digest: graph},
		manifests: map[int64]session.Manifest{2: {
			SchemaVersion: session.ManifestSchemaV1,
			Session: session.SessionRecord{
				SessionID: sessionID, Sequence: 2, Status: session.StatusActive,
				ActiveSegmentID: segmentID, ActiveRunID: runID,
			},
			Segments: map[string]session.SegmentRecord{segmentID: {
				SegmentID: segmentID, GraphRevision: 2,
				PlanHash: strings.Repeat("a", 64), GraphHash: graphBlob.Digest,
			}},
			Attempts: map[string]session.RunAttemptRecord{runID: {
				RunID: runID, SegmentID: segmentID, Status: session.AttemptStatusRunning,
			}},
			AcceptedCommands: map[string]session.CommandReceipt{},
		}},
	}
	frames, err := NewProjector(store, 16).Frames(context.Background(), sessionID, 1)
	if err != nil {
		t.Fatalf("Frames: %v", err)
	}
	if len(frames) < 2 || frames[0].Type != session.FrameSessionSnapshot {
		t.Fatalf("revised segment frames = %#v, want snapshot before graph chunks", frames)
	}
	for index, frame := range frames[1:] {
		if frame.Type != session.FrameSegmentGraph || frame.SequenceIndex != index+1 {
			t.Fatalf("revision frame %d = %#v", index+1, frame)
		}
	}
}

func TestProjectorPausedCloseEmitsTerminalTopologyBeforeSession(t *testing.T) {
	const sessionID = "11111111-1111-4111-8111-111111111111"
	const segmentID = "22222222-2222-4222-8222-222222222222"
	const runID = "33333333-3333-4333-8333-333333333333"
	payload, err := json.Marshal(closedPayload{
		Status: session.StatusEscalated, SegmentID: segmentID, RunID: runID,
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	store := &frameStore{
		events: []session.Event{{
			EventID: "event-close", SessionID: sessionID, Sequence: 2, WriterEpoch: 1,
			Kind: session.EventSessionClosed, Payload: payload,
		}},
		blobs: map[string]json.RawMessage{},
		manifests: map[int64]session.Manifest{2: {
			SchemaVersion: session.ManifestSchemaV1,
			Session:       session.SessionRecord{SessionID: sessionID, Sequence: 2, Status: session.StatusEscalated},
			Segments: map[string]session.SegmentRecord{segmentID: {
				SegmentID: segmentID, Status: session.SegmentStatusCancelled,
			}},
			Attempts: map[string]session.RunAttemptRecord{runID: {
				RunID: runID, SegmentID: segmentID, Status: session.AttemptStatusCancelled,
			}},
		}},
	}
	frames, err := NewProjector(store, 0).Frames(context.Background(), sessionID, 1)
	if err != nil {
		t.Fatalf("Frames: %v", err)
	}
	want := []session.FrameType{
		session.FrameAttemptFinished, session.FrameSegmentFinished, session.FrameSessionFinished,
	}
	if len(frames) != len(want) {
		t.Fatalf("close frame count = %d, want %d", len(frames), len(want))
	}
	for index, frame := range frames {
		if frame.Type != want[index] || frame.SegmentID != segmentID || frame.RunID != runID {
			t.Fatalf("close frame %d = %#v, want %s", index, frame, want[index])
		}
	}
}

func TestProjectorReconstructsPendingAndResolvedInteractionsFromExecutionMutations(t *testing.T) {
	const sessionID = "11111111-1111-4111-8111-111111111111"
	const segmentID = "33333333-3333-4333-8333-333333333333"
	const runID = "22222222-2222-4222-8222-222222222222"
	pending := map[string]*sessionInteraction{
		"turn": {TurnID: "turn", StepID: "choose", Kind: "choice", Status: "pending"},
	}
	answered := map[string]*sessionInteraction{
		"turn": {TurnID: "turn", StepID: "choose", Kind: "choice", Status: "answered", Answer: json.RawMessage(`{"selected":["one"]}`)},
	}
	store := &frameStore{blobs: make(map[string]json.RawMessage), manifests: map[int64]session.Manifest{
		1: {
			SchemaVersion:    session.ManifestSchemaV1,
			Session:          session.SessionRecord{SessionID: sessionID, Sequence: 1, Status: session.StatusActive, ActiveSegmentID: segmentID, ActiveRunID: runID},
			Segments:         map[string]session.SegmentRecord{segmentID: {SegmentID: segmentID, Status: session.SegmentStatusActive}},
			Attempts:         map[string]session.RunAttemptRecord{runID: {RunID: runID, SegmentID: segmentID, Status: session.AttemptStatusWaiting}},
			AcceptedCommands: make(map[string]session.CommandReceipt),
		},
		2: {
			SchemaVersion:    session.ManifestSchemaV1,
			Session:          session.SessionRecord{SessionID: sessionID, Sequence: 2, Status: session.StatusActive, ActiveSegmentID: segmentID, ActiveRunID: runID},
			Segments:         map[string]session.SegmentRecord{segmentID: {SegmentID: segmentID, Status: session.SegmentStatusActive}},
			Attempts:         map[string]session.RunAttemptRecord{runID: {RunID: runID, SegmentID: segmentID, Status: session.AttemptStatusRunning}},
			AcceptedCommands: make(map[string]session.CommandReceipt),
		},
	}}
	pendingProjection := addInteractionProjection(t, store, runID, 1, pending)
	answeredProjection := addInteractionProjection(t, store, runID, 2, answered)
	store.events = []session.Event{
		{EventID: "event-1", SessionID: sessionID, Sequence: 1, WriterEpoch: 1,
			Kind: session.EventExecutionCommitted, Payload: executionProjectionPayload(t, runID, pendingProjection, session.AttemptStatusWaiting, 1)},
		{EventID: "event-2", SessionID: sessionID, Sequence: 2, WriterEpoch: 1,
			Kind: session.EventExecutionCommitted, Payload: executionProjectionPayload(t, runID, answeredProjection, session.AttemptStatusRunning, 2)},
	}
	projector := NewProjector(store, 0)
	pendingFrames, err := projector.Frames(context.Background(), sessionID, 0)
	if err != nil {
		t.Fatalf("pending Frames: %v", err)
	}
	if !hasFrameTypeAtSequence(pendingFrames, session.FrameInteractionPending, 1) ||
		!hasFrameTypeAtSequence(pendingFrames, session.FrameInteractionResolved, 2) {
		t.Fatalf("interaction frames = %#v", pendingFrames)
	}
	for _, sequence := range []int64{1, 2} {
		frame := frameAtSequenceAndType(t, pendingFrames, sequence, session.FrameSessionSnapshot)
		var snapshot session.Manifest
		if err := json.Unmarshal(frame.Payload, &snapshot); err != nil || snapshot.Session.Sequence != sequence {
			t.Fatalf("execution snapshot %d = %#v, %v", sequence, snapshot, err)
		}
	}
	resolvedOnly, err := projector.Frames(context.Background(), sessionID, 1)
	if err != nil {
		t.Fatalf("resolved Frames: %v", err)
	}
	if hasFrameTypeAtSequence(resolvedOnly, session.FrameInteractionPending, 1) ||
		!hasFrameTypeAtSequence(resolvedOnly, session.FrameInteractionResolved, 2) {
		t.Fatalf("resolved suffix = %#v", resolvedOnly)
	}
}

func TestProjectorProjectsTransitionAbortAsManifestSnapshot(t *testing.T) {
	const sessionID = "11111111-1111-4111-8111-111111111111"
	manifest := session.Manifest{
		SchemaVersion: session.ManifestSchemaV1,
		Session:       session.SessionRecord{SessionID: sessionID, Sequence: 1, Status: session.StatusPaused},
		Transitions: map[string]session.TransitionRecord{
			"transition": {TransitionID: "transition", Status: session.TransitionStatusAborted},
		},
		AcceptedCommands: make(map[string]session.CommandReceipt),
	}
	store := &frameStore{
		events: []session.Event{{
			EventID: "event", SessionID: sessionID, Sequence: 1, WriterEpoch: 1,
			Kind: session.EventTransitionAborted, Payload: json.RawMessage(`{"transition_id":"transition","reason":"invalid"}`),
		}},
		manifests: map[int64]session.Manifest{1: manifest},
	}
	frames, err := NewProjector(store, 0).Frames(context.Background(), sessionID, 0)
	if err != nil {
		t.Fatalf("Frames: %v", err)
	}
	if len(frames) != 1 || frames[0].Type != session.FrameSessionSnapshot {
		t.Fatalf("transition abort frames = %#v", frames)
	}
	var snapshot session.Manifest
	if err := json.Unmarshal(frames[0].Payload, &snapshot); err != nil ||
		snapshot.Transitions["transition"].Status != session.TransitionStatusAborted {
		t.Fatalf("transition abort snapshot = %#v, %v", snapshot, err)
	}
}

func TestProjectorManifestFrameBoundsAcceptedCommandProjection(t *testing.T) {
	const sessionID = "11111111-1111-4111-8111-111111111111"
	manifest := session.Manifest{
		SchemaVersion:    session.ManifestSchemaV1,
		Session:          session.SessionRecord{SessionID: sessionID, Sequence: 5_000},
		AcceptedCommands: make(map[string]session.CommandReceipt, 5_000),
	}
	for index := 1; index <= 5_000; index++ {
		commandID := fmt.Sprintf("command-%05d", index)
		manifest.AcceptedCommands[commandID] = session.CommandReceipt{
			CommandID: commandID, EventKind: session.EventSessionDetached, Sequence: int64(index),
		}
	}
	event := session.Event{SessionID: sessionID, Sequence: 5_000}
	frame, _, err := (&Projector{}).manifestFrame(event, manifest)
	if err != nil {
		t.Fatalf("manifestFrame: %v", err)
	}
	var projected session.Manifest
	if err := json.Unmarshal(frame.Payload, &projected); err != nil {
		t.Fatalf("decode projected manifest: %v", err)
	}
	if len(projected.AcceptedCommands) > session.ManifestProjectionReceipts {
		t.Fatalf("projected receipt count = %d, want at most %d",
			len(projected.AcceptedCommands), session.ManifestProjectionReceipts)
	}
}

func TestProjectorRejectsTraceBlobForDifferentRun(t *testing.T) {
	const sessionID = "11111111-1111-4111-8111-111111111111"
	const runID = "22222222-2222-4222-8222-222222222222"
	traceEvent := engine.Event{
		EventID: "33333333-3333-4333-8333-333333333333",
		RunID:   "44444444-4444-4444-8444-444444444444", RunbookID: "other",
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano), Kind: "step/started", Sequence: 1,
		Payload: map[string]any{"step_id": "work"},
	}
	traceData, err := json.Marshal(traceEvent)
	if err != nil {
		t.Fatalf("Marshal trace: %v", err)
	}
	traceBlob := session.NewJSONBlob(traceData)
	payload, err := json.Marshal(map[string]any{
		"run_id": runID, "trace_hash": traceBlob.Digest, "trace_sequence": 1,
	})
	if err != nil {
		t.Fatalf("Marshal payload: %v", err)
	}
	store := &frameStore{
		events: []session.Event{{
			EventID: "55555555-5555-4555-8555-555555555555", SessionID: sessionID,
			Sequence: 1, WriterEpoch: 1, Kind: session.EventTraceCommitted, Payload: payload,
		}},
		blobs: map[string]json.RawMessage{traceBlob.Digest: traceData},
	}
	if _, err := NewProjector(store, 0).Frames(context.Background(), sessionID, 0); err == nil {
		t.Fatal("Frames accepted a trace blob for a different run")
	}
}

func TestProjectorAttemptUpdateCarriesTerminalSegmentState(t *testing.T) {
	const sessionID = "11111111-1111-4111-8111-111111111111"
	const segmentID = "22222222-2222-4222-8222-222222222222"
	const runID = "33333333-3333-4333-8333-333333333333"
	payload := json.RawMessage(`{"run_id":"` + runID + `","status":"completed"}`)
	manifest := session.Manifest{
		SchemaVersion: session.ManifestSchemaV1,
		Session:       session.SessionRecord{SessionID: sessionID, Sequence: 1, Status: session.StatusPaused},
		Segments: map[string]session.SegmentRecord{
			segmentID: {SegmentID: segmentID, Status: session.SegmentStatusCompleted},
		},
		Attempts: map[string]session.RunAttemptRecord{
			runID: {RunID: runID, SegmentID: segmentID, Status: session.AttemptStatusCompleted},
		},
		AcceptedCommands: make(map[string]session.CommandReceipt),
	}
	store := &frameStore{
		events: []session.Event{{
			EventID: "44444444-4444-4444-8444-444444444444", SessionID: sessionID,
			Sequence: 1, WriterEpoch: 1, Kind: session.EventAttemptUpdated, Payload: payload,
		}},
		manifests: map[int64]session.Manifest{1: manifest},
	}
	frames, err := NewProjector(store, 0).Frames(context.Background(), sessionID, 0)
	if err != nil {
		t.Fatalf("Frames: %v", err)
	}
	attemptFrame := frameAtSequenceAndType(t, frames, 1, session.FrameAttemptFinished)
	segmentFrame := frameAtSequenceAndType(t, frames, 1, session.FrameSegmentFinished)
	if attemptFrame.SegmentID != segmentID || attemptFrame.RunID != runID ||
		segmentFrame.SegmentID != segmentID || segmentFrame.RunID != runID {
		t.Fatalf("terminal attempt frames = %#v", frames)
	}
}

func TestProjectorSessionPauseAndResumeCarryActiveAttemptIdentity(t *testing.T) {
	const sessionID = "11111111-1111-4111-8111-111111111111"
	const segmentID = "22222222-2222-4222-8222-222222222222"
	const runID = "33333333-3333-4333-8333-333333333333"
	baseManifest := session.Manifest{
		SchemaVersion: session.ManifestSchemaV1,
		Segments: map[string]session.SegmentRecord{
			segmentID: {SegmentID: segmentID, Status: session.SegmentStatusPaused},
		},
		Attempts: map[string]session.RunAttemptRecord{
			runID: {RunID: runID, SegmentID: segmentID, Status: session.AttemptStatusPausedAtBoundary},
		},
		AcceptedCommands: make(map[string]session.CommandReceipt),
	}
	paused := baseManifest
	paused.Session = session.SessionRecord{
		SessionID: sessionID, Sequence: 1, Status: session.StatusPaused,
		ActiveSegmentID: segmentID, ActiveRunID: runID,
	}
	resumed := baseManifest
	resumed.Session = session.SessionRecord{
		SessionID: sessionID, Sequence: 2, Status: session.StatusActive,
		ActiveSegmentID: segmentID, ActiveRunID: runID,
	}
	resumed.Segments = map[string]session.SegmentRecord{
		segmentID: {SegmentID: segmentID, Status: session.SegmentStatusActive},
	}
	resumed.Attempts = map[string]session.RunAttemptRecord{
		runID: {RunID: runID, SegmentID: segmentID, Status: session.AttemptStatusRunning},
	}
	store := &frameStore{
		events: []session.Event{
			{
				EventID: "44444444-4444-4444-8444-444444444444", SessionID: sessionID,
				Sequence: 1, WriterEpoch: 1, Kind: session.EventSessionPaused,
				Payload: json.RawMessage(`{"run_id":"` + runID + `","reason":"transport lost"}`),
			},
			{
				EventID: "55555555-5555-4555-8555-555555555555", SessionID: sessionID,
				Sequence: 2, WriterEpoch: 2, Kind: session.EventSessionResumed,
				Payload: json.RawMessage(`{"run_id":"` + runID + `"}`),
			},
		},
		manifests: map[int64]session.Manifest{1: paused, 2: resumed},
	}
	frames, err := NewProjector(store, 0).Frames(context.Background(), sessionID, 0)
	if err != nil {
		t.Fatalf("Frames: %v", err)
	}
	pauseFrame := frameAtSequenceAndType(t, frames, 1, session.FrameAttemptPaused)
	resumeFrame := frameAtSequenceAndType(t, frames, 2, session.FrameAttemptStarted)
	for _, frame := range []session.StdioFrame{pauseFrame, resumeFrame} {
		if frame.SegmentID != segmentID || frame.RunID != runID {
			t.Fatalf("attempt lifecycle frame = %#v", frame)
		}
	}
}

func TestProjectorReconstructsRunEventsAndTerminalSegmentFromExecutionMutation(t *testing.T) {
	const sessionID = "11111111-1111-4111-8111-111111111111"
	const segmentID = "22222222-2222-4222-8222-222222222222"
	const runID = "33333333-3333-4333-8333-333333333333"
	traceEvents := []engine.Event{
		{
			EventID: "44444444-4444-4444-8444-444444444444", RunID: runID, RunbookID: "root",
			Timestamp: time.Now().UTC().Format(time.RFC3339Nano), Kind: "step/completed", Sequence: 4,
			Payload: map[string]any{"step_id": "done"},
		},
		{
			EventID: "55555555-5555-4555-8555-555555555555", RunID: runID, RunbookID: "root",
			Timestamp: time.Now().UTC().Format(time.RFC3339Nano), Kind: "execution/committed", Sequence: 5,
			Payload: map[string]any{"checkpoint_sequence": 2},
		},
	}
	store := &frameStore{blobs: make(map[string]json.RawMessage), manifests: map[int64]session.Manifest{
		1: {
			SchemaVersion: session.ManifestSchemaV1,
			Session: session.SessionRecord{
				SessionID: sessionID, Sequence: 1, Status: session.StatusPaused, ActiveSegmentID: segmentID,
			},
			Segments: map[string]session.SegmentRecord{
				segmentID: {SegmentID: segmentID, Status: session.SegmentStatusCompleted},
			},
			Attempts: map[string]session.RunAttemptRecord{
				runID: {RunID: runID, SegmentID: segmentID, Status: session.AttemptStatusCompleted},
			},
			AcceptedCommands: make(map[string]session.CommandReceipt),
		},
	}}
	projection := addRunProjection(t, store, runID, 2, nil, traceEvents)
	store.events = []session.Event{{
		EventID: "66666666-6666-4666-8666-666666666666", SessionID: sessionID,
		Sequence: 1, WriterEpoch: 1, Kind: session.EventExecutionCommitted,
		Payload: executionProjectionPayloadWithTrace(
			t, runID, projection, session.AttemptStatusCompleted, 2, 5,
		),
	}}
	frames, err := NewProjector(store, 0).Frames(context.Background(), sessionID, 0)
	if err != nil {
		t.Fatalf("Frames: %v", err)
	}
	var projectedKinds []string
	for _, frame := range frames {
		if frame.Type != session.FrameRunEvent {
			continue
		}
		var event engine.Event
		if err := json.Unmarshal(frame.Payload, &event); err != nil {
			t.Fatalf("decode run event: %v", err)
		}
		projectedKinds = append(projectedKinds, event.Kind)
	}
	if len(projectedKinds) != 2 || projectedKinds[0] != "step/completed" || projectedKinds[1] != "execution/committed" {
		t.Fatalf("projected run event kinds = %#v", projectedKinds)
	}
	segmentFrame := frameAtSequenceAndType(t, frames, 1, session.FrameSegmentFinished)
	if segmentFrame.SegmentID != segmentID || segmentFrame.RunID != runID {
		t.Fatalf("segment finished frame = %#v", segmentFrame)
	}
	attemptFrame := frameAtSequenceAndType(t, frames, 1, session.FrameAttemptFinished)
	if attemptFrame.SegmentID != segmentID || attemptFrame.RunID != runID {
		t.Fatalf("attempt finished frame = %#v", attemptFrame)
	}
}

func TestProjectorCancellationFinishesAttemptSegmentAndSessionInOrder(t *testing.T) {
	const sessionID = "11111111-1111-4111-8111-111111111111"
	const segmentID = "22222222-2222-4222-8222-222222222222"
	const runID = "33333333-3333-4333-8333-333333333333"
	store := &frameStore{blobs: make(map[string]json.RawMessage), manifests: map[int64]session.Manifest{
		1: {
			SchemaVersion: session.ManifestSchemaV1,
			Session: session.SessionRecord{
				SessionID: sessionID, Sequence: 1, Status: session.StatusCancelled,
				ActiveSegmentID: segmentID,
			},
			Segments: map[string]session.SegmentRecord{
				segmentID: {SegmentID: segmentID, Status: session.SegmentStatusCancelled},
			},
			Attempts: map[string]session.RunAttemptRecord{
				runID: {RunID: runID, SegmentID: segmentID, Status: session.AttemptStatusCancelled},
			},
			AcceptedCommands: make(map[string]session.CommandReceipt),
		},
	}}
	projection := addRunProjection(t, store, runID, 1, nil, nil)
	store.events = []session.Event{{
		EventID: "44444444-4444-4444-8444-444444444444", SessionID: sessionID,
		Sequence: 1, WriterEpoch: 1, Kind: session.EventExecutionCommitted,
		Payload: executionProjectionPayloadWithTrace(
			t, runID, projection, session.AttemptStatusCancelled, 1, 0,
		),
	}}
	frames, err := NewProjector(store, 0).Frames(context.Background(), sessionID, 0)
	if err != nil {
		t.Fatalf("Frames: %v", err)
	}
	if len(frames) < 4 || frames[len(frames)-3].Type != session.FrameAttemptFinished ||
		frames[len(frames)-2].Type != session.FrameSegmentFinished ||
		frames[len(frames)-1].Type != session.FrameSessionFinished {
		t.Fatalf("cancellation frame order = %#v", frames)
	}
	var finished session.SessionRecord
	if err := json.Unmarshal(frames[len(frames)-1].Payload, &finished); err != nil ||
		finished.Status != session.StatusCancelled {
		t.Fatalf("session finished payload = %#v, %v", finished, err)
	}
}

func TestProjectorRejectsReplayCursorAheadOfJournal(t *testing.T) {
	const sessionID = "11111111-1111-4111-8111-111111111111"
	store := &frameStore{
		events: []session.Event{{
			EventID: "event", SessionID: sessionID, Sequence: 1, WriterEpoch: 1,
			Kind: session.EventSessionClosed, Payload: json.RawMessage(`{"status":"resolved"}`),
		}},
		manifests: map[int64]session.Manifest{1: {
			SchemaVersion: session.ManifestSchemaV1,
			Session:       session.SessionRecord{SessionID: sessionID, Sequence: 1, Status: session.StatusResolved},
		}},
	}
	if _, err := NewProjector(store, 0).Frames(context.Background(), sessionID, 2); err == nil {
		t.Fatal("Frames accepted replay cursor ahead of the journal")
	}
}

type sessionInteraction struct {
	TurnID string          `json:"turn_id"`
	StepID string          `json:"step_id"`
	Kind   string          `json:"kind"`
	Status string          `json:"status"`
	Answer json.RawMessage `json:"answer,omitempty"`
}

func addInteractionProjection(
	t *testing.T,
	store *frameStore,
	runID string,
	checkpoint int64,
	interactions map[string]*sessionInteraction,
) string {
	return addRunProjection(t, store, runID, checkpoint, interactions, nil)
}

func addRunProjection(
	t *testing.T,
	store *frameStore,
	runID string,
	checkpoint int64,
	interactions map[string]*sessionInteraction,
	traceEvents []engine.Event,
) string {
	t.Helper()
	snapshot, _ := json.Marshal(map[string]any{
		"RunID": runID, "Interactions": interactions, "pending_trace_events": traceEvents,
	})
	fileDigest := session.DigestBytes(snapshot)
	chunkData, _ := json.Marshal(map[string]any{
		"schema_version": "yawr.session-run-projection-chunk/v1", "data": snapshot,
	})
	chunk := session.NewJSONBlob(chunkData)
	store.blobs[chunk.Digest] = chunk.Data
	rootData, _ := json.Marshal(map[string]any{
		"schema_version": "yawr.session-run-projection/v1", "run_id": runID,
		"checkpoint_sequence": checkpoint,
		"files": []map[string]any{{
			"path": "snapshots/checkpoint.json", "digest": fileDigest,
			"size": len(snapshot), "chunk_digests": []string{chunk.Digest},
		}},
	})
	root := session.NewJSONBlob(rootData)
	store.blobs[root.Digest] = root.Data
	return root.Digest
}

func executionProjectionPayloadWithTrace(
	t *testing.T,
	runID string,
	projectionHash string,
	status session.AttemptStatus,
	checkpoint int64,
	committedTraceSequence int64,
) json.RawMessage {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"run_id": runID, "status": status, "mutation_hash": session.DigestBytes([]byte("mutation")),
		"projection_hash": projectionHash, "checkpoint_sequence": checkpoint,
		"committed_trace_sequence": committedTraceSequence,
	})
	if err != nil {
		t.Fatalf("Marshal execution payload: %v", err)
	}
	return payload
}

func executionProjectionPayload(
	t *testing.T,
	runID string,
	projectionHash string,
	status session.AttemptStatus,
	checkpoint int64,
) json.RawMessage {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"run_id": runID, "status": status, "mutation_hash": session.DigestBytes([]byte("mutation")),
		"projection_hash": projectionHash, "checkpoint_sequence": checkpoint,
		"committed_trace_sequence": 0,
	})
	if err != nil {
		t.Fatalf("Marshal execution payload: %v", err)
	}
	return payload
}

func hasFrameTypeAtSequence(frames []session.StdioFrame, frameType session.FrameType, sequence int64) bool {
	for _, frame := range frames {
		if frame.Type == frameType && frame.SessionSequence == sequence {
			return true
		}
	}
	return false
}

func frameAtSequenceAndType(
	t *testing.T,
	frames []session.StdioFrame,
	sequence int64,
	frameType session.FrameType,
) session.StdioFrame {
	t.Helper()
	for _, frame := range frames {
		if frame.SessionSequence == sequence && frame.Type == frameType {
			return frame
		}
	}
	t.Fatalf("frame %s at sequence %d not found", frameType, sequence)
	return session.StdioFrame{}
}
