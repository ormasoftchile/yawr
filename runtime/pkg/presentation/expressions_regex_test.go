package presentation

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/ormasoftchile/yawr/runtime/pkg/gis"
)

func TestExpressionRegexCanonical(t *testing.T) {
	data, err := os.ReadFile("testdata/regex-values.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Text     string          `json:"text"`
		Expected ExpressionValue `json:"expected"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		got, ok := HighlightExpression(context.Background(), c.Text, c.Expected.Mode, false)
		if !ok || !reflect.DeepEqual(got, c.Expected) {
			t.Fatalf("%s\n got %+v\nwant %+v", c.Text, got, c.Expected)
		}
	}
	caps := ExpressionsCapabilities()
	if caps.GrammarVersion != "yawr-expression/v2" || !reflect.DeepEqual(caps.Modes, []ExpressionMode{ExpressionGXL, ExpressionGIS, ExpressionRegex}) {
		t.Fatal(caps)
	}
}

func TestExpressionRegexSemanticSource(t *testing.T) {
	data, err := os.ReadFile("testdata/regex-source.yaml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	reply := ResolveExpressions(context.Background(), expressionRequest(t, text))
	if reply.Status != "resolved" || reply.GrammarVersion != ExpressionGrammarVersion {
		t.Fatal(reply)
	}
	regex := 0
	for _, r := range reply.Regions {
		if r.YAMLPath == "/flow/0/step/assert/1/expected" {
			t.Fatal("alias expanded into editor region")
		}
		if r.Mode == ExpressionRegex {
			regex++
			scalar := string(utf16.Decode(utf16.Encode([]rune(text))[r.Range.Start:r.Range.End]))
			if scalar != "'^[A-Za-z0-9][A-Za-z0-9-]{0,62}$'" {
				t.Fatal("anchor or YAML syntax in scalar range", scalar)
			}
		}
		if r.YAMLPath == "/flow/0/step/assert/2/subject" {
			found := false
			for _, token := range r.Tokens {
				found = found || token.Class == "keyword"
			}
			if r.Mode != ExpressionGIS || !found {
				t.Fatal("missing builtin nested role", r)
			}
		}
		if strings.HasPrefix(r.YAMLPath, "/flow/0/step/assert/3/") && len(r.Tokens) != 0 {
			t.Fatal("regex-looking eq data colored", r)
		}
	}
	if regex != 1 {
		t.Fatalf("got %d regex regions", regex)
	}
	for _, scalar := range []string{`^ab+$`, `'^ab+$'`, `"^ab+$"`, `&pattern !!str '^ab+$'`, `!!str &pattern '^ab+$'`, "&pattern # scalar property\n            '^ab+$'", "|\n            ^ab+$\n", ">-\n            ^ab+$\n"} {
		source := "flow:\n  - step:\n      type: assert\n      assert:\n        - type: matches\n          expected: " + scalar + "\n"
		got := ResolveExpressions(context.Background(), expressionRequest(t, source))
		if got.Status != "resolved" || len(got.Regions) != 1 || got.Regions[0].Mode != ExpressionRegex || len(got.Regions[0].Tokens) < 4 {
			t.Fatalf("%q: %+v", scalar, got)
		}
	}
	for _, kind := range []string{"eq", "ne", "contains", "exists", ""} {
		source := "flow:\n  - step:\n      type: assert\n      assert:\n        - type: '" + kind + "'\n          expected: '^ab+$'\n"
		got := ResolveExpressions(context.Background(), expressionRequest(t, source))
		if got.Status != "resolved" || len(got.Regions) != 1 || got.Regions[0].Mode != ExpressionGIS || len(got.Regions[0].Tokens) != 0 {
			t.Fatal("nonmatches guessed", kind, got)
		}
	}
}

func TestExpressionRegexGISMapping(t *testing.T) {
	for _, text := range []string{`^\d+${suffix}$`, `^\${literal}$`, `^\\${x}$`, `^\n\t\r\\d+$`, `^ab$${bad}`, "🚀${x}", `^${regex.match(x, "^${notGIS}$")}+$`, `^${x`} {
		value, ok := HighlightExpression(context.Background(), text, ExpressionRegex, false)
		if !ok {
			t.Fatal(text)
		}

		blocks := gis.ScanExpressions(context.Background(), text)
		opens := 0
		units := utf16.Encode([]rune(text))
		last := 0
		for _, token := range value.Tokens {
			if token.Start < last || token.End <= token.Start || token.End > len(units) {
				t.Fatal("bad mapping", text, value)
			}
			for _, pos := range []int{token.Start, token.End} {
				if pos < len(units) && units[pos] >= 0xdc00 && units[pos] <= 0xdfff {
					t.Fatal("split astral unit")
				}
			}
			if token.Class == "interpolation" && string(utf16.Decode(units[token.Start:token.End])) == "${" {
				opens++
			}
			last = token.End
		}
		if opens != len(blocks) {
			t.Fatal("diverged from GIS boundary authority", text, value)
		}
		if text == `^\\${x}$` && (value.Tokens[1].Start != 1 || value.Tokens[1].End != 3) {
			t.Fatal("split decoded GIS backslash")
		}
		if text == `^\${literal}$` && (value.Tokens[1].Start != 1 || value.Tokens[1].End != 4 || value.Tokens[1].Class != "string") {
			t.Fatal("split multi-output GIS escape")
		}
		if text == `^ab$${bad}` && last != 3 {
			t.Fatal("scanned beyond forbidden GIS prefix")
		}
	}
}
