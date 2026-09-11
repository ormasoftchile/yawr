package parser

import (
	"context"
	"reflect"
	"testing"
)

func TestHighlightUsesLexerPrefixAndRegistry(t *testing.T) {
	source := `str.contains(x, "🚀") and len(y) > 2 # GXL`
	got := Highlight(context.Background(), source)
	want := []string{"namespace", "delimiter", "function", "delimiter", "variable", "delimiter", "string", "delimiter", "operator", "function", "delimiter", "variable", "delimiter", "operator", "number", "comment"}
	classes := []string{}
	for _, token := range got {
		classes = append(classes, token.Class)
	}
	if !reflect.DeepEqual(classes, want) {
		t.Fatal(classes)
	}
	for _, text := range []string{`x + "unterminated`, `x + @`, `x + 01`} {
		if _, err := Parse(text); err == nil {
			t.Fatal("strict Parse changed", text)
		}
		if got := Highlight(context.Background(), text); len(got) < 2 || got[len(got)-1].Class != "operator" {
			t.Fatal(text, got)
		}
	}
	if got := Highlight(context.Background(), `str.contains(x)`); got[2].Class != "function" || got[4].Class != "variable" {
		t.Fatal(got)
	}
}

func TestParseRejectsObsoleteVarsNamespace(t *testing.T) {
	if _, err := Parse(`vars.status == "ok"`); err == nil {
		t.Fatal("obsolete vars namespace accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := Highlight(ctx, `status == "ok"`); len(got) != 0 {
		t.Fatal(got)
	}
}
