package schema

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestScopedClosureSharedIntegrityEnvelope(t *testing.T) {
	closure := ScopedFlowClosure{
		SchemaVersion: ScopedFlowClosureVersion, RootScopeID: "owner", ScopeContextDigest: "context",
		Nodes: json.RawMessage(`[{"b":9007199254740993,"a":1}]`),
		Invocation: &RunbookInvocation{Bindings: []Binding{{
			Name: "number", Type: "int", Value: json.Number("9007199254740993"), ValuePresent: true,
		}}},
	}
	first, err := ScopedFlowClosureDigest(closure)
	if err != nil {
		t.Fatal(err)
	}
	closure.Nodes = json.RawMessage(`[ { "a": 1, "b": 9007199254740993 } ]`)
	second, err := ScopedFlowClosureDigest(closure)
	if err != nil || first != second {
		t.Fatal("canonical digest depended on node object key order/whitespace")
	}
	closure.ClosureDigest = second
	body, err := json.Marshal(closure)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeScopedFlowClosure(body)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Invocation.Bindings[0].Value != json.Number("9007199254740993") {
		t.Fatal("large integer lost precision")
	}
	for _, test := range []struct {
		name string
		body []byte
	}{
		{"duplicate", bytes.Replace(body, []byte(`"nodes":`), []byte(`"schema_version":"execution-flow-closure/v3","nodes":`), 1)},
		{"unknown-field", append(append([]byte(nil), body[:len(body)-1]...), []byte(`,"unknown":true}`)...)},
		{"unknown-version", bytes.Replace(body, []byte(ScopedFlowClosureVersion), []byte("execution-flow-closure/v100"), 1)},
		{"invocation-tamper", bytes.Replace(body, []byte(`"name":"number"`), []byte(`"name":"altered"`), 1)},
		{"trailing", append(append([]byte(nil), body...), []byte(`{}`)...)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if bytes.Equal(test.body, body) {
				t.Fatal("test failed to mutate envelope")
			}
			if _, err := DecodeScopedFlowClosure(test.body); err == nil {
				t.Fatal("invalid scoped envelope accepted")
			}
		})
	}
}
