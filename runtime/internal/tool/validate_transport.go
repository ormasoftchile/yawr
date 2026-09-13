package tool

import (
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// knownAuthProviders lists all auth provider names recognized in this
// iteration. To add a second provider: add its name here and implement the
// corresponding AuthProvider in internal/tool/auth_<provider>.go. The
// AuthConfig struct on the schema side is unchanged (backwards-compatible).
var knownAuthProviders = map[string]bool{
	"azure-cli":        true,
	"managed-identity": true,
}

// ValidateTransportConfig enforces mode/field constraints on a transport
// configuration and returns actionable errors an operator can act on. All
// returned errors have a message that names the offending field and what is
// expected. MCP-001/010/011 and MCP-002 are errkit-typed so callers can test
// against sentinels; structural violations (wrong field for mode) are plain
// errors.
//
// Called from ParseToolFile so invalid tool files fail at scan time, not at
// first tool invocation.
func ValidateTransportConfig(cfg schema.TransportConfig) []error {
	// UnmarshalYAML copies Mode → Type; after parsing, Type is canonical.
	mode := cfg.Type

	var errs []error

	// vscode_tool is only valid for vscode-mcp. Check this before the per-mode
	// switch so the error fires regardless of which transport arm is active.
	// Silently ignoring a mode-specific field is exactly the dead-field bug
	// class this project is under a customer blocker for (VSCODE-001).
	if cfg.VscodeTool != nil && mode != schema.TransportVSCodeMCP {
		errs = append(errs, fmt.Errorf("%s: transport.vscode_tool is only valid for mode vscode-mcp; remove it or change the transport mode (VSCODE-001)", string(mode)))
	}

	switch mode {
	case schema.TransportMCPHTTP:
		// url: is required and must be HTTPS (B-22).
		if cfg.URL == "" {
			errs = append(errs, errkit.New("MCP-001", "mcp-http: transport.url is required"))
		} else if !strings.HasPrefix(cfg.URL, "https://") {
			errs = append(errs, errkit.New("MCP-001", fmt.Sprintf("mcp-http: url %q must use https://", cfg.URL)))
		}

		// command: and args: are mutually exclusive with url: (B-14 principle:
		// fields that belong to a different transport arm are rejected, not
		// silently ignored).
		if cfg.Command != "" {
			errs = append(errs, fmt.Errorf("mcp-http: transport.command is not valid for mode mcp-http (mode mcp-http uses url:, not command:)"))
		}
		if len(cfg.Args) > 0 {
			errs = append(errs, fmt.Errorf("mcp-http: transport.args is not valid for mode mcp-http (mode mcp-http uses url:, not command/args)"))
		}

		if cfg.Auth != nil {
			// auth.provider must be a recognized name (MCP-002).
			if !knownAuthProviders[cfg.Auth.Provider] {
				errs = append(errs, errkit.New("MCP-002", fmt.Sprintf("mcp-http: unknown auth provider %q (recognized providers: azure-cli, managed-identity)", cfg.Auth.Provider)))
			}
			if (cfg.Auth.Scope == "") == (cfg.Auth.Resource == "") {
				errs = append(errs, fmt.Errorf("mcp-http: transport.auth must declare exactly one of scope or resource"))
			}
			if cfg.Auth.Resource != "" && cfg.Auth.Provider != "azure-cli" {
				errs = append(errs, fmt.Errorf("mcp-http: transport.auth.resource is only supported for provider azure-cli"))
			}

			// allowed_hosts is required whenever auth is configured (B-32 / MCP-010).
			if len(cfg.Auth.AllowedHosts) == 0 {
				errs = append(errs, errkit.New("MCP-010", "mcp-http: auth.allowed_hosts is required when auth is configured (B-32: prevents bearer token from being sent to attacker-controlled hosts)"))
			} else if cfg.URL != "" && strings.HasPrefix(cfg.URL, "https://") {
				// Static check: url host must be in allowed_hosts (MCP-011).
				// Parsing is done via net/url to avoid prefix/substring attacks
				// (e.g. icm.evil.com matching icm.com). Match is
				// case-insensitive on the hostname; no port is stripped from
				// allowed_hosts entries — authors list bare hostnames.
				// Runtime validation must use the same semantics.
				if u, err := url.Parse(cfg.URL); err == nil {
					urlHost := strings.ToLower(u.Hostname())
					if !hostInList(urlHost, cfg.Auth.AllowedHosts) {
						errs = append(errs, errkit.New("MCP-011", fmt.Sprintf(
							"mcp-http: url host %q is not in auth.allowed_hosts %v",
							urlHost, cfg.Auth.AllowedHosts,
						)))
					}
				}
			}
		}

	case schema.TransportMCP:
		// command: is required for stdio MCP; url: is not valid here.
		if cfg.Command == "" {
			errs = append(errs, fmt.Errorf("mcp: transport.command is required for mode mcp"))
		}
		if cfg.URL != "" {
			errs = append(errs, fmt.Errorf("mcp: transport.url is not valid for mode mcp (mode mcp uses command:, not url:)"))
		}
		if cfg.Auth != nil {
			errs = append(errs, fmt.Errorf("mcp: transport.auth is not valid for mode mcp (auth is only used by mode mcp-http)"))
		}

	case schema.TransportNativeFileOnly:
		if cfg.Command == "" {
			errs = append(errs, fmt.Errorf("native-file-only: transport.command is required"))
		}
		if cfg.SHA256 == "" {
			errs = append(errs, fmt.Errorf("native-file-only: transport.sha256 is required"))
		} else if len(cfg.SHA256) != 64 || strings.ToLower(cfg.SHA256) != cfg.SHA256 {
			errs = append(errs, fmt.Errorf("native-file-only: transport.sha256 must be exactly 64 lower-case hexadecimal characters"))
		} else {
			for _, r := range cfg.SHA256 {
				if !strings.ContainsRune("0123456789abcdef", r) {
					errs = append(errs, fmt.Errorf("native-file-only: transport.sha256 must be exactly 64 lower-case hexadecimal characters"))
					break
				}
			}
		}
		if len(cfg.Env) != 0 {
			errs = append(errs, fmt.Errorf("native-file-only: transport.env is forbidden; the child receives only the fixed sandbox environment"))
		}
		if cfg.URL != "" || cfg.Auth != nil || cfg.VscodeTool != nil {
			errs = append(errs, fmt.Errorf("native-file-only: url, auth, and vscode_tool are not valid for this transport"))
		}

	case schema.TransportVSCodeMCP:
		// vscode-mcp is a loopback bridge transport. It requires NO url:, NO auth:, NO command:.
		// These fields belong to other modes and are rejected here so authoring errors are caught early.
		if cfg.URL != "" {
			errs = append(errs, fmt.Errorf("vscode-mcp: transport.url is not valid for mode vscode-mcp (this is a loopback bridge; authorization stays inside VS Code)"))
		}
		if cfg.Auth != nil {
			errs = append(errs, fmt.Errorf("vscode-mcp: transport.auth is not valid for mode vscode-mcp (no bearer token may reach Yawr; the capability secret is managed by the bridge, not auth:)"))
		}
		if cfg.Command != "" {
			errs = append(errs, fmt.Errorf("vscode-mcp: transport.command is not valid for mode vscode-mcp (mode vscode-mcp uses the VS Code extension bridge, not a command:)"))
		}
		if cfg.VscodeTool != nil && *cfg.VscodeTool == "" {
			errs = append(errs, fmt.Errorf("vscode-mcp: transport.vscode_tool must not be empty when set (VSCODE-001)"))
		}
	}

	return errs
}

// hostInList reports whether host matches any entry in the allowed list.
// Matching is exact and case-insensitive on the parsed hostname component.
// Entries in allowed must be bare hostnames (no scheme, no port, no path).
// A substring or prefix match is explicitly NOT used — "evil.com" must not
// match "icm.evil.com" as a prefix, and "icm-mcp.azure-api.net.evil.com"
// must not match "icm-mcp.azure-api.net" as a prefix.
func hostInList(host string, allowed []string) bool {
	for _, entry := range allowed {
		if strings.ToLower(entry) == host {
			return true
		}
	}
	return false
}

// ValidateVSCodeToolActions enforces per-action vscode_tool constraints.
//
// For every action in the map that declares vscode_tool:
//   - The enclosing tool's transport must be vscode-mcp (VSCODE-001). A
//     vscode_tool on any other transport is a dead field — authoring error.
//   - The value must not be empty. An empty string is always invalid.
//
// Called from ParseToolFile after ValidateTransportConfig so invalid tool
// files fail at scan time.
func ValidateVSCodeToolActions(transport schema.TransportConfig, actions map[string]*schema.ToolAction) error {
	mode := transport.Type
	for actionName, action := range actions {
		if action == nil || action.VscodeTool == nil {
			continue
		}
		if mode != schema.TransportVSCodeMCP {
			return fmt.Errorf("action %q: vscode_tool is only valid for transport mode vscode-mcp; remove it or change the transport mode (VSCODE-001)", actionName)
		}
		if *action.VscodeTool == "" {
			return fmt.Errorf("action %q: vscode_tool must not be empty when set (VSCODE-001)", actionName)
		}
	}
	return nil
}

// ValidateVSCodeInputActions enforces per-action vscode_input constraints.
//
// For every action in the map that declares vscode_input:
//   - The enclosing tool's transport must be vscode-mcp (VSCODE-002). A
//     vscode_input on any other transport is a dead field — authoring error.
//   - Each mapping's from: must name a declared arg on the action.
//   - Each mapping's coerce: (if present) must be a recognized type.
//
// Called from ParseToolFile after ValidateVSCodeToolActions so invalid tool
// files fail at scan time, not at invocation.
func ValidateVSCodeInputActions(transport schema.TransportConfig, actions map[string]*schema.ToolAction) error {
	mode := transport.Type
	knownCoerce := map[string]bool{
		"integer": true, "number": true, "string": true, "boolean": true,
	}
	for actionName, action := range actions {
		if action == nil || len(action.VSCodeInput) == 0 {
			continue
		}
		if mode != schema.TransportVSCodeMCP {
			return fmt.Errorf("action %q: vscode_input is only valid for transport mode vscode-mcp; remove it or change the transport mode (VSCODE-002)", actionName)
		}
		for providerKey, mapping := range action.VSCodeInput {
			if mapping == nil {
				continue
			}
			if mapping.From == "" {
				return fmt.Errorf("action %q: vscode_input[%q].from must not be empty", actionName, providerKey)
			}
			if _, ok := action.Args[mapping.From]; !ok {
				declared := make([]string, 0, len(action.Args))
				for k := range action.Args {
					declared = append(declared, k)
				}
				sort.Strings(declared)
				return fmt.Errorf("action %q: vscode_input[%q].from %q is not a declared arg (declared: %v)", actionName, providerKey, mapping.From, declared)
			}
			if mapping.Coerce != "" && !knownCoerce[mapping.Coerce] {
				return fmt.Errorf("action %q: vscode_input[%q].coerce %q is not a supported coercion type (supported: integer, number, string, boolean)", actionName, providerKey, mapping.Coerce)
			}
		}
	}
	return nil
}

// ValidateMCPHTTPActionMappings enforces per-action mcp_tool/mcp_input constraints.
func ValidateMCPHTTPActionMappings(transport schema.TransportConfig, actions map[string]*schema.ToolAction) error {
	mode := transport.Type
	knownCoerce := map[string]bool{
		"integer": true, "number": true, "string": true, "boolean": true,
	}
	for actionName, action := range actions {
		if action == nil {
			continue
		}
		if action.MCPTool != "" && mode != schema.TransportMCPHTTP {
			return fmt.Errorf("action %q: mcp_tool is only valid for transport mode mcp-http; remove it or change the transport mode", actionName)
		}
		if len(action.MCPInput) == 0 {
			continue
		}
		if mode != schema.TransportMCPHTTP {
			return fmt.Errorf("action %q: mcp_input is only valid for transport mode mcp-http; remove it or change the transport mode", actionName)
		}
		for providerKey, mapping := range action.MCPInput {
			if mapping == nil {
				continue
			}
			if mapping.From == "" {
				return fmt.Errorf("action %q: mcp_input[%q].from must not be empty", actionName, providerKey)
			}
			if _, ok := action.Args[mapping.From]; !ok {
				declared := make([]string, 0, len(action.Args))
				for k := range action.Args {
					declared = append(declared, k)
				}
				sort.Strings(declared)
				return fmt.Errorf("action %q: mcp_input[%q].from %q is not a declared arg (declared: %v)", actionName, providerKey, mapping.From, declared)
			}
			if mapping.Coerce != "" && !knownCoerce[mapping.Coerce] {
				return fmt.Errorf("action %q: mcp_input[%q].coerce %q is not a supported coercion type (supported: integer, number, string, boolean)", actionName, providerKey, mapping.Coerce)
			}
		}
	}
	return nil
}
