package tool

import (
	"context"
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
)

// AuthProvider acquires bearer tokens for HTTP transports that require authentication.
// Implementations handle acquisition, caching, and refresh internally.
//
// The token returned by Token is an in-memory secret. It must not be written
// to any persistent or observable surface (traces, logs, error messages, step
// output). See B-24.
type AuthProvider interface {
	// Token returns a valid bearer token, refreshing proactively when within
	// five minutes of expiry.
	Token(ctx context.Context) (string, error)

	// Invalidate clears any cached token, forcing re-acquisition on the next
	// Token call. Call after receiving HTTP 401 to handle mid-run token expiry.
	Invalidate()
}

// NewAuthProvider constructs an AuthProvider for the given provider name and scope.
// Recognized provider values: "azure-cli", "managed-identity".
// Returns an error (MCP-002) if the provider name is not recognized.
func NewAuthProvider(provider, scope string) (AuthProvider, error) {
	return NewAuthProviderWithClientID(provider, scope, "")
}

// NewAuthProviderWithClientID constructs an AuthProvider for the given provider
// name, scope, and optional client ID. clientID is only used by the
// "managed-identity" provider to select a user-assigned identity; it is ignored
// by all other providers.
func NewAuthProviderWithClientID(provider, scope, clientID string) (AuthProvider, error) {
	return NewAuthProviderForTarget(provider, scope, "", clientID)
}

// NewAuthProviderForTarget constructs an AuthProvider for either a scope or a
// resource target. scope and resource are mutually exclusive and validated by
// transport config parsing; this function passes through the declared target.
func NewAuthProviderForTarget(provider, scope, resource, clientID string) (AuthProvider, error) {
	switch provider {
	case "azure-cli":
		if resource != "" {
			return NewAzureCLIAuthProviderForResource(resource), nil
		}
		return NewAzureCLIAuthProvider(scope), nil
	case "managed-identity":
		return NewManagedIdentityAuthProvider(scope, clientID), nil
	default:
		return nil, errkit.New("MCP-002", fmt.Sprintf("mcp-http: unknown auth provider %q", provider))
	}
}
