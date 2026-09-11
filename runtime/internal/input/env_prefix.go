package input

import (
	"context"
	"strings"

	inputpkg "github.com/ormasoftchile/yawr/runtime/pkg/input"
)

// NewEnvProviderWithPrefix constructs an EnvProvider that prefixes variable names.
func NewEnvProviderWithPrefix(prefix string) inputpkg.InputProvider {
	if prefix == "" {
		return NewEnvProvider()
	}
	return &prefixedEnvProvider{prefix: prefix, inner: NewEnvProvider()}
}

type prefixedEnvProvider struct {
	prefix string
	inner  *EnvProvider
}

func (p *prefixedEnvProvider) Provide(ctx context.Context, req inputpkg.InputRequest) (*inputpkg.InputResponse, error) {
	if req.Metadata == nil {
		req.Metadata = map[string]string{}
	}
	if req.Metadata["env_var"] == "" && req.VarName != "" {
		name := req.VarName
		if !strings.HasPrefix(name, p.prefix) {
			name = p.prefix + strings.ToUpper(req.VarName)
		}
		req.Metadata["env_var"] = name
	}
	return p.inner.Provide(ctx, req)
}

func (p *prefixedEnvProvider) Name() string {
	return p.inner.Name()
}
