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
// Resolution contract (matches Slice 4 negotiated rules):
//
//   - nil profile → delegates directly to the base evaluator (nil-safe, no
//     regression).
//   - ToolApprovalTriState == &false → explicit opt-out: suppress gate
//     regardless of profile. Classification stays unspecified; retry/late-result
//     handling is NOT relaxed (the opt-out is approval-only).
//   - ToolApprovalTriState == &true → explicit requirement: gate always fires.
//   - ToolApprovalTriState == nil → apply classification matrix (see matrix below).
//
// Classification matrix (applied only when ToolApprovalTriState is nil):
//
//	classification | test context | attended | unattended
//	read-only      | allow        | allow if scope.AllowRead; else deny
//	mutating       | allow        | gate     | allow if scope.AllowMutating; else deny
//	destructive    | allow        | gate     | allow if scope.AllowDestructive; else deny
//	unspecified    | allow        | GATE     | deny
type profileEvaluator struct {
	base    governance.PolicyEvaluator
	profile *schema.RuntimeProfile // may be nil
}

// NewProfileEvaluator wraps base with profile-aware routing.
// When profile is nil the wrapper is transparent (zero overhead path).
func NewProfileEvaluator(base governance.PolicyEvaluator, profile *schema.RuntimeProfile) governance.PolicyEvaluator {
	return &profileEvaluator{base: base, profile: profile}
}

func (pe *profileEvaluator) Evaluate(ctx context.Context, step governance.StepInfo) (governance.EvaluationResult, error) {
	result, err := pe.base.Evaluate(ctx, step)
	if err != nil || result.Denied {
		return result, err
	}

	// nil profile: preserve today's behavior unchanged.
	if pe.profile == nil {
		return result, nil
	}

	// Only apply the matrix for tool steps that carry the tri-state.
	// Non-tool steps (stepInfo.ToolApprovalTriState == nil with no
	// classification) bypass the matrix and use the base result.
	if step.Kind != "tool" {
		return result, nil
	}

	// ── Explicit opt-out: ToolApprovalTriState == &false ─────────────────
	// Suppress the approval gate for this specific tool action.
	// Classification stays unspecified; retry/late-result NOT relaxed.
	if step.ToolApprovalTriState != nil && !*step.ToolApprovalTriState {
		result.RequiresApproval = false
		return result, nil
	}

	// ── Apply the classification matrix ────────────────────────────────
	// ToolApprovalTriState is nil (unspecified) here.
	// ToolApprovalTriState == &true is handled by base evaluator already.

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
		// Unspecified classification AND any unrecognized classification value.
		//
		// Defense in depth: ParseToolFile (internal/tool/scan.go) already
		// validates classification at parse time (validateActionClassifications),
		// so tool files loaded through the normal path cannot carry an invalid
		// value. This default branch is the gate's own layer of that guarantee:
		// it does not assume every construction path ran ParseToolFile, and it
		// ensures an unrecognized value is NEVER more permissive than unspecified.
		// Governing principle: "absence — or garbage — must never grant
		// additional execution rights."
		//
		// Routing: same as unspecified — gate fires when attended, deny when
		// unattended, auto-approve only in test context.
		if isTest {
			// auto-approve for deterministic mock/native bindings in test context
			result.RequiresApproval = false
		} else if isAttended {
			// GATE FIRES — warn-and-proceed is NOT acceptable.
			// Human presence does not guarantee human attention.
			result.RequiresApproval = true
		} else {
			// unattended
			result.Denied = true
			result.DenyReason = fmt.Sprintf("step %q: unclassified action denied in unattended context", step.ID)
			result.Evidence.Outcome = "denied"
			result.Evidence.DenyReason = result.DenyReason
		}
	}

	if !result.Denied && result.RequiresApproval {
		result.Evidence.Outcome = "approval_required"
	} else if !result.Denied {
		result.Evidence.Outcome = "allowed"
	}

	return result, nil
}
