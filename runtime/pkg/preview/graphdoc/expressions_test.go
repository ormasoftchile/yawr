package graphdoc

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/presentation"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestExpressionDetailDestinations(t *testing.T) {
	const s = "Hi ${name}!"
	cases := []struct {
		step  schema.Step
		paths []string
	}{
		{schema.Step{Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: s, Args: []string{s}, Run: map[string]string{"windows": s}, Workdir: s, Shell: s, Stdin: s, Env: map[string]string{"PLAIN": s}}}, []string{"/command", "/args/0", "/script/windows", "/workdir", "/shell"}},
		{schema.Step{Type: schema.StepTypeTool, ToolCall: &schema.ToolCallSpec{Tool: schema.ToolInvocation{Name: s, Action: s, Args: map[string]any{"data": map[string]any{"a/b~c": []any{s}}}}}}, []string{"/tool", "/action", "/arguments/0/value/a~1b~0c/0"}},
		{schema.Step{Type: schema.StepTypeInclude, IncludeSpec: &schema.IncludeSpec{Include: schema.IncludeConfig{RunbookRef: s, With: map[string]string{"x": s}, When: "other"}}, Capture: map[string]string{"x": s}}, []string{"/common/captures/0/source", "/bindings/0/value", "/reference"}},
		{schema.Step{Type: schema.StepTypeChoice, ChoiceSpec: &schema.ChoiceSpec{Prompt: s, Default: s, Options: []schema.ChoiceOption{{Label: s, Hint: s, Value: s}}}}, []string{"/prompt", "/default", "/options/0/label", "/options/0/hint"}},
		{schema.Step{Type: schema.StepTypeDecision, DecisionSpec: &schema.DecisionSpec{Prompt: s, Routes: []schema.DecisionRoute{{Label: s, Hint: s, Runbook: s}}}}, []string{"/prompt", "/routes/0/label", "/routes/0/hint"}},
		{schema.Step{Type: schema.StepTypeCollector, CollectorSpec: &schema.CollectorSpec{Prompt: s, Fields: []schema.CollectorField{{Name: "x", Label: s, Hint: s, When: "x", Default: s, Options: []schema.ChoiceOption{{Label: s, Hint: s, Value: s}}}}}}, []string{"/prompt", "/fields/0/label", "/fields/0/hint", "/fields/0/when", "/fields/0/default", "/fields/0/options/0/label", "/fields/0/options/0/hint"}},
		{schema.Step{Type: schema.StepTypeHostAction, HostActionSpec: &schema.HostActionSpec{HostAction: schema.HostActionConfig{Request: map[string]any{"x": []any{s}}}}}, []string{"/request/0/value/0"}},
		{schema.Step{Type: schema.StepTypeBranch, BranchSpec: &schema.BranchSpec{Branches: []schema.BranchArm{{Condition: "count >= 2", Label: s}}}}, []string{"/arms/0/condition"}},
		{schema.Step{Type: schema.StepTypeAssert, AssertSpec: &schema.AssertSpec{Assert: []schema.Assertion{{Subject: s, Expected: s, Path: s}}}}, []string{"/assertions/0/subject", "/assertions/0/expected"}},
		{schema.Step{Type: schema.StepTypeDisplay, DisplaySpec: &schema.DisplaySpec{Display: schema.DisplayConfig{Content: s}}}, []string{"/content"}},
		{schema.Step{Type: schema.StepTypeWaitForEvent, WaitForEventSpec: &schema.WaitForEventSpec{Event: schema.WaitEventConfig{ID: s, Filter: map[string]string{"x": s}}}}, []string{"/event_id", "/filter/0/value"}},
		{schema.Step{Type: schema.StepTypeNoop, Capture: map[string]string{"x": s}}, []string{"/common/captures/0/source"}},
	}
	for _, c := range cases {
		t.Run(string(c.step.Type), func(t *testing.T) {
			c.step.When = "count >= 2"
			c.step.Subtitle = s
			d := detailsForStep(&c.step)
			if d.ExpressionPresentation == nil {
				t.Fatal("missing projection")
			}
			want := append([]string{"/common/when"}, c.paths...)
			got := []string{}
			encoded, _ := json.Marshal(d)
			var decoded any
			json.Unmarshal(encoded, &decoded)
			for _, v := range d.ExpressionPresentation.Values {
				got = append(got, v.Path)
				text, ok := expressionDetailText(decoded, v.Path)
				if !ok || v.TextDigest != presentation.Digest([]byte(text)) || len(v.Tokens) == 0 {
					t.Fatal(v)
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatal(got, want)
			}
			wire, _ := json.Marshal(d.ExpressionPresentation)
			if strings.Contains(string(wire), s) || strings.Contains(string(wire), "count") {
				t.Fatal("duplicated authored text")
			}
		})
	}
	it := DetailsForResolvedStep(engine.ResolvedStep{Kind: "iterate", Spec: &schema.IterateNode{Over: s, Until: "done", Collect: map[string]string{"x": s}}})
	if it.ExpressionPresentation == nil || len(it.ExpressionPresentation.Values) != 3 {
		t.Fatal(it)
	}
}

func TestExpressionProtectionAndLaterInvalidation(t *testing.T) {
	step := &schema.Step{Type: schema.StepTypeTool, ToolCall: &schema.ToolCallSpec{Tool: schema.ToolInvocation{Name: "db", Action: "query", Args: map[string]any{
		"safe": "SELECT '${x}'", "token": "${secret}", "opaque": "${hidden}",
		"nested": map[string]any{"password": "${hidden}", "code": "${ok}"},
	}}}}
	d := detailsForStep(step)
	d.SetCodePresentation(&presentation.Envelope{Arguments: []presentation.Field{{Name: "opaque", ValueType: "secret"}}})
	for _, v := range d.ExpressionPresentation.Values {
		if v.TextDigest == presentation.Digest([]byte("${secret}")) || v.TextDigest == presentation.Digest([]byte("${hidden}")) {
			t.Fatal("secret fingerprint", v)
		}
	}
	for i := range d.Arguments {
		if d.Arguments[i].Name == "safe" {
			d.Arguments[i].Value = "<redacted>"
		}
	}
	doc := &Document{Nodes: []Node{{ID: "query", Details: d}}}
	safe := doc.ForExpressionRendering()
	if len(safe.Nodes[0].Details.ExpressionPresentation.Values) != 1 {
		t.Fatal(safe.Nodes[0].Details.ExpressionPresentation)
	}
	if len(d.ExpressionPresentation.Values) != 2 {
		t.Fatal("render mutated bound document")
	}
	d.PruneExpressionPresentation()
	if len(d.ExpressionPresentation.Values) != 1 {
		t.Fatal(d.ExpressionPresentation)
	}
	plain := detailsForStep(&schema.Step{Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Args: []string{"--token", "${private}", "--x", "${public}"}, Run: "password=${private}\necho ${public}"}})
	if plain.ExpressionPresentation == nil || len(plain.ExpressionPresentation.Values) != 1 || plain.ExpressionPresentation.Values[0].Path != "/args/3" {
		t.Fatal(plain.ExpressionPresentation)
	}
}

func TestExpressionMetadataHasNoHashException(t *testing.T) {
	d := detailsForStep(&schema.Step{Type: schema.StepTypeNoop, When: "count >= 2"})
	doc := &Document{Nodes: []Node{{ID: "x", Details: d}}}
	before, _ := doc.ContentHash()
	d.ExpressionPresentation.Values[0].Tokens[0].Class = "function"
	after, _ := doc.ContentHash()
	if before == after {
		t.Fatal("tokens excluded from hash")
	}
	d.ExpressionPresentation.GrammarVersion = "future"
	future, _ := doc.ContentHash()
	if future == after {
		t.Fatal("grammar excluded from hash")
	}
	d.ExpressionPresentation = nil
	plain, _ := doc.ContentHash()
	if plain == future || plain == before {
		t.Fatal("metadata excluded from hash")
	}
	d.ExpressionPresentation = &presentation.ExpressionPresentation{Version: 999, GrammarVersion: "unknown", Values: []presentation.ExpressionDetailValue{}}
	unknown, _ := doc.ContentHash()
	if unknown == plain {
		t.Fatal("unknown optional metadata excluded from hash")
	}
}

func TestExpressionDetailBudgetsAndLegacyBytes(t *testing.T) {
	plain := detailsForStep(&schema.Step{Type: schema.StepTypeNoop, Capture: map[string]string{"x": "literal"}})
	data, _ := json.Marshal(plain)
	if strings.Contains(string(data), "expression_presentation") {
		t.Fatal(string(data))
	}
	d := detailsForStep(&schema.Step{Type: schema.StepTypeDisplay, DisplaySpec: &schema.DisplaySpec{Display: schema.DisplayConfig{Content: strings.Repeat("x", presentation.MaxExpressionValueUnits+1)}}})
	if d.ExpressionPresentation != nil {
		t.Fatal("oversize scalar")
	}
	args := map[string]any{}
	for i := 0; i < 5; i++ {
		args[string(rune('a'+i))] = "${" + strings.Repeat("+ ", 16000) + "}"
	}
	d = detailsForStep(&schema.Step{Type: schema.StepTypeTool, ToolCall: &schema.ToolCallSpec{Tool: schema.ToolInvocation{Args: args}}})
	if d.ExpressionPresentation != nil {
		t.Fatal("aggregate token budget")
	}
	d = detailsForStep(&schema.Step{Type: schema.StepTypeTool, Capture: map[string]string{"x": "${notGIS}"}, ToolCall: &schema.ToolCallSpec{Tool: schema.ToolInvocation{Args: map[string]any{"x": "literal"}}}})
	if d.ExpressionPresentation != nil {
		t.Fatal("ordinary GCP capture or literal text highlighted")
	}
}

func TestExpressionInvalidOptionalMetadataFallsBack(t *testing.T) {
	for _, kind := range []string{"version", "grammar", "class", "digest", "span"} {
		d := detailsForStep(&schema.Step{Type: schema.StepTypeNoop, When: "count >= 2"})
		switch kind {
		case "version":
			d.ExpressionPresentation.Version = 2
		case "grammar":
			d.ExpressionPresentation.GrammarVersion = "future"
		case "class":
			d.ExpressionPresentation.Values[0].Tokens[0].Class = "future"
		case "digest":
			d.ExpressionPresentation.Values[0].TextDigest = "sha256:invalid"
		case "span":
			d.ExpressionPresentation.Values[0].Tokens[0].End = 999
		}
		d.PruneExpressionPresentation()
		if d.ExpressionPresentation != nil || d.Common.When != "count >= 2" {
			t.Fatal(kind, d)
		}
	}
}

func TestExpressionAuthoredRedactedKeyIsNotProtectionMetadata(t *testing.T) {
	d := detailsForStep(&schema.Step{Type: schema.StepTypeTool, ToolCall: &schema.ToolCallSpec{Tool: schema.ToolInvocation{Args: map[string]any{"data": map[string]any{"redacted": true, "code": "${x}"}}}}})
	d.PruneExpressionPresentation()
	if d.ExpressionPresentation == nil || len(d.ExpressionPresentation.Values) != 1 || d.ExpressionPresentation.Values[0].Path != "/arguments/0/value/code" {
		t.Fatal(d.ExpressionPresentation)
	}
	d.Arguments[0].Redacted = true
	d.PruneExpressionPresentation()
	if d.ExpressionPresentation != nil {
		t.Fatal("actual redacted entry retained metadata")
	}
}
