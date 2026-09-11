package governance

import (
	"context"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/governance"
)

// evaluator is the concrete PolicyEvaluator implementation.
type evaluator struct {
	policy          governance.GovernancePolicy
	approvalGate    governance.ApprovalGate
	requireApproval bool
}

// NewEvaluator constructs a PolicyEvaluator from a policy and an approval gate.
// The requireApproval flag is extracted from the policy at construction time.
func NewEvaluator(pol governance.GovernancePolicy, gate governance.ApprovalGate) governance.PolicyEvaluator {
	// Extract requireApproval from the policy if it's our concrete type
	requireApproval := false
	if p, ok := pol.(*policy); ok {
		requireApproval = p.requireApproval
	}

	return &evaluator{
		policy:          pol,
		approvalGate:    gate,
		requireApproval: requireApproval,
	}
}

// Evaluate performs a pre-flight governance check on a step.
// Evaluation order: deny → allow → env-filter → approval → build Evidence.
func (e *evaluator) Evaluate(ctx context.Context, step governance.StepInfo) (governance.EvaluationResult, error) {
	result := governance.EvaluationResult{
		Allowed:         false,
		Denied:          false,
		MatchedRules:    []governance.MatchedRule{},
		BlockedEnvVars:  []string{},
		FilteredEnvVars: make(map[string]string),
		Evidence: governance.Evidence{
			EvaluatedAt:    time.Now().UTC().Format(time.RFC3339Nano),
			StepID:         step.ID,
			PolicyApplied:  "runbook:governance",
			RulesEvaluated: 0,
			RulesMatched:   []governance.MatchedRule{},
			Outcome:        "allowed",
		},
	}

	// Non-CLI steps (empty Command): skip command checks, still filter env vars
	if step.Command != "" {
		// 1. Check deny list first (deny-wins)
		allowed, matchedRule := e.policy.CheckCommand(step.Command)
		result.Evidence.RulesEvaluated++

		if !allowed {
			// Command denied
			result.Denied = true
			result.DenyReason = "command " + step.Command + " is denied by policy"
			result.Evidence.Outcome = "denied"
			result.Evidence.DenyReason = result.DenyReason

			if matchedRule != "" {
				matched := governance.MatchedRule{
					RuleID:  matchedRule,
					Kind:    "deny",
					Pattern: matchedRule,
					Matched: step.Command,
				}
				result.MatchedRules = append(result.MatchedRules, matched)
				result.Evidence.RulesMatched = append(result.Evidence.RulesMatched, matched)
			}

			return result, nil
		}

		if matchedRule != "" {
			matched := governance.MatchedRule{
				RuleID:  matchedRule,
				Kind:    "allow",
				Pattern: matchedRule,
				Matched: step.Command,
			}
			result.MatchedRules = append(result.MatchedRules, matched)
			result.Evidence.RulesMatched = append(result.Evidence.RulesMatched, matched)
		}

		result.Allowed = true
	} else {
		// Non-command step: always allowed from command perspective
		result.Allowed = true
	}

	// 2. Filter environment variables
	if step.EnvVars != nil {
		filtered, blocked := e.policy.FilterEnvVars(step.EnvVars)
		result.FilteredEnvVars = filtered
		result.BlockedEnvVars = blocked
		result.Evidence.BlockedEnvVars = blocked

		if len(blocked) > 0 {
			result.Evidence.RulesEvaluated++
			for _, varName := range blocked {
				matched := governance.MatchedRule{
					RuleID:  "env_deny",
					Kind:    "env_deny",
					Pattern: "*",
					Matched: varName,
				}
				result.MatchedRules = append(result.MatchedRules, matched)
				result.Evidence.RulesMatched = append(result.Evidence.RulesMatched, matched)
			}
		}
	} else {
		result.FilteredEnvVars = make(map[string]string)
	}

	// 3. Check if approval is required (only if not denied)
	if !result.Denied && (e.requireApproval || step.ToolRequiresApproval) {
		result.RequiresApproval = true
		result.Evidence.Outcome = "approval_required"
	}

	return result, nil
}
