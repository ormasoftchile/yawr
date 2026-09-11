package expr

import (
	"reflect"
	"testing"
)

func TestTemplateEvaluatorTypedValuesPreserveLegacyInterpolation(t *testing.T) {
	evaluator := &TemplateEvaluator{}
	vars := map[string]any{
		"rows": []any{map[string]any{"count": 3, "active": true, "note": nil}},
	}
	value, err := evaluator.EvalValue("rows", vars)
	if err != nil || !reflect.DeepEqual(value, vars["rows"]) {
		t.Fatalf("typed value = %#v, %v", value, err)
	}
	value.([]any)[0].(map[string]any)["count"] = 99
	if vars["rows"].([]any)[0].(map[string]any)["count"] != 3 {
		t.Fatal("typed evaluation aliased the input")
	}
	legacy, err := evaluator.Eval("${rows[0].count}", vars)
	if err != nil || legacy != "3" {
		t.Fatalf("legacy interpolation = %q, %v", legacy, err)
	}
	for _, expression := range []string{"rows[0].count + 2", "rows[0].active", "rows[0].note"} {
		if _, err := evaluator.EvalValue(expression, vars); err != nil {
			t.Fatalf("EvalValue(%q): %v", expression, err)
		}
	}
	if _, err := evaluator.EvalValue("missing", vars); err == nil {
		t.Fatal("missing required value did not fail")
	}
}
