package parser

import (
	"context"
	"reflect"
	"testing"
)

func TestHighlightUsesLexerPrefixAndRegistry(t *testing.T) {
	text := `str.contains(vars.x, "🚀") and len(vars.y) > 2 # GXL`
	got := Highlight(context.Background(), text)
	want := []string{"namespace", "delimiter", "function", "delimiter", "variable", "delimiter", "property", "delimiter", "string", "delimiter", "operator", "function", "delimiter", "variable", "delimiter", "property", "delimiter", "operator", "number", "comment"}
	classes := []string{}
	for _, token := range got {
		classes = append(classes, token.Class)
	}
	if !reflect.DeepEqual(classes, want) {
		t.Fatal(classes)
	}
	for _, text := range []string{`vars.x + "unterminated`, `vars.x + @`, `vars.x + 01`} {
		if _, err := Parse(text); err == nil {
			t.Fatal("strict Parse changed", text)
		}
		if got := Highlight(context.Background(), text); len(got) != 4 || got[3].Class != "operator" {
			t.Fatal(text, got)
		}
	}
	if got := Highlight(context.Background(), `vars.str.contains(x)`); got[2].Class != "property" || got[4].Class != "property" {
		t.Fatal(got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := Highlight(ctx, text); len(got) != 0 {
		t.Fatal(got)
	}
}
