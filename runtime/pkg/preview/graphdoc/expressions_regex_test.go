package graphdoc

import (
	"encoding/json"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/presentation"
)

func TestExpressionRegexAuthoredDetails(t *testing.T) {
	d := &StepDetails{Kind: "assert", Assertions: []AssertionDetails{
		{Type: "matches", Subject: "${vars.name}", Expected: "^ab+$"},
		{Type: "eq", Subject: `${regex.match(vars.name, "^ab+$")}`, Expected: "^ab+$"},
		{Type: "matches", Expected: "<redacted>"},
	}}
	d.ProjectExpressions()
	before, _ := json.Marshal(d.ExpressionPresentation)
	d.PruneExpressionPresentation()
	after, _ := json.Marshal(d.ExpressionPresentation)
	if string(before) != string(after) {
		t.Fatal("pruned valid expression metadata", string(before), string(after))
	}
	if d.ExpressionPresentation.GrammarVersion != presentation.ExpressionGrammarVersion {
		t.Fatal("unexpected expression grammar")
	}
	regex := 0
	for _, value := range d.ExpressionPresentation.Values {
		if value.Mode == presentation.ExpressionRegex {
			regex++
			if value.Path != "/assertions/0/expected" {
				t.Fatal("invented semantic regex site")
			}
		}
	}
	if regex != 1 {
		t.Fatal("missing semantic regex expected")
	}
}

func TestExpressionRegexDetailsRejectTampering(t *testing.T) {
	for _, mutation := range []string{"grammar", "type", "kind", "path", "tokens"} {
		d := &StepDetails{Kind: "assert", Assertions: []AssertionDetails{{Type: "matches", Expected: "^ab+$"}}}
		d.projectExpressions()
		switch mutation {
		case "grammar":
			d.ExpressionPresentation.GrammarVersion = "yawr-expression/v0"
		case "type":
			d.Assertions[0].Type = "eq"
		case "kind":
			d.Kind = "noop"
		case "path":
			d.Command = "^ab+$"
			d.ExpressionPresentation.Values[0].Path = "/command"
		case "tokens":
			d.ExpressionPresentation.Values[0].Tokens[0].Class = "string"
		}
		d.PruneExpressionPresentation()
		if d.ExpressionPresentation != nil {
			t.Fatal("accepted tampered", mutation)
		}
	}
}
