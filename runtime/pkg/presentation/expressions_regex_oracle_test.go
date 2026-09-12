package presentation

import (
	"context"
	"encoding/json"
	"reflect"
	"regexp/syntax"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestExpressionRegexUTCAndBoundaries(t *testing.T) {
	const utc = `^(?:[0-9]{4}-(?:(?:0[13578]|1[02])-(?:0[1-9]|[12][0-9]|3[01])|(?:0[469]|11)-(?:0[1-9]|[12][0-9]|30)|02-(?:0[1-9]|1[0-9]|2[0-8]))|(?:[0-9]{2}(?:0[48]|[2468][048]|[13579][26])|(?:[02468][048]|[13579][26])00)-02-29) (?:[01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9]$`
	if _, err := syntax.Parse(utc, syntax.Perl); err != nil {
		t.Fatal(err)
	}
	text := "flow:\n  - step:\n      type: assert\n      assert:\n        - {type: matches, subject: '${start}', expected: &utc '" + utc + "'}\n        - {type: matches, subject: '${end}', expected: *utc}\n"
	got := ResolveExpressions(context.Background(), expressionRequest(t, text))
	if got.Status != "resolved" {
		t.Fatal(got)
	}
	regions := 0
	for _, region := range got.Regions {
		if region.Mode == ExpressionRegex {
			regions++
			scalar := text[region.Range.Start:region.Range.End]
			if scalar != "'"+utc+"'" || len(region.Tokens) != 224 {
				t.Fatal("UTC mapping/token regression", region)
			}
		}
	}
	if regions != 1 {
		t.Fatal("UTC alias admission regression")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := HighlightExpression(ctx, utc, ExpressionRegex, false); ok {
		t.Fatal("cancelled regex accepted")
	}
	if _, ok := HighlightExpression(context.Background(), strings.Repeat("x", MaxExpressionValueUnits+1), ExpressionRegex, false); ok {
		t.Fatal("oversized regex accepted")
	}
}

func TestExpressionRegexNestedEscapedLiteralMapping(t *testing.T) {
	text := `${list.order(items, 'regex.match(left.name, "^\\\\d+🚀$")')}`
	value, ok := HighlightExpression(context.Background(), text, ExpressionGIS, false)
	if !ok {
		t.Fatal("nested regex rejected")
	}
	units := utf16.Encode([]rune(text))
	escape, astral := false, false
	for _, token := range value.Tokens {
		part := string(utf16.Decode(units[token.Start:token.End]))
		if part == `\\\\d` && token.Class == "keyword" {
			escape = true
		}
		if part == "🚀" && token.Class == "string" && token.End-token.Start == 2 {
			astral = true
		}
	}
	if !escape || !astral {
		t.Fatal("nested source mappings failed", value)
	}
	quoted, _ := json.Marshal(text)
	source := "flow:\n  - step:\n      type: assert\n      assert: [{type: eq, subject: " + string(quoted) + "}]\n"
	reply := ResolveExpressions(context.Background(), expressionRequest(t, source))
	if reply.Status != "resolved" || len(reply.Regions) != 1 || !reflect.DeepEqual(reply.Regions[0].ExpressionValue, value) {
		t.Fatal("YAML changed nested decoded offsets", reply)
	}
}
