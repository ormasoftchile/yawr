package input

import (
	"context"
	"testing"

	inputpkg "github.com/ormasoftchile/yawr/runtime/pkg/input"
)

func TestVaultProvider_Found(t *testing.T) {
	t.Setenv("YAWR_VAULT_SECRET", "vault-token")
	provider := NewVaultProvider()
	resp, err := provider.Provide(context.Background(), inputpkg.InputRequest{VarName: "secret"})
	if err != nil {
		t.Fatalf("Provide: %v", err)
	}
	if resp == nil || resp.Value != "vault-token" {
		t.Fatalf("expected vault-token, got %#v", resp)
	}
	if resp.Source != "vault" {
		t.Fatalf("expected source vault, got %q", resp.Source)
	}
}

func TestVaultProvider_YawrEnvironment(t *testing.T) {
	provider := NewVaultProvider()
	t.Setenv("YAWR_VAULT_SECRET", "vault-token")
	resp, err := provider.Provide(context.Background(), inputpkg.InputRequest{VarName: "secret"})
	if err != nil || resp == nil || resp.Value != "vault-token" {
		t.Fatalf("Provide() = %#v, %v", resp, err)
	}
}

func TestVaultProvider_NotFound(t *testing.T) {
	t.Setenv("YAWR_VAULT_MISSING", "")
	provider := NewVaultProvider()
	resp, err := provider.Provide(context.Background(), inputpkg.InputRequest{VarName: "missing"})
	if err != nil {
		t.Fatalf("Provide: %v", err)
	}
	if resp != nil {
		t.Fatalf("expected nil response, got %#v", resp)
	}
}

func TestVaultProvider_EmptyValue(t *testing.T) {
	t.Setenv("YAWR_VAULT_EMPTY", "")
	provider := NewVaultProvider()
	resp, err := provider.Provide(context.Background(), inputpkg.InputRequest{VarName: "empty"})
	if err != nil {
		t.Fatalf("Provide: %v", err)
	}
	if resp != nil {
		t.Fatalf("expected nil response, got %#v", resp)
	}
}
