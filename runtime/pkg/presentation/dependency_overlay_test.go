package presentation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIncludedDocumentPresentationRefusesInvalidChildOverlay(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(filepath.Dir(filepath.Dir(cwd)), "examples", "dependency-scopes")
	path := filepath.Join(root, "catalog", "right.runbook.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	source := strings.ReplaceAll(string(data), "\r\n", "\n")
	for _, name := range []string{"missing-package", "parent-alias"} {
		t.Run(name, func(t *testing.T) {
			dirty := strings.Replace(source, "tools/right", "tools/missing", 1)
			if name == "parent-alias" {
				dirty = strings.Replace(source, "toolRefs:\n  - {name: query, package: scope.right}\n", "", 1)
			}
			if dirty == source {
				t.Fatal("test did not change the child dependency declaration")
			}
			reply := Resolve(Request{
				SchemaVersion: SchemaVersion, RequestID: "invalid-child",
				Context:  Context{ProjectRoot: root, EntrypointPath: filepath.Join(root, "static-and-lazy.runbook.yaml")},
				Document: Buffer{Path: path, URI: FileURI(path), Version: 2, Text: dirty},
			})
			if reply.Status != "unavailable" || reply.Reason != "missing-dependency" {
				t.Fatalf("invalid child dependency appeared resolved: %+v", reply)
			}
			for _, binding := range reply.Bindings {
				if binding.Status == "resolved" {
					t.Fatalf("child inherited a parent binding: %+v", binding)
				}
			}
		})
	}
}
