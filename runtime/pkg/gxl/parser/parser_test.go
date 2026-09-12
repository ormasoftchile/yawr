package parser

import (
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
)

func TestParseHappyPaths(t *testing.T) {
	cases := []string{
		"42", "0", "-7", "3.14", "1.5e10", "2.0E-3", "1e+5",
		"true", "false", "null", `"hello"`, `'hello'`, `"café résumé"`,
		"hostname", "my_var", "var1", "myVariable", "config.region", "items[0]", "response.items[0].metadata.name", "list[10]",
		`status == "ok"`, `status != "error"`, "count < 100", "count <= 100", "score > 0", "score >= 0",
		"a + b", "count - 1", "price * quantity", "total / count", "value % 3",
		"is_ready and is_enabled", "is_admin or is_owner", "not is_blocked", "not a and b", "a or b and c", "a + b * c", "(a + b) * c",
		"len(items)", `str.contains(message, "error")`, "str.trim(label)", `list.contains(tags, "prod")`, `regex.match(path, "^/api/")`, `str.contains(message, "x") and len(items) > 0`, `str.startsWith(config.prefix, "prod")`,
		"now()", "# comment\ntrue",
	}
	for _, tc := range cases {
		t.Run(tc, func(t *testing.T) {
			if _, err := Parse(tc); err != nil {
				t.Fatalf("Parse(%q) error = %v", tc, err)
			}
		})
	}
}

func TestParseOptionalPathSegments(t *testing.T) {
	expr, err := Parse("items?.[0]?.name")
	if err != nil {
		t.Fatal(err)
	}
	ref, ok := expr.Node.(*PathRef)
	if !ok {
		t.Fatalf("node = %T, want *PathRef", expr.Node)
	}
	if got := len(ref.Segments); got != 2 {
		t.Fatalf("segments = %d, want 2", got)
	}
	for i, seg := range ref.Segments {
		if !seg.Optional {
			t.Fatalf("segment %d Optional = false, want true", i)
		}
	}
}

func TestPrecedenceShape(t *testing.T) {
	expr, err := Parse("a or b and c")
	if err != nil {
		t.Fatal(err)
	}
	root, ok := expr.Node.(*BinaryOp)
	if !ok || root.Op != "or" {
		t.Fatalf("root = %#v, want or BinaryOp", expr.Node)
	}
	if rhs, ok := root.Right.(*BinaryOp); !ok || rhs.Op != "and" {
		t.Fatalf("right = %#v, want and BinaryOp", root.Right)
	}
}

func TestParseErrorCodes(t *testing.T) {
	cases := []struct{ in, code string }{
		{".foo", "GXL-PARSE-001"},
		{"a +", "GXL-PARSE-001"},
		{`"unterminated`, "GXL-PARSE-002"},
		{"007", "GXL-PARSE-003"},
		{`"hello \q world"`, "GXL-PARSE-004"},
		{"math.abs(x)", "GXL-PARSE-005"},
		{"http.get(url)", "GXL-PARSE-005"},
		{"str.explode(s, ',')", "GXL-PARSE-006"},
		{"list.sort(items)", "GXL-PARSE-006"},
		{"a && b", "GXL-PARSE-007"},
		{"a || b", "GXL-PARSE-007"},
		{"!is_ready", "GXL-PARSE-007"},
		{`items contains "prod"`, "GXL-PARSE-007"},
		{"a ? b : c", "GXL-PARSE-007"},
		{"foo[-1]", "GXL-PARSE-007"},
		{"foo()", "GXL-PARSE-007"},
		{"x = 5", "GXL-PARSE-007"},
		{"+5", "GXL-PARSE-007"},
		{"(a + b", "GXL-PARSE-008"},
		{"a + b)", "GXL-PARSE-008"},
		{"a < b < c", "GXL-PARSE-009"},
		{"a == b == c", "GXL-PARSE-009"},
		{"and", "GXL-PARSE-010"},
		{"or", "GXL-PARSE-010"},
		{"str.foo", "GXL-PARSE-001"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			_, err := Parse(tc.in)
			if err == nil {
				t.Fatalf("Parse(%q) succeeded, want %s", tc.in, tc.code)
			}
			e, ok := err.(*errkit.Error)
			if !ok {
				t.Fatalf("error type = %T, want *errkit.Error", err)
			}
			if e.Code() != tc.code {
				t.Fatalf("code = %s, want %s (err %v)", e.Code(), tc.code, err)
			}
		})
	}
}

func TestPositionTracking(t *testing.T) {
	expr, err := Parse("\n  a + b")
	if err != nil {
		t.Fatal(err)
	}
	if expr.Span.Start.Line != 2 || expr.Span.Start.Column != 3 {
		t.Fatalf("start = line %d col %d, want line 2 col 3", expr.Span.Start.Line, expr.Span.Start.Column)
	}
}

func TestMaxNestingDepth(t *testing.T) {
	src := strings.Repeat("(", maxDepth+2) + "true" + strings.Repeat(")", maxDepth+2)
	_, err := Parse(src)
	if err == nil {
		t.Fatal("Parse succeeded, want graceful depth error")
	}
	if _, ok := err.(*errkit.Error); !ok {
		t.Fatalf("error type = %T, want *errkit.Error", err)
	}
}
