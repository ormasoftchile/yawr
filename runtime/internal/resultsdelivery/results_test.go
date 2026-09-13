package resultsdelivery

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
)

func TestPrepareInvalidProtectedAndNoPublication(t *testing.T) {
	record := func(typ string, value any) *engine.RunResults {
		r := &engine.RunResults{SchemaVersion: engine.RunResultsSchemaV1, PublicationID: "pub", PlanSnapshotDigest: "sha256:" + strings.Repeat("a", 64), CheckpointSequence: 1,
			Origin: engine.ResultsOrigin{NodeID: "results", Invocation: 1}, Outputs: map[string]engine.NamedResultValue{"result": {Type: typ, Value: value}}}
		if err := r.Seal(); err != nil {
			t.Fatal(err)
		}
		return r
	}
	for _, test := range []struct {
		record *engine.RunResults
		err    error
		reason string
	}{
		{nil, nil, "no-publication"},
		{nil, errors.New("private error"), "invalid-publication"},
		{record("integer", "0"), nil, "invalid-publication"},
		{record("secret", "private"), nil, "protected-content"},
	} {
		body, unavailable := Prepare(test.record, test.err, engine.RunStatusCompleted, engine.DebugProtection{})
		if body != nil || unavailable == nil || unavailable.Reason() != test.reason {
			t.Fatal("invalid public result outcome")
		}
	}
}

func TestPrepareExecutionAvailabilityMapping(t *testing.T) {
	valid := func() *engine.RunResults {
		record := &engine.RunResults{
			SchemaVersion: engine.RunResultsSchemaV1, PublicationID: "pub",
			PlanSnapshotDigest: "sha256:" + strings.Repeat("a", 64), CheckpointSequence: 1,
			Origin:  engine.ResultsOrigin{NodeID: "results", Invocation: 1},
			Outputs: map[string]engine.NamedResultValue{"result": {Type: "string", Value: "safe"}},
		}
		if err := record.Seal(); err != nil {
			t.Fatal(err)
		}
		return record
	}
	for _, test := range []struct {
		name   string
		record *engine.RunResults
		status engine.RunStatus
		reason string
	}{
		{"pending-without-publication", nil, engine.RunStatusPending, "no-publication"},
		{"failed-denied-without-publication", nil, engine.RunStatusFailed, "no-publication"},
		{"pending-with-committed-publication", valid(), engine.RunStatusPending, "execution-not-completed"},
		{"failed-with-committed-publication", valid(), engine.RunStatusFailed, "execution-not-completed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			body, unavailable := Prepare(test.record, nil, test.status, engine.DebugProtection{})
			if body != nil || unavailable == nil || unavailable.Reason() != test.reason {
				t.Fatalf("status=%s reason=%v", test.status, unavailable)
			}
		})
	}
}

func TestPrepareOversizePublicationFailsClosed(t *testing.T) {
	record := &engine.RunResults{
		SchemaVersion: engine.RunResultsSchemaV1, PublicationID: "oversize",
		PlanSnapshotDigest: "sha256:" + strings.Repeat("a", 64), CheckpointSequence: 1,
		Origin: engine.ResultsOrigin{NodeID: "results", Invocation: 1},
		Outputs: map[string]engine.NamedResultValue{
			"result": {Type: "string", Value: strings.Repeat("<", engine.MaxResultsBytes/6+1)},
		},
		Digest: "sha256:untrusted",
	}
	body, unavailable := Prepare(record, nil, engine.RunStatusCompleted, engine.DebugProtection{})
	if body != nil || unavailable == nil || unavailable.Reason() != "invalid-publication" {
		t.Fatal("oversize publication did not fail closed")
	}
}

func TestUnavailableClosedVocabularyWireRoundTrip(t *testing.T) {
	tests := []struct {
		name   string
		value  *Unavailable
		status string
		reason string
	}{
		{"no-publication", NoPublication(), "unavailable", "no-publication"},
		{"invalid-publication", InvalidPublication(), "unavailable", "invalid-publication"},
		{"protected-content", ProtectedContent(), "redacted", "protected-content"},
		{"execution-not-completed", ExecutionNotCompleted(), "unavailable", "execution-not-completed"},
		{"protection-unavailable", ProtectionUnavailable(), "unavailable", "protection-unavailable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body, err := json.Marshal(test.value)
			if err != nil {
				t.Fatal(err)
			}
			want := `{"status":"` + test.status + `","reason":"` + test.reason + `"}`
			if string(body) != want || test.value.Status() != test.status || test.value.Reason() != test.reason {
				t.Fatalf("wire vocabulary changed: %s", body)
			}
			var decoded Unavailable
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(&decoded, test.value) {
				t.Fatalf("round trip changed value: %+v", decoded)
			}
		})
	}
}

func TestUnavailableStrictDecodeAndSerialization(t *testing.T) {
	invalid := []string{
		`null`, `{}`, `{"status":"unavailable"}`, `{"reason":"no-publication"}`,
		`{"status":"unknown","reason":"no-publication"}`,
		`{"status":"redacted","reason":"no-publication"}`,
		`{"status":"unavailable","reason":"protected-content"}`,
		`{"status":"redacted","reason":"unknown"}`,
		`{"status":"unavailable","reason":"no-publication","extra":true}`,
		`{"status":"unavailable","status":"unavailable","reason":"no-publication"}`,
		`{"status":"unavailable","reason":"no-publication","reason":"no-publication"}`,
		`{"status":1,"reason":"no-publication"}`,
		`{"status":"unavailable","reason":1}`,
		`{"status":"unavailable","reason":"no-publication"} true`,
	}
	for _, input := range invalid {
		t.Run(input, func(t *testing.T) {
			receiver := *ProtectedContent()
			before := receiver
			if err := json.Unmarshal([]byte(input), &receiver); err == nil {
				t.Fatal("invalid unavailable envelope accepted")
			}
			if receiver != before {
				t.Fatal("receiver changed on decode error")
			}
		})
	}
	for _, value := range []Unavailable{
		{},
		{status: statusRedacted, reason: reasonNoPublication},
		{status: statusUnavailable, reason: reasonProtectedContent},
		{status: statusUnavailable, reason: unavailableReason("unknown")},
	} {
		if _, err := json.Marshal(value); err == nil {
			t.Fatalf("invalid unavailable envelope serialized: %+v", value)
		}
	}
}
