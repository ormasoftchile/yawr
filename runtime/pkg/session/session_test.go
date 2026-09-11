package session

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
)

func TestLegacyExecutionMutationEncodingOmitsFrameProjectionHash(t *testing.T) {
	legacy := struct {
		SchemaVersion          string           `json:"schema_version"`
		RunID                  string           `json:"run_id"`
		CheckpointSequence     int64            `json:"checkpoint_sequence"`
		RunStatus              engine.RunStatus `json:"run_status"`
		AttemptStatus          AttemptStatus    `json:"attempt_status"`
		RunWriterEpoch         uint64           `json:"run_writer_epoch"`
		PreviousMutationHash   string           `json:"previous_mutation_hash,omitempty"`
		StateProjectionHash    string           `json:"state_projection_hash"`
		CommittedTraceSequence int64            `json:"committed_trace_sequence"`
		TraceSequenceStart     int64            `json:"trace_sequence_start,omitempty"`
		TraceSequenceEnd       int64            `json:"trace_sequence_end,omitempty"`
	}{
		SchemaVersion: ExecutionMutationSchemaV1, RunID: "run", CheckpointSequence: 7,
		RunStatus: engine.RunStatusWaiting, AttemptStatus: AttemptStatusWaiting, RunWriterEpoch: 3,
		PreviousMutationHash: DigestJSON("previous"), StateProjectionHash: DigestJSON("state"),
		CommittedTraceSequence: 9, TraceSequenceStart: 8, TraceSequenceEnd: 9,
	}
	legacyData, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal legacy mutation: %v", err)
	}
	var mutation ExecutionMutation
	if err := json.Unmarshal(legacyData, &mutation); err != nil {
		t.Fatalf("unmarshal legacy mutation: %v", err)
	}
	blob, err := NewExecutionMutationBlob(mutation)
	if err != nil {
		t.Fatalf("NewExecutionMutationBlob: %v", err)
	}
	if !bytes.Equal(blob.Data, legacyData) || blob.Digest != DigestBytes(legacyData) {
		t.Fatalf("legacy mutation encoding/hash changed: %s != %s", blob.Data, legacyData)
	}
}

func TestOccurrenceIdentityRoundTripPreservesCallPath(t *testing.T) {
	record := OccurrenceRecord{
		SessionID: "session", SegmentID: "segment", RunID: "run",
		QualifiedNodeID: "outer/inner/check", CallPath: []engine.DebugCallFrame{{StepID: "outer"}, {StepID: "inner"}},
		StepID: "check", Phase: "execute", Invocation: 2, RetryAttempt: 3,
		OccurrenceSequence: 7, ExecutionSource: ExecutionSourceSaved, Status: "completed", EventSequence: 42,
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored OccurrenceRecord
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(restored.CallPath) != 2 || restored.CallPath[0].StepID != "outer" ||
		restored.QualifiedNodeID != "outer/inner/check" || restored.RetryAttempt != 3 {
		t.Fatalf("restored occurrence = %#v", restored)
	}
}
