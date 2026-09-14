package planner

import (
	"encoding/json"
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// ValidateTypedBoundClosure includes implicit include propagation and frozen
// tool bodies when proving mutable destinations disjoint under concurrency.
func ValidateTypedBoundClosure(plan *engine.ExecutionPlan) error {
	mutable := func(bindings []schema.Binding) map[string]bool {
		names := make(map[string]bool)
		for _, binding := range bindings {
			if binding.Mutable {
				names[binding.Name] = true
			}
		}
		return names
	}
	var writes func(any, map[string]bool, map[string]bool) (map[string]bool, error)
	writes = func(value any, declared, visiting map[string]bool) (map[string]bool, error) {
		out := make(map[string]bool)
		var scan func(any) error
		scan = func(value any) error {
			switch value := value.(type) {
			case []any:
				for _, child := range value {
					if err := scan(child); err != nil {
						return err
					}
				}
			case map[string]any:
				if captures, ok := value["capture"].(map[string]any); ok {
					for name := range captures {
						if declared[name] {
							out[name] = true
						}
					}
				}
				for _, key := range []string{"assign", "bindings", "resolved_bindings"} {
					if entries, ok := value[key].([]any); ok {
						for _, entry := range entries {
							if entry, ok := entry.(map[string]any); ok {
								name, _ := entry["name"].(string)
								if declared[name] {
									out[name] = true
								}
							}
						}
					}
				}
				if _, dynamic := value["runbook_ref"]; dynamic && len(declared) > 0 {
					return fmt.Errorf("typed concurrency: dynamic include write closure is not frozen")
				}
				if ref, ok := value["tool"].(map[string]any); ok {
					name, _ := ref["name"].(string)
					actionName, _ := ref["action"].(string)
					if definition := plan.Tools[name]; definition != nil {
						if action := definition.Actions[actionName]; action != nil && action.Execute.IsSubstitution() {
							key := name + "#" + actionName
							if visiting[key] || action.FrozenSubstitution == nil {
								return fmt.Errorf("typed concurrency: unresolved or cyclic tool closure %s", key)
							}
							visiting[key] = true
							var body any
							if err := json.Unmarshal(action.FrozenSubstitution.ExecutableClosure, &body); err != nil {
								return err
							}
							if err := scan(body); err != nil {
								return err
							}
							delete(visiting, key)
						}
					}
				}
				for key, child := range value {
					switch key {
					case "value", "value_tree", "args", "default", "with", "vars", "collect_values", "tools":
						continue
					}
					if err := scan(child); err != nil {
						return err
					}
				}
			}
			return nil
		}
		if err := scan(value); err != nil {
			return nil, err
		}
		return out, nil
	}
	// Serialize actual resolved specs, not schema.Step's intentionally omitted
	// inline union payloads.
	var specData func(engine.StepSpec) any
	specData = func(spec engine.StepSpec) any {
		switch spec := spec.(type) {
		case *schema.IncludeSpec:
			if spec == nil {
				return nil
			}
			return map[string]any{"include": spec.Include, "resolved_bindings": spec.ResolvedBindings, "steps": nodesData(spec.ResolvedSteps, specData)}
		case *schema.BranchSpec:
			var branches []any
			for _, arm := range spec.Branches {
				branches = append(branches, nodesData(arm.Steps, specData))
			}
			return branches
		case *schema.IterateNode:
			return nodesData(spec.Steps, specData)
		case *schema.ParallelNode:
			var branches []any
			for _, branch := range spec.Branches {
				branches = append(branches, nodesData(branch.Steps, specData))
			}
			return branches
		case *schema.CompensateSpec:
			return nodesData(spec.Compensate.Steps, specData)
		default:
			return spec
		}
	}
	collect := func(nodes []schema.FlowNode, names map[string]bool) (map[string]bool, error) {
		data, err := json.Marshal(nodesData(nodes, specData))
		if err != nil {
			return nil, err
		}
		var value any
		if err := json.Unmarshal(data, &value); err != nil {
			return nil, err
		}
		return writes(value, names, make(map[string]bool))
	}
	var walkSpec func(engine.StepSpec, map[string]bool) error
	var walkNodes func([]schema.FlowNode, map[string]bool) error
	walkNodes = func(nodes []schema.FlowNode, names map[string]bool) error {
		for _, node := range nodes {
			if node.Step != nil {
				if err := walkSpec(specForStep(node.Step), names); err != nil {
					return err
				}
			}
			if node.Iterate != nil {
				if err := walkSpec(node.Iterate, names); err != nil {
					return err
				}
			}
			if node.Parallel != nil {
				if err := walkSpec(node.Parallel, names); err != nil {
					return err
				}
			}
		}
		return nil
	}
	walkSpec = func(spec engine.StepSpec, names map[string]bool) error {
		switch spec := spec.(type) {
		case *schema.IncludeSpec:
			inherited := mutable(spec.ResolvedBindings)
			for name := range names {
				inherited[name] = true
			}
			return walkNodes(spec.ResolvedSteps, inherited)
		case *schema.BranchSpec:
			for _, arm := range spec.Branches {
				if err := walkNodes(arm.Steps, names); err != nil {
					return err
				}
			}
		case *schema.CompensateSpec:
			return walkNodes(spec.Compensate.Steps, names)
		case *schema.IterateNode:
			if spec.Concurrency > 1 && len(names) > 0 {
				set, err := collect(spec.Steps, names)
				if err != nil {
					return err
				}
				if len(set) > 0 {
					return fmt.Errorf("typed concurrency: iteration %s writes mutable bindings", spec.ID)
				}
			}
			if spec.Concurrency > 1 && schema.HasResults(spec.Steps) {
				return fmt.Errorf("typed concurrency: iteration %s cannot publish terminal results", spec.ID)
			}
			return walkNodes(spec.Steps, names)
		case *schema.ParallelNode:
			all := make(map[string]bool)
			for _, branch := range spec.Branches {
				if schema.HasResults(branch.Steps) {
					return fmt.Errorf("typed concurrency: parallel %s cannot publish terminal results", spec.ID)
				}
				if len(names) > 0 {
					set, err := collect(branch.Steps, names)
					if err != nil {
						return err
					}
					for name := range set {
						if all[name] {
							return fmt.Errorf("typed concurrency: parallel %s conflicts on %s", spec.ID, name)
						}
						all[name] = true
					}
				}
				if err := walkNodes(branch.Steps, names); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, step := range plan.Steps {
		if step.Depth == 0 {
			if err := walkSpec(step.Spec, mutable(plan.Bindings)); err != nil {
				return err
			}
		}
	}
	return nil
}

func nodesData(nodes []schema.FlowNode, specData func(engine.StepSpec) any) []any {
	out := make([]any, 0, len(nodes))
	for _, node := range nodes {
		if node.Step != nil {
			out = append(out, map[string]any{"capture": node.Step.Capture, "spec": specData(specForStep(node.Step))})
		}
		if node.Iterate != nil {
			out = append(out, specData(node.Iterate))
		}
		if node.Parallel != nil {
			out = append(out, specData(node.Parallel))
		}
	}
	return out
}

func ValidateTypedBoundFlow(nodes []schema.FlowNode, bindings []schema.Binding, tools map[string]*schema.ToolDef) error {
	plan := &engine.ExecutionPlan{Bindings: bindings, Tools: tools}
	for _, node := range nodes {
		if node.Step != nil {
			plan.Steps = append(plan.Steps, engine.ResolvedStep{ID: node.Step.ID, Kind: string(node.Step.Type), Spec: specForStep(node.Step)})
		}
		if node.Iterate != nil {
			plan.Steps = append(plan.Steps, engine.ResolvedStep{Spec: node.Iterate})
		}
		if node.Parallel != nil {
			plan.Steps = append(plan.Steps, engine.ResolvedStep{Spec: node.Parallel})
		}
	}
	return ValidateTypedBoundClosure(plan)
}
