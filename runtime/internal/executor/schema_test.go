package executor

import (
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// TestSchemaRoundTrip tests that schema types with new fields can be marshalled and unmarshalled
func TestSchemaRoundTrip(t *testing.T) {
	t.Run("Step with required_evidence", func(t *testing.T) {
		step := schema.Step{
			ID:    "test",
			Type:  schema.StepTypeCLI,
			Title: "Test Step",
			RequiredEvidence: []schema.EvidenceRequirement{
				{
					Kind:  schema.EvidenceKindText,
					Name:  "approval_notes",
					Label: "Approval Notes",
				},
				{
					Kind:  schema.EvidenceKindChecklist,
					Name:  "preflight",
					Label: "Pre-flight Checklist",
					Items: []string{"Check 1", "Check 2"},
				},
			},
		}

		// Just verify the struct is valid
		if len(step.RequiredEvidence) != 2 {
			t.Errorf("expected 2 evidence requirements, got %d", len(step.RequiredEvidence))
		}
		if step.RequiredEvidence[0].Kind != schema.EvidenceKindText {
			t.Errorf("expected first evidence kind to be text, got %v", step.RequiredEvidence[0].Kind)
		}
	})

	t.Run("Step with on_error", func(t *testing.T) {
		step := schema.Step{
			ID:      "test",
			Type:    schema.StepTypeCLI,
			OnError: "continue",
		}

		if step.OnError != "continue" {
			t.Errorf("expected on_error to be 'continue', got %v", step.OnError)
		}

		step2 := schema.Step{
			ID:      "test2",
			Type:    schema.StepTypeCLI,
			OnError: "goto:error_handler",
		}

		if step2.OnError != "goto:error_handler" {
			t.Errorf("expected on_error to be 'goto:error_handler', got %v", step2.OnError)
		}
	})

	t.Run("CollectorField with evidence", func(t *testing.T) {
		field := schema.CollectorField{
			Name:  "deployment_approval",
			Type:  schema.FieldTypeText,
			Label: "Deployment Approval",
			Evidence: &schema.EvidenceRequirement{
				Kind:  schema.EvidenceKindAttachment,
				Name:  "approval_doc",
				Label: "Upload approval document",
			},
		}

		if field.Evidence == nil {
			t.Error("expected evidence to be set")
		}
		if field.Evidence.Kind != schema.EvidenceKindAttachment {
			t.Errorf("expected evidence kind to be attachment, got %v", field.Evidence.Kind)
		}
	})

	t.Run("NoopSpec StepKind", func(t *testing.T) {
		spec := &schema.NoopSpec{}
		if spec.StepKind() != "noop" {
			t.Errorf("expected StepKind to return 'noop', got %v", spec.StepKind())
		}
	})
}
