package tool

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

// TokenGate is the single policy decision point for bearer-token attachment.
// It encapsulates the host allow-list check (B-32), token acquisition,
// Authorization header mutation, and mcp/authAttached audit emission (B-27).
//
// Every token attachment MUST go through AttachToken. This ensures the
// allow-list is always enforced and the audit trail is always complete.
//
// A nil *TokenGate is safe to use.
type TokenGate struct {
	provider     AuthProvider
	targetKind   string
	targetValue  string
	allowedHosts []string // case-insensitive; matched against req.URL.Hostname()
}

// NewTokenGate constructs a TokenGate for a scope target. provider and scope
// come from AuthConfig; allowedHosts is AuthConfig.AllowedHosts. If provider is
// nil the gate is a no-op (no Authorization header is ever set).
func NewTokenGate(provider AuthProvider, scope string, allowedHosts []string) *TokenGate {
	return NewTokenGateForTarget(provider, "scope", scope, allowedHosts)
}

// NewTokenGateForTarget constructs a TokenGate for a scope or resource target.
func NewTokenGateForTarget(provider AuthProvider, targetKind, targetValue string, allowedHosts []string) *TokenGate {
	return &TokenGate{
		provider:     provider,
		targetKind:   targetKind,
		targetValue:  targetValue,
		allowedHosts: allowedHosts,
	}
}

// Scope returns the OAuth2 scope configured on this gate.
func (g *TokenGate) Scope() string {
	if g == nil || g.targetKind != "scope" {
		return ""
	}
	return g.targetValue
}

// AttachToken is the single chokepoint for bearer-token attachment (B-32).
//
// Host-matching semantics:
//   - Comparison is against req.URL.Hostname() — the port-stripped host
//     component. Standard ports (80, 443) never appear in Hostname() output.
//   - Matching is case-insensitive (RFC 4343: DNS names are case-insensitive).
//   - Wildcards are NOT supported. An entry beginning with '*' is treated as a
//     literal hostname and will never match. Authors who write *.azure-api.net
//     believing it wildcards will get MCP-012 on every request — this is
//     intentional: silently ignoring a wildcard would violate the same
//     "no surprise" principle as B-14.
//   - IDN/punycode normalization is NOT performed. Authors must use the
//     wire-format hostname that appears in the transport.url host component.
//   - Port-specific entries in allowed_hosts (e.g. "host:8443") are matched
//     against the full Hostname() output (port-stripped). "host:8443" will
//     NOT match requests to "host:8443" — authors must list the plain hostname.
//     Explicit port restrictions are not supported; use URL-level controls.
//
// Redirects: the http.Client on MCPHTTPTransport is configured to refuse
// redirects when a gate is present (CheckRedirect returns MCP-013). This
// prevents a redirect from an approved host to an unapproved one from
// bypassing the host check. See NewMCPHTTPTransport.
//
// If the gate is nil or provider is nil, AttachToken is a no-op.
//
// If req.URL.Hostname() is in allowedHosts: the token is acquired, the
// Authorization: Bearer header is set on req, and an mcp/authAttached trace
// event is emitted carrying host and scope only (never the token, B-24/B-27).
//
// If req.URL.Hostname() is NOT in allowedHosts: returns MCP-012 (fatal). The
// request is NOT sent. The error names the host and references allowed_hosts.
//
// Returns a non-nil error on host mismatch (MCP-012) or token acquisition
// failure (MCP-007).
func (g *TokenGate) AttachToken(ctx context.Context, req *http.Request) error {
	if g == nil || g.provider == nil {
		return nil
	}

	host := req.URL.Hostname()
	if !g.hostAllowed(host) {
		return errkit.New("MCP-012",
			fmt.Sprintf("mcp-http: request host %q is not in auth.allowed_hosts — token not attached (update allowed_hosts or url: to match)", host))
	}

	tok, err := g.provider.Token(ctx)
	if err != nil {
		return err // already MCP-007; token never appears in error message
	}
	req.Header.Set("Authorization", "Bearer "+tok)

	// B-27: audit trail — host and declared token target only, never the token value.
	if emit := trace.EmitterFromContext(ctx); emit != nil {
		payload := map[string]any{"url_host": host}
		if g.targetKind == "resource" {
			payload["resource"] = g.targetValue
		} else {
			payload["scope"] = g.targetValue
		}
		emit(string(trace.EventKindMCPAuthAttached), payload)
	}
	return nil
}

// Invalidate clears any cached token on the underlying provider. Call after
// receiving HTTP 401 so the next Token() call forces re-acquisition.
func (g *TokenGate) Invalidate() {
	if g != nil && g.provider != nil {
		g.provider.Invalidate()
	}
}

func (g *TokenGate) hostAllowed(host string) bool {
	h := strings.ToLower(host)
	for _, allowed := range g.allowedHosts {
		if strings.ToLower(allowed) == h {
			return true
		}
	}
	return false
}

// ValidateAuthConfig performs static validation of the auth block against the
// declared transport URL. These checks run at configuration load time, before
// any token is acquired, and return fatal errors.
//
//   - MCP-010: auth is configured but allowed_hosts is empty or omitted.
//   - MCP-011: the transport URL's hostname is not present in allowed_hosts.
//
// Call this before constructing a TokenGate.
// Returns nil when auth is nil (unauthenticated transport).
func ValidateAuthConfig(rawURL string, auth *schema.AuthConfig) error {
	if auth == nil {
		return nil
	}
	if len(auth.AllowedHosts) == 0 {
		return errkit.New("MCP-010",
			"mcp-http: auth.allowed_hosts is required when auth is configured")
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		// URL validity is validated elsewhere; skip host check if unparseable.
		return nil
	}
	urlHost := strings.ToLower(u.Hostname())
	for _, h := range auth.AllowedHosts {
		if strings.ToLower(h) == urlHost {
			return nil
		}
	}
	return errkit.New("MCP-011",
		fmt.Sprintf("mcp-http: url host %q is not in auth.allowed_hosts", u.Hostname()))
}
