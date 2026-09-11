package tool

import (
	"fmt"
	"strconv"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

// applyVSCodeInputAdaptation translates logical tool action args into the
// provider-keyed (MCP) parameter set declared in action.VSCodeInput.
//
// When VSCodeInput is nil or empty, args are returned unchanged so callers
// that shipped before vscode_input existed continue to work without edits.
//
// Semantics (all fail-closed):
//   - required: true and value absent → error before any bridge request.
//   - A logical arg present in the call but not referenced by any mapping's
//     from: → error (silently dropping an arg invokes a different operation).
//   - coerce: integer/number/string/boolean → type conversion; lossy = error.
//   - coerce: absent → pass value through as-is.
func applyVSCodeInputAdaptation(action *toolpkg.ToolAction, args map[string]any) (map[string]any, error) {
	if action == nil || len(action.VSCodeInput) == 0 {
		return args, nil
	}
	return applyMappedMCPInput("vscode-mcp", "vscode_input", action.VSCodeInput, args)
}

func applyMCPInputAdaptation(action *toolpkg.ToolAction, args map[string]any) (map[string]any, error) {
	if action == nil || len(action.MCPInput) == 0 {
		return args, nil
	}
	return applyMappedMCPInput("mcp-http", "mcp_input", action.MCPInput, args)
}

func applyMappedMCPInput(prefix, field string, mappings map[string]*schema.VSCodeInputMapping, args map[string]any) (map[string]any, error) {
	// Build referenced set for unmapped-arg detection.
	referenced := make(map[string]bool, len(mappings))
	for _, mapping := range mappings {
		if mapping != nil {
			referenced[mapping.From] = true
		}
	}

	// Every logical arg supplied by the caller must be mapped. A silently
	// dropped arg means the caller may have invoked a different operation
	// than intended — fail rather than guess.
	for argName := range args {
		if !referenced[argName] {
			return nil, fmt.Errorf("%s: logical arg %q is not mapped to any provider parameter in %s; add a mapping or remove the arg", prefix, argName, field)
		}
	}

	// Build provider-keyed args.
	adapted := make(map[string]any, len(mappings))
	for providerKey, mapping := range mappings {
		if mapping == nil {
			continue
		}
		val, present := args[mapping.From]
		if !present || val == nil {
			if mapping.Required {
				return nil, fmt.Errorf("%s: required arg %q (provider parameter %q) is absent", prefix, mapping.From, providerKey)
			}
			continue
		}
		coerced, err := coerceMCPMappedValue(prefix, val, mapping.Coerce, mapping.From)
		if err != nil {
			return nil, err
		}
		adapted[providerKey] = coerced
	}
	return adapted, nil
}

// coerceMCPMappedValue applies the declared coercion to val. fromName is used
// only in error messages. Lossy conversion is always a hard error.
func coerceMCPMappedValue(prefix string, val any, coerce, fromName string) (any, error) {
	if coerce == "" {
		return val, nil
	}
	switch coerce {
	case "integer":
		return coerceToMappedInteger(prefix, val, fromName)
	case "number":
		return coerceToMappedNumber(prefix, val, fromName)
	case "string":
		return fmt.Sprintf("%v", val), nil
	case "boolean":
		return coerceToMappedBoolean(prefix, val, fromName)
	default:
		// validate_transport.go catches unknown coerce values at scan time;
		// this branch guards against a theoretical bypass.
		return nil, fmt.Errorf("%s: unsupported coerce type %q for arg %q", prefix, coerce, fromName)
	}
}

func coerceToMappedInteger(prefix string, val any, fromName string) (any, error) {
	switch v := val.(type) {
	case int:
		return int64(v), nil
	case int32:
		return int64(v), nil
	case int64:
		return v, nil
	case float32:
		i := int64(v)
		if float32(i) != v {
			return nil, fmt.Errorf("%s: coerce integer: arg %q (%.17g) is not losslessly representable as integer", prefix, fromName, v)
		}
		return i, nil
	case float64:
		i := int64(v)
		if float64(i) != v {
			return nil, fmt.Errorf("%s: coerce integer: arg %q (%.17g) is not losslessly representable as integer", prefix, fromName, v)
		}
		return i, nil
	case string:
		if v == "" {
			return nil, fmt.Errorf("%s: coerce integer: arg %q is empty string, cannot convert to integer", prefix, fromName)
		}
		i, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s: coerce integer: arg %q (%q) cannot be converted to integer: %v", prefix, fromName, v, err)
		}
		return i, nil
	case bool:
		return nil, fmt.Errorf("%s: coerce integer: arg %q (bool) cannot be converted to integer", prefix, fromName)
	default:
		return nil, fmt.Errorf("%s: coerce integer: arg %q (type %T) cannot be converted to integer", prefix, fromName, val)
	}
}

func coerceToMappedNumber(prefix string, val any, fromName string) (any, error) {
	switch v := val.(type) {
	case int:
		return float64(v), nil
	case int32:
		return float64(v), nil
	case int64:
		return float64(v), nil
	case float32:
		return float64(v), nil
	case float64:
		return v, nil
	case string:
		if v == "" {
			return nil, fmt.Errorf("%s: coerce number: arg %q is empty string, cannot convert to number", prefix, fromName)
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil, fmt.Errorf("%s: coerce number: arg %q (%q) cannot be converted to number: %v", prefix, fromName, v, err)
		}
		return f, nil
	default:
		return nil, fmt.Errorf("%s: coerce number: arg %q (type %T) cannot be converted to number", prefix, fromName, val)
	}
}

func coerceToMappedBoolean(prefix string, val any, fromName string) (any, error) {
	switch v := val.(type) {
	case bool:
		return v, nil
	case string:
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("%s: coerce boolean: arg %q (%q) cannot be converted to boolean: %v", prefix, fromName, v, err)
		}
		return b, nil
	default:
		return nil, fmt.Errorf("%s: coerce boolean: arg %q (type %T) cannot be converted to boolean", prefix, fromName, val)
	}
}
