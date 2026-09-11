package executor

import (
	"fmt"
	"sort"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func applyActionDefaults(action *schema.ToolAction, args map[string]any) {
	// Clone defaults so a child never acquires mutable registry-owned values.
	for name, arg := range schema.CloneToolAction(&schema.ToolAction{Args: action.Args}).Args {
		if _, supplied := args[name]; !supplied && arg != nil && arg.Default != nil {
			args[name] = arg.Default
		}
	}
}

func validateSubstitutionArgs(action *schema.ToolAction, args map[string]any) error {
	names := make([]string, 0, len(action.Args))
	for name := range action.Args {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		arg := action.Args[name]
		if arg == nil {
			continue
		}
		value, present := args[name]
		if !present || value == nil {
			if arg.Required {
				return fmt.Errorf("tool executor: required argument %q is missing", name)
			}
			if !present {
				continue
			}
		}
		declaredType := arg.Type
		if declaredType == "secret" {
			declaredType = "string"
		}
		// Substitution passes typed values, never a transport's string coercion.
		if _, text := value.(string); text && declaredType != "" && declaredType != "string" && declaredType != "any" {
			return fmt.Errorf("tool executor: argument %q requires %s, got string", name, arg.Type)
		}
		if _, err := coerceOutputAny(value, declaredType); err != nil {
			return fmt.Errorf("tool executor: argument %q: %w", name, err)
		}
	}
	return CheckSchemaArgEnums(action, args)
}
