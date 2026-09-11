package parser

import "testing"

func TestOrderComparatorValidatedDuringParse(t *testing.T) {
	for _, source := range []string{
		`list.order(items, "left +")`,
		`false and list.order(items, "left +")`,
		`list.order(items, comparator)`,
		`list.order(items, "secret < right")`,
		`list.order(items, "now() == now()")`,
		`list.order(items, "list.order(left, 'true')")`,
	} {
		if _, err := Parse(source); err == nil {
			t.Fatalf("invalid authored comparator accepted: %s", source)
		}
	}
	if _, err := Parse(`list.order(items, "date.compare(left.time, right.time) < 0")`); err != nil {
		t.Fatal(err)
	}
}
