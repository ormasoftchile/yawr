package schema

import (
	"encoding/json"
	"fmt"

	"gopkg.in/yaml.v3"
)

// ToolRef is a reference to a tool declared in a runbook's toolRefs list.
type ToolRef struct {
	Name    string `yaml:"name"              json:"name"`
	Package string `yaml:"package,omitempty" json:"package,omitempty"`
	Version string `yaml:"version,omitempty" json:"version,omitempty"`
	Path    string `yaml:"path,omitempty"    json:"path,omitempty"`
	// Alias is REMOVED from the schema (design/yawr/sections/06-tool-runtime.tex
	// §ToolRef field disposition). The field is retained here only so the
	// parser's pre-schema raw scan can detect its presence and raise
	// PKG-021; it MUST NOT be read for any resolution purpose.
	Alias string `yaml:"alias,omitempty"   json:"alias,omitempty"`
	// Source is deprecated (D-001): retained as schema-valid, non-enforcing
	// provenance-only text. Presence emits PKG-W002.
	Source string `yaml:"source,omitempty"  json:"source,omitempty"`
	// Actions is deprecated (D-002): a documentation-only, plan-time
	// reachability assertion, never an allowlist. Presence emits PKG-W001.
	Actions []string `yaml:"actions,omitempty" json:"actions,omitempty"`
}

// ToolMeta is the canonical, nested `meta:` identity block of a .tool.yaml
// document (design/yawr/sections/06-tool-runtime.tex §Tool Definition
// Schema). This is the shape used by every package-exported tool file in
// the Tess conformance corpus (design/yawr/conformance/tv-pkg-resolve.yaml).
type ToolMeta struct {
	Name        string `yaml:"name"                  json:"name"`
	Version     string `yaml:"version,omitempty"     json:"version,omitempty"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	Binary      string `yaml:"binary,omitempty"      json:"binary,omitempty"`
}

// ToolGovernance is a tool definition's own governance block
// (design/yawr/sections/06-tool-runtime.tex §Tool Definition Schema). It
// continues to gate an action whether the action is process-backed or
// substituted (design/yawr/sections/12-governance-policy.tex §Substitution
// Governance Composition).
type ToolGovernance struct {
	RequiresCapabilities []string `yaml:"requires-capabilities,omitempty" json:"requires-capabilities,omitempty"`
	AllowedEnvironments  []string `yaml:"allowed-environments,omitempty"  json:"allowed-environments,omitempty"`
	// RequiresApproval is tri-state: nil = never expressed (unspecified),
	// &false = explicit legacy approval opt-out, &true = approval required.
	// Use pointer so YAML absence is distinguishable from explicit false.
	RequiresApproval *bool `yaml:"requires-approval,omitempty" json:"requires-approval,omitempty"`
	// AllowCommands is the tool-level command allowlist that composes with
	// a substituted action's own governance per §Substitution Governance
	// Composition (design/yawr/sections/12-governance-policy.tex): the
	// substitute's allow_commands is INTERSECTED with this set, never
	// unioned (a substitute may only narrow, never widen, PKG-014).
	AllowCommands []string `yaml:"allow-commands,omitempty" json:"allow-commands,omitempty"`
	// AllowedModes restricts which RunMode values may invoke this tool.
	// Valid values: "real", "dry-run", "replay". Absent = unrestricted.
	// This is the RunMode discriminator; deployment-context restrictions
	// belong in AllowedEnvironments.
	AllowedModes []string `yaml:"allowed-modes,omitempty" json:"allowed-modes,omitempty"`
}

// ToolDef is the parsed definition of a .tool.yaml file.
//
// actions: is always an array of {name, ...} entries in the canonical
// yawr.tool/v1 shape.
type ToolDef struct {
	APIVersion  string                 `yaml:"apiVersion"            json:"apiVersion"`
	Meta        *ToolMeta              `yaml:"meta,omitempty"        json:"meta,omitempty"`
	Name        string                 `yaml:"name,omitempty"        json:"name"`
	Version     string                 `yaml:"version,omitempty"     json:"version,omitempty"`
	Description string                 `yaml:"description,omitempty" json:"description,omitempty"`
	Transport   TransportConfig        `yaml:"transport"             json:"transport"`
	Governance  *ToolGovernance        `yaml:"governance,omitempty"  json:"governance,omitempty"`
	Actions     map[string]*ToolAction `yaml:"actions"               json:"actions"`
	Metadata    map[string]string      `yaml:"metadata,omitempty"    json:"metadata,omitempty"`
	// Impl holds per-platform native SDK implementation descriptors.
	// Key is platform name ("ios", "android"). Absent key = capability unavailable on that platform.
	Impl map[string]*PlatformImpl `yaml:"impl,omitempty" json:"impl,omitempty"`
}

// rawToolDef mirrors ToolDef but captures actions: as a raw yaml.Node so
// UnmarshalYAML can dispatch on its Kind (sequence vs mapping) before
// deciding how to build the canonical map[string]*ToolAction.
type rawToolDef struct {
	APIVersion string                   `yaml:"apiVersion"`
	Meta       *ToolMeta                `yaml:"meta,omitempty"`
	Transport  TransportConfig          `yaml:"transport"`
	Governance *ToolGovernance          `yaml:"governance,omitempty"`
	Actions    yaml.Node                `yaml:"actions"`
	Metadata   map[string]string        `yaml:"metadata,omitempty"`
	Impl       map[string]*PlatformImpl `yaml:"impl,omitempty"`
}

// UnmarshalYAML decodes the canonical yawr.tool/v1 vocabulary and builds the
// action lookup used by the runtime.
func (t *ToolDef) UnmarshalYAML(node *yaml.Node) error {
	var raw rawToolDef
	if err := node.Decode(&raw); err != nil {
		return err
	}

	t.APIVersion = raw.APIVersion
	t.Meta = raw.Meta
	t.Transport = raw.Transport
	t.Governance = raw.Governance
	t.Metadata = raw.Metadata
	t.Impl = raw.Impl

	if raw.Meta != nil {
		if raw.Meta.Name != "" {
			t.Name = raw.Meta.Name
		}
		if raw.Meta.Version != "" {
			t.Version = raw.Meta.Version
		}
		if raw.Meta.Description != "" {
			t.Description = raw.Meta.Description
		}
	}

	actions, err := decodeToolActions(&raw.Actions)
	if err != nil {
		return err
	}
	t.Actions = actions
	return nil
}

func decodeToolActions(node *yaml.Node) (map[string]*ToolAction, error) {
	out := make(map[string]*ToolAction)
	if node == nil || node.Kind == 0 {
		return out, nil
	}
	if node.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("actions: must be a sequence, got %v", node.Kind)
	}
	for i, item := range node.Content {
		var entry struct {
			Name       string `yaml:"name"`
			ToolAction `yaml:",inline"`
		}
		if err := item.Decode(&entry); err != nil {
			return nil, fmt.Errorf("actions[%d]: %w", i, err)
		}
		if entry.Name == "" {
			return nil, fmt.Errorf("actions[%d]: missing required name:", i)
		}
		if _, dup := out[entry.Name]; dup {
			return nil, fmt.Errorf("actions[%d]: duplicate action name %q", i, entry.Name)
		}
		action := entry.ToolAction
		out[entry.Name] = &action
	}
	return out, nil
}

// PlatformImpl describes how to invoke a tool on a specific platform.
type PlatformImpl struct {
	Transport string `yaml:"transport" json:"transport"` // e.g. "native-sdk"
	Handler   string `yaml:"handler"   json:"handler"`   // e.g. "YawrSDK.Camera.capture"
}

// ExecuteSpec is the per-action substitution declaration
// (design/yawr/sections/06-tool-runtime.tex §Action Substitution). The
// per-action key is execute: (not the tool-level impl: block, which is an
// unrelated, per-platform mobile-dispatch concept).
type ExecuteSpec struct {
	// Kind is "process" (default) or "runbook".
	Kind string `yaml:"kind,omitempty" json:"kind,omitempty"`
	// Path resolves relative to the DECLARING .tool.yaml's own directory
	// (design/yawr/sections/06-tool-runtime.tex Table tab:tool-path-bases),
	// never the calling runbook, workspace root, or process cwd.
	Path string `yaml:"path,omitempty" json:"path,omitempty"`
}

type FrozenToolSubstitution struct {
	Bindings           []Binding          `yaml:"-" json:"bindings,omitempty"`
	PackageName        string             `yaml:"-" json:"package_name,omitempty"`
	RunbookPath        string             `yaml:"-" json:"runbook_path"`
	RunbookID          string             `yaml:"-" json:"runbook_id"`
	RunbookName        string             `yaml:"-" json:"runbook_name"`
	RunbookContentHash string             `yaml:"-" json:"runbook_content_hash"`
	ExecutableClosure  json.RawMessage    `yaml:"-" json:"executable_closure"`
	Inputs             map[string]*Input  `yaml:"-" json:"inputs,omitempty"`
	Outputs            map[string]*Output `yaml:"-" json:"outputs,omitempty"`
	Governance         *GovernanceConfig  `yaml:"-" json:"governance,omitempty"`
}

// IsSubstitution reports whether this execute spec designates a
// runbook-backed (substituted) action rather than a process-backed one.
func (e *ExecuteSpec) IsSubstitution() bool {
	return e != nil && e.Kind == "runbook"
}

// ToolAction is a single invocable action declared in a tool definition.
type ToolAction struct {
	Description string             `yaml:"description,omitempty" json:"description,omitempty"`
	Argv        []string           `yaml:"argv,omitempty"        json:"argv,omitempty"`
	Args        map[string]*ArgDef `yaml:"args,omitempty"        json:"args,omitempty"`
	Returns     string             `yaml:"returns,omitempty"     json:"returns,omitempty"`

	// Classification is per-action: "read-only", "mutating", "destructive",
	// or "unspecified". nil means unspecified. Pointer + omitempty so nil
	// (absent) is distinguishable from an explicit empty string.
	// Classification is ORTHOGONAL to RequiresApproval: an explicit
	// requires-approval: false never implies read-only, and a read-only
	// classification does not suppress approval requirements.
	Classification *string `yaml:"classification,omitempty" json:"classification,omitempty"`

	// Idempotent declares that this action is safe to retry after a lost or
	// late result. Only meaningful when Classification is "read-only"; ignored
	// for mutating, destructive, and unspecified actions (which are never
	// automatically retried regardless of this flag).
	// Absent (nil) means the action does NOT declare idempotency.
	Idempotent *bool `yaml:"idempotent,omitempty" json:"idempotent,omitempty"`

	// Execute declares this action's implementation kind. Absent or
	// kind: process is a normal process-backed action; kind: runbook makes
	// this a substituted action (design/yawr/sections/06-tool-runtime.tex
	// §Action Substitution).
	Execute *ExecuteSpec `yaml:"execute,omitempty" json:"execute,omitempty"`

	FrozenSubstitution *FrozenToolSubstitution `yaml:"-" json:"frozen_substitution,omitempty"`

	// Outputs declares the named, typed values this action MUST produce.
	// Required for substituted (execute.kind: runbook) actions; also
	// enforced at runtime for non-substituted actions whose tool definition
	// declares this block (ca867a1, 4e67655). Absent means no output
	// contract is enforced.
	Outputs map[string]*ArgDef `yaml:"outputs,omitempty" json:"outputs,omitempty"`

	// OutputContract is the OPT-IN per-action policy controlling how the
	// output enforcement step treats raw semantic output keys that are NOT
	// part of the declared outputs: contract. Absent (nil) preserves the
	// strict default: any undeclared, non-process-channel key REJECTS the
	// result with the standard output contract violation error. See
	// OutputContract for the intended use and supported values.
	OutputContract *OutputContract `yaml:"output_contract,omitempty" json:"output_contract,omitempty"`

	// Result declares an opt-in action-level parser for native process results.
	// Absent means native stdout/stderr remain plain process channels only.
	Result *ActionResultContract `yaml:"result,omitempty" json:"result,omitempty"`

	// VscodeTool overrides the VS Code vscode.lm.tools registered name for
	// this specific action when the transport is vscode-mcp. Takes
	// precedence over the transport-level vscode_tool. Absent falls back to
	// the transport-level value, then to the logical tool name.
	// Use schema.ResolveVSCodeToolName to apply the full resolution order.
	// Only valid when the resolved transport mode is vscode-mcp (VSCODE-001).
	VscodeTool *string `yaml:"vscode_tool,omitempty" json:"vscode_tool,omitempty"`

	// VSCodeInput declares the parameter mapping from logical action args to
	// MCP provider input parameters for vscode-mcp transport invocations.
	// Map key = provider (MCP) parameter name; value = mapping descriptor.
	// Only valid when the transport mode is vscode-mcp (VSCODE-002).
	// Absent = pass args through unchanged (backward compatible).
	// Validated at scan/plan time; applied in core before bridge wire.
	VSCodeInput map[string]*VSCodeInputMapping `yaml:"vscode_input,omitempty" json:"vscode_input,omitempty"`

	// MCPTool overrides the remote MCP tool name for this action when the
	// transport is mcp-http. Absent means use the logical action name.
	MCPTool string `yaml:"mcp_tool,omitempty" json:"mcp_tool,omitempty"`

	// MCPInput declares the parameter mapping from logical action args to remote
	// MCP tool arguments for mcp-http. Map key = remote MCP parameter name.
	// Absent = pass args through unchanged.
	MCPInput map[string]*VSCodeInputMapping `yaml:"mcp_input,omitempty" json:"mcp_input,omitempty"`
}

// AdditionalOutputsIgnore is the only non-empty additional_outputs policy
// value. It DROPS undeclared raw semantic output keys from the validated
// output rather than rejecting them.
const AdditionalOutputsIgnore = "ignore"

// OutputContract configures how an action's output enforcement step treats
// raw semantic output keys that are not declared in its outputs: block.
//
// It is intended for response-shaped tools whose provider OWNS the payload
// and may add non-routing metadata over time (e.g. the IcM incident record,
// which has grown acknowledge*, howFixed, description, isOutage fields). It
// is deliberately NOT an escape hatch from declaring fields a runbook
// consumes: every routing-critical or operator-relevant output MUST still be
// declared in outputs:, so router behavior and captures depend only on
// explicitly contracted fields.
type OutputContract struct {
	// AdditionalOutputs governs unknown (undeclared, non-process-channel)
	// raw semantic output keys:
	//   - "" (absent): STRICT default — unknown keys REJECT the result with
	//     the standard "undeclared output" contract violation error.
	//   - "ignore" (AdditionalOutputsIgnore): unknown keys are deliberately
	//     DROPPED from the validated output. They are NOT captured,
	//     templated, persisted, or passed downstream. This is a drop policy,
	//     NOT an allow / pass-through policy.
	// Any other value is a schema validation error at parse time (TOOL-OC1),
	// never a silent runtime fallback.
	AdditionalOutputs string `yaml:"additional_outputs,omitempty" json:"additional_outputs,omitempty"`
}

const (
	ActionResultFormatQueryResultV1 = "yawr.query-result/v1"
	ActionResultSourceStdoutJSON    = "stdout-json"
)

// ActionResultContract is an opt-in, action-scoped parser for native process
// stdout. It is intentionally narrow: yawr.query-result/v1 maps a single JSON
// object from stdout to typed query outputs while preserving stdout/stderr and
// exit_code as process diagnostics.
type ActionResultContract struct {
	Format   string `yaml:"format"             json:"format"`
	Source   string `yaml:"source"             json:"source"`
	RowCount string `yaml:"row_count"          json:"row_count"`
	Columns  string `yaml:"columns"            json:"columns"`
	Rows     string `yaml:"rows"               json:"rows"`
	Metadata string `yaml:"metadata,omitempty" json:"metadata,omitempty"`
}

func (c *ActionResultContract) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("result must be a mapping")
	}
	known := map[string]bool{
		"format":    true,
		"source":    true,
		"row_count": true,
		"columns":   true,
		"rows":      true,
		"metadata":  true,
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		if !known[key] {
			return fmt.Errorf("result.%s is not a recognized field", key)
		}
	}
	type alias ActionResultContract
	var out alias
	if err := node.Decode(&out); err != nil {
		return err
	}
	*c = ActionResultContract(out)
	return nil
}

// VSCodeInputMapping declares how one MCP provider parameter is populated
// from a logical tool action argument. Used in ToolAction.VSCodeInput and
// ToolAction.MCPInput; retained under its existing name for compatibility.
// Map key (in either input map) = provider/remote MCP parameter name.
type VSCodeInputMapping struct {
	// From is the logical argument name from the action's declared args:.
	// Required; must name a declared arg (validated at scan time).
	From string `yaml:"from"               json:"from"`
	// Coerce optionally coerces the logical argument value to the given type
	// before placing it on the bridge wire. Supported: "integer", "number",
	// "string", "boolean". Absent = no coercion (pass through as-is).
	// Lossy conversion is a hard error, not a truncation.
	Coerce string `yaml:"coerce,omitempty"   json:"coerce,omitempty"`
	// Required, when true, causes invocation to fail before any bridge
	// request if the logical argument named by From is absent or nil.
	Required bool `yaml:"required,omitempty" json:"required,omitempty"`
}

// ArgDef defines a single argument for a tool action. The same shape is
// used for both S1 (action args:) and S2 (substituted action outputs:).
type ArgDef struct {
	Presentation *PresentationDescriptor `yaml:"presentation,omitempty" json:"presentation,omitempty"`
	Type         string                  `yaml:"type"                 json:"type"`
	Required     bool                    `yaml:"required,omitempty"   json:"required,omitempty"`
	Description  string                  `yaml:"description,omitempty" json:"description,omitempty"`
	Default      any                     `yaml:"default,omitempty"    json:"default,omitempty"`
	// From projects an output declaration from a differently named or nested
	// field in the raw tool result. Supported syntax is intentionally narrow:
	// dotted object paths plus keyed array selectors such as
	// customFields[Name=DatabaseName|CustomerDatabaseName].StringValue.
	// It is only meaningful for action outputs.
	From string `yaml:"from,omitempty" json:"from,omitempty"`
	// Optional marks an output declaration as allowed to be absent. Missing
	// optional outputs are not invented in the validated output map.
	Optional bool `yaml:"optional,omitempty" json:"optional,omitempty"`
	// Enum declares S1/S2's value-domain constraint (AR-ENUM-1..15): valid
	// only when Type == "string"; forbidden on Type == "secret" (ENUM-001,
	// C1). No JSON Schema governs .tool.yaml (C3, no tool.v1.schema.json),
	// so well-formedness (ENUM-002..005) is enforced by EnumConstraint's
	// own UnmarshalYAML plus ValidateMembers, called from
	// internal/tool.ParseToolFile.
	Enum EnumConstraint `yaml:"enum,omitempty" json:"enum,omitempty"`
}

// TransportConfig declares how the tool binary or remote service is invoked.
//
// # Mode: mcp-http
//
// Use mode: mcp-http to connect to a remote MCP server over HTTPS.  No local
// binary is spawned; the host speaks the MCP Streamable HTTP protocol
// (version 2025-03-26) over a persistent session.
//
//	transport:
//	  mode: mcp-http
//	  url: https://icm-mcp-prod.azure-api.net/v1/
//	  auth:
//	    provider: azure-cli
//	    scope: api://icmmcpapi-prod/mcp.tools
//	    allowed_hosts:
//	      - icm-mcp-prod.azure-api.net
//
// url: is required and must use https://.  Plain HTTP is rejected at
// validation time (MCP-001); there is no escape hatch.
//
// auth: is optional.  When absent, no Authorization header is sent.  When
// present, the provider acquires a bearer token at runtime — see [AuthConfig].
//
// command: and args: are invalid on mcp-http (MCP rejects them rather than
// silently ignoring them — an authoring error is worse than a clear failure).
//
// # Mode: mcp (stdio)
//
// Use mode: mcp to spawn a local MCP server subprocess.  command: is
// required; url: and auth: are invalid.  The stdio MCP transport uses
// protocol version 2024-11-05 — deliberately different from mcp-http's
// 2025-03-26, because they are different transport specifications.
type TransportConfig struct {
	Type Transport `yaml:"-" json:"-"`
	// Mode is the serialized field name. Type is the runtime's typed view.
	Mode    string            `yaml:"mode,omitempty"    json:"mode,omitempty"`
	Command string            `yaml:"command,omitempty" json:"command,omitempty"`
	Args    []string          `yaml:"args,omitempty"    json:"args,omitempty"`
	Env     map[string]string `yaml:"env,omitempty"     json:"env,omitempty"`
	// URL is required for mode: mcp-http; must use https:// (B-22).
	// Plain HTTP is rejected at validation time — there is no override.
	URL string `yaml:"url,omitempty" json:"url,omitempty"`
	// Auth configures bearer-token acquisition for mode: mcp-http.
	// When absent, no Authorization header is sent. When present,
	// AllowedHosts must be declared — see AuthConfig.
	Auth *AuthConfig `yaml:"auth,omitempty" json:"auth,omitempty"`
	// VscodeTool declares the default VS Code vscode.lm.tools registered
	// name for all actions in this tool. Only meaningful when mode is
	// vscode-mcp; rejected at parse time for any other transport (VSCODE-001).
	// Per-action vscode_tool overrides this value. Absent means fall back to
	// the logical tool name. Use schema.ResolveVSCodeToolName for resolution.
	VscodeTool *string `yaml:"vscode_tool,omitempty" json:"vscode_tool,omitempty"`
}

// AuthConfig declares which auth provider to use to acquire a bearer token
// for an mcp-http transport.
//
// # What this block is (and is not)
//
// auth: names a provider and a scope.  It never contains a credential, token,
// or secret.  Yawr acquires the token at runtime; the token value never
// appears in YAML, logs, trace events, or evidence stores.
//
// # AllowedHosts — what it protects against
//
// Bearer tokens are audience-scoped: a token for api://icmmcpapi-prod/mcp.tools
// is rejected by any service that isn't the declared audience.  This prevents
// an attacker-controlled server from consuming the token at their own service.
//
// It does NOT prevent replay.  A token delivered to an attacker-controlled host
// can be exfiltrated and then presented to the real IcM endpoint, impersonating
// the operator for the token's full lifetime.  Possession is authorization.
//
// AllowedHosts is the prevention mechanism.  Before attaching an Authorization
// header, the HTTP transport checks the request URL's hostname against this
// list.  If the hostname is not listed, the request fails (MCP-012) rather than
// delivering the token to an unapproved host.
//
// # Matching semantics
//
// Matching is exact, case-insensitive, on the parsed hostname with port
// stripped.  Wildcards are not supported — *.azure-api.net does not match
// icm-mcp-prod.azure-api.net.  List each permitted hostname explicitly.
//
// The same semantics apply at parse time (MCP-011 on url:) and at runtime
// (MCP-012 in TokenGate.AttachToken).  The two checks use identical logic
// so a config that passes validation cannot fail at runtime due to drift.
//
// # Adding a new provider
//
// Provider is a plain string.  To add a new provider:
//  1. Add its name to knownAuthProviders in internal/tool/validate_transport.go.
//  2. Implement AuthProvider in internal/tool/auth_<provider>.go.
//  3. Add a case in NewAuthProvider.
//
// The AuthConfig struct itself is unchanged — no breaking change to .tool.yaml syntax.
type AuthConfig struct {
	// Provider selects the token-acquisition mechanism. Recognised values
	// in this iteration: "azure-cli". Future: "env", "managed-identity",
	// "device-code".
	Provider string `yaml:"provider"       json:"provider"`
	// Scope is the OAuth2 scope passed to the auth provider (for Azure CLI,
	// az account get-access-token --scope <scope>). Not a credential.
	Scope string `yaml:"scope,omitempty" json:"scope,omitempty"`
	// Resource is the OAuth2 resource/audience passed to providers that support
	// resource-token acquisition (for Azure CLI, az account get-access-token
	// --resource <resource>). Scope and Resource are mutually exclusive.
	Resource string `yaml:"resource,omitempty" json:"resource,omitempty"`
	// AllowedHosts is the exhaustive list of hostnames (without port, without
	// scheme) that are permitted to receive the bearer token acquired by this
	// auth provider.  It is REQUIRED whenever auth: is configured (B-32).
	//
	// Why this exists: audience-scoped tokens prevent a malicious host from
	// consuming the token at their own service (wrong audience is rejected),
	// but they do NOT prevent a malicious host from replaying the token
	// against the real, legitimate endpoint — because possession is
	// authorization. AllowedHosts is the prevention mechanism: the HTTP
	// transport checks the request URL's hostname against this list before
	// attaching the Authorization header. A match must be exact
	// (case-insensitive, parsed host, no wildcard). If the URL's host is not
	// listed, the request fails with MCP-012.
	//
	// Hosts that are never expected to receive the token (e.g. a proxy, a
	// CDN endpoint, a health check URL) should be routed through an
	// unauthenticated transport instead.
	AllowedHosts []string `yaml:"allowed_hosts,omitempty" json:"allowed_hosts,omitempty"`
}

// UnmarshalYAML converts the serialized mode to the runtime's typed view.
func (t *TransportConfig) UnmarshalYAML(node *yaml.Node) error {
	type rawTransport TransportConfig
	var raw rawTransport
	if err := node.Decode(&raw); err != nil {
		return err
	}
	*t = TransportConfig(raw)
	t.Type = Transport(t.Mode)
	return nil
}

func (t *TransportConfig) UnmarshalJSON(data []byte) error {
	type rawTransport TransportConfig
	var raw rawTransport
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*t = TransportConfig(raw)
	t.Type = Transport(t.Mode)
	return nil
}

func (t TransportConfig) MarshalJSON() ([]byte, error) {
	type rawTransport TransportConfig
	raw := rawTransport(t)
	if raw.Mode == "" {
		raw.Mode = string(raw.Type)
	}
	return json.Marshal(raw)
}

func (t TransportConfig) MarshalYAML() (any, error) {
	type rawTransport TransportConfig
	raw := rawTransport(t)
	if raw.Mode == "" {
		raw.Mode = string(raw.Type)
	}
	return raw, nil
}

// Transport enumerates the supported tool transport protocols.
type Transport string

const (
	TransportStdio     Transport = "stdio"
	TransportJSONRPC   Transport = "jsonrpc"
	TransportMCP       Transport = "mcp"
	TransportNative    Transport = "native"
	TransportMCPHTTP   Transport = "mcp-http"
	TransportVSCodeMCP Transport = "vscode-mcp"
)
