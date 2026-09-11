package tool

import (
	"context"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// ToolTransport enum
type TransportType string

const (
	TransportStdio     TransportType = "stdio"
	TransportJSONRPC   TransportType = "stdio-jsonrpc"
	TransportMCP       TransportType = "mcp"
	TransportNative    TransportType = "native"
	TransportMCPHTTP   TransportType = "mcp-http"
	TransportVSCodeMCP TransportType = "vscode-mcp"
)

// ToolTransport — invoke a tool over a transport
type ToolTransport interface {
	Invoke(ctx context.Context, def ToolDef, action string, args map[string]any) (*ToolResult, error)
	Close() error
}

// ToolResult — structured output from a tool
type ToolResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
	Output   map[string]any // parsed JSON output (if tool emits JSON)
}

// ToolDef — tool definition (matches schema.ToolDef shape)
type ToolDef struct {
	Name      string
	Source    string // "builtin://slack-notify", "tool://auth-service", etc.
	Transport TransportType
	Command   string
	Args      []string
	Env       map[string]string
	Actions   map[string]*ToolAction

	// SourcePath is the absolute path of the .tool.yaml this definition was
	// parsed from. Empty for tier-0 (compiled-in builtin) definitions.
	// Required to resolve a substituted action's execute.path
	// (design/yawr/sections/06-tool-runtime.tex Table tab:tool-path-bases:
	// KindExecutePath resolves relative to the declaring .tool.yaml).
	SourcePath string
	// PackageRoot is the absolute root directory execute.path is contained
	// within: the exporting package's root for a package-tier (tier 1)
	// tool, or SourcePath's own directory for an ad-hoc (tier 2/4,
	// non-package) tool, which has no package concept of its own.
	PackageRoot string
	// PackageName identifies the exporting package for tier-1 tools (empty
	// for ad-hoc tools), used for substitution frame identity
	// (pkg/pkgsubst.Frame) and trace provenance.
	PackageName string
	// Governance is this tool definition's own governance block
	// (design/yawr/sections/06-tool-runtime.tex §Tool Definition Schema),
	// the starting point for a substituted action's governance composition
	// (pkg/pkgsubst.EffectiveGovernanceFromTool). Kept as the parsed schema
	// type directly (rather than a mirrored runtime type) since pkgsubst's
	// planning API already consumes *schema.ToolGovernance.
	Governance *schema.ToolGovernance
	// URL is the endpoint for mcp-http transports.
	URL string
	// Auth is the auth config for mcp-http transports.
	Auth *schema.AuthConfig
}

// ToolAction mirrors schema.ToolAction for runtime use
type ToolAction struct {
	Description string
	Argv        []string
	Args        map[string]*ArgDef
	Returns     string

	// Execute mirrors schema.ToolAction.Execute: non-nil with
	// Kind=="runbook" makes this a substituted action
	// (design/yawr/sections/06-tool-runtime.tex §Action Substitution). Kept
	// as the parsed schema type directly since pkgsubst.Plan consumes a
	// *schema.ToolAction (Execute + Outputs together) as a unit.
	Execute *schema.ExecuteSpec
	// Outputs mirrors schema.ToolAction.Outputs: the named, typed values a
	// substituted action MUST produce.
	Outputs map[string]*schema.ArgDef

	// OutputContract mirrors schema.ToolAction.OutputContract: the opt-in
	// per-action policy for how the executor's output enforcement step
	// treats undeclared raw semantic output keys. nil = strict default
	// (undeclared keys reject). Retained as the parsed schema type directly,
	// consistent with Outputs and Execute above.
	OutputContract *schema.OutputContract

	// Result mirrors schema.ToolAction.Result: an opt-in native stdout result
	// parser for yawr.query-result/v1. nil preserves byte-for-byte plain native
	// behavior.
	Result *schema.ActionResultContract

	// VSCodeInput carries the per-action vscode_input mapping declared in the
	// tool definition. Only populated for vscode-mcp transport actions. nil
	// means pass args through unchanged (backward compatible). Applied in
	// internal/tool/runtime.go before args go onto the bridge wire.
	VSCodeInput map[string]*schema.VSCodeInputMapping

	// MCPTool and MCPInput carry the per-action mcp-http remote tool name and
	// argument mapping. Empty/nil means use the logical action and args.
	MCPTool  string
	MCPInput map[string]*schema.VSCodeInputMapping

	// schemaAction is the original parsed action, retained only for
	// substituted (execute.kind: runbook) actions so pkgsubst.Plan can be
	// called with the exact schema shape it validates against, without a
	// lossy runtime<->schema re-conversion step.
	schemaAction *schema.ToolAction
}

// SchemaAction returns the original parsed *schema.ToolAction this runtime
// action was converted from, for substitution planning
// (pkg/pkgsubst.Plan). Returns nil for actions that were not converted from
// a parsed schema action (e.g. compiled-in builtins).
func (a *ToolAction) SchemaAction() *schema.ToolAction {
	if a == nil {
		return nil
	}
	return a.schemaAction
}

// WithSchemaAction attaches the original parsed schema action to a runtime
// ToolAction and returns it, for use by conversion code outside this
// package (internal/tool.RuntimeToolDef).
func (a *ToolAction) WithSchemaAction(schemaAction *schema.ToolAction) *ToolAction {
	a.schemaAction = schemaAction
	return a
}

// ArgDef mirrors schema.ArgDef for runtime use
type ArgDef struct {
	Presentation *schema.PresentationDescriptor
	Type         string
	Required     bool
	Description  string
	Default      any
	From         string
	Optional     bool
	// Enum mirrors schema.ArgDef.Enum (S1 value-domain constraint,
	// AR-ENUM-1..15), carried through for runtime binding-time enforcement
	// (ENUM-008) in the tool executor.
	Enum schema.EnumConstraint
}

// ToolRuntime — the top-level invoker
type ToolRuntime interface {
	Invoke(ctx context.Context, toolName string, action string, args map[string]any) (*ToolResult, error)
}

// ToolDefLookup is an optional capability a ToolRuntime implementation may
// expose so callers (e.g. the tool executor) can inspect a resolved tool
// definition's action metadata -- specifically execute.kind: runbook
// substitution declarations -- before deciding how to invoke it. Not every
// ToolRuntime (e.g. test fakes) implements this; callers MUST type-assert
// and fall back to the plain process-invocation path when absent.
type ToolDefLookup interface {
	LookupDef(name string) (*ToolDef, bool)
}

// ToolRegistry — discovery and lookup
type ToolRegistry interface {
	Lookup(name string) (*ToolDef, bool)
	All() []ToolDef
	Register(def ToolDef) error
}
