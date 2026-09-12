package eval

import (
	"errors"
	"testing"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/gxl/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/pjvm"
)

func TestEvalOperatorsAndFunctions(t *testing.T) {
	cases := []struct {
		name string
		expr string
		vars map[string]any
		want any
	}{
		{"and short circuit", "false and missing", nil, false},
		{"or short circuit", "true or 1 / 0", nil, true},
		{"not", "not false", nil, true},
		{"number arithmetic", "1 + 2 * 3", nil, float64(7)},
		{"float division", "7 / 2", nil, 3.5},
		{"negative modulo", "-7 % 3", nil, float64(-1)},
		{"number comparison", "1 < 2", nil, true},
		{"string comparison", `"abc" < "abd"`, nil, true},
		{"null equality", "null == null", nil, true},
		{"len string", `len("hé")`, nil, float64(2)},
		{"len list", "len(items)", map[string]any{"items": []any{1, 2}}, float64(2)},
		{"str contains", `str.contains("hello", "ell")`, nil, true},
		{"str trim prefix", `str.trimPrefix("prod-east", "prod-")`, nil, "east"},
		{"list contains", "list.contains(items, 2)", map[string]any{"items": []any{1, 2, 3}}, true},
		{"list index", "list.indexOf(items, 3)", map[string]any{"items": []any{1, 2, 3}}, float64(2)},
		{"regex match", `regex.match("abc123", "^[a-z]+\\d+$")`, nil, true},
		{"path ref", "user.id == 42", map[string]any{"user": map[string]any{"id": 42}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := evalString(t, tc.expr, tc.vars)
			want, err := pjvm.FromAny(tc.want)
			if err != nil {
				t.Fatalf("want PJVM: %v", err)
			}
			if !got.Equal(want) {
				t.Fatalf("Eval(%q) = %s, want %s", tc.expr, got.String(), want.String())
			}
		})
	}
}

func TestEvalErrors(t *testing.T) {
	cases := []struct {
		name string
		expr string
		vars map[string]any
		code string
	}{
		{"cross type comparison", `1 == "1"`, nil, "GXL-TYPE-001"},
		{"bool ordered comparison", "false < true", nil, "GXL-TYPE-005"},
		{"null ordered comparison", "null < 0", nil, "GXL-EVAL-004"},
		{"division by zero", "1 / 0", nil, "GXL-EVAL-002"},
		{"arithmetic type", `1 + "x"`, nil, "GXL-TYPE-003"},
		{"len type", "len(null)", nil, "GXL-TYPE-003"},
		{"wrong arity", `str.startsWith("hello")`, nil, "GXL-TYPE-004"},
		{"missing variable", "missing", nil, "GXL-PATH-001"},
		{"missing field", "obj.missing", map[string]any{"obj": map[string]any{}}, "GXL-PATH-001"},
		{"index out of range", "items[3]", map[string]any{"items": []any{1}}, "GXL-PATH-002"},
		{"index on non array", "name[0]", map[string]any{"name": "yawr"}, "GXL-PATH-003"},
		{"field on non object", "name.first", map[string]any{"name": "yawr"}, "GXL-PATH-004"},
		{"invalid regex", `regex.match("x", "[")`, nil, "GXL-EVAL-003"},
		{"list equality", "items == items", map[string]any{"items": []any{1}}, "GXL-TYPE-001"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := evalError(t, tc.expr, tc.vars)
			assertCode(t, err, tc.code)
		})
	}
}

func TestScopeParentAndClock(t *testing.T) {
	parentVal, _ := pjvm.NewString("parent")
	childVal, _ := pjvm.NewString("child")
	parent := NewScope(map[string]pjvm.Value{"name": parentVal}, nil)
	child := NewScope(map[string]pjvm.Value{"name": childVal}, parent)
	got := evalParsed(t, `name`, child)
	if got.String() != "child" {
		t.Fatalf("shadowed name = %s, want child", got.String())
	}

	clocked := child.WithClock(func() time.Time { return time.Date(2026, 6, 7, 19, 42, 0, 123, time.UTC) })
	got = evalParsed(t, `now()`, clocked)
	if got.String() != "2026-06-07T19:42:00Z" {
		t.Fatalf("now() = %s", got.String())
	}
}

func evalString(t *testing.T, src string, vars map[string]any) pjvm.Value {
	t.Helper()
	scope, err := FromAny(vars)
	if err != nil {
		t.Fatalf("scope: %v", err)
	}
	return evalParsed(t, src, scope)
}

func evalParsed(t *testing.T, src string, scope *Scope) pjvm.Value {
	t.Helper()
	expr, err := parser.Parse(src)
	if err != nil {
		t.Fatalf("Parse(%q): %v", src, err)
	}
	got, err := Eval(expr, scope)
	if err != nil {
		t.Fatalf("Eval(%q): %v", src, err)
	}
	return got
}

func evalError(t *testing.T, src string, vars map[string]any) error {
	t.Helper()
	expr, err := parser.Parse(src)
	if err != nil {
		return err
	}
	scope, err := FromAny(vars)
	if err != nil {
		t.Fatalf("scope: %v", err)
	}
	_, err = Eval(expr, scope)
	if err == nil {
		t.Fatalf("Eval(%q) succeeded, want error", src)
	}
	return err
}

func assertCode(t *testing.T, err error, code string) {
	t.Helper()
	var got *errkit.Error
	if !errors.As(err, &got) {
		t.Fatalf("%v is not errkit.Error", err)
	}
	if got.Code() != code {
		t.Fatalf("code = %s, want %s (%v)", got.Code(), code, err)
	}
}
