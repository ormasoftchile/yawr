package executor

import (
	"fmt"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func capturesWholeOutputs(step engine.ResolvedStep) bool {
	for _, source := range step.Capture {
		if strings.TrimSpace(source) == "outputs" {
			return true
		}
	}
	return false
}

func projectPublicOutputs(declarations map[string]*schema.ArgDef, raw map[string]any) (map[string]any, error) {
	if len(declarations) == 0 {
		return nil, fmt.Errorf("outputs: a declared semantic output contract is required")
	}
	public := make(map[string]any, len(declarations))
	for name, declaration := range declarations {
		if processLevelOutputs[name] || name == "terminal" || strings.HasPrefix(name, "__") {
			continue
		}
		if declaration == nil {
			return nil, fmt.Errorf("outputs.%s: missing declaration", name)
		}
		source := declaration.From
		if source == "" {
			source = name
		}
		value, found := projectOutputValue(raw, source, true)
		if !found {
			if declaration.Optional {
				continue
			}
			return nil, fmt.Errorf("outputs.%s: required value missing", name)
		}
		if err := ValidateStrictValue(value, declaration.Type, declaration.Enum); err != nil {
			return nil, typedDiagnostic("outputs."+name, declaration.Type, value, err)
		}
		public[name] = value
	}
	return public, nil
}
