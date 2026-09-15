package tool

import (
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestSchemaFromRuntimePreservesContractsAndOwnsCopies(t *testing.T) {
	action := &ToolAction{Args: map[string]*ArgDef{
		"server": {Type: "string", Required: true, Default: "fixture", From: "args.server", Optional: true},
	}, Outputs: map[string]*schema.ArgDef{"marker": {Type: "string"}},
		OutputContract: &schema.OutputContract{AdditionalOutputs: schema.AdditionalOutputsIgnore},
		VSCodeInput:    map[string]*schema.VSCodeInputMapping{"server": {From: "server"}}}
	runtime := ToolDef{Name: "query", Transport: TransportMCPHTTP, URL: "https://example.invalid",
		Auth:    &schema.AuthConfig{Provider: "azure-cli", Scope: "fixture", AllowedHosts: []string{"example.invalid"}},
		Actions: map[string]*ToolAction{"run": action}}
	declaration := SchemaFromRuntime(runtime)
	arg := declaration.Actions["run"].Args["server"]
	if arg.From != "args.server" || !arg.Optional || !arg.Required ||
		declaration.Transport.URL != runtime.URL ||
		declaration.Actions["run"].OutputContract.AdditionalOutputs != schema.AdditionalOutputsIgnore ||
		declaration.Actions["run"].VSCodeInput["server"].From != "server" {
		t.Fatalf("compiled definition lost contract metadata: %+v", declaration)
	}
	declaration.Actions["run"].Outputs["marker"].Type = "integer"
	declaration.Transport.Auth.AllowedHosts[0] = "changed.invalid"
	if action.Outputs["marker"].Type != "string" || runtime.Auth.AllowedHosts[0] != "example.invalid" {
		t.Fatal("schema conversion shared mutable contract state")
	}
}
