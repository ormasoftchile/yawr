package executor

import (
	internaldebugprotect "github.com/ormasoftchile/yawr/runtime/internal/debugprotect"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func secretBindingDebugProtection(bindings map[string]string, parentVars map[string]any, inputs map[string]*schema.Input) (engine.DebugProtection, bool) {
	protection := engine.DebugProtection{}
	complete := true
	for childName, declaration := range inputs {
		if declaration == nil || declaration.Type != "secret" {
			continue
		}
		template, bound := bindings[childName]
		if !bound {
			continue
		}
		names, all, err := internaldebugprotect.TemplateVariables(template)
		if err != nil || all {
			complete = false
			names = make([]string, 0, len(parentVars))
			for name := range parentVars {
				names = append(names, name)
			}
		}
		for _, name := range names {
			protection.ProtectedVars = append(protection.ProtectedVars, name)
			value, exists := parentVars[name]
			if !exists {
				continue
			}
			if secret, ok := value.(string); ok && secret != "" {
				protection.SecretValues = append(protection.SecretValues, secret)
			}
		}
	}
	return engine.MergeDebugProtection(engine.DebugProtection{}, protection), complete
}

func inheritedBindingDebugProtection(
	bindings map[string]string,
	childVars map[string]any,
	inherited engine.DebugProtection,
) engine.DebugProtection {
	protected := make(map[string]bool, len(inherited.ProtectedVars))
	for _, name := range inherited.ProtectedVars {
		protected[name] = true
	}
	additional := engine.DebugProtection{}
	for destination, template := range bindings {
		names, all, err := internaldebugprotect.TemplateVariables(template)
		matched := all || err != nil
		for _, name := range names {
			matched = matched || protected[name]
		}
		if !matched {
			continue
		}
		additional.ProtectedVars = append(additional.ProtectedVars, destination)
		if value, ok := childVars[destination].(string); ok && value != "" {
			additional.SecretValues = append(additional.SecretValues, value)
		}
	}
	return engine.MergeDebugProtection(engine.DebugProtection{}, additional)
}
