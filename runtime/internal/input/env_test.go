package input

import (
	"context"
	"testing"

	inputpkg "github.com/ormasoftchile/yawr/runtime/pkg/input"
)

func TestEnvProvider_Found(t *testing.T) {
	t.Setenv("YAWR_TEST_ENV", "alpha")
	provider := NewEnvProvider()

	resp, err := provider.Provide(context.Background(), inputpkg.InputRequest{
		StepID:    "step-1",
		VarName:   "YAWR_TEST_ENV",
		Sensitive: true,
	})
	if err != nil {
		t.Fatalf("Provide: %v", err)
	}
	if resp == nil || resp.Value != "alpha" {
		t.Fatalf("expected value alpha, got %#v", resp)
	}
	if resp.Source != "env" {
		t.Fatalf("expected source env, got %q", resp.Source)
	}
	if resp.CacheKey != "step-1:YAWR_TEST_ENV" {
		t.Fatalf("expected cache key, got %q", resp.CacheKey)
	}
	if !resp.Sensitive {
		t.Fatalf("expected sensitive to propagate")
	}
}

func TestEnvProvider_NotFound(t *testing.T) {
	t.Setenv("YAWR_TEST_MISSING", "")
	provider := NewEnvProvider()

	resp, err := provider.Provide(context.Background(), inputpkg.InputRequest{
		VarName: "YAWR_TEST_MISSING",
	})
	if err != nil {
		t.Fatalf("Provide: %v", err)
	}
	if resp != nil {
		t.Fatalf("expected nil response, got %#v", resp)
	}
}

func TestEnvProvider_MetadataOverride(t *testing.T) {
	t.Setenv("YAWR_ENV_OVERRIDE", "beta")
	provider := NewEnvProvider()

	resp, err := provider.Provide(context.Background(), inputpkg.InputRequest{
		VarName: "IGNORED",
		Metadata: map[string]string{
			"env_var": "YAWR_ENV_OVERRIDE",
		},
	})
	if err != nil {
		t.Fatalf("Provide: %v", err)
	}
	if resp == nil || resp.Value != "beta" {
		t.Fatalf("expected value beta, got %#v", resp)
	}
}

func TestEnvProvider_EmptyValue(t *testing.T) {
	t.Setenv("YAWR_ENV_EMPTY", "")
	provider := NewEnvProvider()

	resp, err := provider.Provide(context.Background(), inputpkg.InputRequest{
		VarName: "YAWR_ENV_EMPTY",
	})
	if err != nil {
		t.Fatalf("Provide: %v", err)
	}
	if resp != nil {
		t.Fatalf("expected nil response, got %#v", resp)
	}
}
