package gis

import (
	"testing"

	gxleval "github.com/ormasoftchile/yawr/runtime/pkg/gxl/eval"
)

func TestInterpolate(t *testing.T) {
	tests := []struct {
		name string
		in   string
		vars map[string]any
		want string
	}{
		{name: "empty", in: "", want: ""},
		{name: "literal", in: "hello", want: "hello"},
		{name: "path", in: "hello ${user.name}", vars: map[string]any{"user": map[string]any{"name": "Alice"}}, want: "hello Alice"},
		{name: "escape", in: `literal: \${not interpolated}`, want: "literal: ${not interpolated}"},
		{name: "adjacent", in: "${a}${b}", vars: map[string]any{"a": "x", "b": "y"}, want: "xy"},
		{name: "stdlib", in: "${str.toUpper(user.name)}", vars: map[string]any{"user": map[string]any{"name": "alice"}}, want: "ALICE"},
		{name: "number", in: "${n}", vars: map[string]any{"n": 42}, want: "42"},
		{name: "bool", in: "${b}", vars: map[string]any{"b": false}, want: "false"},
		{name: "null", in: "${n}", vars: map[string]any{"n": nil}, want: "null"},
		{name: "array", in: "${a}", vars: map[string]any{"a": []any{1, "x", true}}, want: `[1,"x",true]`},
		{name: "object", in: "${o}", vars: map[string]any{"o": map[string]any{"z": 1, "a": 2}}, want: `{"a":2,"z":1}`},
		{name: "optional root miss", in: "${a?.b}", vars: map[string]any{}, want: ""},
		{name: "optional deep miss", in: "${a?.b?.c}", vars: map[string]any{"a": map[string]any{"b": map[string]any{}}}, want: ""},
		{name: "optional tail short circuit", in: "${a?.b.c}", vars: map[string]any{"a": map[string]any{}}, want: ""},
		{name: "optional null", in: "${a?.b}", vars: map[string]any{"a": map[string]any{"b": nil}}, want: ""},
		{name: "optional false", in: "${a?.b}", vars: map[string]any{"a": map[string]any{"b": false}}, want: "false"},
		{name: "optional zero", in: "${a?.b}", vars: map[string]any{"a": map[string]any{"b": 0}}, want: "0"},
		{name: "optional empty array", in: "${a?.b}", vars: map[string]any{"a": map[string]any{"b": []any{}}}, want: "[]"},
		{name: "optional bracket", in: "${items?.[0]?.name}", vars: map[string]any{"items": []any{map[string]any{"name": "x"}}}, want: "x"},
		{name: "optional bracket miss", in: "${items?.[5]?.name}", vars: map[string]any{"items": []any{map[string]any{"name": "x"}}}, want: ""},
		{name: "optional in call", in: "${str.toLower(user?.name)}", vars: map[string]any{}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scope, err := gxleval.FromAny(tt.vars)
			if err != nil {
				t.Fatalf("FromAny() error = %v", err)
			}
			got, err := Interpolate(tt.in, scope)
			if err != nil {
				t.Fatalf("Interpolate() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("Interpolate() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestInterpolateErrors(t *testing.T) {
	tests := []struct {
		name string
		in   string
		vars map[string]any
		code string
	}{
		{name: "mandatory miss", in: "${a.b?.c}", vars: map[string]any{}, code: "GIS-PATH-MISSING"},
		{name: "strict truthy", in: "${name and true}", vars: map[string]any{"name": "Alice"}, code: "GIS-TYPE-002"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scope, err := gxleval.FromAny(tt.vars)
			if err != nil {
				t.Fatalf("FromAny() error = %v", err)
			}
			_, err = Interpolate(tt.in, scope)
			if err == nil {
				t.Fatal("Interpolate() error = nil")
			}
			if got := errorCode(err); got != tt.code {
				t.Fatalf("code = %s, want %s (%v)", got, tt.code, err)
			}
		})
	}
}
