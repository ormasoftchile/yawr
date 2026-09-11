package gis

import (
	"context"
	"testing"
)

func TestScanExpressionsUsesRuntimeBoundaries(t *testing.T) {
	cases := []struct {
		text              string
		count, start, end int
		closed            bool
	}{
		{`Hi ${vars.name}!`, 1, 3, 14, true},
		{`\${vars.name}`, 0, 0, 0, false},
		{`\\${vars.name}`, 1, 2, 13, true},
		{`\\\${vars.name}`, 0, 0, 0, false},
		{`$${vars.name} ${vars.other}`, 0, 0, 0, false},
		{`${vars.name`, 1, 0, 11, false},
		{`${vars.x + }`, 1, 0, 11, true},
		{`${str.contains(vars.x, "}")}`, 1, 0, 27, true},
		{`${vars.items[0]}`, 1, 0, 15, true},
		{`${vars.items[(0}`, 1, 0, 16, false},
	}
	for _, c := range cases {
		got := ScanExpressions(context.Background(), c.text)
		if len(got) != c.count {
			t.Fatal(c.text, got)
		}
		if c.count > 0 && (got[0].Start != c.start || got[0].ExpressionEnd != c.end || got[0].Closed != c.closed) {
			t.Fatal(c.text, got, c)
		}
	}
	for _, text := range []string{`${vars.name`, `${vars.x + }`, `$${vars.name}`} {
		if _, err := Parse(text); err == nil {
			t.Fatal("strict GIS changed", text)
		}
	}
}
