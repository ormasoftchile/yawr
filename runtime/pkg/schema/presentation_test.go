package schema

import (
	"encoding/json"
	"gopkg.in/yaml.v3"
	"strings"
	"testing"
)

func TestPresentationDeclaration(t *testing.T) {
	for _, source := range []string{
		"type: secret\npresentation: {version: 1, kind: code, language: sql}",
		"type: object\npresentation: {version: 1, kind: code, language: sql}",
		"type: string\npresentation: {version: 1, kind: code, language: sql, extra: true}",
		"type: string\npresentation: {version: 1, kind: code, language: SQL}",
		"type: string\npresentation: null",
		"type: string\npresentation: {version: '1', kind: code, language: sql}",
	} {
		var a ArgDef
		if yaml.Unmarshal([]byte(source), &a) == nil {
			t.Fatalf("accepted %s", source)
		}
	}
	for _, language := range []string{"sql", "kql", "powershell", "future"} {
		var a ArgDef
		if err := yaml.Unmarshal([]byte("type: string\npresentation: {version: 1, kind: code, language: "+language+"}"), &a); err != nil {
			t.Fatal(err)
		}
		def := &ToolDef{Actions: map[string]*ToolAction{"run": {Args: map[string]*ArgDef{"text": &a}}}}
		clone := CloneToolDef(def)
		clone.Actions["run"].Args["text"].Presentation.Language = "changed"
		if a.Presentation.Language != language {
			t.Fatal("clone aliases metadata")
		}
	}
	var p PresentationDescriptor
	if json.Unmarshal([]byte(`{"version":1,"version":2,"kind":"code","language":"sql"}`), &p) == nil {
		t.Fatal("duplicate version accepted")
	}
	var a ArgDef
	yaml.Unmarshal([]byte("type: string"), &a)
	data, _ := json.Marshal(a)
	if strings.Contains(string(data), "presentation") {
		t.Fatal("absent presentation metadata was serialized")
	}
}
