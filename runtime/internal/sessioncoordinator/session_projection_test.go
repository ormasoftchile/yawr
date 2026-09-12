package sessioncoordinator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/session"
)

func TestSessionRunProjectionChunksFilesAndDropsHistoricalSnapshots(t *testing.T) {
	runID := uuid.NewString()
	traceData := bytes.Repeat([]byte("trace-data"), sessionProjectionChunkBytes/10+1)
	referencedDigest := "sha256:" + strings.Repeat("a", 64)
	currentSnapshot := []byte(`{"DurableVars":{"value":{"blob":{"digest":"` + referencedDigest + `"}}}}`)
	archive, err := json.Marshal(runProjectionArchive{
		SchemaVersion: "yawr.run-projection/v1",
		RunID:         runID,
		Files: []runProjectionArchiveFile{
			{Path: "blobs/" + strings.Repeat("a", 64) + ".json.gz", Digest: session.DigestBytes([]byte("current")), Data: []byte("current")},
			{Path: "blobs/" + strings.Repeat("b", 64) + ".json.gz", Digest: session.DigestBytes([]byte("stale")), Data: []byte("stale")},
			{Path: "plan.v1.json", Digest: session.DigestBytes([]byte(`{"plan":true}`)), Data: []byte(`{"plan":true}`)},
			{Path: "snapshots/checkpoint-00000000000000000001.json", Digest: session.DigestBytes([]byte(`{"checkpoint":1}`)), Data: []byte(`{"checkpoint":1}`)},
			{Path: "snapshots/checkpoint-00000000000000000002.json", Digest: session.DigestBytes(currentSnapshot), Data: currentSnapshot},
			{Path: "trace.jsonl", Digest: session.DigestBytes(traceData), Data: traceData},
		},
	})
	if err != nil {
		t.Fatalf("marshal archive: %v", err)
	}
	root, chunks, err := encodeSessionRunProjection(archive, engine.RunState{
		RunID: runID, CheckpointSequence: 2,
	})
	if err != nil {
		t.Fatalf("encodeSessionRunProjection: %v", err)
	}
	if len(root.Data) >= len(archive) || len(chunks) < 3 {
		t.Fatalf("chunked projection root=%d archive=%d chunks=%d", len(root.Data), len(archive), len(chunks))
	}
	stored := make(map[string]json.RawMessage, len(chunks))
	for _, chunk := range chunks {
		if len(chunk.Data) > 64<<20 {
			t.Fatalf("chunk %s exceeds session blob limit: %d", chunk.Digest, len(chunk.Data))
		}
		stored[chunk.Digest] = chunk.Data
	}
	restored, err := decodeSessionRunProjection(context.Background(), root.Data, func(digest string) (json.RawMessage, error) {
		data, found := stored[digest]
		if !found {
			return nil, fmt.Errorf("missing chunk %s", digest)
		}
		return data, nil
	})
	if err != nil {
		t.Fatalf("decodeSessionRunProjection: %v", err)
	}
	var decoded runProjectionArchive
	if err := json.Unmarshal(restored, &decoded); err != nil {
		t.Fatalf("unmarshal restored archive: %v", err)
	}
	if len(decoded.Files) != 4 {
		t.Fatalf("restored files = %#v", decoded.Files)
	}
	for _, file := range decoded.Files {
		if file.Path == "snapshots/checkpoint-00000000000000000001.json" {
			t.Fatal("historical snapshot was retained")
		}
		if strings.Contains(file.Path, strings.Repeat("b", 64)) {
			t.Fatal("unreferenced state blob was retained")
		}
		if file.Path == "trace.jsonl" && !bytes.Equal(file.Data, traceData) {
			t.Fatal("trace data changed during chunk round trip")
		}
	}
}

func TestExecutionFrameProjectionAcceptsContentBelowStdioFrameBudget(t *testing.T) {
	runID := uuid.NewString()
	largeValue := string(bytes.Repeat([]byte{'x'}, session.MaxStdioFrameProjectionBytes/2))
	root, chunks, err := encodeExecutionFrameProjection(engine.RunState{
		RunID: runID, CheckpointSequence: 4, CommittedTraceSequence: 9,
		PendingTraceEvents: []engine.Event{{
			EventID: uuid.NewString(), RunID: runID, Sequence: 9, Kind: "step/completed",
			Payload: map[string]any{"output": largeValue},
		}},
	})
	if err != nil {
		t.Fatalf("encodeExecutionFrameProjection: %v", err)
	}
	var projection session.ExecutionFrameProjection
	if err := json.Unmarshal(root.Data, &projection); err != nil {
		t.Fatalf("decode frame projection root: %v", err)
	}
	if len(chunks) != 1 || len(projection.ChunkDigests) != len(chunks) ||
		projection.ContentSize >= session.MaxStdioFrameProjectionBytes {
		t.Fatalf("frame projection chunks/size = %d/%d/%d", len(chunks), len(projection.ChunkDigests), projection.ContentSize)
	}
	content := make([]byte, 0, projection.ContentSize)
	for index, chunkBlob := range chunks {
		if chunkBlob.Digest != projection.ChunkDigests[index] {
			t.Fatalf("chunk %d digest = %s, want %s", index, chunkBlob.Digest, projection.ChunkDigests[index])
		}
		var chunk session.ExecutionFrameProjectionChunk
		if err := json.Unmarshal(chunkBlob.Data, &chunk); err != nil ||
			chunk.SchemaVersion != session.ExecutionFrameProjectionChunkSchemaV1 ||
			len(chunk.Data) > sessionProjectionChunkBytes {
			t.Fatalf("chunk %d = %#v, %v", index, chunk, err)
		}
		content = append(content, chunk.Data...)
	}
	if int64(len(content)) != projection.ContentSize || session.DigestBytes(content) != projection.ContentDigest {
		t.Fatalf("rebuilt frame projection content = %d/%s", len(content), session.DigestBytes(content))
	}
}

func TestExecutionFrameProjectionRejectsContentBeyondStdioSequenceBudget(t *testing.T) {
	runID := uuid.NewString()
	largeValue := string(bytes.Repeat([]byte{'x'}, session.MaxStdioFrameProjectionBytes+1))
	_, _, err := encodeExecutionFrameProjection(engine.RunState{
		RunID: runID, CheckpointSequence: 1, CommittedTraceSequence: 1,
		PendingTraceEvents: []engine.Event{{
			EventID: uuid.NewString(), RunID: runID, Sequence: 1, Kind: "step/completed",
			Payload: map[string]any{"output": largeValue},
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "exceeds limits") {
		t.Fatalf("oversized frame projection error = %v", err)
	}
}

func TestSessionRunProjectionRejectsInvalidChunks(t *testing.T) {
	runID := uuid.NewString()
	traceData := bytes.Repeat([]byte("trace-chunk"), sessionProjectionChunkBytes/10+1)
	snapshot := []byte(`{"checkpoint":1}`)
	archive, err := json.Marshal(runProjectionArchive{
		SchemaVersion: "yawr.run-projection/v1", RunID: runID,
		Files: []runProjectionArchiveFile{
			{Path: "plan.v1.json", Digest: session.DigestBytes([]byte(`{"plan":true}`)), Data: []byte(`{"plan":true}`)},
			{Path: "snapshots/checkpoint-00000000000000000001.json", Digest: session.DigestBytes(snapshot), Data: snapshot},
			{Path: "trace.jsonl", Digest: session.DigestBytes(traceData), Data: traceData},
		},
	})
	if err != nil {
		t.Fatalf("marshal archive: %v", err)
	}
	root, chunks, err := encodeSessionRunProjection(archive, engine.RunState{RunID: runID, CheckpointSequence: 1})
	if err != nil {
		t.Fatalf("encodeSessionRunProjection: %v", err)
	}
	stored := make(map[string]json.RawMessage, len(chunks))
	for _, chunk := range chunks {
		stored[chunk.Digest] = chunk.Data
	}
	load := func(values map[string]json.RawMessage) func(string) (json.RawMessage, error) {
		return func(digest string) (json.RawMessage, error) {
			data, found := values[digest]
			if !found {
				return nil, fmt.Errorf("missing chunk %s", digest)
			}
			return data, nil
		}
	}

	t.Run("missing", func(t *testing.T) {
		missing := maps.Clone(stored)
		delete(missing, chunks[0].Digest)
		if _, err := decodeSessionRunProjection(context.Background(), root.Data, load(missing)); err == nil {
			t.Fatal("missing projection chunk was accepted")
		}
	})

	t.Run("tampered", func(t *testing.T) {
		tampered := maps.Clone(stored)
		tampered[chunks[0].Digest] = json.RawMessage(`{"schema_version":"yawr.session-run-projection-chunk/v1","data":"dGFtcGVyZWQ="}`)
		if _, err := decodeSessionRunProjection(context.Background(), root.Data, load(tampered)); err == nil {
			t.Fatal("tampered projection chunk was accepted")
		}
	})

	t.Run("reordered", func(t *testing.T) {
		var projection sessionRunProjection
		if err := json.Unmarshal(root.Data, &projection); err != nil {
			t.Fatalf("decode root: %v", err)
		}
		for index := range projection.Files {
			if projection.Files[index].Path == "trace.jsonl" {
				digests := projection.Files[index].ChunkDigests
				if len(digests) < 2 {
					t.Fatal("trace fixture did not produce multiple chunks")
				}
				digests[0], digests[len(digests)-1] = digests[len(digests)-1], digests[0]
			}
		}
		reordered, err := json.Marshal(projection)
		if err != nil {
			t.Fatalf("encode reordered root: %v", err)
		}
		if _, err := decodeSessionRunProjection(context.Background(), reordered, load(stored)); err == nil {
			t.Fatal("reordered projection chunks were accepted")
		}
	})

	t.Run("oversized", func(t *testing.T) {
		var projection sessionRunProjection
		if err := json.Unmarshal(root.Data, &projection); err != nil {
			t.Fatalf("decode root: %v", err)
		}
		oversizedData, err := json.Marshal(sessionRunProjectionChunk{
			SchemaVersion: sessionProjectionChunkSchemaV1,
			Data:          bytes.Repeat([]byte{'x'}, sessionProjectionChunkBytes+1),
		})
		if err != nil {
			t.Fatalf("encode oversized chunk: %v", err)
		}
		oversized := session.NewJSONBlob(oversizedData)
		values := maps.Clone(stored)
		values[oversized.Digest] = oversized.Data
		projection.Files[0].ChunkDigests[0] = oversized.Digest
		oversizedRoot, err := json.Marshal(projection)
		if err != nil {
			t.Fatalf("encode oversized root: %v", err)
		}
		if _, err := decodeSessionRunProjection(context.Background(), oversizedRoot, load(values)); err == nil {
			t.Fatal("oversized projection chunk was accepted")
		}
	})
}
