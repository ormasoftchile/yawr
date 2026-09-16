package pkgcatalog

import (
	"path/filepath"
	"strings"
	"testing"

	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

func TestClosureDefinitionCanonicalIdentityAcrossAliasesAndSourceEdits(t *testing.T) {
	ws := t.TempDir()
	options := closureOptions(t, ws)
	source := filepath.Join(ws, "local", "kubectl.tool.yaml")
	writeFile(t, source, kubectlToolYAML)
	writeFile(t, options.Entrypoint, closureRunbook(`toolRefs:
  - {name: first, path: local/kubectl.tool.yaml}
  - {name: second, path: local/kubectl.tool.yaml}
flow:
  - step: {id: one, type: tool, tool: {name: first, action: drain, args: {node: fixture}}}
  - step: {id: two, type: tool, tool: {name: second, action: drain, args: {node: fixture}}}
`))
	closure := requireClosure(t, options)
	document := closureDocument(t, closure, options.Entrypoint)
	first, err := closure.Definition(document.ID, "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := closure.Definition(document.ID, "second")
	if err != nil {
		t.Fatal(err)
	}
	firstID, err := toolpkg.DefinitionID(first)
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := toolpkg.DefinitionID(second)
	if err != nil || firstID != secondID || first.Runtime.Name != "kubectl" || second.Runtime.Name != "kubectl" {
		t.Fatalf("local aliases changed canonical tool identity: %s %s %v", firstID, secondID, err)
	}
	first.Runtime.Actions["drain"].Argv[0] = "mutated"
	first.Declaration.Name = "mutated"
	writeFile(t, source, strings.Replace(kubectlToolYAML, "command: kubectl", "command: edited-after-freeze", 1))
	again, err := closure.Definition(document.ID, "first")
	if err != nil {
		t.Fatal(err)
	}
	againID, err := toolpkg.DefinitionID(again)
	if err != nil || againID != secondID {
		t.Fatalf("returned value or filesystem changed captured meaning: %s %s %v", againID, secondID, err)
	}
}
