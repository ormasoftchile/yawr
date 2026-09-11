package input

import (
	"context"
	"errors"

	inputpkg "github.com/ormasoftchile/yawr/runtime/pkg/input"
)

// ChainProvider tries providers in order.
type ChainProvider struct {
	providers []inputpkg.InputProvider
}

// NewChainProvider constructs a ChainProvider.
func NewChainProvider(providers ...inputpkg.InputProvider) *ChainProvider {
	return &ChainProvider{providers: providers}
}

// Provide resolves the request by walking the provider chain.
func (p *ChainProvider) Provide(ctx context.Context, req inputpkg.InputRequest) (*inputpkg.InputResponse, error) {
	for _, provider := range p.providers {
		if provider == nil {
			continue
		}
		resp, err := provider.Provide(ctx, req)
		if err != nil {
			if errors.Is(err, ErrNoProvider) {
				continue
			}
			return nil, err
		}
		if resp != nil {
			return resp, nil
		}
	}
	return nil, ErrNoProvider
}

// Name returns the provider identifier.
func (p *ChainProvider) Name() string {
	return "chain"
}
