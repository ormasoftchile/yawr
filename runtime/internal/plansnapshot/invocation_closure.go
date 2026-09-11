package plansnapshot

import (
	"encoding/json"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// Invocation declarations are persisted by the enclosing frozen tool or pin.
// The current closure version also governs the invocation child body.
func EncodeInvocationFlowClosure(nodes []schema.FlowNode, invocation *schema.RunbookInvocation, tools ...map[string]*schema.ToolDef) (json.RawMessage, error) {
	data, err := EncodeFlowClosure(nodes, tools...)
	if err != nil {
		return nil, err
	}
	if invocation == nil || len(invocation.Bindings) == 0 && !invocation.Results && !hasTypedMetadata(invocation.Outputs) {
		return data, nil
	}
	var closure flowClosureV1
	if err := decodeStrictJSON(data, &closure); err != nil {
		return nil, err
	}
	closure.SchemaVersion = FlowClosureSchemaV3
	closure.ClosureDigest, err = flowClosureDigest(closure)
	if err != nil {
		return nil, err
	}
	return json.Marshal(closure)
}
