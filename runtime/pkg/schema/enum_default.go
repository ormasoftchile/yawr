package schema

import (
	"fmt"
	"sort"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
)

// ValidateEnumDefault applies ENUM-006 to a declaration's static default.
// declaration is its diagnostic label, such as `input "environment"`.
// Absent defaults and unconstrained declarations are ignored. Strings
// containing ${ are deferred to runtime; other values retain the planner's
// existing fmt.Sprint conversion and NFC-aware membership comparison.
func ValidateEnumDefault(members EnumConstraint, value any, declaration string) error {
	if len(members) == 0 || value == nil {
		return nil
	}
	if text, ok := value.(string); ok && strings.Contains(text, "${") {
		return nil
	}
	candidate := fmt.Sprint(value)
	if members.Contains(candidate) {
		return nil
	}
	return errkit.New("ENUM-006", fmt.Sprintf("%s default %q is not a declared enum member", declaration, candidate))
}

// ValidateToolActionEnumDefaults checks argument defaults, then output defaults,
// each in name order, before substitution contract admission. Declaration
// well-formedness and interpolated-value syntax are validated separately.
func ValidateToolActionEnumDefaults(toolName, actionName string, action *ToolAction) []error {
	if action == nil {
		return nil
	}
	var issues []error
	names := make([]string, 0, len(action.Args))
	for name := range action.Args {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		arg := action.Args[name]
		if arg != nil {
			if err := ValidateEnumDefault(arg.Enum, arg.Default, fmt.Sprintf("tool %q action %q arg %q", toolName, actionName, name)); err != nil {
				issues = append(issues, err)
			}
		}
	}
	names = make([]string, 0, len(action.Outputs))
	for name := range action.Outputs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		out := action.Outputs[name]
		if out != nil {
			if err := ValidateEnumDefault(out.Enum, out.Default, fmt.Sprintf("tool %q action %q output %q", toolName, actionName, name)); err != nil {
				issues = append(issues, err)
			}
		}
	}
	return issues
}
