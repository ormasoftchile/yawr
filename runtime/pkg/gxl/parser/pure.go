package parser

import (
	"context"
	"fmt"
)

const PureExpressionUnits = 32768
const PureNodeVisits = 65536

// ParsePure uses the existing lexer/parser with bounded admission. Comparator
// literals are admitted recursively; runtime-provided comparators are forbidden.
func ParsePure(source string) (*Expr, error) {
	units := 0
	for _, r := range source {
		units++
		if r > 0xffff {
			units++
		}
		if units > PureExpressionUnits {
			return nil, fmt.Errorf("typed expression exceeds %d UTF-16 units", PureExpressionUnits)
		}
	}
	l := &lexer{src: source, line: 1, col: 1, ctx: context.Background(), tokenLimit: PureNodeVisits}
	if err := l.scan(); err != nil {
		return nil, err
	}
	return parseTokens(l.tokens, false, true)
}

// PureReferences returns the parsed entry dependencies, excluding comparator
// local left/right names. It never evaluates an expression.
func PureReferences(expression *Expr) ([]string, error) {
	if expression == nil {
		return nil, fmt.Errorf("nil typed expression")
	}
	return validatePureTree(expression.Node)
}

func validatePureTree(root Node) ([]string, error) {
	type entry struct {
		node  Node
		depth int
	}
	queue := []entry{{root, 1}}
	var refs []string
	visits := 0
	for len(queue) > 0 {
		current := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		visits++
		if current.depth > 128 || visits > PureNodeVisits {
			return nil, fmt.Errorf("typed expression syntax budget exceeded")
		}
		add := func(n Node) { queue = append(queue, entry{n, current.depth + 1}) }
		switch node := current.node.(type) {
		case *Literal:
		case *PathRef:
			name := node.Root
			if name == "vars" && len(node.Segments) > 0 {
				name = node.Segments[0].Name
			}
			refs = append(refs, name)
		case *Group:
			add(node.Expr)
		case *UnaryOp:
			add(node.Expr)
		case *BinaryOp:
			add(node.Left)
			add(node.Right)
		case *Call:
			if node.Name == "now" {
				return nil, fmt.Errorf("now() is forbidden in typed pure expressions")
			}
			if node.Namespace == "list" && node.Name == "order" {
				if len(node.Args) != 2 {
					return nil, fmt.Errorf("list.order requires two arguments")
				}
				literal, ok := node.Args[1].(*Literal)
				if !ok || literal.Kind != LiteralString {
					return nil, fmt.Errorf("typed list.order requires a literal comparator")
				}
				source, ok := literal.Value.(string)
				if !ok {
					return nil, fmt.Errorf("invalid comparator literal")
				}
				comparator, err := ParsePure(source)
				if err != nil {
					return nil, err
				}
				if err := validateOrdering(comparator.Node, true); err != nil {
					return nil, err
				}
			}
			for _, arg := range node.Args {
				add(arg)
			}
		default:
			return nil, fmt.Errorf("unsupported typed expression node %T", current.node)
		}
	}
	return refs, nil
}
