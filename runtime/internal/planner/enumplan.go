package planner

// Plan-time enum-constraint enforcement (AR-ENUM-1..15, ENUM-006/007) and
// ValidatedPlan enum metadata carriage (AR-ENUM-10, C1 redaction). Runs as
// part of validatePlan (validate.go), after GXL/GIS/GCP field validation,
// against the four declaration sites: tool action args.<name> (S1), tool
// action outputs.<name> (S2), runbook inputs.<name> (S3), runbook
// outputs.<name> (S4).

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// validateEnumConstraints populates vp.EnumConstraints with every
// enum-constrained declaration's metadata (AR-ENUM-10) and raises
// ENUM-006 (default not a member) and ENUM-007 (statically-known literal
// bound value not a member) plan-time errors.
func validateEnumConstraints(vp *engine.ValidatedPlan, errs *[]engine.PlanValidationError, plan *engine.ExecutionPlan) {
	vp.EnumConstraints = make(map[string]engine.EnumMeta)

	redactPatterns := compiledRedactPatterns(plan)

	// S3: runbook inputs.<name>.
	for _, name := range sortedKeys(plan.Inputs) {
		in := plan.Inputs[name]
		if in == nil || len(in.Enum) == 0 {
			continue
		}
		key := "inputs." + name
		recordEnumMeta(vp, key, in.Enum, isRedactedName(name, redactPatterns))
		if in.Default != nil {
			defStr := fmt.Sprint(in.Default)
			// GIS-interpolated defaults are checked at runtime only
			// (AR-ENUM-7: "Explicitly NOT plan time: any value containing
			// GIS interpolation"), mirroring the ENUM-007 literal-check
			// skip below.
			if s, ok := in.Default.(string); ok && strings.Contains(s, "${") {
				// skip: runtime-only
			} else if !in.Enum.Contains(defStr) {
				*errs = append(*errs, validationErr("", key, defStr, errkit.New("ENUM-006",
					fmt.Sprintf("input %q default %q is not a declared enum member", name, defStr))))
			}
		}
	}

	// S4: runbook outputs.<name> (Output has no default: field).
	for _, name := range sortedKeys(plan.Outputs) {
		out := plan.Outputs[name]
		if out == nil || len(out.Enum) == 0 {
			continue
		}
		recordEnumMeta(vp, "outputs."+name, out.Enum, isRedactedName(name, redactPatterns))
	}

	// S1/S2: tool action args.<name> / outputs.<name>.
	for _, toolName := range sortedToolKeys(plan.Tools) {
		toolDef := plan.Tools[toolName]
		if toolDef == nil {
			continue
		}
		for _, actionName := range sortedActionKeys(toolDef.Actions) {
			action := toolDef.Actions[actionName]
			if action == nil {
				continue
			}
			for _, argName := range sortedKeys(action.Args) {
				arg := action.Args[argName]
				if arg == nil || len(arg.Enum) == 0 {
					continue
				}
				key := fmt.Sprintf("tool.%s.%s.args.%s", toolName, actionName, argName)
				recordEnumMeta(vp, key, arg.Enum, isRedactedName(argName, redactPatterns))
				if arg.Default != nil {
					defStr := fmt.Sprint(arg.Default)
					if s, ok := arg.Default.(string); ok && strings.Contains(s, "${") {
						// GIS-interpolated: runtime-only check (AR-ENUM-7).
					} else if !arg.Enum.Contains(defStr) {
						*errs = append(*errs, validationErr("", key, defStr, errkit.New("ENUM-006",
							fmt.Sprintf("tool %q action %q arg %q default %q is not a declared enum member", toolName, actionName, argName, defStr))))
					}
				}
			}
			for _, outName := range sortedKeys(action.Outputs) {
				out := action.Outputs[outName]
				if out == nil || len(out.Enum) == 0 {
					continue
				}
				key := fmt.Sprintf("tool.%s.%s.outputs.%s", toolName, actionName, outName)
				recordEnumMeta(vp, key, out.Enum, isRedactedName(outName, redactPatterns))
				// S2 (tool action outputs.<name>.default, AR-ENUM-6: "Applies
				// at S1, S3, S4 and to a substituted action's declared
				// output default"): a substitute's declared output default
				// not itself a member is ENUM-006 at plan time, independent
				// of the substitute's own runtime ENUM-009 check.
				if out.Default != nil {
					defStr := fmt.Sprint(out.Default)
					if s, ok := out.Default.(string); ok && strings.Contains(s, "${") {
						// GIS-interpolated: runtime-only check (AR-ENUM-7).
					} else if !out.Enum.Contains(defStr) {
						*errs = append(*errs, validationErr("", key, defStr, errkit.New("ENUM-006",
							fmt.Sprintf("tool %q action %q output %q default %q is not a declared enum member", toolName, actionName, outName, defStr))))
					}
				}
			}
		}
	}

	// ENUM-007: every statically-known literal (no GIS interpolation)
	// step.tool.args value bound to an enum-constrained arg.
	for _, step := range plan.Steps {
		spec, ok := step.Spec.(*schema.ToolCallSpec)
		if !ok || spec == nil {
			continue
		}
		toolDef, ok := plan.Tools[spec.Tool.Name]
		if !ok || toolDef == nil {
			continue
		}
		action, ok := toolDef.Actions[spec.Tool.Action]
		if !ok || action == nil {
			continue
		}
		for argName, val := range spec.Tool.Args {
			argDef, ok := action.Args[argName]
			if !ok || argDef == nil || len(argDef.Enum) == 0 {
				continue
			}
			s, ok := val.(string)
			if !ok || strings.Contains(s, "${") {
				// Not a static string literal (GIS-interpolated values
				// are checked at runtime only, AR-ENUM-7).
				continue
			}
			if !argDef.Enum.Contains(s) {
				*errs = append(*errs, validationErr(step.ID, fmt.Sprintf("tool.args.%s", argName), s, errkit.New("ENUM-007",
					fmt.Sprintf("step %q: tool arg %q literal %q is not a declared enum member", step.ID, argName, s))))
			}
		}
	}
}

func recordEnumMeta(vp *engine.ValidatedPlan, key string, enum schema.EnumConstraint, redacted bool) {
	meta := engine.EnumMeta{MemberCount: len(enum)}
	if redacted {
		meta.Redacted = true
	} else {
		meta.Members = append([]string(nil), enum...)
	}
	vp.EnumConstraints[key] = meta
}

// compiledRedactPatterns compiles plan.Governance's redaction patterns
// (if any) for name-matching. plan.Governance may be nil (no governance
// configured) or a policy type whose RedactionPatterns() returns nil.
func compiledRedactPatterns(plan *engine.ExecutionPlan) []*regexp.Regexp {
	if plan == nil || plan.Governance == nil {
		return nil
	}
	patterns := plan.Governance.RedactionPatterns()
	out := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		if p == nil || p.Pattern == "" {
			continue
		}
		if re, err := regexp.Compile(p.Pattern); err == nil {
			out = append(out, re)
		}
	}
	return out
}

// isRedactedName reports whether declName matches any compiled redaction
// pattern (C1): a best-effort proxy for "tool arg with redact: true" /
// "governance.sensitive_inputs" / "governance.redact regex rule" -- the
// only redaction primitive this codebase actually models today is
// governance.RedactionPattern (a value-scrubbing regex), so name-matching
// against it is the closest available signal short of adding a new
// per-declaration schema field, which is out of this MVP's scope.
func isRedactedName(declName string, patterns []*regexp.Regexp) bool {
	for _, re := range patterns {
		if re.MatchString(declName) {
			return true
		}
	}
	return false
}

func sortedToolKeys(m map[string]*schema.ToolDef) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedActionKeys(m map[string]*schema.ToolAction) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
