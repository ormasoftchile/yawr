package tool

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// parseToolFileFromString writes content to a temp file and calls ParseToolFile.
func parseToolFileFromString(t *testing.T, content string) (*schema.ToolDef, error) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.tool.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp tool file: %v", err)
	}
	return ParseToolFile(path)
}

// ─── ValidateTransportConfig unit tests ────────────────────────────────────

func TestValidateTransport_MCPHTTP_ValidMinimal(t *testing.T) {
	errs := ValidateTransportConfig(schema.TransportConfig{
		Type: schema.TransportMCPHTTP,
		URL:  "https://icm-mcp-prod.azure-api.net/v1/",
	})
	if len(errs) != 0 {
		t.Errorf("expected no errors, got: %v", errs)
	}
}

func TestValidateTransport_MCPHTTP_ValidWithAuth(t *testing.T) {
	errs := ValidateTransportConfig(schema.TransportConfig{
		Type: schema.TransportMCPHTTP,
		URL:  "https://icm-mcp-prod.azure-api.net/v1/",
		Auth: &schema.AuthConfig{
			Provider:     "azure-cli",
			Scope:        "api://icmmcpapi-prod/mcp.tools",
			AllowedHosts: []string{"icm-mcp-prod.azure-api.net"},
		},
	})
	if len(errs) != 0 {
		t.Errorf("expected no errors, got: %v", errs)
	}
}

func TestValidateTransport_MCPHTTP_MissingURL_MCP001(t *testing.T) {
	errs := ValidateTransportConfig(schema.TransportConfig{
		Type: schema.TransportMCPHTTP,
	})
	if len(errs) == 0 {
		t.Fatal("expected an error for missing url")
	}
	if !errors.Is(errs[0], errkit.ErrMCP001) {
		t.Errorf("expected ErrMCP001, got: %v", errs[0])
	}
	if !strings.Contains(errs[0].Error(), "url is required") {
		t.Errorf("error message must name the missing field, got: %q", errs[0].Error())
	}
}

func TestValidateTransport_MCPHTTP_HTTPNotHTTPS_MCP001(t *testing.T) {
	url := "http://insecure.example.com/v1/"
	errs := ValidateTransportConfig(schema.TransportConfig{
		Type: schema.TransportMCPHTTP,
		URL:  url,
	})
	if len(errs) == 0 {
		t.Fatal("expected an error for non-https url")
	}
	if !errors.Is(errs[0], errkit.ErrMCP001) {
		t.Errorf("expected ErrMCP001, got: %v", errs[0])
	}
	if !strings.Contains(errs[0].Error(), url) {
		t.Errorf("error message must quote the offending url, got: %q", errs[0].Error())
	}
	if !strings.Contains(errs[0].Error(), "https://") {
		t.Errorf("error message must tell operator to use https://, got: %q", errs[0].Error())
	}
}

func TestValidateTransport_MCPHTTP_CommandRejected(t *testing.T) {
	errs := ValidateTransportConfig(schema.TransportConfig{
		Type:    schema.TransportMCPHTTP,
		URL:     "https://example.com/",
		Command: "some-binary",
	})
	if len(errs) == 0 {
		t.Fatal("expected an error for command on mcp-http")
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "command is not valid") {
			found = true
		}
	}
	if !found {
		t.Errorf("error must say 'command is not valid', got: %v", errs)
	}
}

func TestValidateTransport_MCPHTTP_ArgsRejected(t *testing.T) {
	errs := ValidateTransportConfig(schema.TransportConfig{
		Type: schema.TransportMCPHTTP,
		URL:  "https://example.com/",
		Args: []string{"--flag"},
	})
	if len(errs) == 0 {
		t.Fatal("expected an error for args on mcp-http")
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "args is not valid") {
			found = true
		}
	}
	if !found {
		t.Errorf("error must say 'args is not valid', got: %v", errs)
	}
}

func TestValidateTransport_MCPHTTP_UnknownProvider_MCP002(t *testing.T) {
	errs := ValidateTransportConfig(schema.TransportConfig{
		Type: schema.TransportMCPHTTP,
		URL:  "https://example.com/",
		Auth: &schema.AuthConfig{
			Provider:     "magic-beans",
			AllowedHosts: []string{"example.com"},
		},
	})
	if len(errs) == 0 {
		t.Fatal("expected an error for unknown auth provider")
	}
	if !errors.Is(errs[0], errkit.ErrMCP002) {
		t.Errorf("expected ErrMCP002, got: %v", errs[0])
	}
	msg := errs[0].Error()
	if !strings.Contains(msg, "magic-beans") {
		t.Errorf("error must name the offending provider, got: %q", msg)
	}
	if !strings.Contains(msg, "azure-cli") {
		t.Errorf("error must list recognized providers, got: %q", msg)
	}
}

func TestValidateTransport_MCPHTTP_MissingAllowedHosts_MCP010(t *testing.T) {
	errs := ValidateTransportConfig(schema.TransportConfig{
		Type: schema.TransportMCPHTTP,
		URL:  "https://example.com/",
		Auth: &schema.AuthConfig{
			Provider: "azure-cli",
			Scope:    "api://test/mcp",
			// AllowedHosts deliberately absent
		},
	})
	if len(errs) == 0 {
		t.Fatal("expected MCP-010 for auth without allowed_hosts")
	}
	var got *errkit.Error
	for _, e := range errs {
		var ee *errkit.Error
		if errors.As(e, &ee) && errors.Is(ee, errkit.ErrMCP010) {
			got = ee
			break
		}
	}
	if got == nil {
		t.Fatalf("expected ErrMCP010 in errors, got: %v", errs)
	}
	if !strings.Contains(got.Error(), "allowed_hosts is required") {
		t.Errorf("error must say allowed_hosts is required, got: %q", got.Error())
	}
}

func TestValidateTransport_MCPHTTP_HostNotInAllowedHosts_MCP011(t *testing.T) {
	errs := ValidateTransportConfig(schema.TransportConfig{
		Type: schema.TransportMCPHTTP,
		URL:  "https://real-endpoint.azure-api.net/v1/",
		Auth: &schema.AuthConfig{
			Provider:     "azure-cli",
			Scope:        "api://test/mcp",
			AllowedHosts: []string{"other.azure-api.net"},
		},
	})
	if len(errs) == 0 {
		t.Fatal("expected MCP-011 for url host not in allowed_hosts")
	}
	var got *errkit.Error
	for _, e := range errs {
		var ee *errkit.Error
		if errors.As(e, &ee) && errors.Is(ee, errkit.ErrMCP011) {
			got = ee
			break
		}
	}
	if got == nil {
		t.Fatalf("expected ErrMCP011 in errors, got: %v", errs)
	}
	msg := got.Error()
	if !strings.Contains(msg, "real-endpoint.azure-api.net") {
		t.Errorf("error must quote the url host, got: %q", msg)
	}
}

// Ensure a substring of a valid host does NOT satisfy the check (replay-attack guard).
func TestValidateTransport_MCPHTTP_SubstringHostDoesNotMatch_MCP011(t *testing.T) {
	errs := ValidateTransportConfig(schema.TransportConfig{
		Type: schema.TransportMCPHTTP,
		URL:  "https://icm-mcp-prod.azure-api.net.evil.com/v1/",
		Auth: &schema.AuthConfig{
			Provider:     "azure-cli",
			Scope:        "api://test/mcp",
			AllowedHosts: []string{"icm-mcp-prod.azure-api.net"},
		},
	})
	hasM011 := false
	for _, e := range errs {
		var ee *errkit.Error
		if errors.As(e, &ee) && errors.Is(ee, errkit.ErrMCP011) {
			hasM011 = true
		}
	}
	if !hasM011 {
		t.Errorf("suffix-extension of allowed host must produce MCP-011, got: %v", errs)
	}
}

// No auth block: allowed_hosts is never required and no host check fires.
func TestValidateTransport_MCPHTTP_NoAuth_AllowedHostsNotRequired(t *testing.T) {
	errs := ValidateTransportConfig(schema.TransportConfig{
		Type: schema.TransportMCPHTTP,
		URL:  "https://unauthenticated.example.com/v1/",
	})
	for _, e := range errs {
		var ee *errkit.Error
		if errors.As(e, &ee) && (errors.Is(ee, errkit.ErrMCP010) || errors.Is(ee, errkit.ErrMCP011)) {
			t.Errorf("unauthenticated mcp-http must not produce MCP-010 or MCP-011, got: %v", e)
		}
	}
}

func TestValidateTransport_MCPStdio_ValidMinimal(t *testing.T) {
	errs := ValidateTransportConfig(schema.TransportConfig{
		Type:    schema.TransportMCP,
		Command: "my-tool-binary",
	})
	if len(errs) != 0 {
		t.Errorf("expected no errors, got: %v", errs)
	}

}

func TestValidateTransport_NativeFileOnlyContract(t *testing.T) {
	valid := schema.TransportConfig{
		Type:    schema.TransportNativeFileOnly,
		Command: `bin\fixture.exe`,
		SHA256:  strings.Repeat("a", 64),
		Inputs:  []string{`data\input.txt`},
	}
	if errs := ValidateTransportConfig(valid); len(errs) != 0 {
		t.Fatalf("valid native-file-only rejected: %v", errs)
	}
	for name, mutate := range map[string]func(*schema.TransportConfig){
		"missing digest": func(c *schema.TransportConfig) { c.SHA256 = "" },
		"upper digest":   func(c *schema.TransportConfig) { c.SHA256 = strings.ToUpper(c.SHA256) },
		"environment":    func(c *schema.TransportConfig) { c.Env = map[string]string{"X": "Y"} },
		"transport args": func(c *schema.TransportConfig) { c.Args = []string{"--silently-ignored"} },
		"url":            func(c *schema.TransportConfig) { c.URL = "https://example.invalid" },
		"auth":           func(c *schema.TransportConfig) { c.Auth = &schema.AuthConfig{Provider: "azure-cli"} },
		"vscode tool":    func(c *schema.TransportConfig) { value := "tool"; c.VscodeTool = &value },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := valid
			mutate(&cfg)
			if errs := ValidateTransportConfig(cfg); len(errs) == 0 {
				t.Fatal("invalid native-file-only config accepted")
			}
		})
	}
	errs := ValidateTransportConfig(schema.TransportConfig{
		Type: schema.TransportNativeFileOnly, Command: "fixture.exe",
		SHA256: strings.Repeat("a", 64), Args: []string{"--flag"},
	})
	if len(errs) != 1 || errs[0].Error() != "native-file-only: transport.args is not valid; declare all invocation arguments on actions[].argv" {
		t.Fatalf("transport.args must have a stable rejection, got %v", errs)
	}
}

func TestParseToolFile_NativeFileOnlyRejectsUnknownTransportField(t *testing.T) {
	_, err := parseToolFileFromString(t, `apiVersion: yawr.tool/v1
meta: {name: closed-contract}
transport:
  mode: native-file-only
  command: fixture.exe
  sha256: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  inputs: []
  extra: silently-ignored
actions:
  - name: read
    classification: read-only
    argv: []
`)
	if err == nil || !strings.Contains(err.Error(), `native-file-only: transport field "extra" is not recognized`) {
		t.Fatalf("unknown native-file-only transport field must reject, got %v", err)
	}
}

func TestValidateTransport_MCPStdio_MissingCommand(t *testing.T) {
	errs := ValidateTransportConfig(schema.TransportConfig{
		Type: schema.TransportMCP,
	})
	if len(errs) == 0 {
		t.Fatal("expected an error for missing command")
	}
	if !strings.Contains(errs[0].Error(), "command is required") {
		t.Errorf("error must say 'command is required', got: %q", errs[0].Error())
	}
}

func TestValidateTransport_MCPStdio_URLRejected(t *testing.T) {
	errs := ValidateTransportConfig(schema.TransportConfig{
		Type:    schema.TransportMCP,
		Command: "my-tool",
		URL:     "https://example.com/",
	})
	if len(errs) == 0 {
		t.Fatal("expected an error for url on mcp")
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "url is not valid") {
			found = true
		}
	}
	if !found {
		t.Errorf("error must say 'url is not valid', got: %v", errs)
	}
}

func TestValidateTransport_MCPStdio_AuthRejected(t *testing.T) {
	errs := ValidateTransportConfig(schema.TransportConfig{
		Type:    schema.TransportMCP,
		Command: "my-tool",
		Auth:    &schema.AuthConfig{Provider: "azure-cli"},
	})
	if len(errs) == 0 {
		t.Fatal("expected an error for auth on mcp")
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "auth is not valid") {
			found = true
		}
	}
	if !found {
		t.Errorf("error must say 'auth is not valid', got: %v", errs)
	}
}

// ─── ParseToolFile integration tests ───────────────────────────────────────

const toolFileTemplate = `
meta:
  name: test-tool
  description: integration test tool
transport:
%s
actions:
  - name: ping
    description: ping action
    parameters: []
`

func TestParseToolFile_MCPHTTP_ValidMinimal(t *testing.T) {
	content := `
meta:
  name: test-tool
  description: integration test tool
transport:
  mode: mcp-http
  url: https://icm-mcp-prod.azure-api.net/v1/
actions:
  - name: ping
    description: ping action
    parameters: []
`
	def, err := parseToolFileFromString(t, content)
	if err != nil {
		t.Fatalf("expected parse OK, got: %v", err)
	}
	if def.Transport.URL != "https://icm-mcp-prod.azure-api.net/v1/" {
		t.Errorf("URL not populated: got %q", def.Transport.URL)
	}
}

func TestParseToolFile_MCPHTTP_ValidWithAuth(t *testing.T) {
	content := `
meta:
  name: test-tool
  description: integration test tool
transport:
  mode: mcp-http
  url: https://icm-mcp-prod.azure-api.net/v1/
  auth:
    provider: azure-cli
    scope: api://icmmcpapi-prod/mcp.tools
    allowed_hosts:
      - icm-mcp-prod.azure-api.net
actions:
  - name: ping
    description: ping action
    parameters: []
`
	def, err := parseToolFileFromString(t, content)
	if err != nil {
		t.Fatalf("expected parse OK, got: %v", err)
	}
	if def.Transport.Auth == nil {
		t.Fatal("Auth block not populated")
	}
	if def.Transport.Auth.Provider != "azure-cli" {
		t.Errorf("Auth.Provider = %q, want azure-cli", def.Transport.Auth.Provider)
	}
	if def.Transport.Auth.Scope != "api://icmmcpapi-prod/mcp.tools" {
		t.Errorf("Auth.Scope = %q, want api://icmmcpapi-prod/mcp.tools", def.Transport.Auth.Scope)
	}
	if len(def.Transport.Auth.AllowedHosts) != 1 || def.Transport.Auth.AllowedHosts[0] != "icm-mcp-prod.azure-api.net" {
		t.Errorf("Auth.AllowedHosts = %v, want [icm-mcp-prod.azure-api.net]", def.Transport.Auth.AllowedHosts)
	}
}

func TestParseToolFile_MCPHTTP_MissingURL_Fails(t *testing.T) {
	content := `
meta:
  name: test-tool
  description: integration test tool
transport:
  mode: mcp-http
actions:
  - name: ping
    description: ping action
    parameters: []
`
	_, err := parseToolFileFromString(t, content)
	if err == nil {
		t.Fatal("expected parse to fail for mcp-http with no url")
	}
	if !errors.Is(err, errkit.ErrMCP001) {
		t.Errorf("expected ErrMCP001 in chain, got: %v", err)
	}
}

func TestParseToolFile_MCPHTTP_HTTPScheme_Fails(t *testing.T) {
	content := `
meta:
  name: test-tool
  description: integration test tool
transport:
  mode: mcp-http
  url: http://insecure.example.com/v1/
actions:
  - name: ping
    description: ping action
    parameters: []
`
	_, err := parseToolFileFromString(t, content)
	if err == nil {
		t.Fatal("expected parse to fail for http:// url on mcp-http")
	}
	if !errors.Is(err, errkit.ErrMCP001) {
		t.Errorf("expected ErrMCP001 in chain, got: %v", err)
	}
}

func TestParseToolFile_MCPHTTP_UnknownProvider_Fails(t *testing.T) {
	content := `
meta:
  name: test-tool
  description: integration test tool
transport:
  mode: mcp-http
  url: https://example.com/v1/
  auth:
    provider: magic-beans
    scope: api://test
    allowed_hosts:
      - example.com
actions:
  - name: ping
    description: ping action
    parameters: []
`
	_, err := parseToolFileFromString(t, content)
	if err == nil {
		t.Fatal("expected parse to fail for unknown auth provider")
	}
	if !errors.Is(err, errkit.ErrMCP002) {
		t.Errorf("expected ErrMCP002 in chain, got: %v", err)
	}
}
