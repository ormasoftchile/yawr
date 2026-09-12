package executor

import (
	"errors"
	"fmt"
	"sort"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/gdp"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// EvaluateNamedOutputs is scratch-only. The engine's completed-checkpoint
// boundary, not this evaluator, owns identity, sealing, durability and transport.
func EvaluateNamedOutputs(outputs map[string]*schema.Output, vars map[string]any) (map[string]engine.NamedResultValue, error) {
	names := make([]string, 0, len(outputs))
	for name := range outputs {
		names = append(names, name)
	}
	sort.Strings(names)
	resolved := make(map[string]engine.NamedResultValue, len(outputs))
	evaluator := &pureEvaluator{remaining: 65536}
	for _, name := range names {
		declaration := outputs[name]
		if declaration == nil {
			return nil, fmt.Errorf("outputs.%s: missing declaration", name)
		}
		forms := 0
		if declaration.ValueTreePresent {
			forms++
		}
		if declaration.ValueExpr != "" {
			forms++
		}
		if declaration.Value != "" {
			forms++
		}
		if forms != 1 {
			return nil, fmt.Errorf("outputs.%s: exactly one value form is required", name)
		}
		var value any
		var err error
		switch {
		case declaration.ValueTreePresent:
			value, err = resolveTypedValue(evaluator, declaration.ValueTree, vars)
		case declaration.ValueExpr != "":
			value, err = evaluator.EvalValue(declaration.ValueExpr, vars)
		default:
			value, err = evaluator.Eval(declaration.Value, vars)
		}
		if err != nil {
			var missing *gdp.MissingPathError
			if declaration.Optional && errors.As(err, &missing) {
				continue
			}
			return nil, typedDiagnostic("outputs."+name, declaration.Type, value, err)
		}
		if err := ValidateStrictValue(value, declaration.Type, declaration.Enum); err != nil {
			return nil, typedDiagnostic("outputs."+name, declaration.Type, value, err)
		}
		resolved[name] = engine.NamedResultValue{Type: declaration.Type, Value: value}
	}
	return resolved, nil
}
