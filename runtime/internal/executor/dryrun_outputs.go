package executor

import (
	"context"
	"fmt"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// DeclaredOutputs resolves the outputs: contract declared by the action this
// step invokes, without planning, expanding, or dispatching it. It follows the
// same binding resolution Execute uses (scoped bound invocation first, then the
// frozen plan definition, then the runtime registry) so the contract it reports
// is the one the real run would enforce.
//
// A nil map with a nil error means "resolved, but this action declares no
// outputs": every caller treats that as nothing to synthesize.
func (e *ToolExecutor) DeclaredOutputs(ctx context.Context, step engine.ResolvedStep, vars map[string]any) (map[string]*schema.ArgDef, error) {
	spec, ok := step.Spec.(*schema.ToolCallSpec)
	if !ok || spec == nil {
		return nil, fmt.Errorf("tool executor: invalid spec for step %s", step.ID)
	}
	toolName, err := resolveTemplate(e.evaluator, spec.Tool.Name, vars)
	if err != nil {
		return nil, err
	}
	action := spec.Tool.Action
	if action == "" {
		action = "run"
	}
	if action, err = resolveTemplate(e.evaluator, action, vars); err != nil {
		return nil, err
	}
	bound, err := resolveBoundInvocation(ctx, step, toolName, action)
	if err != nil {
		return nil, err
	}
	frozen, err := frozenInvocationDefinition(ctx, toolName, action, bound)
	if err != nil {
		return nil, err
	}
	if frozen != nil {
		if actionDef := frozen.Actions[action]; actionDef != nil {
			return actionDef.Outputs, nil
		}
		return nil, nil
	}
	lookup := e.invocationLookup(bound)
	if lookup == nil {
		return nil, nil
	}
	definition, found := lookup.LookupDef(toolName)
	if !found || definition == nil {
		return nil, nil
	}
	actionDef := definition.Actions[action]
	if actionDef == nil {
		return nil, nil
	}
	return actionDef.Outputs, nil
}

// SynthesizeDeclaredOutputs builds the outputs: payload a non-dispatching mode
// (dry-run) presents in place of a real tool result.
//
// A dry-run never crosses a transport and never expands a substituted action,
// so nothing produces the declared outputs. Without a stand-in, every capture
// reading outputs.<name> fails with GCP-RESOLVE-002 and the step is reported as
// failed, which makes dry-run unusable as a validation gate for any
// tool-bearing runbook. Synthesizing the declaration keeps the capture surface
// identical to a real run while still dispatching nothing.
//
// Declared names get the zero value of their declared type, so a downstream
// comparison sees the type the contract promises rather than a type error. An
// enum-constrained string gets its first declared member: "" is never a legal
// member, and a runbook branching on the enum should see a value from the
// declared domain. captures additionally fills in the intermediate structure
// for nested outputs.<a>.<b> paths, whose leaf types the declaration does not
// describe; those leaves are null, which compares cleanly against any scalar
// (gxl.ebnf 5.2).
func SynthesizeDeclaredOutputs(declared map[string]*schema.ArgDef, captures map[string]string) map[string]any {
	if len(declared) == 0 {
		return nil
	}
	synthesized := make(map[string]any, len(declared))
	for name, declaration := range declared {
		if declaration == nil || declaration.Optional {
			continue
		}
		synthesized[name] = declaredZeroValue(declaration)
	}
	for _, source := range captures {
		fillCapturePath(synthesized, source)
	}
	return synthesized
}

// declaredZeroValue mirrors the type vocabulary coerceOutputAny accepts, so a
// synthesized value would survive the real output contract unchanged.
func declaredZeroValue(declaration *schema.ArgDef) any {
	switch declaration.Type {
	case "object":
		return map[string]any{}
	case "array":
		return []any{}
	case "", "string":
		if len(declaration.Enum) > 0 {
			return declaration.Enum[0]
		}
		return ""
	case "int", "integer":
		return 0
	case "number", "float":
		return float64(0)
	case "bool", "boolean":
		return false
	default:
		return nil
	}
}

// fillCapturePath materializes the objects along a nested outputs.<a>.<b>
// capture path so the capture resolves. Paths using GDP indexing or optional
// segments are left alone: their shape is not inferable from the declaration,
// and inventing one would be a guess rather than a stand-in.
func fillCapturePath(synthesized map[string]any, source string) {
	source = strings.TrimSpace(source)
	if !strings.HasPrefix(source, "outputs.") {
		return
	}
	path := strings.TrimPrefix(source, "outputs.")
	if path == "" || strings.ContainsAny(path, "[?") {
		return
	}
	segments := strings.Split(path, ".")
	cursor := synthesized
	for i, segment := range segments {
		if segment == "" {
			return
		}
		if i == len(segments)-1 {
			if _, exists := cursor[segment]; !exists {
				cursor[segment] = nil
			}
			return
		}
		next, exists := cursor[segment]
		if !exists || next == nil {
			created := map[string]any{}
			cursor[segment] = created
			cursor = created
			continue
		}
		nested, ok := next.(map[string]any)
		if !ok {
			return
		}
		cursor = nested
	}
}
