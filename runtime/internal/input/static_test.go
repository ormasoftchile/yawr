package input

import (
	"context"
	"testing"

	inputpkg "github.com/ormasoftchile/yawr/runtime/pkg/input"
)

func TestStaticProvider_Found(t *testing.T) {
	provider := NewStaticProvider(map[string]any{
		"token": "abc123",
	})

	resp, err := provider.Provide(context.Background(), inputpkg.InputRequest{
		StepID:  "step-1",
		VarName: "token",
	})
	if err != nil {
		t.Fatalf("Provide: %v", err)
	}
	if resp == nil || resp.Value != "abc123" {
		t.Fatalf("expected value abc123, got %#v", resp)
	}
	if resp.Source != "static" {
		t.Fatalf("expected source static, got %q", resp.Source)
	}
	if resp.CacheKey != "step-1:token" {
		t.Fatalf("expected cache key, got %q", resp.CacheKey)
	}
}

func TestStaticProvider_NotFound(t *testing.T) {
	provider := NewStaticProvider(map[string]any{
		"token": "abc123",
	})

	resp, err := provider.Provide(context.Background(), inputpkg.InputRequest{
		VarName: "missing",
	})
	if err != nil {
		t.Fatalf("Provide: %v", err)
	}
	if resp != nil {
		t.Fatalf("expected nil response, got %#v", resp)
	}
}

func TestStaticProvider_NilMap(t *testing.T) {
	provider := NewStaticProvider(nil)

	resp, err := provider.Provide(context.Background(), inputpkg.InputRequest{
		VarName: "token",
	})
	if err != nil {
		t.Fatalf("Provide: %v", err)
	}
	if resp != nil {
		t.Fatalf("expected nil response, got %#v", resp)
	}
}

func TestStaticProvider_Sensitive(t *testing.T) {
	provider := NewStaticProvider(map[string]any{
		"token": "secret",
	})

	resp, err := provider.Provide(context.Background(), inputpkg.InputRequest{
		VarName:   "token",
		Sensitive: true,
	})
	if err != nil {
		t.Fatalf("Provide: %v", err)
	}
	if resp == nil || !resp.Sensitive {
		t.Fatalf("expected sensitive response, got %#v", resp)
	}
}
