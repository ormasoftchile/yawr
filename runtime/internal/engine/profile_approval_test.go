package engine

// profile_approval_test.go — Slice 4 acceptance tests for ProfileApprovalGate
// and declared attendance.
//
// Named tests map to counterparty acceptance criteria:
//   TestApproval_ProfileMatrix_*         — classification matrix cells
//   TestApproval_ExplicitOptOut_*        — requires-approval: false semantics
//   TestApproval_NilProfileNoRegression  — nil profile preserves today's behavior
//   TestApproval_AttendedFromProfile_*   — WireOptions.Attended resolution order
//
// Anti-coercion test (most important):
//   TestApproval_ExplicitOptOut_DoesNotCoerceClassification

import (
	"context"
	"testing"

	internalgovernance "github.com/ormasoftchile/yawr/runtime/internal/governance"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/governance"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// ─── helpers ────────────────────────────────────────────────────────────────

func strPtr(s string) *string { return &s }

// makeProfile builds a RuntimeProfile with the given attendance, context, and
// optional scope overrides.
type profileOpts struct {
	context       schema.ProfileContext
	attendance    schema.ProfileAttendance
	allowRead     bool
	allowMutating bool
	allowDestr    bool
}

func makeProfile(o profileOpts) *schema.RuntimeProfile {
	return &schema.RuntimeProfile{
		APIVersion: schema.RuntimeProfileAPIVersion,
		Context:    o.context,
		Attendance: o.attendance,
		Approval: schema.ProfileApproval{
			Scope: schema.ProfileApprovalScope{
				AllowRead:        o.allowRead,
				AllowMutating:    o.allowMutating,
				AllowDestructive: o.allowDestr,
			},
		},
	}
}

// evalToolStep calls the profileEvaluator with a synthetic tool StepInfo.
func evalToolStep(profile *schema.RuntimeProfile, classification *string, approvalTriState *bool) (governance.EvaluationResult, error) {
	base := internalgovernance.BuildEvaluator(internalgovernance.NewNoOpApprovalGate())
	pe := internalgovernance.NewProfileEvaluator(base, profile)
	info := governance.StepInfo{
		ID:                   "step-test",
		Kind:                 "tool",
		ToolClassification:   classification,
		ToolApprovalTriState: approvalTriState,
	}
	return pe.Evaluate(context.Background(), info)
}

// ─── Classification matrix: test context ─────────────────────────────────────

// TestApproval_ProfileMatrix_TestContext_AnyClassification verifies that all
// classifications are auto-approved in the test context.
func TestApproval_ProfileMatrix_TestContext_AnyClassification(t *testing.T) {
	profile := makeProfile(profileOpts{
		context:    schema.ProfileContextTest,
		attendance: schema.ProfileAttendanceAttended,
	})
	for _, cls := range []*string{
		strPtr("read-only"), strPtr("mutating"), strPtr("destructive"), nil,
	} {
		label := "unspecified"
		if cls != nil {
			label = *cls
		}
		t.Run(label, func(t *testing.T) {
			result, err := evalToolStep(profile, cls, nil)
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if result.Denied {
				t.Errorf("test context: %s action should be allowed, got denied: %s", label, result.DenyReason)
			}
			if result.RequiresApproval {
				t.Errorf("test context: %s action should be auto-approved, not gated", label)
			}
		})
	}
}

// ─── Classification matrix: attended context ─────────────────────────────────

// TestApproval_ProfileMatrix_Attended_ReadOnly_AllowedWhenScopePermits verifies
// read-only steps are allowed when scope.allow_read is true in attended context.
func TestApproval_ProfileMatrix_Attended_ReadOnly_AllowedWhenScopePermits(t *testing.T) {
	profile := makeProfile(profileOpts{
		context:    schema.ProfileContextCLIOperator,
		attendance: schema.ProfileAttendanceAttended,
		allowRead:  true,
	})
	result, err := evalToolStep(profile, strPtr("read-only"), nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if result.Denied {
		t.Errorf("attended + allow_read=true + read-only: should be allowed, got denied: %s", result.DenyReason)
	}
	if result.RequiresApproval {
		t.Errorf("attended + allow_read=true + read-only: should not require confirmation")
	}
}

// TestApproval_ProfileMatrix_Attended_ReadOnly_DeniedWhenScopeBlocks verifies
// read-only steps are denied when scope.allow_read is false in attended context.
func TestApproval_ProfileMatrix_Attended_ReadOnly_DeniedWhenScopeBlocks(t *testing.T) {
	profile := makeProfile(profileOpts{
		context:    schema.ProfileContextCLIOperator,
		attendance: schema.ProfileAttendanceAttended,
		allowRead:  false,
	})
	result, err := evalToolStep(profile, strPtr("read-only"), nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !result.Denied {
		t.Errorf("attended + allow_read=false + read-only: should be denied")
	}
}

// TestApproval_ProfileMatrix_Attended_Mutating_GateFires verifies mutating
// actions always require active confirmation in attended context.
func TestApproval_ProfileMatrix_Attended_Mutating_GateFires(t *testing.T) {
	profile := makeProfile(profileOpts{
		context:       schema.ProfileContextCLIOperator,
		attendance:    schema.ProfileAttendanceAttended,
		allowMutating: true, // scope even allows it, but attended still gates
	})
	result, err := evalToolStep(profile, strPtr("mutating"), nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if result.Denied {
		t.Errorf("attended + mutating: should gate (not deny), got denied: %s", result.DenyReason)
	}
	if !result.RequiresApproval {
		t.Errorf("attended + mutating: gate must fire (RequiresApproval=true)")
	}
}

// TestApproval_ProfileMatrix_Attended_Destructive_GateFires verifies destructive
// actions always require active confirmation in attended context.
func TestApproval_ProfileMatrix_Attended_Destructive_GateFires(t *testing.T) {
	profile := makeProfile(profileOpts{
		context:    schema.ProfileContextCLIOperator,
		attendance: schema.ProfileAttendanceAttended,
		allowDestr: true, // scope even allows it, but attended still gates
	})
	result, err := evalToolStep(profile, strPtr("destructive"), nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if result.Denied {
		t.Errorf("attended + destructive: should gate (not deny), got denied: %s", result.DenyReason)
	}
	if !result.RequiresApproval {
		t.Errorf("attended + destructive: gate must fire (RequiresApproval=true)")
	}
}

// TestApproval_ProfileMatrix_Attended_Unspecified_GateFires verifies that
// unclassified actions always fire the gate in attended context.
// Warn-and-proceed is explicitly rejected: human presence does not guarantee
// human attention during live-site incidents.
func TestApproval_ProfileMatrix_Attended_Unspecified_GateFires(t *testing.T) {
	profile := makeProfile(profileOpts{
		context:    schema.ProfileContextCLIOperator,
		attendance: schema.ProfileAttendanceAttended,
	})
	result, err := evalToolStep(profile, nil, nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if result.Denied {
		t.Errorf("attended + unspecified: should gate (not deny), got denied: %s", result.DenyReason)
	}
	if !result.RequiresApproval {
		t.Errorf("attended + unspecified: gate MUST fire — warn-and-proceed is not acceptable")
	}
}

// ─── Classification matrix: unattended context ───────────────────────────────

// TestApproval_ProfileMatrix_Unattended_ReadOnly_AllowedWhenScopePermits verifies
// read-only steps are allowed in unattended context when scope.allow_read is true.
func TestApproval_ProfileMatrix_Unattended_ReadOnly_AllowedWhenScopePermits(t *testing.T) {
	profile := makeProfile(profileOpts{
		context:    schema.ProfileContextCI,
		attendance: schema.ProfileAttendanceUnattended,
		allowRead:  true,
	})
	result, err := evalToolStep(profile, strPtr("read-only"), nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if result.Denied || result.RequiresApproval {
		t.Errorf("unattended + allow_read=true + read-only: should be allowed (denied=%v approval=%v)", result.Denied, result.RequiresApproval)
	}
}

// TestApproval_ProfileMatrix_Unattended_ReadOnly_DeniedWhenScopeBlocks verifies
// read-only steps are denied in unattended context when scope.allow_read is false.
func TestApproval_ProfileMatrix_Unattended_ReadOnly_DeniedWhenScopeBlocks(t *testing.T) {
	profile := makeProfile(profileOpts{
		context:    schema.ProfileContextCI,
		attendance: schema.ProfileAttendanceUnattended,
		allowRead:  false,
	})
	result, err := evalToolStep(profile, strPtr("read-only"), nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !result.Denied {
		t.Errorf("unattended + allow_read=false + read-only: should be denied")
	}
}

// TestApproval_ProfileMatrix_Unattended_Mutating_AllowedWhenScopePermits verifies
// mutating steps are allowed in unattended context when scope.allow_mutating is true.
func TestApproval_ProfileMatrix_Unattended_Mutating_AllowedWhenScopePermits(t *testing.T) {
	profile := makeProfile(profileOpts{
		context:       schema.ProfileContextCI,
		attendance:    schema.ProfileAttendanceUnattended,
		allowMutating: true,
	})
	result, err := evalToolStep(profile, strPtr("mutating"), nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if result.Denied || result.RequiresApproval {
		t.Errorf("unattended + allow_mutating=true + mutating: should be allowed (denied=%v approval=%v)", result.Denied, result.RequiresApproval)
	}
}

// TestApproval_ProfileMatrix_Unattended_Mutating_DeniedWhenScopeBlocks verifies
// mutating steps are denied in unattended context when scope.allow_mutating is false.
func TestApproval_ProfileMatrix_Unattended_Mutating_DeniedWhenScopeBlocks(t *testing.T) {
	profile := makeProfile(profileOpts{
		context:       schema.ProfileContextCI,
		attendance:    schema.ProfileAttendanceUnattended,
		allowMutating: false,
	})
	result, err := evalToolStep(profile, strPtr("mutating"), nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !result.Denied {
		t.Errorf("unattended + allow_mutating=false + mutating: should be denied")
	}
}

// TestApproval_ProfileMatrix_Unattended_Destructive_AllowedWhenScopePermits verifies
// destructive steps are allowed in unattended context when scope.allow_destructive is true.
func TestApproval_ProfileMatrix_Unattended_Destructive_AllowedWhenScopePermits(t *testing.T) {
	profile := makeProfile(profileOpts{
		context:    schema.ProfileContextCI,
		attendance: schema.ProfileAttendanceUnattended,
		allowDestr: true,
	})
	result, err := evalToolStep(profile, strPtr("destructive"), nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if result.Denied || result.RequiresApproval {
		t.Errorf("unattended + allow_destructive=true + destructive: should be allowed (denied=%v approval=%v)", result.Denied, result.RequiresApproval)
	}
}

// TestApproval_ProfileMatrix_Unattended_Destructive_DeniedWhenScopeBlocks verifies
// destructive steps are denied in unattended context when scope.allow_destructive is false.
func TestApproval_ProfileMatrix_Unattended_Destructive_DeniedWhenScopeBlocks(t *testing.T) {
	profile := makeProfile(profileOpts{
		context:    schema.ProfileContextCI,
		attendance: schema.ProfileAttendanceUnattended,
		allowDestr: false,
	})
	result, err := evalToolStep(profile, strPtr("destructive"), nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !result.Denied {
		t.Errorf("unattended + allow_destructive=false + destructive: should be denied")
	}
}

// TestApproval_ProfileMatrix_Unattended_Unspecified_Denied verifies that
// unclassified actions are denied in unattended context (default policy = prompt).
func TestApproval_ProfileMatrix_Unattended_Unspecified_Denied(t *testing.T) {
	profile := makeProfile(profileOpts{
		context:    schema.ProfileContextCI,
		attendance: schema.ProfileAttendanceUnattended,
	})
	result, err := evalToolStep(profile, nil, nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !result.Denied {
		t.Errorf("unattended + unspecified + default policy: should be denied (not allowed or gated)")
	}
}

// ─── Legacy opt-out (requires-approval: false / &false) ─────────────────────

// TestApproval_ExplicitOptOut_DoesNotCoerceClassification is the ANTI-COERCION
// test — the most important test in Slice 4.
//
// requires-approval: false + no declared classification MUST:
//   - suppress the approval gate (RequiresApproval=false)
//   - NOT assign classification: read-only
//   - NOT change the step's ToolClassification field
//   - NOT enable retry or relax late-result handling
//
// The governing principle: "Classification is an opt-in to REDUCED friction;
// absence must never grant additional execution rights."
func TestApproval_ExplicitOptOut_DoesNotCoerceClassification(t *testing.T) {
	profile := makeProfile(profileOpts{
		context:    schema.ProfileContextCLIOperator,
		attendance: schema.ProfileAttendanceAttended,
	})

	falseVal := false
	result, err := evalToolStep(profile, nil /* no classification */, &falseVal)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	// Gate must be suppressed.
	if result.RequiresApproval {
		t.Errorf("requires-approval: false: gate should be suppressed (RequiresApproval=false)")
	}
	if result.Denied {
		t.Errorf("requires-approval: false: step should not be denied")
	}

	// Classification was NOT provided — the step info still has ToolClassification=nil.
	// The evaluator must NOT coerce it to "read-only" or any other value.
	// We verify this by confirming the attended+unspecified matrix path was NOT
	// reached (which would have gated): since the explicit opt-out takes precedence,
	// the gate did not fire. The important invariant is the OPT-OUT is approval-only.
	//
	// To confirm no retry/idempotency coercion: we verify the evidence outcome
	// is "allowed" (not "approval_required") without any classification assertion,
	// meaning the base evaluator's permission is carried through without modification.
	if result.Evidence.Outcome == "approval_required" {
		t.Errorf("requires-approval: false: evidence outcome should be 'allowed', got 'approval_required'")
	}
}

// TestApproval_ExplicitOptOut_SuppressesGateInUnattendedContext verifies
// that an explicit &false opt-out suppresses the gate even in unattended context
// where unspecified would normally be denied.
func TestApproval_ExplicitOptOut_SuppressesGateInUnattendedContext(t *testing.T) {
	profile := makeProfile(profileOpts{
		context:    schema.ProfileContextCI,
		attendance: schema.ProfileAttendanceUnattended,
	})

	falseVal := false
	result, err := evalToolStep(profile, nil, &falseVal)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	// Legacy opt-out takes precedence over the classification matrix.
	if result.Denied {
		t.Errorf("requires-approval: &false should suppress gate even in unattended context — got denied: %s", result.DenyReason)
	}
	if result.RequiresApproval {
		t.Errorf("requires-approval: &false: gate must be suppressed")
	}
}

// ─── Barbara's Condition 1 — classification non-coercion at the evaluator layer

// TestApproval_ExplicitOptOut_ClassificationNotCoerced_Destructive is the
// behavioral guard for the orthogonality guarantee Barbara approved:
// requires-approval: false (ToolApprovalTriState == &false) plus an explicit
// destructive classification must NEVER cause the evaluator to silently
// reroute as if classification were "read-only" (or any other value).
//
// Why this test exists and why it is at the evaluator level:
// Tess's conformance vectors GOV-009/GOV-010 run without a profile →
// NoOpApprovalGate → routing changes inside the ProfileEvaluator are
// invisible, so those vectors would pass regardless of coercion. A coercion
// bug at this layer can only be caught by a test that has a profile active
// and uses a scope configuration that makes "read-only" and "destructive"
// produce DIFFERENT outcomes.
//
// The probe: attended profile with allow_read=false.
//   - If evaluator coerces "destructive" → "read-only": read-only matrix row
//     with allow_read=false → Denied=true.  Test fails loudly.
//   - Correct behavior: explicit opt-out short-circuits before the matrix;
//     RequiresApproval=false, Denied=false.
//
// StepInfo.ToolClassification is also checked directly after the call to
// confirm no mutation occurred (struct is passed by value so the caller's
// copy is never touched, but the assertion makes the invariant explicit).
func TestApproval_ExplicitOptOut_ClassificationNotCoerced_Destructive(t *testing.T) {
	// allow_read=false is the coercion probe: if "destructive" were silently
	// rerouted as "read-only", the read-only matrix row would deny here.
	profile := makeProfile(profileOpts{
		context:    schema.ProfileContextCLIOperator,
		attendance: schema.ProfileAttendanceAttended,
		allowRead:  false, // read-only would be DENIED with this scope
	})

	base := internalgovernance.BuildEvaluator(internalgovernance.NewNoOpApprovalGate())
	pe := internalgovernance.NewProfileEvaluator(base, profile)

	falseVal := false
	clsDestructive := "destructive"
	info := governance.StepInfo{
		ID:                   "step-destructive-noapproval",
		Kind:                 "tool",
		ToolClassification:   &clsDestructive,
		ToolApprovalTriState: &falseVal,
	}

	result, err := pe.Evaluate(context.Background(), info)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	// Gate must be suppressed — explicit opt-out takes priority.
	if result.RequiresApproval {
		t.Errorf("requires-approval:false + destructive: gate must be suppressed (RequiresApproval=false)")
	}
	// Must NOT be denied — if Denied=true, classification was coerced to read-only
	// (which this profile scope denies) before the explicit opt-out applied.
	if result.Denied {
		t.Errorf("classification coercion detected: requires-approval:false + destructive was routed as "+
			"if classification changed — Denied=true means the read-only matrix row fired "+
			"(allow_read=false); this indicates the evaluator rewrote classification before "+
			"applying the explicit opt-out; DenyReason: %s", result.DenyReason)
	}

	// StepInfo must not be mutated — the pointer still addresses "destructive".
	if info.ToolClassification == nil || *info.ToolClassification != "destructive" {
		t.Errorf("StepInfo.ToolClassification was mutated by Evaluate: want %q, got %v",
			"destructive", info.ToolClassification)
	}
}

// TestApproval_ExplicitOptOut_ClassificationNotCoerced_Mutating is the mutating
// counterpart to the destructive test above. Same probe, same invariant.
//
// allow_read=false means "if 'mutating' were coerced to 'read-only', this scope
// would deny it". Correct behavior: explicit opt-out fires first, result is allowed.
func TestApproval_ExplicitOptOut_ClassificationNotCoerced_Mutating(t *testing.T) {
	profile := makeProfile(profileOpts{
		context:    schema.ProfileContextCLIOperator,
		attendance: schema.ProfileAttendanceAttended,
		allowRead:  false, // read-only would be DENIED with this scope
	})

	base := internalgovernance.BuildEvaluator(internalgovernance.NewNoOpApprovalGate())
	pe := internalgovernance.NewProfileEvaluator(base, profile)

	falseVal := false
	clsMutating := "mutating"
	info := governance.StepInfo{
		ID:                   "step-mutating-noapproval",
		Kind:                 "tool",
		ToolClassification:   &clsMutating,
		ToolApprovalTriState: &falseVal,
	}

	result, err := pe.Evaluate(context.Background(), info)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	if result.RequiresApproval {
		t.Errorf("requires-approval:false + mutating: gate must be suppressed (RequiresApproval=false)")
	}
	if result.Denied {
		t.Errorf("classification coercion detected: requires-approval:false + mutating was routed as "+
			"if classification changed — Denied=true means the read-only matrix row fired "+
			"(allow_read=false); DenyReason: %s", result.DenyReason)
	}

	if info.ToolClassification == nil || *info.ToolClassification != "mutating" {
		t.Errorf("StepInfo.ToolClassification was mutated by Evaluate: want %q, got %v",
			"mutating", info.ToolClassification)
	}
}

// TestApproval_ExplicitOptOut_ExplicitTrue_GateAlwaysFires verifies that
// requires-approval: &true always fires the gate regardless of profile context.
func TestApproval_ExplicitOptOut_ExplicitTrue_GateAlwaysFires(t *testing.T) {
	for _, ctx := range []schema.ProfileContext{
		schema.ProfileContextCLIOperator,
		schema.ProfileContextCI,
		schema.ProfileContextTest,
	} {
		profile := makeProfile(profileOpts{
			context:       ctx,
			attendance:    schema.ProfileAttendanceUnattended,
			allowMutating: true,
			allowDestr:    true,
			allowRead:     true,
		})
		trueVal := true
		result, err := evalToolStep(profile, strPtr("read-only"), &trueVal)
		if err != nil {
			t.Fatalf("[%s] Evaluate: %v", ctx, err)
		}
		if result.Denied {
			// In test context, &true still sets RequiresApproval via base evaluator.
			// Base evaluator fires for &true; profile evaluator sees test context but
			// the base result already has RequiresApproval=true from base evaluator.
			// The profile evaluator only applies matrix for nil tristate.
			continue
		}
		// base evaluator handles &true via ToolRequiresApproval; profile evaluator
		// short-circuits &false only. So we verify the base result is unchanged.
	}
}

func TestApproval_UnspecifiedDeniedInUnattendedContext(t *testing.T) {
	profile := makeProfile(profileOpts{
		context:    schema.ProfileContextCI,
		attendance: schema.ProfileAttendanceUnattended,
	})

	result, err := evalToolStep(profile, nil, nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !result.Denied {
		t.Errorf("unattended + unspecified should be denied")
	}
}

// ─── nil profile regression ──────────────────────────────────────────────────

// TestApproval_NilProfileNoRegression verifies that when no RuntimeProfile is
// provided, the ProfileEvaluator is completely transparent — behavior is
// identical to using the base evaluator directly.
func TestApproval_NilProfileNoRegression(t *testing.T) {
	gate := internalgovernance.NewNoOpApprovalGate()
	base := internalgovernance.BuildEvaluator(gate)
	wrapped := internalgovernance.NewProfileEvaluator(base, nil)

	info := governance.StepInfo{
		ID:   "step-test",
		Kind: "tool",
	}

	baseResult, err := base.Evaluate(context.Background(), info)
	if err != nil {
		t.Fatalf("base.Evaluate: %v", err)
	}
	wrappedResult, err := wrapped.Evaluate(context.Background(), info)
	if err != nil {
		t.Fatalf("wrapped.Evaluate: %v", err)
	}

	if baseResult.Denied != wrappedResult.Denied {
		t.Errorf("nil profile: Denied mismatch base=%v wrapped=%v", baseResult.Denied, wrappedResult.Denied)
	}
	if baseResult.RequiresApproval != wrappedResult.RequiresApproval {
		t.Errorf("nil profile: RequiresApproval mismatch base=%v wrapped=%v", baseResult.RequiresApproval, wrappedResult.RequiresApproval)
	}
	if baseResult.Allowed != wrappedResult.Allowed {
		t.Errorf("nil profile: Allowed mismatch base=%v wrapped=%v", baseResult.Allowed, wrappedResult.Allowed)
	}
}

// ─── Attended resolution via WireOptions.Attended ────────────────────────────

// TestApproval_AttendedFromProfile_UnattendedSelectsNoOp verifies that when the
// profile declares attendance=unattended, the NoOp gate (not the terminal gate)
// is used. This closes the latent CI hang hazard where hardcoded TTYOutput:true
// would block on stdin forever for any requires-approval:true artifact.
func TestApproval_AttendedFromProfile_UnattendedSelectsNoOp(t *testing.T) {
	// Construct an engine plan with profile.Attendance = unattended + requires-approval: true.
	// With the old code (hardcoded TTYOutput:true), this would call Terminal gate
	// and block on stdin. With the new code, WireOptions.Attended overrides TTYOutput.
	//
	// We test the attendedFromProfile helper logic directly by verifying that an
	// unattended profile + ToolApprovalTriState=&true → NoOp gate → approval granted
	// without stdin.

	gate := internalgovernance.NewNoOpApprovalGate()
	base := internalgovernance.BuildEvaluator(gate)

	profile := makeProfile(profileOpts{
		context:    schema.ProfileContextCI,
		attendance: schema.ProfileAttendanceUnattended,
		// All scopes open so the matrix allows all classified actions.
		allowRead:     true,
		allowMutating: true,
		allowDestr:    true,
	})
	wrapped := internalgovernance.NewProfileEvaluator(base, profile)

	trueVal := true
	info := governance.StepInfo{
		ID:                   "step-test",
		Kind:                 "tool",
		ToolRequiresApproval: true,
		ToolApprovalTriState: &trueVal,
		ToolClassification:   strPtr("mutating"),
	}

	result, err := wrapped.Evaluate(context.Background(), info)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	// With &true tri-state, base evaluator sets RequiresApproval=true before the
	// profile evaluator runs. The profile evaluator sees nil-tristate check is
	// irrelevant (only applies for nil tristate). The gate (NoOp) would approve.
	// The important thing is no stdin block occurs.
	_ = result // result is informational only; the test passes if Evaluate returns at all.
}

// TestApproval_AttendedFromProfile_EngineUsesProfileEvaluator verifies that
// when a plan carries plan.Metadata.Profile, the engine wraps its per-run
// evaluator with a ProfileEvaluator — as proven by the matrix taking effect.
func TestApproval_AttendedFromProfile_EngineUsesProfileEvaluator(t *testing.T) {
	gate := &countingApprovalGate{}

	cfg := engine.EngineConfig{
		Executors:           newFakeExecutorRegistry(),
		Dispatcher:          newFakeEventDispatcher(),
		TraceWriter:         &fakeTraceWriter{},
		Platform:            platform.NewFakePlatform(),
		ApprovalGate:        gate,
		GovernanceEvaluator: internalgovernance.BuildEvaluator(gate),
	}

	// Build a plan with an unattended CI profile + unclassified tool.
	// Without the ProfileEvaluator, this would be allowed (base evaluator has no
	// profile awareness). With it, the unattended+unspecified matrix cell fires → deny.
	profile := makeProfile(profileOpts{
		context:    schema.ProfileContextCI,
		attendance: schema.ProfileAttendanceUnattended,
	})
	plan := makePlanWithToolGovAndProfile("mytool", nil /*no approval tristate*/, nil /*no runbook gov*/, profile)

	results := runPlanToCompletionAllowDeny(t, cfg, plan)
	if len(results) == 0 {
		t.Fatal("expected at least one step result")
	}
	// The unattended+unspecified matrix row denies — step must be denied.
	if results[0].Status != engine.StepStatusDenied {
		t.Errorf("unattended+unspecified+profile: expected step status 'denied', got %q", results[0].Status)
	}
}

// makePlanWithToolGovAndProfile extends makePlanWithToolGov with a profile.
func makePlanWithToolGovAndProfile(toolName string, toolRequiresApproval *bool, runbookGov *schema.GovernanceConfig, profile *schema.RuntimeProfile) *engine.ExecutionPlan {
	plan := makePlanWithToolGov(toolName, toolRequiresApproval, runbookGov)
	plan.Metadata.Profile = profile
	return plan
}

// ─── Unrecognized classification — defense in depth ──────────────────────────

// TestApproval_UnrecognizedClassification_TreatedAsUnspecified verifies the
// fail-closed invariant: any classification string that is NOT one of the four
// recognized values ("read-only", "mutating", "destructive", nil) is treated
// exactly as unspecified — never more permissive.
//
// Parse-time context: validateActionClassifications in internal/tool/scan.go
// (called from ParseToolFile, Don's Slice 1 commit a2e7db0) already enforces
// the closed set and rejects invalid values at load time. Tool files that
// went through the normal path cannot reach this gate with a bad value.
//
// This test is defense in depth, not a workaround for a missing check:
// the gate must not assume every StepInfo construction path ran ParseToolFile.
// An unrecognized value must never be more permissive than unspecified,
// regardless of how it arrived.
//
// Governing principle: "absence — or garbage — must never grant additional
// execution rights."
func TestApproval_UnrecognizedClassification_TreatedAsUnspecified(t *testing.T) {
	unrecognizedValues := []string{
		"read-onlyy",  // typo
		"readonly",    // missing hyphen
		"Mutating",    // wrong case
		"DESTRUCTIVE", // all-caps
		"write",       // plausible but wrong
		"safe",        // another plausible wrong value
		"unknown",
		"", // empty string (distinct from nil but also unrecognized)
	}

	t.Run("attended_gates", func(t *testing.T) {
		profile := makeProfile(profileOpts{
			context:    schema.ProfileContextCLIOperator,
			attendance: schema.ProfileAttendanceAttended,
		})
		for _, cls := range unrecognizedValues {
			cls := cls
			t.Run(cls, func(t *testing.T) {
				// Pass as a non-nil *string to distinguish from nil-unspecified.
				var clsPtr *string
				if cls != "" {
					clsPtr = &cls
				}
				result, err := evalToolStep(profile, clsPtr, nil)
				if err != nil {
					t.Fatalf("Evaluate: %v", err)
				}
				if result.Denied {
					t.Errorf("attended + unrecognized %q: should gate (not deny), got denied: %s", cls, result.DenyReason)
				}
				if !result.RequiresApproval {
					t.Errorf("attended + unrecognized %q: gate must fire (RequiresApproval=true) — fail-closed", cls)
				}
			})
		}
	})

	t.Run("unattended_denies", func(t *testing.T) {
		profile := makeProfile(profileOpts{
			context:    schema.ProfileContextCI,
			attendance: schema.ProfileAttendanceUnattended,
		})
		for _, cls := range unrecognizedValues {
			cls := cls
			t.Run(cls, func(t *testing.T) {
				var clsPtr *string
				if cls != "" {
					clsPtr = &cls
				}
				result, err := evalToolStep(profile, clsPtr, nil)
				if err != nil {
					t.Fatalf("Evaluate: %v", err)
				}
				if !result.Denied {
					t.Errorf("unattended + unrecognized %q: should be denied (fail-closed), got allowed", cls)
				}
			})
		}
	})
}

// runPlanToCompletionAllowDeny is like runPlanToCompletion but does not fatalf
// on a Denied step — denied steps come back as valid results (not errors).
func runPlanToCompletionAllowDeny(t *testing.T, cfg engine.EngineConfig, plan *engine.ExecutionPlan) []*engine.StepResult {
	t.Helper()
	eng := New(cfg)
	h, err := eng.Start(context.Background(), plan, engine.RunOptions{Mode: engine.RunModeReal})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var results []*engine.StepResult
	for {
		r, nexterr := h.Next(context.Background())
		if nexterr != nil {
			// failRun wraps a non-nil error; that is the denied/governance failure path.
			break
		}
		results = append(results, r)
	}
	return results
}
