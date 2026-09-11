package parser

import (
	"context"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func tokenTexts(t *testing.T, src string, tokens []HighlightToken, class string) []string {
	t.Helper()
	var out []string
	last := 0
	for _, token := range tokens {
		if token.Start < last || token.End <= token.Start || token.End > len(src) ||
			!utf8.ValidString(src[token.Start:token.End]) {
			t.Fatalf("invalid token in %q: %+v", src, token)
		}
		last = token.End
		if token.Class == class {
			out = append(out, src[token.Start:token.End])
		}
	}
	return out
}

func TestHighlightExpressionArgumentRoles(t *testing.T) {
	for _, test := range []struct {
		src       string
		functions []string
	}{
		{`list.order(items, 'date.compare(left.a, right.a) < 0')`, []string{"order", "compare"}},
		{`list.order(list.contains(items, 'x,y'), 'date.compare(left.a, right.a) < 0')`, []string{"order", "contains", "compare"}},
		{`list.order(items[0], 'date.compare(left.a, right.a) < 0')`, []string{"order", "compare"}},
		{`list.order(items?.[0], 'date.compare(left.a, right.a) < 0')`, []string{"order", "compare"}},
		{`list.order((items), # comment
 'date.compare(left.a, right.a) < 0')`, []string{"order", "compare"}},
		{`list.order("date.compare(left.a, right.a)", comparator)`, []string{"order"}},
		{`list.order(items, ('date.compare(left.a, right.a) < 0'))`, []string{"order"}},
		{`list.order(items, 'date.compare(left.a, right.a)' + suffix)`, []string{"order"}},
		{`list.order(items, str.trim('date.compare(left.a, right.a)'))`, []string{"order", "trim"}},
		{`list.order(items, comparator, 'date.compare(left.a, right.a)')`, []string{"order"}},
		{`str.contains(text, 'date.compare(left.a, right.a)')`, []string{"contains"}},
		{`"date.compare(left.a, right.a)" == text`, nil},
		{`list.filter(items, 'date.compare(left.a, right.a)')`, nil},
		{`list.map(items, 'date.compare(left.a, right.a)')`, nil},
		{`custom.order(items, 'date.compare(left.a, right.a)')`, nil},
		{`obj.list.order(items, 'date.compare(left.a, right.a)')`, nil},
		{`obj?.list.order(items, 'date.compare(left.a, right.a)')`, nil},
		{`obj[0].list.order(items, 'date.compare(left.a, right.a)')`, nil},
		{`obj.list.order(items, 'list.order(items, "left.a")')`, nil},
		{`list?.order(items, 'date.compare(left.a, right.a)')`, nil},
		{`obj list.order(items, 'date.compare(left.a, right.a)')`, nil},
		{`list.order(items, 'date.compare(left.a, right.a)'`, []string{"order"}},
		{`list.order(items[0), 'date.compare(left.a, right.a)')`, []string{"order"}},
	} {
		t.Run(test.src, func(t *testing.T) {
			got := tokenTexts(t, test.src, Highlight(context.Background(), test.src), "function")
			if !reflect.DeepEqual(got, test.functions) {
				t.Fatalf("functions %v, want %v", got, test.functions)
			}
		})
	}
}

func TestHighlightLiteralDecodingAuthority(t *testing.T) {
	src := `list.order(items, 'date.\u0063ompare(left.a, right.a) < \u0030 and str.contains(left.name, "🚀,\\\"\\\\\U0001F680")')`
	got := Highlight(context.Background(), src)
	for class, want := range map[string][]string{
		"namespace": {"list", "date", "str"},
		"function":  {"order", `\u0063ompare`, "contains"},
		"number":    {`\u0030`},
		"operator":  {"<", "and"},
	} {
		if texts := tokenTexts(t, src, got, class); !reflect.DeepEqual(texts, want) {
			t.Fatalf("%s: %v, want %v", class, texts, want)
		}
	}
	stringsFound := tokenTexts(t, src, got, "string")
	if !strings.Contains(strings.Join(stringsFound, ""), `"🚀,\\\"\\\\\U0001F680"`) ||
		stringsFound[0] != "'" || stringsFound[len(stringsFound)-1] != "'" {
		t.Fatal("lost quotes or escape spelling", stringsFound)
	}
	for _, src := range []string{
		`list.order(items, 'left.a < 0\nand right.a > 2')`,
		`list.order(items, 'left.a < 0\r\nand right.a > 2')`,
		`list.order(items, 'left.a < 0\tand right.a > 2')`,
		`list.order(items, "str.contains(left.name, \"a,\\\"b\") and left.a < 0 or right.a > 2")`,
		`list.order(items, 'str.contains(left.name, \'a,\\\'b\') and left.a < 0 or right.a > 2')`,
	} {
		if got := tokenTexts(t, src, Highlight(context.Background(), src), "number"); !reflect.DeepEqual(got, []string{"0", "2"}) {
			t.Fatal(got)
		}
	}
	for _, src := range []string{
		`list.order(items, 'left.a < 0 @ right.a')`,
		`list.order(items, 'left.a < 0 "unfinished')`,
	} {
		got := Highlight(context.Background(), src)
		if numbers := tokenTexts(t, src, got, "number"); !reflect.DeepEqual(numbers, []string{"0"}) {
			t.Fatal(numbers)
		}
		if got[len(got)-2].Class != "string" || !strings.HasSuffix(src[got[len(got)-2].Start:got[len(got)-2].End], "'") {
			t.Fatal("unscanned suffix not preserved", got)
		}
	}
	// GXL rejects surrogate escapes rather than pairing them. Highlighting must
	// not introduce a second decoder that accepts them or changes strict parsing.
	for _, src := range []string{`list.order(items, '\uD83D\uDE80')`, `list.order(items, '\q')`} {
		if _, err := Parse(src); err == nil {
			t.Fatal("strict parse accepted invalid escape")
		}
		if strings := tokenTexts(t, src, Highlight(context.Background(), src), "string"); len(strings) != 0 {
			t.Fatal(strings)
		}
	}
}

func TestHighlightNestedLiteralsAndBounds(t *testing.T) {
	src := `list.order(items, 'list.order(left.items, "date.compare(left.a, right.a) < 0")')`
	if got := tokenTexts(t, src, Highlight(context.Background(), src), "function"); !reflect.DeepEqual(got, []string{"order", "order", "compare"}) {
		t.Fatal(got)
	}
	if _, err := Parse(src); err == nil {
		t.Fatal("highlighting must not permit runtime nested ordering")
	}
	budget := 65536
	if got := highlight(context.Background(), src, 17, &budget); len(got) != 0 || budget != 65536 {
		t.Fatal("literal depth cap")
	}
	budget = 1
	if got := highlight(context.Background(), src, 1, &budget); len(got) != 0 {
		t.Fatal("nested scan budget")
	}
	budget = 20
	got := highlight(context.Background(), src, 0, &budget)
	tokenTexts(t, src, got, "string")
	if functions := tokenTexts(t, src, got, "function"); !reflect.DeepEqual(functions, []string{"order", "order"}) {
		t.Fatal("nested budget must preserve lexical strings", functions)
	}
	deep := strings.Repeat("(", 129) + src + strings.Repeat(")", 129)
	if got := tokenTexts(t, deep, Highlight(context.Background(), deep), "function"); !reflect.DeepEqual(got, []string{"order"}) {
		t.Fatal("delimiter depth cap", got)
	}
	large := "list.order(items, " + strconv.Quote(strings.Repeat("left.a < 0 or ", 5000)+"false") + ")"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := Highlight(ctx, large); len(got) != 0 {
		t.Fatal("cancellation")
	}
	duringScan := &highlightCancellationContext{Context: context.Background(), remaining: 1000}
	if got := Highlight(duringScan, large); len(got) != 0 || duringScan.remaining > 0 {
		t.Fatal("cancellation during literal scanning")
	}
}

type highlightCancellationContext struct {
	context.Context
	remaining int
}

func (c *highlightCancellationContext) Err() error {
	c.remaining--
	if c.remaining <= 0 {
		return context.Canceled
	}
	return nil
}
