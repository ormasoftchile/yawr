package parser

import (
	"context"
	"reflect"
	"regexp/syntax"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestHighlightRegexRE2(t *testing.T) {
	for _, text := range []string{
		`^ab+$`, `[A-Za-z0-9_-]{0,62}`, `[[:alpha:][:^digit:]]`,
		`(?P<label>\p{Greek}+)|(?<name>\P{L}\d\w\s)`,
		`(?imsU-i:ab.*?)(?:c+?)(d{2,4}?)(?i)`,
		`\A\b\B\z\D\S\W\pL`, `\Q(a[🚀]+)\E\.\*\x41\x{1F680}\077`,
		`[]a][-a][a-][^]a]`, "🚀+a",
	} {
		t.Run(text, func(t *testing.T) {
			if _, err := syntax.Parse(text, syntax.Perl); err != nil {
				t.Fatal("fixture is not Go regexp", err)
			}
			tokens := HighlightRegex(context.Background(), text)
			if len(tokens) == 0 {
				t.Fatal("no tokens")
			}
			last := 0
			for _, token := range tokens {
				if token.Start != last || token.End <= token.Start || token.End > len(text) || !utf8.ValidString(text[token.Start:token.End]) {
					t.Fatalf("unsafe/incomplete coverage: %+v", tokens)
				}
				last = token.End
			}
			if last != len(text) {
				t.Fatal("missing suffix")
			}
		})
	}
	want := []HighlightToken{{0, 1, "keyword"}, {1, 3, "string"}, {3, 4, "operator"}, {4, 5, "keyword"}}
	if got := HighlightRegex(context.Background(), "^ab+$"); !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
	for _, text := range []string{`(?=x)`, `(?!x)`, `(?<=x)`, `\1`, `\k<name>`, `\p{NotAProperty}`, `(?P<`, `\x`, `\`} {
		if got := HighlightRegex(context.Background(), text); len(got) != 1 || got[0].Class != "string" || got[0].End != len(text) {
			t.Fatalf("unsupported prefix represented as supported: %q %+v", text, got)
		}
	}
}

func TestHighlightRegexExactArgumentRole(t *testing.T) {
	for _, text := range []string{
		`regex.match(vars.x, "^ab+$")`,
		`list.order(vars.items, 'regex.match(left, "^ab+$")')`,
	} {
		got := Highlight(context.Background(), text)
		found := false
		for _, token := range got {
			if token.Class == "keyword" && text[token.Start:token.End] == "^" {
				found = true
			}
		}
		if !found {
			t.Fatal("missing nested regex", text, got)
		}
	}
	for _, text := range []string{
		`regex.match("^ab+$", vars.pattern)`, `regex.match(vars.x, ("^ab+$"))`,
		`regex.match(vars.x, "^ab+$" + "")`, `obj.regex.match(vars.x, "^ab+$")`,
		`obj?.regex.match(vars.x, "^ab+$")`, `"^ab+$"`,
	} {
		for _, token := range Highlight(context.Background(), text) {
			if token.Class == "keyword" && text[token.Start:token.End] == "^" {
				t.Fatal("regex guessed from data", text)
			}
		}
	}
	text := `regex.match(vars.x, "^${vars.no}\\\\d+🚀$")`
	for _, token := range Highlight(context.Background(), text) {
		if token.Class == "interpolation" || token.Class == "property" && text[token.Start:token.End] == "no" {
			t.Fatal("GXL literal acquired nested GIS", token)
		}
	}
	if _, err := Parse(`regex.match(vars.x, "^ab+$")`); err != nil {
		t.Fatal("regex role entered comparator validation", err)
	}
}

func TestHighlightRegexBounds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := HighlightRegex(ctx, "^ab+$"); len(got) != 0 {
		t.Fatal(got)
	}
	budget := 2
	if got := highlightRegex(context.Background(), "^ab+$", &budget); len(got) != 0 {
		t.Fatal("budget returned partial nested result", got)
	}
	if got := HighlightRegex(context.Background(), strings.Repeat("x", 32768*3+1)); len(got) != 0 {
		t.Fatal("overlong input accepted")
	}
}
