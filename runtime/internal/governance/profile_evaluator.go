package governance

import (
	"context"
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/pkg/governance"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// profileEvaluator wraps a base PolicyEvaluator and applies the
// counterparty-ratified classification matrix from a RuntimeProfile.
//
// Resolution contract:
//
//   - nil profile → the base runbook governance evaluator remains authoritative.
//   - ToolApprovalTriState == &false → denied as an unsupported opt-out.
//   - ToolApprovalTriState == &true → explicit requirement: gate always fires.
//   - ToolApprovalTriState == nil → apply the required classification matrix.
//
// Classification matrix (applied only when ToolApprovalTriState is nil):
//
//	classification | test context | attended | unattended
//	read-only      | allow        | allow if scope.AllowRead; else deny
//	mutating       | allow        | gate     | allow if scope.AllowMutating; else deny
//	destructive    | allow        | gate     | allow if scope.AllowDestructive; else deny
type profileEvaluator struct {
	base    governance.PolicyEvaluator
	profile *schema.RuntimeProfile // may be nil
}

// NewProfileEvaluator wraps base with profile-aware routing.
func NewProfileEvaluator(base governance.PolicyEvaluator, profile *schema.RuntimeProfile) governance.PolicyEvaluator {
	return &profileEvaluator{base: base, profile: profile}
}

func (pe *profileEvaluator) Evaluate(ctx context.Context, step governance.StepInfo) (governance.EvaluationResult, error) {
	result, err := pe.base.Evaluate(ctx, step)
	if err != nil || result.Denied {
		return result, err
	}

	if pe.profile == nil {
		return result, nil
	}

	// Only apply the matrix for tool steps that carry the tri-state.
	// Non-tool steps (stepInfo.ToolApprovalTriState == nil with no
	// classification) bypass the matrix and use the base result.
	if step.Kind != "tool" {
		return result, nil
	}

	if step.ToolApprovalTriState != nil && !*step.ToolApprovalTriState {
		result.Denied = true
		result.Allowed = false
		result.RequiresApproval = false
		result.DenyReason = fmt.Sprintf("step %q: requires-approval false is unsupported", step.ID)
		result.Evidence.Outcome = "denied"
		result.Evidence.DenyReason = result.DenyReason
		return result, nil
	}
	if step.ToolApprovalTriState != nil {
		result.RequiresApproval = true
		result.Evidence.Outcome = "approval_required"
		return result, nil
	}

	// ── Apply the classification matrix ────────────────────────────────
	// ToolApprovalTriState is nil here.

	isTest := pe.profile.Context == schema.ProfileContextTest
	isAttended := pe.profile.Attendance == schema.ProfileAttendanceAttended
	scope := pe.profile.Approval.Scope

	classification := ""
	if step.ToolClassification != nil {
		classification = *step.ToolClassification
	}

	switch classification {
	case "read-only":
		if isTest {
			result.RequiresApproval = false
		} else if scope.AllowRead {
			result.RequiresApproval = false
		} else {
			result.Denied = true
			result.DenyReason = fmt.Sprintf("step %q: read-only action not permitted by profile scope (allow_read=false)", step.ID)
			result.Evidence.Outcome = "denied"
			result.Evidence.DenyReason = result.DenyReason
		}

	case "mutating":
		if isTest {
			result.RequiresApproval = false
		} else if isAttended {
			result.RequiresApproval = true
		} else {
			// unattended
			if scope.AllowMutating {
				result.RequiresApproval = false
			} else {
				result.Denied = true
				result.DenyReason = fmt.Sprintf("step %q: mutating action denied in unattended context (allow_mutating=false)", step.ID)
				result.Evidence.Outcome = "denied"
				result.Evidence.DenyReason = result.DenyReason
			}
		}

	case "destructive":
		if isTest {
			result.RequiresApproval = false
		} else if isAttended {
			result.RequiresApproval = true
		} else {
			// unattended
			if scope.AllowDestructive {
				result.RequiresApproval = false
			} else {
				result.Denied = true
				result.DenyReason = fmt.Sprintf("step %q: destructive action denied in unattended context (allow_destructive=false)", step.ID)
				result.Evidence.Outcome = "denied"
				result.Evidence.DenyReason = result.DenyReason
			}
		}

	default:
		result.Denied = true
		result.Allowed = false
		result.RequiresApproval = false
		result.DenyReason = fmt.Sprintf("step %q: explicit action classification is required", step.ID)
		result.Evidence.Outcome = "denied"
		result.Evidence.DenyReason = result.DenyReason
	}

	if !result.Denied && result.RequiresApproval {
		result.Evidence.Outcome = "approval_required"
	} else if !result.Denied {
		result.Evidence.Outcome = "allowed"
	}

	return result, nil
}
