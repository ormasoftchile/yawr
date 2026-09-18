package executor

import (
	"reflect"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestSynthesizeDeclaredOutputs_ZeroValuesMatchDeclaredTypes(t *testing.T) {
	declared := map[string]*schema.ArgDef{
		"flag":    {Type: "boolean"},
		"count":   {Type: "integer"},
		"ratio":   {Type: "number"},
		"label":   {Type: "string"},
		"bare":    {Type: ""},
		"rows":    {Type: "array"},
		"detail":  {Type: "object"},
		"opaque":  {Type: "any"},
		"status":  {Type: "string", Enum: schema.EnumConstraint{"observed", "no-data"}},
		"skipped": {Type: "string", Optional: true},
	}
	got := SynthesizeDeclaredOutputs(declared, nil)
	want := map[string]any{
		"flag":   false,
		"count":  0,
		"ratio":  float64(0),
		"label":  "",
		"bare":   "",
		"rows":   []any{},
		"detail": map[string]any{},
		"opaque": nil,
		// An enum-constrained string takes its first declared member: "" is
		// never a legal member, so a runbook branching on the enum would
		// otherwise see a value outside the declared domain.
		"status": "observed",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("synthesized = %#v, want %#v", got, want)
	}
	// An optional output is genuinely allowed to be absent; inventing it would
	// assert something the real run does not.
	if _, present := got["skipped"]; present {
		t.Fatalf("optional output manufactured: %#v", got)
	}
}

func TestSynthesizeDeclaredOutputs_FillsNestedCapturePaths(t *testing.T) {
	declared := map[string]*schema.ArgDef{"ordering": {Type: "object"}}
	captures := map[string]string{
		"status": "outputs.ordering.status",
		"items":  "outputs.ordering.items",
		"deep":   "outputs.ordering.meta.source",
	}
	got := SynthesizeDeclaredOutputs(declared, captures)
	want := map[string]any{"ordering": map[string]any{
		"status": nil,
		"items":  nil,
		"meta":   map[string]any{"source": nil},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("nested capture paths = %#v, want %#v", got, want)
	}
}

func TestSynthesizeDeclaredOutputs_LeavesUninferablePathsAlone(t *testing.T) {
	declared := map[string]*schema.ArgDef{
		"rows":  {Type: "array"},
		"label": {Type: "string"},
	}
	captures := map[string]string{
		"indexed":  "outputs.rows[0].name",
		"optional": "outputs.rows?.name",
		// A path descending into a declared scalar has no consistent shape.
		"throughScalar": "outputs.label.inner",
		"processText":   "stdout",
		"parsedStdout":  "json.items",
	}
	got := SynthesizeDeclaredOutputs(declared, captures)
	want := map[string]any{"rows": []any{}, "label": ""}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("synthesized = %#v, want %#v", got, want)
	}
}

func TestSynthesizeDeclaredOutputs_UndeclaredActionSynthesizesNothing(t *testing.T) {
	if got := SynthesizeDeclaredOutputs(nil, map[string]string{"x": "outputs.y"}); got != nil {
		t.Fatalf("synthesized %#v for an action with no outputs: contract", got)
	}
}
