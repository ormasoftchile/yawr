package expr

import (
	"fmt"
	"strings"

	gcpparser "github.com/ormasoftchile/yawr/runtime/pkg/gcp/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/gdp"
	"github.com/ormasoftchile/yawr/runtime/pkg/gis"
	gxleval "github.com/ormasoftchile/yawr/runtime/pkg/gxl/eval"
	gxlparser "github.com/ormasoftchile/yawr/runtime/pkg/gxl/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/pjvm"
)

func evalBoolNative(condition string, vars map[string]any) (bool, error) {
	cond := strings.TrimSpace(condition)
	if cond == "" {
		return true, nil
	}
	scope, err := nativeScope(vars)
	if err != nil {
		return false, err
	}
	ast, err := gxlparser.Parse(cond)
	if err != nil {
		return false, fmt.Errorf("parse condition %q: %w", cond, err)
	}
	value, err := gxleval.Eval(ast, scope)
	if err != nil {
		return false, fmt.Errorf("eval condition %q: %w", cond, err)
	}
	result, ok := value.BoolValue()
	if !ok {
		return false, fmt.Errorf("condition %q: expected bool, got %s", cond, value.Kind())
	}
	return result, nil
}

func interpolateNative(tmpl string, vars map[string]any) (string, error) {
	scope, err := nativeScope(vars)
	if err != nil {
		return "", err
	}
	return gis.Interpolate(tmpl, scope)
}

func evalValueNative(expression string, vars map[string]any) (any, error) {
	scope, err := nativeScope(vars)
	if err != nil {
		return nil, err
	}
	parsed, err := gxlparser.Parse(expression)
	if err != nil {
		return nil, err
	}
	value, err := gxleval.Eval(parsed, scope)
	if err != nil {
		return nil, err
	}
	return pjvmToAny(value), nil
}

func nativeScope(vars map[string]any) (*gxleval.Scope, error) {
	if vars == nil {
		vars = map[string]any{}
	}
	scopeVars := make(map[string]any, len(vars))
	for key, value := range vars {
		scopeVars[key] = value
	}
	return gxleval.FromAny(scopeVars)
}

type captureInput struct {
	Stdout   any
	Stderr   any
	ExitCode any
	Output   map[string]any
}

func resolveCaptureNative(source string, input captureInput) (any, bool, error) {
	path, err := gcpparser.Parse(source)
	if err != nil {
		return nil, false, fmt.Errorf("parse capture path %q: %w", source, err)
	}
	value, err := gcpRootValue(path, input)
	if err != nil {
		return nil, false, err
	}
	if len(path.Segments) > 0 {
		value, err = gdp.Resolve(value, &gdp.Path{Dialect: gdp.DialectGCP, Segments: path.Segments})
		if err != nil {
			return nil, false, fmt.Errorf("resolve capture path %q: %w", source, err)
		}
	}
	return pjvmToAny(value), true, nil
}

func gcpRootValue(path *gdp.Path, input captureInput) (pjvm.Value, error) {
	switch path.Source.Kind {
	case gdp.SourceLocal:
		switch path.Source.Field {
		case "stdout":
			return pjvm.FromAny(input.Stdout)
		case "stderr":
			return pjvm.FromAny(input.Stderr)
		case "exit_code":
			return pjvm.FromAny(input.ExitCode)
		case "json":
			stdout, _ := input.Stdout.(string)
			return pjvm.FromJSON([]byte(stdout))
		case "yaml":
			stdout, _ := input.Stdout.(string)
			return pjvm.FromYAML([]byte(stdout))
		}
	}
	return pjvm.Null(), fmt.Errorf("capture source %q is not available before P7", path.Source.Field)
}

func pjvmToAny(v pjvm.Value) any {
	switch v.Kind() {
	case pjvm.KindNull:
		return nil
	case pjvm.KindBool:
		b, _ := v.BoolValue()
		return b
	case pjvm.KindNumber:
		n, _ := v.NumberValue()
		if intValue := int(n); float64(intValue) == n {
			return intValue
		}
		return n
	case pjvm.KindString:
		s, _ := v.StringValue()
		return s
	case pjvm.KindArray:
		arr, _ := v.ArrayValue()
		out := make([]any, len(arr))
		for i, elem := range arr {
			out[i] = pjvmToAny(elem)
		}
		return out
	case pjvm.KindObject:
		obj, _ := v.ObjectValue()
		out := make(map[string]any, len(obj))
		for key, elem := range obj {
			out[key] = pjvmToAny(elem)
		}
		return out
	default:
		return nil
	}
}
