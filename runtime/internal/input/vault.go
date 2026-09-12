package input

import (
	"context"
	"os"
	"strings"

	inputpkg "github.com/ormasoftchile/yawr/runtime/pkg/input"
)

// VaultProvider resolves values from vault.
// TODO: replace with real vault client in future release
type VaultProvider struct{}

// NewVaultProvider constructs a VaultProvider.
func NewVaultProvider() *VaultProvider {
	return &VaultProvider{}
}

// Provide resolves the request from YAWR_VAULT_{VARNAME}.
func (p *VaultProvider) Provide(ctx context.Context, req inputpkg.InputRequest) (*inputpkg.InputResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.VarName == "" {
		return nil, nil
	}
	suffix := strings.ToUpper(req.VarName)
	value := os.Getenv("YAWR_VAULT_" + suffix)
	if value == "" {
		return nil, nil
	}
	return ensureResponse(req, &inputpkg.InputResponse{
		Value: value,
	}, p.Name()), nil
}

// Name returns the provider identifier.
func (p *VaultProvider) Name() string {
	return "vault"
}
