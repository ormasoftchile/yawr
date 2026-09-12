package tool

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

// ─── ValidateAuthConfig ──────────────────────────────────────────────────────

func TestValidateAuthConfig_NilAuth(t *testing.T) {
	if err := ValidateAuthConfig("https://example.com/v1/", nil); err != nil {
		t.Fatalf("expected nil for nil auth, got %v", err)
	}
}

func TestValidateAuthConfig_MissingAllowedHosts(t *testing.T) {
	auth := &schema.AuthConfig{Provider: "azure-cli", Scope: "api://foo/bar"}
	err := ValidateAuthConfig("https://example.com/v1/", auth)
	if err == nil {
		t.Fatal("expected MCP-010 error, got nil")
	}
	if !strings.Contains(err.Error(), "MCP-010") {
		t.Fatalf("expected MCP-010 in error, got %q", err.Error())
	}
}

func TestValidateAuthConfig_URLHostNotInList(t *testing.T) {
	auth := &schema.AuthConfig{
		Provider:     "azure-cli",
		Scope:        "api://foo/bar",
		AllowedHosts: []string{"other.example.com"},
	}
	err := ValidateAuthConfig("https://example.com/v1/", auth)
	if err == nil {
		t.Fatal("expected MCP-011 error, got nil")
	}
	if !strings.Contains(err.Error(), "MCP-011") {
		t.Fatalf("expected MCP-011 in error, got %q", err.Error())
	}
}

func TestValidateAuthConfig_URLHostInList(t *testing.T) {
	auth := &schema.AuthConfig{
		Provider:     "azure-cli",
		Scope:        "api://foo/bar",
		AllowedHosts: []string{"example.com"},
	}
	if err := ValidateAuthConfig("https://example.com/v1/", auth); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestValidateAuthConfig_CaseInsensitiveHostMatch(t *testing.T) {
	auth := &schema.AuthConfig{
		Provider:     "azure-cli",
		Scope:        "api://foo/bar",
		AllowedHosts: []string{"EXAMPLE.COM"},
	}
	if err := ValidateAuthConfig("https://example.com/v1/", auth); err != nil {
		t.Fatalf("expected nil (case-insensitive match), got %v", err)
	}
}

// ─── TokenGate.AttachToken ───────────────────────────────────────────────────

func makeStaticProvider(token string) AuthProvider {
	return &staticAuthProvider{token: token}
}

type staticAuthProvider struct{ token string }

func (p *staticAuthProvider) Token(_ context.Context) (string, error) {
	return p.token, nil
}
func (p *staticAuthProvider) Invalidate() {}

func makeTestRequest(rawURL string) *http.Request {
	u, _ := url.Parse(rawURL)
	return &http.Request{URL: u, Header: make(http.Header)}
}

func TestTokenGate_NilGate(t *testing.T) {
	var g *TokenGate
	req := makeTestRequest("https://example.com/v1/")
	if err := g.AttachToken(context.Background(), req); err != nil {
		t.Fatalf("nil gate must be a no-op, got %v", err)
	}
	if req.Header.Get("Authorization") != "" {
		t.Fatal("nil gate must not set Authorization header")
	}
}

func TestTokenGate_NilProvider(t *testing.T) {
	g := NewTokenGate(nil, "api://foo/bar", []string{"example.com"})
	req := makeTestRequest("https://example.com/v1/")
	if err := g.AttachToken(context.Background(), req); err != nil {
		t.Fatalf("nil provider must be a no-op, got %v", err)
	}
}

func TestTokenGate_AttachesTokenToAllowedHost(t *testing.T) {
	const wantToken = "tok_abc123"
	g := NewTokenGate(makeStaticProvider(wantToken), "api://foo/bar", []string{"example.com"})
	req := makeTestRequest("https://example.com/v1/")
	if err := g.AttachToken(context.Background(), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := req.Header.Get("Authorization")
	if got != "Bearer "+wantToken {
		t.Fatalf("got Authorization %q, want %q", got, "Bearer "+wantToken)
	}
}

func TestTokenGate_RejectsDisallowedHost(t *testing.T) {
	g := NewTokenGate(makeStaticProvider("tok"), "api://foo/bar", []string{"allowed.example.com"})
	req := makeTestRequest("https://evil.example.com/v1/")
	err := g.AttachToken(context.Background(), req)
	if err == nil {
		t.Fatal("expected MCP-012 error for disallowed host, got nil")
	}
	if !strings.Contains(err.Error(), "MCP-012") {
		t.Fatalf("expected MCP-012 in error, got %q", err.Error())
	}
	if req.Header.Get("Authorization") != "" {
		t.Fatal("Authorization header must not be set when host is rejected")
	}
}

// Suffix confusion: "icm.com.evil.com" must NOT match "icm.com".
func TestTokenGate_SuffixConfusionRejected(t *testing.T) {
	g := NewTokenGate(makeStaticProvider("tok"), "api://icm/mcp.tools", []string{"icm.com"})
	req := makeTestRequest("https://icm.com.evil.com/v1/")
	err := g.AttachToken(context.Background(), req)
	if err == nil {
		t.Fatal("suffix confusion: icm.com.evil.com must not match icm.com")
	}
	if !strings.Contains(err.Error(), "MCP-012") {
		t.Fatalf("expected MCP-012, got %q", err.Error())
	}
}

func TestTokenGate_CaseInsensitiveAllowedHost(t *testing.T) {
	g := NewTokenGate(makeStaticProvider("tok"), "api://foo/bar", []string{"EXAMPLE.COM"})
	req := makeTestRequest("https://example.com/v1/")
	if err := g.AttachToken(context.Background(), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Header.Get("Authorization") == "" {
		t.Fatal("expected Authorization header to be set")
	}
}

func TestTokenGate_EmitsAuthAttachedEvent(t *testing.T) {
	const scope = "api://icm/mcp.tools"
	g := NewTokenGate(makeStaticProvider("tok"), scope, []string{"example.com"})
	req := makeTestRequest("https://example.com/v1/")

	var emitted []map[string]any
	emitFn := func(kind string, payload map[string]any) {
		if kind == string(trace.EventKindMCPAuthAttached) {
			emitted = append(emitted, payload)
		}
	}
	ctx := trace.WithEventEmitter(context.Background(), emitFn)
	if err := g.AttachToken(ctx, req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(emitted) != 1 {
		t.Fatalf("expected 1 mcp/authAttached event, got %d", len(emitted))
	}
	if emitted[0]["url_host"] != "example.com" {
		t.Errorf("wrong url_host in event: %v", emitted[0]["url_host"])
	}
	if emitted[0]["scope"] != scope {
		t.Errorf("wrong scope in event: %v", emitted[0]["scope"])
	}
}

// MCP-012 error message must name the host and reference allowed_hosts.
func TestTokenGate_MCP012ErrorIsActionable(t *testing.T) {
	const blockedHost = "evil.com"
	g := NewTokenGate(makeStaticProvider("tok"), "api://foo/bar", []string{"allowed.com"})
	req := makeTestRequest("https://" + blockedHost + "/v1/")
	err := g.AttachToken(context.Background(), req)
	if err == nil {
		t.Fatal("expected MCP-012 error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "MCP-012") {
		t.Errorf("error missing MCP-012: %q", msg)
	}
	if !strings.Contains(msg, blockedHost) {
		t.Errorf("error must name the blocked host %q: %q", blockedHost, msg)
	}
	if !strings.Contains(msg, "allowed_hosts") {
		t.Errorf("error must reference allowed_hosts: %q", msg)
	}
}

// B-24 proof: trace event must not contain the token value.
func TestTokenGate_AuthAttachedEventDoesNotContainToken(t *testing.T) {
	const secret = "super-secret-bearer-token-12345"
	g := NewTokenGate(makeStaticProvider(secret), "api://foo/bar", []string{"example.com"})
	req := makeTestRequest("https://example.com/v1/")

	var events []map[string]any
	emitFn := func(kind string, payload map[string]any) {
		events = append(events, payload)
	}
	ctx := trace.WithEventEmitter(context.Background(), emitFn)
	if err := g.AttachToken(ctx, req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, ev := range events {
		for k, v := range ev {
			if strings.Contains(fmt.Sprintf("%v", v), secret) {
				t.Errorf("token leaked into trace event field %q: %v", k, v)
			}
		}
	}
}
