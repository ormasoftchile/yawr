package schema

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
)

func TestValidateEnumDefaultPreservesMembershipSemantics(t *testing.T) {
	var typedNil *string
	for _, test := range []struct {
		name    string
		members EnumConstraint
		value   any
		invalid bool
	}{
		{"valid", EnumConstraint{"ready"}, "ready", false},
		{"invalid", EnumConstraint{"ready"}, "other", true},
		{"nil", EnumConstraint{"ready"}, nil, false},
		{"unconstrained", nil, "other", false},
		{"interpolated", EnumConstraint{"ready"}, "prefix-${state}", false},
		{"malformed-interpolation-is-separate", EnumConstraint{"ready"}, "${", false},
		{"empty-string", EnumConstraint{"ready"}, "", true},
		{"empty-array", EnumConstraint{"ready"}, []any{}, true},
		{"empty-object", EnumConstraint{"ready"}, map[string]any{}, true},
		{"false", EnumConstraint{"ready"}, false, true},
		{"zero", EnumConstraint{"ready"}, 0, true},
		{"typed-nil-is-present", EnumConstraint{"ready"}, typedNil, true},
		{"number-conversion", EnumConstraint{"9007199254740993"}, json.Number("9007199254740993"), false},
		{"boolean-conversion", EnumConstraint{"false"}, false, false},
		{"case-sensitive", EnumConstraint{"ready"}, "Ready", true},
		{"no-trimming", EnumConstraint{"ready"}, " ready ", true},
		{"nfc-candidate", EnumConstraint{"é"}, "e\u0301", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateEnumDefault(test.members, test.value, `input "state"`)
			if !test.invalid {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if !errors.Is(err, errkit.ErrENUM006) {
				t.Fatalf("expected ENUM-006, got %v", err)
			}
			expected := fmt.Sprintf(`input "state" default %q is not a declared enum member`, fmt.Sprint(test.value))
			if !strings.Contains(err.Error(), expected) {
				t.Fatalf("changed default diagnostic: %v", err)
			}
		})
	}
}

func TestValidateToolActionEnumDefaultsStableOrder(t *testing.T) {
	if issues := ValidateToolActionEnumDefaults("tool", "action", nil); len(issues) != 0 {
		t.Fatal(issues)
	}
	action := &ToolAction{
		Args: map[string]*ArgDef{
			"z":        {Enum: EnumConstraint{"ok"}, Default: "bad"},
			"a":        {Enum: EnumConstraint{"ok"}, Default: "bad"},
			"nil":      nil,
			"deferred": {Enum: EnumConstraint{"ok"}, Default: "${state}"},
		},
		Outputs: map[string]*ArgDef{
			"z":      {Enum: EnumConstraint{"ok"}, Default: "bad"},
			"a":      {Enum: EnumConstraint{"ok"}, Default: "bad"},
			"nil":    nil,
			"valid":  {Enum: EnumConstraint{"ok"}, Default: "ok"},
			"absent": {Enum: EnumConstraint{"ok"}},
		},
	}
	issues := ValidateToolActionEnumDefaults("tool", "action", action)
	want := []string{`arg "a"`, `arg "z"`, `output "a"`, `output "z"`}
	if len(issues) != len(want) {
		t.Fatalf("issues=%v", issues)
	}
	for index, err := range issues {
		if !errors.Is(err, errkit.ErrENUM006) || !strings.Contains(err.Error(), `tool "tool" action "action" `+want[index]) {
			t.Fatalf("diagnostic %d: %v", index, err)
		}
	}
	if action.Args["deferred"].Default != "${state}" || action.Outputs["absent"].Default != nil {
		t.Fatal("admission changed declarations")
	}
}
