package schema

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// Binding is one ordered invocation-local declaration. ValuePresent distinguishes
// an authored null from an omitted initializer in frozen JSON.
type Binding struct {
	Name         string         `yaml:"name" json:"name"`
	Type         string         `yaml:"type" json:"type"`
	Mutable      bool           `yaml:"mutable,omitempty" json:"mutable,omitempty"`
	Value        any            `yaml:"value" json:"value"`
	ValuePresent bool           `yaml:"-" json:"value_present"`
	Enum         EnumConstraint `yaml:"enum,omitempty" json:"enum,omitempty"`
}

type Assignment struct {
	Name         string `yaml:"name" json:"name"`
	Value        any    `yaml:"value" json:"value"`
	ValuePresent bool   `yaml:"-" json:"value_present"`
}

type AssignSpec struct {
	Assign []Assignment `yaml:"assign" json:"assign"`
}

type ResultsSpec struct{}

func (*AssignSpec) StepKind() string  { return "assign" }
func (*ResultsSpec) StepKind() string { return "results" }

func hasYAMLKey(node *yaml.Node, key string) bool {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return true
		}
	}
	return false
}

func (b *Binding) UnmarshalYAML(node *yaml.Node) error {
	type plain Binding
	var value plain
	if err := node.Decode(&value); err != nil {
		return err
	}
	*b = Binding(value)
	b.ValuePresent = hasYAMLKey(node, "value")
	if !b.ValuePresent {
		return fmt.Errorf("binding %q requires value (null is allowed)", b.Name)
	}
	return nil
}

func (a *Assignment) UnmarshalYAML(node *yaml.Node) error {
	type plain Assignment
	var value plain
	if err := node.Decode(&value); err != nil {
		return err
	}
	*a = Assignment(value)
	a.ValuePresent = hasYAMLKey(node, "value")
	if !a.ValuePresent {
		return fmt.Errorf("assignment %q requires value (null is allowed)", a.Name)
	}
	return nil
}

func (o *Output) UnmarshalYAML(node *yaml.Node) error {
	type plain Output
	var value plain
	if err := node.Decode(&value); err != nil {
		return err
	}
	*o = Output(value)
	o.ValueTreePresent = hasYAMLKey(node, "value_tree")
	forms := 0
	for _, name := range []string{"value", "value_expr", "value_tree"} {
		if hasYAMLKey(node, name) {
			forms++
		}
	}
	if forms > 1 {
		return fmt.Errorf("output requires exactly one of value, value_expr, value_tree")
	}
	return nil
}
