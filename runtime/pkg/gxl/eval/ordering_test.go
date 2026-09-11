package eval

import (
	"fmt"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/pjvm"
)

func TestDateFunctions(t *testing.T) {
	for _, test := range []struct {
		expression string
		want       float64
	}{
		{`date.compare("2026-01-15T10:00:00Z", "2026-01-15T11:00:00+01:00")`, 0},
		{`date.compare("2026-01-15 10:00:00", "2026-01-15T10:00:01Z")`, -1},
		{`date.compare("2026-01-15T10:00:01Z", "2026-01-15 10:00:00")`, 1},
		{`date.diffSeconds("2026-01-15T10:30:00Z", "2026-01-15T10:00:00Z")`, 1800},
		{`date.diffSeconds("2026-01-15T10:00:00Z", "2026-01-15T10:30:00Z")`, -1800},
		{`date.diffSeconds("2026-01-15T10:00:00.500Z", "2026-01-15T10:00:00.250Z")`, 0.25},
		{`date.diffSeconds("2024-03-01T00:00:00Z", "2024-02-28T00:00:00Z")`, 172800},
		{`date.diffSeconds("3000-01-01T00:00:00Z", "1000-01-01T00:00:00Z")`, 63113904000},
	} {
		got := evalString(t, test.expression, nil)
		value, ok := got.NumberValue()
		if !ok || value != test.want {
			t.Fatalf("%s = %s, want %v", test.expression, got.String(), test.want)
		}
	}
	for _, value := range []string{"2026-02-30 10:00:00", "2026-01-15T10:00:00", "0000-01-01T00:00:00Z", "not-a-date"} {
		assertCode(t, evalError(t, fmt.Sprintf(`date.compare(%q, "2026-01-15T10:00:00Z")`, value), nil), "GXL-DATE-001")
	}
	assertCode(t, evalError(t, `date.compare(null, "2026-01-15T10:00:00Z")`, nil), "GXL-TYPE-003")
	assertCode(t, evalError(t, `date.diffSeconds("2026-01-15T10:00:00Z")`, nil), "GXL-TYPE-004")
	if value := evalString(t, "record.date == date", map[string]any{"date": "kept", "record": map[string]any{"date": "kept"}}); value.String() != "true" {
		t.Fatal("new namespace reserved an existing variable or field")
	}
}

func TestListOrderReportsUnresolvedComparisons(t *testing.T) {
	for _, test := range []struct {
		name       string
		items      []any
		comparator string
		status     string
		want       []any
	}{
		{"ascending", []any{3, 1, 2}, "left < right", "ordered", []any{1, 2, 3}},
		{"descending", []any{3, 1, 2}, "left > right", "ordered", []any{3, 2, 1}},
		{"equal", []any{2, 2, 1}, "left < right", "ambiguous", []any{2, 2, 1}},
		{"contradictory", []any{1, 2}, "true", "inconsistent", []any{1, 2}},
		{"cycle", []any{1, 2, 3}, "(left == 1 and right == 2) or (left == 2 and right == 3) or (left == 3 and right == 1)", "inconsistent", []any{1, 2, 3}},
		{"cycle-with-ties", []any{1, 2, 3, 4}, "(left == 1 and right == 2) or (left == 2 and right == 3) or (left == 3 and right == 1)", "inconsistent", []any{1, 2, 3, 4}},
		{"empty", []any{}, "left < right", "ordered", []any{}},
		{"singleton", []any{1}, "left < right", "ordered", []any{1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := evalString(t, fmt.Sprintf("list.order(items, %q)", test.comparator), map[string]any{"items": test.items})
			fields, ok := result.ObjectValue()
			if !ok || fields["status"].String() != test.status {
				t.Fatalf("ordering result = %s", result.String())
			}
			want, err := pjvm.FromAny(test.want)
			if err != nil || !fields["items"].Equal(want) {
				t.Fatalf("items = %s, want %v (%v)", fields["items"].String(), test.want, err)
			}
		})
	}
	assertCode(t, evalError(t, `list.order(items, "left")`, map[string]any{"items": []any{1, 2}}), "GXL-TYPE-002")
	assertCode(t, evalError(t, `list.order(items, "left < right")`, map[string]any{"items": "not-a-list"}), "GXL-TYPE-003")
	assertCode(t, evalError(t, `list.order(items, "now() == now()")`, map[string]any{"items": []any{1, 2}}), "GXL-ORDER-001")
	assertCode(t, evalError(t, `list.order(items, "list.order(left, 'true')")`, map[string]any{"items": []any{1, 2}}), "GXL-ORDER-001")
	assertCode(t, evalError(t, `list.order(items, "left < right")`, map[string]any{"items": make([]any, 257)}), "GXL-ORDER-002")
}

func TestListOrderSupportsAuthoredDateClosenessRule(t *testing.T) {
	comparator := `date.diffSeconds(left.min, right.min) < -1800 or
		(date.diffSeconds(left.min, right.min) >= -1800 and date.diffSeconds(left.min, right.min) <= 1800
		 and date.compare(left.max, right.max) < 0)`
	for _, first := range []string{"10:29:59", "10:30:00", "10:30:00.000000001"} {
		for _, reverse := range []bool{false, true} {
			a := map[string]any{"min": "2026-01-15T10:00:00Z", "max": "2026-01-15T12:00:00Z"}
			b := map[string]any{"min": "2026-01-15T" + first + "Z", "max": "2026-01-15T11:00:00Z"}
			items := []any{a, b}
			if reverse {
				items = []any{b, a}
			}
			result := evalString(t, fmt.Sprintf("list.order(items, %q)", comparator), map[string]any{"items": items})
			fields, _ := result.ObjectValue()
			wantItems := []any{b, a}
			if first == "10:30:00.000000001" {
				wantItems = []any{a, b}
			}
			want, _ := pjvm.FromAny(wantItems)
			if fields["status"].String() != "ordered" || !fields["items"].Equal(want) {
				t.Fatalf("inclusive +/-1800 boundary %s reverse=%v: %s", first, reverse, result.String())
			}
		}
	}
	older := map[string]any{"id": "older", "min": "2026-01-15T10:10:00Z", "max": "2026-01-15T10:20:00Z"}
	newer := map[string]any{"id": "newer", "min": "2026-01-15T10:00:00Z", "max": "2026-01-15T10:40:00Z"}
	last := map[string]any{"id": "last", "min": "2026-01-15T11:00:00Z", "max": "2026-01-15T11:30:00Z"}
	result := evalString(t, fmt.Sprintf("list.order(items, %q)", comparator), map[string]any{"items": []any{last, newer, older}})
	fields, _ := result.ObjectValue()
	want, _ := pjvm.FromAny([]any{older, newer, last})
	if fields["status"].String() != "ordered" || !fields["items"].Equal(want) {
		t.Fatalf("authored rule = %s", result.String())
	}
	cyclic := []any{
		map[string]any{"min": "2026-01-15T10:00:00Z", "max": "2026-01-15T12:00:00Z"},
		map[string]any{"min": "2026-01-15T10:20:00Z", "max": "2026-01-15T11:50:00Z"},
		map[string]any{"min": "2026-01-15T10:40:00Z", "max": "2026-01-15T11:40:00Z"},
	}
	result = evalString(t, fmt.Sprintf("list.order(items, %q)", comparator), map[string]any{"items": cyclic})
	fields, _ = result.ObjectValue()
	if fields["status"].String() != "inconsistent" {
		t.Fatalf("non-transitive authored rule must not choose an arbitrary order: %s", result.String())
	}
}
