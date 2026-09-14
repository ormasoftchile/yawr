package schema

import (
	"fmt"
	"regexp"

	"github.com/ormasoftchile/yawr/runtime/pkg/gis"
	gxlparser "github.com/ormasoftchile/yawr/runtime/pkg/gxl/parser"
)

var bindingNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func ReservedBindingName(name string) bool {
	switch name {
	case "vars", "outputs", "steps", "inputs", "env", "secrets", "run", "str", "list", "date", "regex", "left", "right":
		return true
	}
	return len(name) >= 2 && name[:2] == "__"
}

func ValidateTypedDeclaration(kind string, members EnumConstraint) error {
	switch kind {
	case "any", "string", "bool", "boolean", "int", "integer", "number", "float", "array", "object":
	default:
		return fmt.Errorf("unsupported typed declaration %q", kind)
	}
	if members != nil {
		if kind != "string" {
			return fmt.Errorf("enum requires string declaration")
		}
		err, _ := ValidateMembers(members)
		return err
	}
	return nil
}

func TypedTreeReferences(value any) ([]string, error) {
	type item struct {
		value any
		depth int
	}
	queue := []item{{value, 0}}
	var references []string
	visits := 0
	for len(queue) > 0 {
		current := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		visits++
		if visits > 65536 || current.depth > 128 {
			return nil, fmt.Errorf("typed construction syntax budget exceeded")
		}
		switch typed := current.value.(type) {
		case string:
			template, err := gis.ParsePure(typed)
			if err != nil {
				return nil, err
			}
			for _, segment := range template.Segments {
				if expression, ok := segment.(*gis.Expr); ok {
					refs, err := gxlparser.PureReferences(expression.Parsed)
					if err != nil {
						return nil, err
					}
					references = append(references, refs...)
				}
			}
		case map[string]any:
			for _, value := range typed {
				queue = append(queue, item{value, current.depth + 1})
			}
		case []any:
			for _, value := range typed {
				queue = append(queue, item{value, current.depth + 1})
			}
		}
	}
	return references, nil
}

func HasResults(nodes []FlowNode) bool {
	for _, node := range nodes {
		if step := node.Step; step != nil {
			if step.Type == StepTypeResults || PublishesResults(step.EndSpec) ||
				PublishesResults(step.BranchSpec) || PublishesResults(step.CompensateSpec) {
				return true
			}
		}
		if PublishesResults(node.Iterate) || PublishesResults(node.Parallel) {
			return true
		}
	}
	return false
}

// PublishesResults stays inside the declaring invocation; includes own their outputs.
func PublishesResults(spec any) bool {
	switch spec := spec.(type) {
	case *ResultsSpec:
		return spec != nil
	case *EndSpec:
		return spec != nil && spec.PublishResults
	case *BranchSpec:
		if spec != nil {
			for _, arm := range spec.Branches {
				if HasResults(arm.Steps) {
					return true
				}
			}
		}
	case *IterateNode:
		return spec != nil && HasResults(spec.Steps)
	case *ParallelNode:
		if spec != nil {
			for _, arm := range spec.Branches {
				if HasResults(arm.Steps) {
					return true
				}
			}
		}
	case *CompensateSpec:
		return spec != nil && HasResults(spec.Compensate.Steps)
	}
	return false
}

// ValidateTypedRunbook admits invocation declarations before dispatch. The
// ordered list is not topologically reordered, including ambient-name shadows.
func ValidateTypedRunbook(rb *Runbook) error {
	if rb == nil {
		return nil
	}
	bindings := make(map[string]Binding, len(rb.Bindings))
	for _, binding := range rb.Bindings {
		if !bindingNamePattern.MatchString(binding.Name) || ReservedBindingName(binding.Name) {
			return fmt.Errorf("bindings.%s: invalid or reserved name", binding.Name)
		}
		if _, exists := bindings[binding.Name]; exists {
			return fmt.Errorf("bindings.%s: duplicate declaration", binding.Name)
		}
		if !binding.ValuePresent {
			return fmt.Errorf("bindings.%s: value is required", binding.Name)
		}
		if err := ValidateTypedDeclaration(binding.Type, binding.Enum); err != nil {
			return fmt.Errorf("bindings.%s: %w", binding.Name, err)
		}
		bindings[binding.Name] = binding
	}
	available := make(map[string]bool)
	for name := range rb.Inputs {
		available[name] = true
	}
	for name := range rb.Vars {
		available[name] = true
	}
	for name := range bindings {
		delete(available, name)
	}
	for _, binding := range rb.Bindings {
		references, err := TypedTreeReferences(binding.Value)
		if err != nil {
			return fmt.Errorf("bindings.%s: %w", binding.Name, err)
		}
		for _, reference := range references {
			if !available[reference] {
				return fmt.Errorf("bindings.%s: dependency %q is missing, self-referencing or forward", binding.Name, reference)
			}
		}
		available[binding.Name] = true
	}
	var walk func([]FlowNode, bool) (map[string]bool, error)
	walk = func(nodes []FlowNode, nested bool) (map[string]bool, error) {
		writes := make(map[string]bool)
		merge := func(other map[string]bool) {
			for name := range other {
				writes[name] = true
			}
		}
		for index, node := range nodes {
			if node.Step != nil {
				step := node.Step
				if step.Type == StepTypeAssign || step.Type == StepTypeResults || PublishesResults(step.EndSpec) {
					if step.Retry != nil || step.Delay != "" || step.OnError != "" ||
						(rb.Defaults != nil && rb.Defaults.RetryMax != 0) {
						return nil, fmt.Errorf("%s: typed atomic steps forbid retry, delay and failure routing", step.ID)
					}
				}
				if step.Type == StepTypeResults && (nested || index != len(nodes)-1) {
					return nil, fmt.Errorf("%s: results must be last at runbook top level", step.ID)
				}
				if PublishesResults(step.EndSpec) && step.EndSpec.Outcome != nil {
					for name, value := range map[string]string{"category": step.EndSpec.Outcome.Category, "code": step.EndSpec.Outcome.Code} {
						if _, err := TypedTreeReferences(value); err != nil {
							return nil, fmt.Errorf("%s.outcome.%s: %w", step.ID, name, err)
						}
					}
				}
				for name := range step.Capture {
					if ReservedBindingName(name) && (len(bindings) > 0 || HasResults(rb.Flow)) {
						return nil, fmt.Errorf("%s: reserved capture destination %s", step.ID, name)
					}
					if declaration, ok := bindings[name]; ok {
						if !declaration.Mutable {
							return nil, fmt.Errorf("%s: capture cannot write immutable %s", step.ID, name)
						}
						writes[name] = true
					}
				}
				if step.Type == StepTypeAssign {
					if step.AssignSpec == nil || len(step.AssignSpec.Assign) == 0 {
						return nil, fmt.Errorf("%s: assign requires writes", step.ID)
					}
					for _, assignment := range step.AssignSpec.Assign {
						declaration, ok := bindings[assignment.Name]
						if !ok || !declaration.Mutable {
							return nil, fmt.Errorf("%s: assign destination %s is not mutable", step.ID, assignment.Name)
						}
						if !assignment.ValuePresent {
							return nil, fmt.Errorf("%s: assignment value is required", step.ID)
						}
						if _, err := TypedTreeReferences(assignment.Value); err != nil {
							return nil, err
						}
						writes[assignment.Name] = true
					}
				}
				if step.BranchSpec != nil {
					for _, branch := range step.BranchSpec.Branches {
						child, err := walk(branch.Steps, true)
						if err != nil {
							return nil, err
						}
						merge(child)
					}
				}
				if step.CompensateSpec != nil {
					child, err := walk(step.CompensateSpec.Compensate.Steps, true)
					if err != nil {
						return nil, err
					}
					merge(child)
				}
			}
			if node.Iterate != nil {
				if node.Iterate.Concurrency > 1 && HasResults(node.Iterate.Steps) {
					return nil, fmt.Errorf("%s: concurrent iterations cannot publish terminal results", node.Iterate.ID)
				}
				child, err := walk(node.Iterate.Steps, true)
				if err != nil {
					return nil, err
				}
				if node.Iterate.Concurrency > 1 && len(child) > 0 {
					return nil, fmt.Errorf("%s: concurrent iterations write declared mutable bindings", node.Iterate.ID)
				}
				merge(child)
			}
			if node.Parallel != nil {
				parallel := make(map[string]bool)
				for _, branch := range node.Parallel.Branches {
					if HasResults(branch.Steps) {
						return nil, fmt.Errorf("%s: parallel branches cannot publish terminal results", node.Parallel.ID)
					}
					child, err := walk(branch.Steps, true)
					if err != nil {
						return nil, err
					}
					for name := range child {
						if parallel[name] {
							return nil, fmt.Errorf("%s: conflicting parallel write to %s", node.Parallel.ID, name)
						}
						parallel[name] = true
					}
				}
				merge(parallel)
			}
		}
		return writes, nil
	}
	if _, err := walk(rb.Flow, false); err != nil {
		return err
	}
	for name, output := range rb.Outputs {
		if output == nil {
			continue
		}
		if !output.ValueTreePresent && !HasResults(rb.Flow) {
			continue
		}
		if err := ValidateTypedDeclaration(output.Type, output.Enum); err != nil {
			return fmt.Errorf("outputs.%s: %w", name, err)
		}
		if output.ValueTreePresent {
			if _, err := TypedTreeReferences(output.ValueTree); err != nil {
				return fmt.Errorf("outputs.%s: %w", name, err)
			}
		} else if output.ValueExpr != "" {
			if _, err := gxlparser.ParsePure(output.ValueExpr); err != nil {
				return fmt.Errorf("outputs.%s: %w", name, err)
			}
		} else if _, err := TypedTreeReferences(output.Value); err != nil {
			return fmt.Errorf("outputs.%s: %w", name, err)
		}
	}
	return nil
}
