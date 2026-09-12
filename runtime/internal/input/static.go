package input

import (
	"context"

	inputpkg "github.com/ormasoftchile/yawr/runtime/pkg/input"
)

// StaticProvider returns pre-seeded values.
type StaticProvider struct {
	values map[string]any
}

// NewStaticProvider constructs a StaticProvider.
func NewStaticProvider(values map[string]any) *StaticProvider {
	return &StaticProvider{values: values}
}

// Provide resolves the request from the static values map.
func (p *StaticProvider) Provide(ctx context.Context, req inputpkg.InputRequest) (*inputpkg.InputResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.values == nil {
		return nil, nil
	}
	value, ok := p.values[req.VarName]
	if !ok {
		return nil, nil
	}
	return ensureResponse(req, &inputpkg.InputResponse{
		Value: value,
	}, p.Name()), nil
}

// Name returns the provider identifier.
func (p *StaticProvider) Name() string {
	return "static"
}
