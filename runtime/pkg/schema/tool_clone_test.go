package schema

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestCloneToolDefinitionOwnsNestedState(t *testing.T) {
	approval := false
	original := &ToolDef{Name: "shared",
		Governance: &ToolGovernance{RequiresApproval: &approval, AllowCommands: []string{"safe"}},
		Actions: map[string]*ToolAction{"run": {
			Args: map[string]*ArgDef{"payload": {Type: "object", Default: map[string]any{"values": []any{int64(9007199254740993), false, ""}}}},
			FrozenSubstitution: &FrozenToolSubstitution{
				ExecutableClosure: json.RawMessage(`[]`),
				Inputs:            map[string]*Input{"flag": {Type: "boolean", Default: false}},
				Outputs:           map[string]*Output{"result": {Type: "string", ValueExpr: `"ok"`}},
			},
		}},
	}
	cloned := CloneToolDef(original)
	if !reflect.DeepEqual(cloned, original) {
		t.Fatal("clone changed schema or native values")
	}
	*cloned.Governance.RequiresApproval = true
	cloned.Governance.AllowCommands[0] = "changed"
	action := cloned.Actions["run"]
	action.Args["payload"].Default.(map[string]any)["values"].([]any)[0] = int64(1)
	action.FrozenSubstitution.ExecutableClosure[0] = '{'
	action.FrozenSubstitution.Inputs["flag"].Default = true
	action.FrozenSubstitution.Outputs["result"].ValueExpr = `"changed"`
	if approval || original.Governance.AllowCommands[0] != "safe" ||
		original.Actions["run"].Args["payload"].Default.(map[string]any)["values"].([]any)[0] != int64(9007199254740993) ||
		string(original.Actions["run"].FrozenSubstitution.ExecutableClosure) != "[]" ||
		original.Actions["run"].FrozenSubstitution.Inputs["flag"].Default != false ||
		original.Actions["run"].FrozenSubstitution.Outputs["result"].ValueExpr != `"ok"` {
		t.Fatal("clone mutated shared nested state")
	}
	if CloneToolDef(nil) != nil || CloneToolAction(nil) != nil {
		t.Fatal("nil clone")
	}
}
