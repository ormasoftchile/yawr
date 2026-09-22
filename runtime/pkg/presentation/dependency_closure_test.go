package presentation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIncludedDocumentPresentationUsesOwnDependenciesAndDirtyTool(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(filepath.Dir(filepath.Dir(cwd)), "examples", "dependency-scopes")
	for _, entry := range []string{"static-and-lazy", "parallel", "dynamic"} {
		for _, child := range []string{"left", "right"} {
			t.Run(entry+"/"+child, func(t *testing.T) {
				path := filepath.Join(root, "catalog", child+".yawr")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				toolPath := filepath.Join(root, "catalog", "tools", child, "query.yawt")
				toolData, err := os.ReadFile(toolPath)
				if err != nil {
					t.Fatal(err)
				}
				dirty := strings.Replace(string(toolData), "marker: {type: string, required: true}",
					"marker: {type: string, required: true, presentation: {version: 1, kind: code, language: sql}}", 1)
				reply := Resolve(Request{
					SchemaVersion: SchemaVersion, RequestID: "lexical",
					Context:  Context{ProjectRoot: root, EntrypointPath: filepath.Join(root, entry+".yawr")},
					Document: Buffer{Path: path, URI: FileURI(path), Version: 1, Text: string(data)},
					Overlays: []Buffer{{Path: toolPath, URI: FileURI(toolPath), Version: 7, Text: dirty}},
				})
				if reply.Status != "resolved" || len(reply.Bindings) != 1 || reply.Bindings[0].Status != "resolved" {
					t.Fatalf("child dependencies unresolved: %+v", reply)
				}
				binding := reply.Bindings[0]
				if binding.SourceURI != FileURI(toolPath) || len(binding.Actions) != 1 ||
					len(binding.Actions[0].Outputs) != 1 || binding.Actions[0].Outputs[0].Presentation == nil ||
					binding.Actions[0].Outputs[0].Presentation.Language != "sql" {
					t.Fatalf("wrong scope or lost dirty tool metadata: %+v", binding)
				}
			})
		}
	}
}
