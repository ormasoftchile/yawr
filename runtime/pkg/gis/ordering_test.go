package gis

import (
	"testing"

	gxleval "github.com/ormasoftchile/yawr/runtime/pkg/gxl/eval"
)

func TestInterpolateDateAndOrderingBuiltins(t *testing.T) {
	scope, err := gxleval.FromAny(map[string]any{"items": []any{3, 1, 2}})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		source string
		want   string
	}{
		{`${date.diffSeconds("2026-01-15T10:30:00Z", "2026-01-15T10:00:00Z")}`, "1800"},
		{`${list.order(items, "left < right")}`, `{"items":[1,2,3],"status":"ordered"}`},
	} {
		value, err := Interpolate(test.source, scope)
		if err != nil || value != test.want {
			t.Fatalf("Interpolate(%q) = %q, %v; want %q", test.source, value, err, test.want)
		}
	}
}
