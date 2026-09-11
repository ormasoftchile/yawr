package executor

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/expr"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestDisplayExecutor(t *testing.T) {
	eval := &expr.TemplateEvaluator{}

	t.Run("display step renders content and completes", func(t *testing.T) {
		var buf bytes.Buffer
		exec := NewDisplayExecutor(eval, &buf)

		step := engine.ResolvedStep{
			ID:   "display1",
			Kind: "display",
			Spec: &schema.DisplaySpec{Display: schema.DisplayConfig{
				Content: "Hello, world!",
				Format:  "text",
			}},
		}

		result, err := exec.Execute(context.Background(), step, map[string]any{})

		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if result.Status != engine.StepStatusCompleted {
			t.Errorf("expected status Completed, got %v", result.Status)
		}
		if result.StepID != "display1" {
			t.Errorf("expected StepID display1, got %v", result.StepID)
		}

		output := buf.String()
		if !strings.Contains(output, "Hello, world!") {
			t.Errorf("expected output to contain 'Hello, world!', got %q", output)
		}
	})

	t.Run("display step renders template with vars", func(t *testing.T) {
		var buf bytes.Buffer
		exec := NewDisplayExecutor(eval, &buf)

		step := engine.ResolvedStep{
			ID:   "display2",
			Kind: "display",
			Spec: &schema.DisplaySpec{Display: schema.DisplayConfig{
				Content: "Service: ${service}\nStatus: ${status}",
				Format:  "markdown",
			}},
		}

		vars := map[string]any{
			"service": "api.contoso.com",
			"status":  "healthy",
		}

		result, err := exec.Execute(context.Background(), step, vars)

		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if result.Status != engine.StepStatusCompleted {
			t.Errorf("expected status Completed, got %v", result.Status)
		}

		output := buf.String()
		if !strings.Contains(output, "api.contoso.com") {
			t.Errorf("expected output to contain 'api.contoso.com', got %q", output)
		}
		if !strings.Contains(output, "healthy") {
			t.Errorf("expected output to contain 'healthy', got %q", output)
		}
	})

	t.Run("display with nil spec returns error", func(t *testing.T) {
		var buf bytes.Buffer
		exec := NewDisplayExecutor(eval, &buf)

		step := engine.ResolvedStep{
			ID:   "display3",
			Kind: "display",
			Spec: nil,
		}

		_, err := exec.Execute(context.Background(), step, map[string]any{})

		if err == nil {
			t.Fatal("expected error for nil spec, got none")
		}
	})

	t.Run("display with wrong spec type returns error", func(t *testing.T) {
		var buf bytes.Buffer
		exec := NewDisplayExecutor(eval, &buf)

		step := engine.ResolvedStep{
			ID:   "display4",
			Kind: "display",
			Spec: &schema.CLISpec{}, // Wrong spec type
		}

		_, err := exec.Execute(context.Background(), step, map[string]any{})

		if err == nil {
			t.Fatal("expected error for wrong spec type, got none")
		}
	})

	t.Run("display with template error returns error", func(t *testing.T) {
		var buf bytes.Buffer
		exec := NewDisplayExecutor(eval, &buf)

		step := engine.ResolvedStep{
			ID:   "display5",
			Kind: "display",
			Spec: &schema.DisplaySpec{Display: schema.DisplayConfig{
				Content: "${undefined.nested.field}",
				Format:  "text",
			}},
		}

		_, err := exec.Execute(context.Background(), step, map[string]any{})

		if err == nil {
			t.Fatal("expected error for template evaluation, got none")
		}
	})

	t.Run("display with multiline content", func(t *testing.T) {
		var buf bytes.Buffer
		exec := NewDisplayExecutor(eval, &buf)

		step := engine.ResolvedStep{
			ID:   "display6",
			Kind: "display",
			Spec: &schema.DisplaySpec{Display: schema.DisplayConfig{
				Content: `Health Report
=============

Service 1: OK
Service 2: OK
Service 3: FAIL`,
				Format: "text",
			}},
		}

		result, err := exec.Execute(context.Background(), step, map[string]any{})

		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if result.Status != engine.StepStatusCompleted {
			t.Errorf("expected status Completed, got %v", result.Status)
		}

		output := buf.String()
		if !strings.Contains(output, "Health Report") {
			t.Errorf("expected output to contain 'Health Report', got %q", output)
		}
		if !strings.Contains(output, "Service 3: FAIL") {
			t.Errorf("expected output to contain 'Service 3: FAIL', got %q", output)
		}
	})
}
