package schema

// ResolveVSCodeToolName returns the VS Code vscode.lm.tools registered name
// for a given action in a vscode-mcp tool definition.
//
// Resolution order (highest priority first):
//  1. action.VscodeTool — per-action override declared in the actions: block
//  2. transport.VscodeTool — transport-level default declared in transport:
//  3. logicalName — the tool's logical name (fallback; always non-empty)
//
// This is the single authoritative implementation of the resolution order.
// Both the VS Code extension and any Go-side consumer must call this function
// so the precedence logic cannot drift between consumers.
func ResolveVSCodeToolName(transport TransportConfig, logicalName string, action *ToolAction) string {
	if action != nil && action.VscodeTool != nil {
		return *action.VscodeTool
	}
	if transport.VscodeTool != nil {
		return *transport.VscodeTool
	}
	return logicalName
}
