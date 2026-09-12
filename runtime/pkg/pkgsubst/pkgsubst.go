// Package pkgsubst implements action-substitution planning
// Given a
// tool action whose execute.kind is "runbook", it resolves the substitute
// runbook's file, validates the input/output contract, detects cycles and
// excessive nesting depth, and composes the effective governance policy per
// Substitution governance
// Composition. It does not itself execute anything — planning is a pure,
// static operation performed before any run starts, matching the corpus's
// "checked statically at plan time" wording throughout tv-pkg-resolve.yaml.
package pkgsubst

import (
	"context"
	"fmt"
	"sort"

	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	parserpkg "github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/pkgpath"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// MaxDepth is the maximum substitution nesting depth
// Nesting
// beyond 4 levels is PKG-028"). Depth 1 is the caller's own directly
// substituted action; each substitute action that is itself substituted
// adds one level.
const MaxDepth = 4

// Frame identifies one substitution call in the active expansion stack, used
// for both cycle detection (PKG-015) and depth accounting (PKG-028).
type Frame struct {
	Package string
	Tool    string
	Action  string
}

func (f Frame) String() string {
	return fmt.Sprintf("%s/%s#%s", f.Package, f.Tool, f.Action)
}

// EffectiveGovernance is the composed policy for a single substitution
// frame.
// Governance Composition table).
type EffectiveGovernance struct {
	RequireApproval bool
	AllowCommands   []string
	DenyCommands    []string
	DenyEnvVars     []string
	Redact          []schema.RedactRule
}

// PlanOptions carries everything Plan needs to resolve and validate one
// substitution frame.
type PlanOptions struct {
	// ToolFilePath is the absolute path of the .tool.yaml declaring action.
	ToolFilePath string
	// PackageRoot is the absolute path of the package root that exports the
	// declaring tool (execute.path's containment root).
	PackageRoot string
	// WorkspaceRoot is the workspace root (unused for execute.path itself,
	// which is always package-internal, but threaded through for callers
	// that also need it for other kinds).
	WorkspaceRoot string
	// Frames is the active expansion stack (not including the frame being
	// planned right now); empty for a top-level, directly-invoked action.
	Frames []Frame
	// CallerGovernance is the caller-effective policy the substitute's own
	// policy composes against: for a top-level action this is the
	// declaring tool's own governance.allow-commands closure; for a nested
	// substitution frame it is the enclosing frame's own EffectiveGovernance.
	CallerGovernance *EffectiveGovernance
	// Parser parses the substitute runbook file using the same
	// structural/semantic validation path as any other runbook
	// (internal/parser.New(platform.Real())), not a raw yaml.Unmarshal.
	Parser parserpkg.Parser
}

// Result is a fully-validated, planned substitution frame.
type Result struct {
	Frame               Frame
	SubstitutePath      string
	SubstituteRunbook   *schema.Runbook
	EffectiveGovernance *EffectiveGovernance
}

// Plan validates and plans the substitution declared by action (named
// actionName, on tool at opts.ToolFilePath). It returns (nil, nil) if the
// action is not a substitution (execute is absent or execute.kind !=
// "runbook") — that is not an error, just a no-op for callers that plan
// every action uniformly.
func Plan(action *schema.ToolAction, actionName string, packageName, toolName string, opts PlanOptions) (*Result, []error) {
	if action == nil || action.Execute == nil || !action.Execute.IsSubstitution() {
		return nil, nil
	}

	frame, frameErrs := validateFrame(actionName, packageName, toolName, opts.Frames)
	if len(frameErrs) > 0 {
		return nil, frameErrs
	}

	resolvedPath, _, perr := pkgpath.ResolveKind(pkgpath.KindExecutePath, opts.ToolFilePath, action.Execute.Path, opts.WorkspaceRoot, opts.PackageRoot)
	if perr != nil {
		return nil, []error{perr}
	}

	if opts.Parser == nil {
		return nil, []error{fmt.Errorf("pkgsubst: PlanOptions.Parser is required")}
	}
	parsed, err := opts.Parser.Parse(context.Background(), resolvedPath)
	if err != nil {
		return nil, []error{errkit.New("PKG-010", fmt.Sprintf(
			"substitute runbook %s: parse error: %v", resolvedPath, err))}
	}
	return finishPlan(action, frame, resolvedPath, parsed.Runbook, opts.CallerGovernance)
}

func PlanFrozen(
	action *schema.ToolAction,
	actionName string,
	packageName string,
	toolName string,
	substitutePath string,
	substitute *schema.Runbook,
	opts PlanOptions,
) (*Result, []error) {
	if action == nil || action.Execute == nil || !action.Execute.IsSubstitution() {
		return nil, nil
	}
	frame, frameErrs := validateFrame(actionName, packageName, toolName, opts.Frames)
	if len(frameErrs) > 0 {
		return nil, frameErrs
	}
	if substitutePath == "" || substitute == nil {
		return nil, []error{fmt.Errorf("pkgsubst: frozen substitute runbook is required")}
	}
	return finishPlan(action, frame, substitutePath, substitute, opts.CallerGovernance)
}

func validateFrame(actionName, packageName, toolName string, frames []Frame) (Frame, []error) {
	frame := Frame{Package: packageName, Tool: toolName, Action: actionName}
	for _, active := range frames {
		if active == frame {
			return Frame{}, []error{errkit.New("PKG-015", fmt.Sprintf(
				"substitution cycle detected: %s already active in %v", frame, frames))}
		}
	}
	if len(frames)+1 > MaxDepth {
		return Frame{}, []error{errkit.New("PKG-028", fmt.Sprintf(
			"substitution nesting depth %d exceeds maximum %d at %s", len(frames)+1, MaxDepth, frame))}
	}
	return frame, nil
}

func finishPlan(
	action *schema.ToolAction,
	frame Frame,
	substitutePath string,
	sub *schema.Runbook,
	callerGovernance *EffectiveGovernance,
) (*Result, []error) {
	var errs []error
	errs = append(errs, validateInputs(action, sub)...)
	errs = append(errs, validateOutputs(action, sub)...)
	if len(errs) > 0 {
		return nil, errs
	}

	eff, gerrs := composeGovernance(callerGovernance, sub.Governance)
	if len(gerrs) > 0 {
		return nil, gerrs
	}

	return &Result{
		Frame:               frame,
		SubstitutePath:      substitutePath,
		SubstituteRunbook:   sub,
		EffectiveGovernance: eff,
	}, nil
}

// validateInputs enforces the exact input-signature contract
// The
// substitute's inputs: key set must exactly match the action's args: key
// set (no extras, no omissions), types must match exactly (no coercion),
// an action-required arg implies a substitute-required input, and every
// substitute input's from: (when set) must be "context" — never
// "prompt"/"env" (PKG-026), since a substitute may only see what the
// caller's evaluated context already contains.
func validateInputs(action *schema.ToolAction, sub *schema.Runbook) []error {
	var errs []error
	seen := make(map[string]bool, len(sub.Inputs))
	for name, in := range sub.Inputs {
		seen[name] = true
		argDef, ok := action.Args[name]
		if !ok {
			errs = append(errs, errkit.New("PKG-013", fmt.Sprintf(
				"substitute input %q is not declared in the action's args: contract", name)))
			continue
		}
		if in.Type != argDef.Type {
			errs = append(errs, errkit.New("PKG-013", fmt.Sprintf(
				"substitute input %q type %q does not match action arg type %q", name, in.Type, argDef.Type)))
		}
		if argDef.Required && !in.Required {
			errs = append(errs, errkit.New("PKG-013", fmt.Sprintf(
				"substitute input %q must be required (action arg %q is required)", name, name)))
		}
		// AR-ENUM-8: an enum-constrained action arg's substitute input
		// must declare the exact same member set (order-insensitive,
		// NFC-normalized) -- no subset/superset allowed either direction.
		// This stays inside PKG-013's catalog (a signature mismatch), not
		// a new ENUM-0xx code.
		if len(argDef.Enum) > 0 || len(in.Enum) > 0 {
			if !schema.SetEqual(argDef.Enum, in.Enum) {
				errs = append(errs, errkit.New("PKG-013", fmt.Sprintf(
					"substitute input %q enum set does not match action arg %q enum set", name, name)))
			}
		}
		if in.From != "" && in.From != "context" {
			errs = append(errs, errkit.New("PKG-026", fmt.Sprintf(
				"substitute input %q declares from: %q; substitute inputs MUST bind only from the caller's evaluated context (from: context), never prompt/env directly", name, in.From)))
		}
	}
	for name := range action.Args {
		if !seen[name] {
			errs = append(errs, errkit.New("PKG-013", fmt.Sprintf(
				"action arg %q has no corresponding substitute input", name)))
		}
	}
	return errs
}

// validateOutputs enforces the exact output-signature contract: the
// substitute's outputs: block must exactly match the action's outputs:
// contract by name and type (PKG-013, no supersets/subsets), and every
// declared output must be producible — approximated here (as in the
// corpus's TV-PKG-SUBST-005 fixtures) by requiring a non-empty value:
// expression; an output with an empty value: can never be produced on any
// terminal path, which is PKG-027.
func validateOutputs(action *schema.ToolAction, sub *schema.Runbook) []error {
	var errs []error
	seen := make(map[string]bool, len(sub.Outputs))
	for name, out := range sub.Outputs {
		seen[name] = true
		wantDef, ok := action.Outputs[name]
		if !ok {
			errs = append(errs, errkit.New("PKG-013", fmt.Sprintf(
				"substitute output %q is not declared in the action's outputs: contract", name)))
			continue
		}
		if out.Type != wantDef.Type {
			errs = append(errs, errkit.New("PKG-013", fmt.Sprintf(
				"substitute output %q type %q does not match action outputs type %q", name, out.Type, wantDef.Type)))
		}
		// AR-ENUM-8: same exact-set rule as inputs, applied to the
		// substitute's own outputs.<name>.enum (S4) against the action's
		// outputs.<name>.enum contract (S2).
		if len(wantDef.Enum) > 0 || len(out.Enum) > 0 {
			if !schema.SetEqual(wantDef.Enum, out.Enum) {
				errs = append(errs, errkit.New("PKG-013", fmt.Sprintf(
					"substitute output %q enum set does not match action output %q enum set", name, name)))
			}
		}
		if out.Optional && wantDef.Required {
			errs = append(errs, errkit.New("PKG-013", fmt.Sprintf(
				"substitute output %q is optional but action output %q is required", name, name)))
		}
		if (out.Value != "" && out.ValueExpr != "") || (out.ValueTreePresent && (out.Value != "" || out.ValueExpr != "")) {
			errs = append(errs, errkit.New("PKG-027", fmt.Sprintf(
				"substitute output %q cannot declare both value and value_expr", name)))
		}
		if out.Value == "" && out.ValueExpr == "" && !out.ValueTreePresent && !out.Optional {
			errs = append(errs, errkit.New("PKG-027", fmt.Sprintf(
				"substitute output %q has no value or value_expr and can never be produced", name)))
		}
	}
	for name := range action.Outputs {
		if !seen[name] {
			errs = append(errs, errkit.New("PKG-013", fmt.Sprintf(
				"action output %q has no corresponding substitute output", name)))
		}
	}
	return errs
}

// composeGovernance implements the composition table
// Substitution governance
// Composition): require_approval = OR, deny_commands/deny_env_vars/redact =
// union, allow_commands = intersection. Any allow_commands entry the
// substitute declares that is absent from the caller-effective set is a
// static widening attempt and is PKG-014 — raised even though the composed
// (intersected) value can never itself widen, because silently dropping the
// extra entry would mask an authoring mistake or a compromised package
// trying to claim more than the caller granted.
func composeGovernance(caller *EffectiveGovernance, subGov *schema.GovernanceConfig) (*EffectiveGovernance, []error) {
	eff := &EffectiveGovernance{}
	if caller != nil {
		eff.RequireApproval = caller.RequireApproval
		eff.DenyCommands = append(eff.DenyCommands, caller.DenyCommands...)
		eff.DenyEnvVars = append(eff.DenyEnvVars, caller.DenyEnvVars...)
		eff.Redact = append(eff.Redact, caller.Redact...)
	}

	var subAllow, subDeny, subDenyEnv []string
	var subRedact []schema.RedactRule
	var subRequireApproval bool
	if subGov != nil {
		subAllow = subGov.AllowCommands
		subDeny = subGov.DenyCommands
		subDenyEnv = subGov.DenyEnvVars
		subRedact = subGov.Redact
		subRequireApproval = subGov.RequireApproval
	}

	eff.RequireApproval = eff.RequireApproval || subRequireApproval
	eff.DenyCommands = unionStrings(eff.DenyCommands, subDeny)
	eff.DenyEnvVars = unionStrings(eff.DenyEnvVars, subDenyEnv)
	eff.Redact = unionRedact(eff.Redact, subRedact)

	var errs []error
	if len(subAllow) > 0 {
		if caller == nil || len(caller.AllowCommands) == 0 {
			// No caller-effective restriction to widen beyond; the
			// substitute's set is simply adopted (intersection with an
			// unrestricted/absent set is the substitute's own set).
			eff.AllowCommands = append([]string(nil), subAllow...)
		} else {
			callerSet := make(map[string]bool, len(caller.AllowCommands))
			for _, c := range caller.AllowCommands {
				callerSet[c] = true
			}
			var widened []string
			var intersection []string
			for _, s := range subAllow {
				if callerSet[s] {
					intersection = append(intersection, s)
				} else {
					widened = append(widened, s)
				}
			}
			if len(widened) > 0 {
				sort.Strings(widened)
				errs = append(errs, errkit.New("PKG-014", fmt.Sprintf(
					"substitute governance widens allow_commands beyond the caller-effective set: %v not in caller allow_commands %v", widened, caller.AllowCommands)))
			}
			eff.AllowCommands = intersection
		}
	} else if caller != nil {
		eff.AllowCommands = append([]string(nil), caller.AllowCommands...)
	}

	if len(errs) > 0 {
		return nil, errs
	}
	return eff, nil
}

func unionStrings(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	var out []string
	for _, s := range a {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, s := range b {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func unionRedact(a, b []schema.RedactRule) []schema.RedactRule {
	seen := make(map[string]bool, len(a)+len(b))
	var out []schema.RedactRule
	for _, r := range append(append([]schema.RedactRule{}, a...), b...) {
		key := r.Pattern + "\x00" + r.Replace
		if !seen[key] {
			seen[key] = true
			out = append(out, r)
		}
	}
	return out
}

// EffectiveGovernanceFromTool derives the top-level caller-effective policy
// from a declaring tool's own governance block, before any substitution
// composition is applied — the starting point for the first (depth-1)
// substitution frame's Plan call.
func EffectiveGovernanceFromTool(g *schema.ToolGovernance) *EffectiveGovernance {
	if g == nil {
		return &EffectiveGovernance{}
	}
	eff := &EffectiveGovernance{
		AllowCommands: append([]string(nil), g.AllowCommands...),
	}
	if g.RequiresApproval != nil {
		eff.RequireApproval = *g.RequiresApproval
	}
	return eff
}
