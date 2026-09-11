package session

import (
	"encoding/json"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
)

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
