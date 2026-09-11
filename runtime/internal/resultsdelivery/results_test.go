package resultsdelivery

import (
	"errors"
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
		if body != nil || unavailable == nil || unavailable.Reason != test.reason {
			t.Fatal("invalid public result outcome")
		}
	}
}
