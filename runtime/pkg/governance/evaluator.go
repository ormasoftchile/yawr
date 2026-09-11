package governance

import "context"

// StepInfo carries the step metadata the evaluator needs.
// The engine converts ResolvedStep → StepInfo before calling Evaluate.
// This avoids an import from pkg/governance → pkg/engine.
type StepInfo struct {
	ID      string
	Kind    string            // "cli", "tool", etc.
	Command string            // argv[0] for cli steps; empty for non-command steps
	EnvVars map[string]string // environment variables passed to the step
	// ToolRequiresApproval carries per-tool requires-approval governance for
	// tool steps. The evaluator ORs this with the runbook-level policy flag so
	// the two levels compose monotone-increasing: neither can suppress the other.
	ToolRequiresApproval bool

	// ToolApprovalTriState carries the optional explicit approval requirement.
	// False is invalid and is rejected fail-closed.
	ToolApprovalTriState *bool

	// ToolClassification carries the tool action's declared classification:
	// "read-only", "mutating", or "destructive".
	// The ProfileEvaluator applies the counterparty-ratified matrix using this field.
	ToolClassification *string
}

// PolicyEvaluator performs governance pre-flight checks on steps.
// The engine calls Evaluate before invoking any StepExecutor.
//
// Contract:
//   - Returns EvaluationResult with Allowed=true if the step may proceed
//   - Returns EvaluationResult with Denied=true if the step is blocked (deny-wins)
//   - Returns EvaluationResult with RequiresApproval=true if a gate must be cleared
//   - error is reserved for infrastructure failures (evaluator crash, not policy denial)
//   - Deny ALWAYS takes precedence over allow (invariant enforced by all implementations)
type PolicyEvaluator interface {
	Evaluate(ctx context.Context, step StepInfo) (EvaluationResult, error)
}

// EvaluationResult is the outcome of a pre-flight governance check.
// It carries enough information for the engine to act without knowing policy internals.
//
// Interpretation rules (engine MUST follow these):
//  1. If Denied == true: do NOT execute the step. Return StepResult with Status "denied".
//  2. If RequiresApproval == true AND Denied == false: call ApprovalGate before proceeding.
//  3. If Allowed == true AND Denied == false AND RequiresApproval == false: proceed normally.
//  4. Use FilteredEnvVars (not the original) as the step's environment.
//  5. Attach Evidence to the trace event.
type EvaluationResult struct {
	// Allowed is true when the step passes all governance checks.
	Allowed bool `json:"allowed"`

	// Denied is true when a deny rule matched. Deny ALWAYS wins over allow.
	Denied bool `json:"denied"`

	// DenyReason is a human-readable explanation when Denied is true.
	DenyReason string `json:"deny_reason,omitempty"`

	// RequiresApproval is true when the policy or step has require_approval set.
	// Only meaningful when Denied is false.
	RequiresApproval bool `json:"requires_approval,omitempty"`

	// MatchedRules lists every rule that matched during evaluation (both allow and deny).
	MatchedRules []MatchedRule `json:"matched_rules,omitempty"`

	// BlockedEnvVars lists the names of environment variables removed by policy.
	// Names only — never values (values must never appear in traces or logs).
	BlockedEnvVars []string `json:"blocked_env_vars,omitempty"`

	// FilteredEnvVars is the sanitised environment the step should execute with.
	// This is the input EnvVars minus any variables blocked by policy.
	FilteredEnvVars map[string]string `json:"filtered_env_vars,omitempty"`

	// Evidence is the structured governance record to attach to the trace.
	Evidence Evidence `json:"evidence"`
}

// MatchedRule records a single rule match during evaluation.
type MatchedRule struct {
	// RuleID identifies the rule (e.g., "allow:kubectl", "deny:rm").
	RuleID string `json:"rule_id"`

	// Kind is the rule type: "allow", "deny", "env_deny".
	Kind string `json:"kind"`

	// Pattern is the glob or string that matched.
	Pattern string `json:"pattern"`

	// Matched is the actual value that was matched against the pattern.
	Matched string `json:"matched,omitempty"`
}
