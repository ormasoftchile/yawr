package schema

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestCaptureIncludeAliasesUsesOnlyDeclaringDocument(t *testing.T) {
	childStep := &Step{ID: "include", Type: StepTypeInclude, IncludeSpec: &IncludeSpec{Include: IncludeConfig{Runbook: "leaf.yaml"}}}
	rootStep := &Step{ID: "include", Type: StepTypeInclude, IncludeSpec: &IncludeSpec{
		Include: IncludeConfig{Runbook: "child.yaml"}, ResolvedSteps: []FlowNode{{Step: childStep}},
	}}
	parent := &Runbook{Imports: map[string]string{"parent-label": "child.yaml", "wrong-child-label": "leaf.yaml"},
		Flow: []FlowNode{{Iterate: &IterateNode{Steps: []FlowNode{{Step: rootStep}}}}}}
	child := &Runbook{Imports: map[string]string{"child-label": "leaf.yaml"}, Flow: rootStep.IncludeSpec.ResolvedSteps}
	CaptureIncludeAliases(parent)
	if rootStep.IncludeAlias != "parent-label" || childStep.IncludeAlias != "" {
		t.Fatal("parent imports leaked across include boundary")
	}
	CaptureIncludeAliases(child)
	if childStep.IncludeAlias != "child-label" {
		t.Fatal("child label was not captured")
	}
	child.Imports = map[string]string{"changed": "leaf.yaml"}
	CaptureIncludeAliases(child)
	if childStep.IncludeAlias != "child-label" {
		t.Fatal("captured alias was rebound")
	}
	body, err := json.Marshal(childStep)
	if err != nil {
		t.Fatal(err)
	}
	var restored Step
	if err := json.Unmarshal(body, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.IncludeAlias != "child-label" {
		t.Fatal("durable common-step metadata lost alias")
	}
	field, ok := reflect.TypeOf(Step{}).FieldByName("IncludeAlias")
	if !ok || field.Tag.Get("yaml") != "-" {
		t.Fatal("runtime metadata became an authored override")
	}
}

func TestLocalIncludeAliasDoesNotGuessAmbiguousLabels(t *testing.T) {
	imports := map[string]string{"left": "leaf.yaml", "right": "./leaf.yaml"}
	if got := LocalIncludeAlias(imports, "right"); got != "right" {
		t.Fatalf("authored alias = %q", got)
	}
	if got := LocalIncludeAlias(imports, "leaf.yaml"); got != "" {
		t.Fatalf("ambiguous reverse lookup guessed %q", got)
	}
	if got := LocalIncludeAlias(map[string]string{"check": `nested\leaf.yaml`}, "nested/leaf.yaml"); got != "check" {
		t.Fatalf("source-local path spelling = %q", got)
	}
}
