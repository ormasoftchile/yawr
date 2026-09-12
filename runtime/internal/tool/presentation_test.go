package tool

import (
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"testing"
)

func TestPresentationRuntimeAdapterPreservesDeclaredSlots(t *testing.T) {
	for _, transport := range []string{"native", "stdio", "mcp-http", "vscode-mcp"} {
		source := []byte("apiVersion: yawr.tool/v1\nmeta: {name: db, version: 1.0.0}\ntransport: {mode: " + transport + "}\nactions:\n  - name: run\n    args:\n      code: {type: string, presentation: {version: 1, kind: code, language: sql}}\n    outputs:\n      script: {type: string, optional: true, from: raw, presentation: {version: 7, kind: code, language: future}}\n")
		def, err := ParseToolBytes(source, "metadata.tool.yaml", true)
		if err != nil {
			t.Fatal(err)
		}
		// Metadata-only parsing does not require a command, URL or auth profile.
		if def.Actions["run"].Args["code"].Presentation.Language != "sql" {
			t.Fatal("input metadata missing")
		}
		def.Transport = schema.TransportConfig{Type: schema.Transport("native")}
		runtime, err := RuntimeToolDef(def)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.Actions["run"].Args["code"].Presentation.Language != "sql" || runtime.Actions["run"].Outputs["script"].From != "raw" || runtime.Actions["run"].Outputs["script"].Presentation.Version != 7 {
			t.Fatal("adapter lost descriptor or declared output mapping")
		}
	}
}
