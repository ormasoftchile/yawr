package schema

import (
	"path"
	"strings"
)

// CaptureIncludeAliases retains source-local labels before runbook imports
// disappear at a frozen-flow boundary. It does not visit included documents:
// each child must capture labels using its own imports before expansion.
func CaptureIncludeAliases(runbook *Runbook) {
	if runbook == nil {
		return
	}
	var walk func([]FlowNode)
	walk = func(nodes []FlowNode) {
		for _, node := range nodes {
			if step := node.Step; step != nil {
				if step.IncludeSpec != nil && !step.IncludeSpec.Include.IsDynamic() && step.IncludeAlias == "" {
					step.IncludeAlias = LocalIncludeAlias(runbook.Imports, step.IncludeSpec.Include.Runbook)
				}
				if step.BranchSpec != nil {
					for _, arm := range step.BranchSpec.Branches {
						walk(arm.Steps)
					}
				}
				if step.CompensateSpec != nil {
					walk(step.CompensateSpec.Compensate.Steps)
				}
				if step.ParallelSpec != nil {
					for _, branch := range step.ParallelSpec.Branches {
						walk(branch.Steps)
					}
				}
			}
			if node.Iterate != nil {
				walk(node.Iterate.Steps)
			}
			if node.Parallel != nil {
				for _, branch := range node.Parallel.Branches {
					walk(branch.Steps)
				}
			}
		}
	}
	walk(runbook.Flow)
}

// LocalIncludeAlias prefers the exact authored alias. Literal paths receive a
// label only when a single local import names that path; ambiguity stays blank.
func LocalIncludeAlias(imports map[string]string, reference string) string {
	if _, ok := imports[reference]; ok {
		return reference
	}
	key := func(value string) string { return path.Clean(strings.ReplaceAll(value, "\\", "/")) }
	reference = key(reference)
	found := ""
	for alias, target := range imports {
		if key(target) == reference {
			if found != "" {
				return ""
			}
			found = alias
		}
	}
	return found
}
