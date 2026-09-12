package runstore

import (
	"fmt"
	"path/filepath"

	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func validateFrameDeclarations(state engine.RunState, plan *engine.ExecutionPlan) error {
	allowed := make(map[string]bool)
	key := func(kind, path, digest string) string { return kind + "\x00" + filepath.Clean(path) + "\x00" + digest }
	add := func(kind, path string, bindings []schema.Binding, outputs map[string]*schema.Output, flow []schema.FlowNode) {
		allowed[key(kind, path, engine.InvocationDigest(schema.InvocationForRunbook(bindings, outputs, flow)))] = true
	}
	var walkNodes func([]schema.FlowNode)
	var walkSpec func(engine.StepSpec)
	walkNodes = func(nodes []schema.FlowNode) {
		for _, node := range nodes {
			if node.Step != nil {
				walkSpec(node.Step.IncludeSpec)
				walkSpec(node.Step.BranchSpec)
				walkSpec(node.Step.CompensateSpec)
			}
			if node.Iterate != nil {
				walkSpec(node.Iterate)
			}
			if node.Parallel != nil {
				walkSpec(node.Parallel)
			}
		}
	}
	walkSpec = func(spec engine.StepSpec) {
		switch spec := spec.(type) {
		case *schema.IncludeSpec:
			if spec == nil {
				return
			}
			path := spec.ResolvedRunbookPath
			if spec.LazyRunbookPath != "" {
				path = filepath.Clean(spec.LazyRunbookPath)
			}
			add("include", path, spec.ResolvedBindings, spec.ResolvedOutputs, spec.ResolvedSteps)
			walkNodes(spec.ResolvedSteps)
		case *schema.BranchSpec:
			if spec != nil {
				for _, branch := range spec.Branches {
					walkNodes(branch.Steps)
				}
			}
		case *schema.CompensateSpec:
			if spec != nil {
				walkNodes(spec.Compensate.Steps)
			}
		case *schema.IterateNode:
			if spec != nil {
				walkNodes(spec.Steps)
			}
		case *schema.ParallelNode:
			if spec != nil {
				for _, branch := range spec.Branches {
					walkNodes(branch.Steps)
				}
			}
		}
	}
	for _, step := range plan.Steps {
		if step.Depth == 0 {
			walkSpec(step.Spec)
		}
	}
	seen := make(map[string]bool)
	var tools func(map[string]*schema.ToolDef) error
	tools = func(definitions map[string]*schema.ToolDef) error {
		for _, definition := range definitions {
			if definition == nil {
				continue
			}
			for _, action := range definition.Actions {
				if action == nil || action.FrozenSubstitution == nil {
					continue
				}
				frozen := action.FrozenSubstitution
				id := frozen.RunbookPath + "\x00" + string(frozen.ExecutableClosure)
				if seen[id] {
					continue
				}
				seen[id] = true
				flow, err := plansnapshot.RestoreFlowClosure(frozen.ExecutableClosure)
				if err != nil {
					return err
				}
				add("tool-substitution", frozen.RunbookPath, frozen.Bindings, frozen.Outputs, flow)
				walkNodes(flow)
				nested, err := plansnapshot.RestoreFlowTools(frozen.ExecutableClosure)
				if err != nil {
					return err
				}
				if err := tools(nested); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := tools(plan.Tools); err != nil {
		return err
	}
	pins := append([]schema.LockedDynamicInclude(nil), plan.Metadata.DynamicIncludes...)
	for _, resolution := range state.DynamicIncludes {
		if resolution != nil {
			pins = append(pins, resolution.Pin)
		}
	}
	for _, pin := range pins {
		flow, err := plansnapshot.RestoreFlowClosure(pin.ExecutableClosure)
		if err != nil {
			return err
		}
		add("include", pin.AbsPath, pin.ResolvedBindings, pin.ResolvedOutputs, flow)
		walkNodes(flow)
		nested, err := plansnapshot.RestoreFlowTools(pin.ExecutableClosure)
		if err != nil {
			return err
		}
		if err := tools(nested); err != nil {
			return err
		}
	}
	for _, frame := range state.ExecutionFrames {
		if frame == nil || frame.BindingScope == nil {
			continue
		}
		scope := frame.BindingScope
		if scope.FrameID != frame.FrameID {
			owner := state.BindingScope
			if scope.FrameID != "" {
				parent := state.ExecutionFrames[scope.FrameID]
				if parent == nil {
					return fmt.Errorf("runstore: binding owner frame missing")
				}
				owner = parent.BindingScope
			}
			if owner == nil || owner.DeclarationDigest != scope.DeclarationDigest {
				return fmt.Errorf("runstore: structural binding scope differs from its owner")
			}
			continue
		}
		if len(frame.CallPath) == 0 || !allowed[key(frame.Kind, frame.CallPath[len(frame.CallPath)-1].RunbookPath, scope.DeclarationDigest)] {
			return fmt.Errorf("runstore: frame declaration does not originate in frozen closure")
		}
	}
	return nil
}
