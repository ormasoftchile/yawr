package presentation

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"unicode/utf16"

	"gopkg.in/yaml.v3"
)

func expressionClassTexts(t *testing.T, text string, value ExpressionValue, class ExpressionClass) []string {
	t.Helper()
	units := utf16.Encode([]rune(text))
	var texts []string
	last := 0
	for _, token := range value.Tokens {
		if token.Start < last || token.End <= token.Start || token.End > len(units) {
			t.Fatalf("invalid token %+v", token)
		}
		for _, pos := range []int{token.Start, token.End} {
			if pos > 0 && pos < len(units) && units[pos] >= 0xdc00 && units[pos] <= 0xdfff {
				t.Fatalf("split UTF16 surrogate: %+v", token)
			}
		}
		if token.Class == class {
			texts = append(texts, string(utf16.Decode(units[token.Start:token.End])))
		}
		last = token.End
	}
	return texts
}

func TestExpressionNestedLiteralGISIsolation(t *testing.T) {
	expr := `list.order(items, 'str.contains(left.name, "🚀,\\\"\\\\\U0001F680") and date.\u0063ompare(left.a, right.a) < \u0030')`
	for _, host := range []string{
		"print x='${EXPR}' | where n > 0",
		"SELECT '${EXPR}' FROM data WHERE n > 0",
		"Write-Host '${EXPR}'; $other = 42",
		"🚀\r\n${EXPR}\r\n🚀",
	} {
		text := strings.ReplaceAll(host, "EXPR", expr)
		value, ok := HighlightExpression(context.Background(), text, ExpressionGIS, false)
		if !ok {
			t.Fatal("GIS highlight unavailable")
		}
		if got := expressionClassTexts(t, text, value, "function"); !reflect.DeepEqual(got, []string{"order", "contains", `\u0063ompare`}) {
			t.Fatal(got)
		}
		start := utf16Length(text[:strings.Index(text, "${")])
		end := utf16Length(text[:strings.LastIndex(text, "}")+1])
		for _, token := range value.Tokens {
			if token.Start < start || token.End > end {
				t.Fatal("painted host text", token)
			}
		}
	}
	text := `\${list.order(items, 'date.compare(left.a, right.a) < 0')}`
	value, ok := HighlightExpression(context.Background(), text, ExpressionGIS, false)
	if !ok || len(value.Tokens) != 0 {
		t.Fatal("escaped interpolation became code", value)
	}
}

func TestExpressionNestedComparatorFoldedScalar(t *testing.T) {
	const scalar = `          ${list.order(db_observations,
          'date.diffSeconds(left.configuration.min_PreciseTimeStamp, right.configuration.min_PreciseTimeStamp) < -1800 or
          (date.diffSeconds(left.configuration.min_PreciseTimeStamp, right.configuration.min_PreciseTimeStamp) >= -1800 and
          date.diffSeconds(left.configuration.min_PreciseTimeStamp, right.configuration.min_PreciseTimeStamp) <= 1800 and
          date.compare(left.configuration.max_PreciseTimeStamp, right.configuration.max_PreciseTimeStamp) < 0)')}
`
	for _, eol := range []string{"\n", "\r\n"} {
		source := strings.ReplaceAll("flow:\n  - iterate:\n      collect_values:\n        db_order_results: >-\n"+scalar, "\n", eol)
		reply := ResolveExpressions(context.Background(), expressionRequest(t, source))
		if reply.Status != "resolved" || len(reply.Regions) != 1 {
			t.Fatalf("%+v", reply)
		}
		var doc yaml.Node
		if err := yaml.Unmarshal([]byte(source), &doc); err != nil {
			t.Fatal(err)
		}
		iterate := mapValue(mapValue(doc.Content[0], "flow").Content[0], "iterate")
		decoded := mapValue(mapValue(iterate, "collect_values"), "db_order_results").Value
		region := reply.Regions[0]
		if region.YAMLPath != "/flow/0/iterate/collect_values/db_order_results" ||
			region.TextLength != 471 || strings.ContainsAny(decoded, "\r\n") ||
			region.TextDigest != Digest([]byte(decoded)) {
			t.Fatal(region, decoded)
		}
		for class, want := range map[ExpressionClass][]string{
			"namespace": {"list", "date", "date", "date", "date"},
			"function":  {"order", "diffSeconds", "diffSeconds", "diffSeconds", "compare"},
			"number":    {"1800", "1800", "1800", "0"},
			"operator":  {"<", "-", "or", ">=", "-", "and", "<=", "and", "<"},
		} {
			if got := expressionClassTexts(t, decoded, region.ExpressionValue, class); !reflect.DeepEqual(got, want) {
				t.Fatalf("%s: %v, want %v", class, got, want)
			}
		}
		for _, token := range region.Tokens {
			if token.Class == "string" && token.End-token.Start > 1 {
				t.Fatal("comparator still covered by a string token", token)
			}
		}
		finalCompare := strings.LastIndex(decoded, "date.compare")
		finalClasses := map[ExpressionClass]bool{}
		for _, token := range region.Tokens {
			if token.Start >= finalCompare {
				finalClasses[token.Class] = true
			}
		}
		for _, class := range []ExpressionClass{"namespace", "function", "property", "operator", "number"} {
			if !finalClasses[class] {
				t.Fatalf("final comparator line missing %s", class)
			}
		}
	}
}

func TestExpressionNestedAggregateLimits(t *testing.T) {
	expr := "list.order(items, '" + strings.Repeat("left.a < 0 or ", 1900) + "false')"
	if _, ok := HighlightExpression(context.Background(), expr, ExpressionGXL, false); !ok {
		t.Fatal("bounded expression rejected")
	}
	source := "flow:\n"
	for i := 0; i < 6; i++ {
		source += "  - step:\n      when: >-\n        " + expr + "\n"
	}
	if reply := ResolveExpressions(context.Background(), expressionRequest(t, source)); reply.Reason != "limit-exceeded" || len(reply.Regions) != 0 {
		t.Fatal("nested tokens bypassed aggregate cap", reply.Status, reply.Reason, len(reply.Regions))
	}
}
