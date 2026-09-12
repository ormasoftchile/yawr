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
		{`Hi ${name}!`, 1, 3, 9, true},
		{`\${name}`, 0, 0, 0, false},
		{`\\${name}`, 1, 2, 8, true},
		{`\\\${name}`, 0, 0, 0, false},
		{`$${name} ${other}`, 0, 0, 0, false},
		{`${name`, 1, 0, 6, false},
		{`${x + }`, 1, 0, 6, true},
		{`${str.contains(x, "}")}`, 1, 0, 22, true},
		{`${items[0]}`, 1, 0, 10, true},
		{`${items[(0}`, 1, 0, 11, false},
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
	for _, text := range []string{`${name`, `${x + }`, `$${name}`} {
		if _, err := Parse(text); err == nil {
			t.Fatal("strict GIS changed", text)
		}
	}
}
