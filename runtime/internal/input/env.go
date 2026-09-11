package input

import (
	"context"
	"os"

	inputpkg "github.com/ormasoftchile/yawr/runtime/pkg/input"
)

// EnvProvider resolves values from environment variables.
type EnvProvider struct{}

// NewEnvProvider constructs an EnvProvider.
func NewEnvProvider() *EnvProvider {
	return &EnvProvider{}
}

// Provide resolves the request using os.Getenv.
func (p *EnvProvider) Provide(ctx context.Context, req inputpkg.InputRequest) (*inputpkg.InputResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	name := req.VarName
	if req.Metadata != nil {
		if override := req.Metadata["env_var"]; override != "" {
			name = override
		}
	}
	if name == "" {
		return nil, nil
	}
	value := os.Getenv(name)
	if value == "" {
		return nil, nil
	}
	return ensureResponse(req, &inputpkg.InputResponse{
		Value: value,
	}, p.Name()), nil
}

// Name returns the provider identifier.
func (p *EnvProvider) Name() string {
	return "env"
}
