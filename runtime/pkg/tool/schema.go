package tool

import "github.com/ormasoftchile/yawr/runtime/pkg/schema"

// SchemaFromRuntime is for compiled definitions without an authored tool file.
// Authored definitions should retain their original schema instead.
func SchemaFromRuntime(definition ToolDef) *schema.ToolDef {
	out := &schema.ToolDef{
		Name: definition.Name, Governance: definition.Governance,
		Transport: schema.TransportConfig{Type: schema.Transport(definition.Transport),
			Command: definition.Command, Args: definition.Args, Env: definition.Env,
			URL: definition.URL, Auth: definition.Auth},
		Actions: map[string]*schema.ToolAction{},
	}
	for name, action := range definition.Actions {
		if action == nil {
			continue
		}
		if retained := action.SchemaAction(); retained != nil {
			out.Actions[name] = retained
			continue
		}
		args := map[string]*schema.ArgDef{}
		for name, argument := range action.Args {
			if argument == nil {
				continue
			}
			args[name] = &schema.ArgDef{Type: argument.Type, Required: argument.Required,
				Description: argument.Description, Default: argument.Default, From: argument.From,
				Optional: argument.Optional, Enum: argument.Enum, Presentation: argument.Presentation}
		}
		out.Actions[name] = &schema.ToolAction{Description: action.Description, Argv: action.Argv,
			Args: args, Returns: action.Returns, Execute: action.Execute, Outputs: action.Outputs,
			OutputContract: action.OutputContract, Result: action.Result,
			VSCodeInput: action.VSCodeInput, MCPTool: action.MCPTool, MCPInput: action.MCPInput}
	}
	return schema.CloneToolDef(out)
}
