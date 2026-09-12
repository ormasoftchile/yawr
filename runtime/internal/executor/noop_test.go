package executor

import (
	"context"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/expr"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestNoopExecutor(t *testing.T) {
	eval := &expr.TemplateEvaluator{}
	exec := NewNoopExecutor(eval)

	t.Run("noop step completes successfully", func(t *testing.T) {
		step := engine.ResolvedStep{
			ID:   "noop1",
			Kind: "noop",
			Spec: &schema.NoopSpec{},
		}

		result, err := exec.Execute(context.Background(), step, map[string]any{})

		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if result.Status != engine.StepStatusCompleted {
			t.Errorf("expected status Completed, got %v", result.Status)
		}
		if result.StepID != "noop1" {
			t.Errorf("expected StepID noop1, got %v", result.StepID)
		}
	})

	t.Run("noop with nil spec returns error", func(t *testing.T) {
		step := engine.ResolvedStep{
			ID:   "noop2",
			Kind: "noop",
			Spec: nil,
		}

		_, err := exec.Execute(context.Background(), step, map[string]any{})

		if err == nil {
			t.Fatal("expected error for nil spec, got none")
		}
	})

	t.Run("noop with wrong spec type returns error", func(t *testing.T) {
		step := engine.ResolvedStep{
			ID:   "noop3",
			Kind: "noop",
			Spec: &schema.CLISpec{}, // Wrong spec type
		}

		_, err := exec.Execute(context.Background(), step, map[string]any{})

		if err == nil {
			t.Fatal("expected error for wrong spec type, got none")
		}
	})
}
