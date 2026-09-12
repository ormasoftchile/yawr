package eval

import "testing"

func TestStrictTruthyRejectsNonBoolBooleanPosition(t *testing.T) {
	cases := []struct {
		name string
		expr string
		vars map[string]any
	}{
		{"not null", "not null", nil},
		{"not number", "not n", map[string]any{"n": 1}},
		{"not string", "not s", map[string]any{"s": "hello"}},
		{"not list", "not xs", map[string]any{"xs": []any{1}}},
		{"and left string", `"x" and true`, nil},
		{"and right number", "true and 1", nil},
		{"or left object", "obj or true", map[string]any{"obj": map[string]any{"a": 1}}},
		{"or right null", "false or null", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := evalError(t, tc.expr, tc.vars)
			assertCode(t, err, "GXL-TYPE-002")
		})
	}
}
