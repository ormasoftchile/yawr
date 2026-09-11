package input

import (
	"bytes"
	"context"
	"strings"
	"testing"

	inputpkg "github.com/ormasoftchile/yawr/runtime/pkg/input"
)

func TestPromptProvider_TrimmedInput(t *testing.T) {
	reader := strings.NewReader("  hello world \n")
	out := &bytes.Buffer{}
	provider := NewPromptProvider(reader, out)

	resp, err := provider.Provide(context.Background(), inputpkg.InputRequest{
		StepID:  "step-1",
		VarName: "message",
	})
	if err != nil {
		t.Fatalf("Provide: %v", err)
	}
	if resp == nil || resp.Value != "hello world" {
		t.Fatalf("expected trimmed value, got %#v", resp)
	}
	if resp.CacheKey != "step-1:message" {
		t.Fatalf("expected cache key, got %q", resp.CacheKey)
	}
}

func TestPromptProvider_EnumValidationPass(t *testing.T) {
	reader := strings.NewReader("b\n")
	provider := NewPromptProvider(reader, nil)

	resp, err := provider.Provide(context.Background(), inputpkg.InputRequest{
		VarName: "letter",
		Schema: map[string]any{
			"enum": []any{"a", "b"},
		},
	})
	if err != nil {
		t.Fatalf("Provide: %v", err)
	}
	if resp == nil || resp.Value != "b" {
		t.Fatalf("expected value b, got %#v", resp)
	}
}

func TestPromptProvider_EnumValidationFail(t *testing.T) {
	reader := strings.NewReader("c\n")
	provider := NewPromptProvider(reader, nil)

	_, err := provider.Provide(context.Background(), inputpkg.InputRequest{
		VarName: "letter",
		Schema: map[string]any{
			"enum": []string{"a", "b"},
		},
	})
	if err == nil {
		t.Fatalf("expected error for invalid enum value")
	}
}

func TestPromptProvider_EnumInvalidSchema(t *testing.T) {
	reader := strings.NewReader("a\n")
	provider := NewPromptProvider(reader, nil)

	_, err := provider.Provide(context.Background(), inputpkg.InputRequest{
		VarName: "letter",
		Schema: map[string]any{
			"enum": "bad",
		},
	})
	if err == nil {
		t.Fatalf("expected error for invalid enum schema")
	}
}

func TestPromptProvider_ContextCancelled(t *testing.T) {
	reader := strings.NewReader("ignored\n")
	provider := NewPromptProvider(reader, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := provider.Provide(ctx, inputpkg.InputRequest{VarName: "value"})
	if err == nil {
		t.Fatalf("expected context error")
	}
}
