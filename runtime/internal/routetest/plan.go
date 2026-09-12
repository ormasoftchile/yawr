package routetest

import (
	"fmt"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// ValidateScenarioPlan resolves every saved selector against the frozen plan.
func ValidateScenarioPlan(scenario Scenario, plan *engine.ExecutionPlan) error {
	if plan == nil {
		return fmt.Errorf("route test: plan is required")
	}
	index := buildPlanIndex(plan)
	for _, step := range plan.Steps {
		switch step.Kind {
		case "include":
			spec, _ := step.Spec.(*schema.IncludeSpec)
			if spec == nil {
				return fmt.Errorf("route test: include %s has no frozen definition", step.ID)
			}
			if spec.Include.IsDynamic() {
				return fmt.Errorf("route test: dynamic include %s is not supported", step.ID)
			}
			if spec.LazyRunbookPath != "" {
				return fmt.Errorf("route test: include %s must be expanded eagerly", step.ID)
			}
		case "iterate":
			spec, _ := step.Spec.(*schema.IterateNode)
			if spec == nil {
				return fmt.Errorf("route test: iterate %s has no frozen definition", step.ID)
			}
			if spec.Concurrency > 1 {
				return fmt.Errorf("route test: iterate %s concurrency must not exceed 1", step.ID)
			}
		}
	}
	for name := range scenario.Inputs {
		declaration := plan.Inputs[name]
		if declaration == nil {
			return fmt.Errorf("route test: input %q is not declared by the runbook", name)
		}
		if declaration.Type == "secret" {
			return fmt.Errorf("route test: secret input %q cannot be persisted", name)
		}
	}
	if _, err := index.resolve(scenario.Target); err != nil {
		return fmt.Errorf("route test target %s %w", selectorLabel(scenario.Target), err)
	}
	if index.parallelDescendants[planSelectorKey(scenario.Target.CallPath, scenario.Target.Step)] {
		return fmt.Errorf("route test target %s is beneath parallel; exact multi-cursor pause is not supported", selectorLabel(scenario.Target))
	}
	for _, binding := range scenario.StepResponses {
		step, err := index.resolve(binding.At)
		if err != nil {
			return fmt.Errorf("route test %s response at %s %w", binding.Kind, selectorLabel(binding.At), err)
		}
		if step.Kind != binding.Kind {
			return kindMismatch(binding.At, binding.Kind, step.Kind)
		}
	}
	for _, binding := range scenario.HostActionResponses {
		step, err := index.resolve(binding.At)
		if err != nil {
			return fmt.Errorf("route test host action at %s %w", selectorLabel(binding.At), err)
		}
		if step.Kind != "host_action" {
			return kindMismatch(binding.At, "host_action", step.Kind)
		}
		if spec, ok := step.Spec.(*schema.HostActionSpec); ok && spec != nil && binding.Capability != "" && binding.Capability != spec.HostAction.Capability {
			return fmt.Errorf("route test: host action at %s capability is %q, want %q", selectorLabel(binding.At), binding.Capability, spec.HostAction.Capability)
		}
	}
	for _, binding := range scenario.InteractionAnswers {
		step, err := index.resolve(binding.At)
		if err != nil {
			return fmt.Errorf("route test %s answer at %s %w", binding.Kind, selectorLabel(binding.At), err)
		}
		if step.Kind != binding.Kind {
			return kindMismatch(binding.At, binding.Kind, step.Kind)
		}
		if binding.Kind == "collector" {
			if err := validateCollectorPersistence(step, binding.Values); err != nil {
				return err
			}
		}
	}
	for _, binding := range scenario.TestApprovals {
		if _, err := index.resolve(binding.At); err != nil {
			return fmt.Errorf("route test approval at %s %w", selectorLabel(binding.At), err)
		}
	}
	return nil
}

type planIndex struct {
	byQualified         map[string]engine.ResolvedStep
	parallelDescendants map[string]bool
}

func buildPlanIndex(plan *engine.ExecutionPlan) planIndex {
	qualified := make(map[string]engine.ResolvedStep, len(plan.Steps))
	parallelDescendants := make(map[string]bool)
	var ancestors []string
	var ancestorKinds []string
	for _, step := range plan.Steps {
		if step.Depth < len(ancestors) {
			ancestors = ancestors[:step.Depth]
			ancestorKinds = ancestorKinds[:step.Depth]
		}
		path := append([]string(nil), ancestors...)
		key := planSelectorKey(path, step.ID)
		qualified[key] = step
		for _, kind := range ancestorKinds {
			if kind == "parallel" {
				parallelDescendants[key] = true
				break
			}
		}
		if step.Depth == len(ancestors) {
			ancestors = append(ancestors, step.ID)
			ancestorKinds = append(ancestorKinds, step.Kind)
		}
	}
	return planIndex{byQualified: qualified, parallelDescendants: parallelDescendants}
}

func (index planIndex) resolve(selector Selector) (engine.ResolvedStep, error) {
	step, ok := index.byQualified[planSelectorKey(selector.CallPath, selector.Step)]
	if !ok {
		return engine.ResolvedStep{}, fmt.Errorf("does not resolve in the frozen plan")
	}
	return step, nil
}

func planSelectorKey(path []string, step string) string {
	return strings.Join(append(append([]string(nil), path...), step), "\x00")
}

func kindMismatch(selector Selector, want, got string) error {
	return fmt.Errorf("route test: binding at %s has kind %q, but the frozen plan has %q", selectorLabel(selector), want, got)
}

func validateCollectorPersistence(step engine.ResolvedStep, values map[string]any) error {
	spec, ok := step.Spec.(*schema.CollectorSpec)
	if !ok || spec == nil {
		return fmt.Errorf("route test: collector at %s has no resolved field contract", step.ID)
	}
	fields := make(map[string]schema.CollectorField, len(spec.Fields))
	for _, field := range spec.Fields {
		fields[field.Name] = field
	}
	for name := range values {
		field, exists := fields[name]
		if !exists {
			return fmt.Errorf("route test: collector %s field %q is not declared", step.ID, name)
		}
		if field.Ephemeral {
			return fmt.Errorf("route test: collector %s field %q is ephemeral and cannot be persisted", step.ID, name)
		}
	}
	return nil
}
