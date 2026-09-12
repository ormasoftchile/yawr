package executor

import (
	"fmt"
	"math"

	"github.com/ormasoftchile/yawr/runtime/pkg/capture"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/gis"
	gxleval "github.com/ormasoftchile/yawr/runtime/pkg/gxl/eval"
	gxlparser "github.com/ormasoftchile/yawr/runtime/pkg/gxl/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/pjvm"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

type pureEvaluator struct{ remaining int }

func pureScope(vars map[string]any) (*gxleval.Scope, error) {
	all := make(map[string]any, len(vars))
	for name, value := range vars {
		all[name] = value
	}
	return gxleval.FromAny(all)
}

func (e *pureEvaluator) EvalValue(source string, vars map[string]any) (any, error) {
	parsed, err := gxlparser.ParsePure(source)
	if err != nil {
		return nil, err
	}
	scope, err := pureScope(vars)
	if err != nil {
		return nil, err
	}
	value, err := gxleval.EvalBounded(parsed, scope, &e.remaining)
	if err != nil {
		return nil, err
	}
	return capture.ToAny(value), nil
}

func (e *pureEvaluator) Eval(source string, vars map[string]any) (string, error) {
	template, err := gis.ParsePure(source)
	if err != nil {
		return "", err
	}
	scope, err := pureScope(vars)
	if err != nil {
		return "", err
	}
	return template.InterpolateBounded(scope, &e.remaining)
}

func ResolvePureTree(value any, vars map[string]any) (any, error) {
	return resolveTypedValue(&pureEvaluator{remaining: gxlparser.PureNodeVisits}, value, vars)
}

func ValidateStrictValue(value any, kind string, members schema.EnumConstraint) error {
	native, err := pjvm.FromAny(value)
	if err != nil {
		return err
	}
	valid := false
	switch kind {
	case "any":
		valid = true
	case "string":
		valid = native.Kind() == pjvm.KindString
	case "bool", "boolean":
		valid = native.Kind() == pjvm.KindBool
	case "number", "float":
		valid = native.Kind() == pjvm.KindNumber
	case "int", "integer":
		number, ok := native.NumberValue()
		valid = ok && math.Trunc(number) == number
	case "object":
		valid = native.Kind() == pjvm.KindObject
	case "array":
		valid = native.Kind() == pjvm.KindArray
	default:
		return fmt.Errorf("unsupported typed declaration %q", kind)
	}
	if !valid {
		return typedDiagnostic("", kind, value, nil)
	}
	if members != nil {
		if kind != "string" {
			return fmt.Errorf("enum requires string declaration")
		}
		if err, _ := schema.ValidateMembers(members); err != nil {
			return err
		}
		text, _ := native.StringValue()
		for _, member := range members {
			if text == member {
				return nil
			}
		}
		diagnostic := typedDiagnostic("", kind+" enum", value, nil)
		diagnostic.Code = "TYPED-ENUM"
		return diagnostic
	}
	return nil
}

// InitializeBindings resolves and validates the entire ordered declaration list
// without changing the caller's seed map on failure.
func InitializeBindings(bindings []schema.Binding, seed map[string]any) (map[string]any, error) {
	scratch := make(map[string]any, len(seed)+len(bindings))
	for name, value := range seed {
		scratch[name] = value
	}
	available := make(map[string]bool, len(seed))
	for name := range seed {
		available[name] = true
	}
	declared := make(map[string]bool)
	for _, binding := range bindings {
		if declared[binding.Name] || schema.ReservedBindingName(binding.Name) {
			return nil, fmt.Errorf("bindings.%s: duplicate or reserved declaration", binding.Name)
		}
		declared[binding.Name] = true
		delete(available, binding.Name)
	}
	evaluator := &pureEvaluator{remaining: gxlparser.PureNodeVisits}
	for _, binding := range bindings {
		if !binding.ValuePresent {
			return nil, fmt.Errorf("binding %q has no initializer", binding.Name)
		}
		references, err := schema.TypedTreeReferences(binding.Value)
		if err != nil {
			return nil, fmt.Errorf("bindings.%s: %w", binding.Name, err)
		}
		for _, reference := range references {
			if !available[reference] {
				return nil, fmt.Errorf("bindings.%s: dependency %q is unavailable or forward", binding.Name, reference)
			}
		}
		value, err := resolveTypedValue(evaluator, binding.Value, scratch)
		if err == nil {
			err = ValidateStrictValue(value, binding.Type, binding.Enum)
		}
		if err != nil {
			return nil, typedDiagnostic("bindings."+binding.Name, binding.Type, value, err)
		}
		scratch[binding.Name] = value
		available[binding.Name] = true
	}
	return scratch, nil
}

// AssignBindings returns only the writes after every ordered RHS and constraint
// succeeds. No caller-visible binding is mutated during construction.
func AssignBindings(bindings []schema.Binding, assignments []schema.Assignment, vars map[string]any) (map[string]any, error) {
	declarations := make(map[string]schema.Binding, len(bindings))
	for _, binding := range bindings {
		declarations[binding.Name] = binding
	}
	scratch := make(map[string]any, len(vars))
	for name, value := range vars {
		scratch[name] = value
	}
	writes := make(map[string]any, len(assignments))
	evaluator := &pureEvaluator{remaining: gxlparser.PureNodeVisits}
	for _, assignment := range assignments {
		binding, exists := declarations[assignment.Name]
		if !exists || !binding.Mutable {
			return nil, fmt.Errorf("assign.%s: destination is not a declared mutable binding", assignment.Name)
		}
		if !assignment.ValuePresent {
			return nil, fmt.Errorf("assign.%s: value is required", assignment.Name)
		}
		value, err := resolveTypedValue(evaluator, assignment.Value, scratch)
		if err == nil {
			err = ValidateStrictValue(value, binding.Type, binding.Enum)
		}
		if err != nil {
			return nil, typedDiagnostic("assign."+assignment.Name, binding.Type, value, err)
		}
		scratch[assignment.Name], writes[assignment.Name] = value, value
	}
	return writes, nil
}

func typedDiagnostic(path, expected string, value any, cause error) *engine.TypedDiagnostic {
	actual := "unavailable"
	if native, err := pjvm.FromAny(value); err == nil {
		actual = fmt.Sprint(native.Kind())
	}
	return &engine.TypedDiagnostic{Code: "TYPED-VALUE", Path: path, Expected: expected,
		ActualType: actual, Preview: "<redacted>", Cause: cause}
}
