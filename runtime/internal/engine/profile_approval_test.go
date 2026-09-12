package engine

import (
	"context"
	"testing"

	internalgovernance "github.com/ormasoftchile/yawr/runtime/internal/governance"
	"github.com/ormasoftchile/yawr/runtime/pkg/governance"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func profileForApproval(contextValue schema.ProfileContext, attendance schema.ProfileAttendance) *schema.RuntimeProfile {
	return &schema.RuntimeProfile{
		APIVersion: schema.RuntimeProfileAPIVersion,
		Context:    contextValue,
		Attendance: attendance,
		Approval: schema.ProfileApproval{Scope: schema.ProfileApprovalScope{
			AllowRead: true,
		}},
	}
}

func evaluateApproval(t *testing.T, profile *schema.RuntimeProfile, classification *string, explicit *bool) governance.EvaluationResult {
	t.Helper()
	base := internalgovernance.BuildEvaluator(internalgovernance.NewNoOpApprovalGate())
	result, err := internalgovernance.NewProfileEvaluator(base, profile).Evaluate(context.Background(), governance.StepInfo{
		ID: "step", Kind: "tool", ToolClassification: classification, ToolApprovalTriState: explicit,
		ToolRequiresApproval: explicit != nil && *explicit,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestApprovalRequiresExplicitClassification(t *testing.T) {
	profile := profileForApproval(schema.ProfileContextCLIOperator, schema.ProfileAttendanceAttended)
	for _, classification := range []*string{nil, stringPointer(""), stringPointer("unknown"), stringPointer("unspecified")} {
		result := evaluateApproval(t, profile, classification, nil)
		if !result.Denied || result.RequiresApproval {
			t.Fatalf("classification %v did not fail closed: %#v", classification, result)
		}
	}
}

func TestApprovalRejectsOptOut(t *testing.T) {
	classification := "read-only"
	optOut := false
	result := evaluateApproval(t, profileForApproval(schema.ProfileContextTest, schema.ProfileAttendanceAttended), &classification, &optOut)
	if !result.Denied || result.RequiresApproval {
		t.Fatalf("approval opt-out did not fail closed: %#v", result)
	}
}

func TestApprovalCurrentClassificationMatrix(t *testing.T) {
	readOnly, mutating, destructive := "read-only", "mutating", "destructive"
	attended := profileForApproval(schema.ProfileContextCLIOperator, schema.ProfileAttendanceAttended)
	if result := evaluateApproval(t, attended, &readOnly, nil); result.Denied || result.RequiresApproval {
		t.Fatalf("read-only action not allowed: %#v", result)
	}
	for _, classification := range []*string{&mutating, &destructive} {
		if result := evaluateApproval(t, attended, classification, nil); result.Denied || !result.RequiresApproval {
			t.Fatalf("mutating action did not gate: %#v", result)
		}
	}
}

func TestApprovalExplicitRequirementAlwaysGates(t *testing.T) {
	readOnly := "read-only"
	required := true
	result := evaluateApproval(t, profileForApproval(schema.ProfileContextTest, schema.ProfileAttendanceAttended), &readOnly, &required)
	if result.Denied || !result.RequiresApproval {
		t.Fatalf("explicit approval requirement lost: %#v", result)
	}
}

func TestApprovalWithoutProfileUsesRunbookGovernance(t *testing.T) {
	readOnly := "read-only"
	result := evaluateApproval(t, nil, &readOnly, nil)
	if result.Denied {
		t.Fatalf("runbook governance unexpectedly overridden: %#v", result)
	}
}

func stringPointer(value string) *string { return &value }
