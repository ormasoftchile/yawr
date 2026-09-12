package engine

import (
	"bytes"
	"encoding/json"
	"math"
	"testing"
)

func TestRunResultsCanonicalMatchesHistoricalSerialization(t *testing.T) {
	for _, value := range []any{nil, false, 0, json.Number("-0"), math.Copysign(0, -1), 1e30,
		[]any{"á😀<&\u2028\u2029", map[string]any{"z": nil, "a": false}},
		map[string]any{"\ue000": 0, "😀": false, "a": []any{}}} {
		record := &RunResults{Outputs: map[string]NamedResultValue{"result": {Type: "any", Value: value}}}
		old, _ := json.Marshal(record)
		decoder := json.NewDecoder(bytes.NewReader(old))
		decoder.UseNumber()
		var tree any
		if err := decoder.Decode(&tree); err != nil {
			t.Fatal(err)
		}
		expected, _ := json.Marshal(tree)
		actual, err := CanonicalResultsJSON(record, true)
		if err != nil || !bytes.Equal(actual, expected) {
			t.Fatalf("serialization drift: %s vs %s (%v)", actual, expected, err)
		}
	}
}

func TestRunResultsRejectInvalidUTF8AndRecursiveData(t *testing.T) {
	cycle := map[string]any{}
	cycle["cycle"] = cycle
	for _, value := range []any{string([]byte{0xff}), cycle} {
		record := &RunResults{Outputs: map[string]NamedResultValue{"result": {Type: "any", Value: value}}}
		if _, err := CanonicalResultsJSON(record, true); err == nil {
			t.Fatal("invalid/unbounded canonical value accepted")
		}
	}
}

func TestRunResultsCanonicalNativeRecord(t *testing.T) {
	result := &RunResults{
		SchemaVersion: RunResultsSchemaV1, PublicationID: "run:root:results:1",
		PlanSnapshotDigest: "sha256:plan", CheckpointSequence: 42,
		Origin: ResultsOrigin{NodeID: "results", Invocation: 1},
		Outputs: map[string]NamedResultValue{
			"z": {Type: "any", Value: nil},
			"a": {Type: "object", Value: map[string]any{"zero": 0, "false": false, "rows": []any{"á😀", nil, 0}, "empty": ""}},
		},
	}
	if err := result.Seal(); err != nil {
		t.Fatal(err)
	}
	if err := result.Validate(); err != nil {
		t.Fatal(err)
	}
	encoded, err := CanonicalResultsJSON(result, true)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(encoded, []byte(`{"checkpoint_sequence":42,"digest":`)) ||
		!bytes.Contains(encoded, []byte(`"value":null`)) ||
		bytes.Contains(encoded, []byte(`"frame_id"`)) {
		t.Fatalf("not canonical/presence-safe: %s", encoded)
	}
	clone, err := CloneRunResults(result)
	if err != nil {
		t.Fatal(err)
	}
	clone.Outputs["z"] = NamedResultValue{Type: "any", Value: false}
	if err := clone.Validate(); err == nil {
		t.Fatal("modified result digest accepted")
	}
	if err := result.Validate(); err != nil {
		t.Fatalf("clone mutated original: %v", err)
	}
}
