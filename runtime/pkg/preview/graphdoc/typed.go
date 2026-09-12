package graphdoc

import "github.com/ormasoftchile/yawr/runtime/pkg/schema"

func typedInvocationDetails(runbook *schema.Runbook) *schema.RunbookInvocation {
	return InvocationDetails(schema.InvocationForRunbook(runbook.Bindings, runbook.Outputs, runbook.Flow))
}

// InvocationDetails projects authored and frozen declarations through the same
// safe graph boundary. It is not a live binding or a public Results record.
func InvocationDetails(invocation *schema.RunbookInvocation) *schema.RunbookInvocation {
	if invocation == nil {
		return nil
	}
	typed := len(invocation.Bindings) != 0 || invocation.Results
	for _, output := range invocation.Outputs {
		typed = typed || output != nil && output.ValueTreePresent
	}
	if !typed {
		return nil
	}
	invocation = schema.CloneInvocation(invocation)
	for i := range invocation.Bindings {
		binding := &invocation.Bindings[i]
		binding.Value = safeAuthoredValue(binding.Name, binding.Value)
		if sensitiveDetailName(binding.Name) {
			binding.Enum = nil
		}
	}
	for name, output := range invocation.Outputs {
		if output == nil {
			continue
		}
		output.ValueTree = safeAuthoredValue(name, output.ValueTree)
		output.Value = safeText(output.Value)
		output.ValueExpr = safeText(output.ValueExpr)
		if sensitiveDetailName(name) {
			output.Enum = nil
		}
	}
	return invocation
}
