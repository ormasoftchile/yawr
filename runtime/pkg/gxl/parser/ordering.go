package parser

import "github.com/ormasoftchile/yawr/runtime/pkg/errkit"

// expressionArgument identifies authored GXL, not a string inferred to be code.
// Keep presentation and strict comparator validation on the same argument role.
func expressionArgument(namespace, name string, argument int) bool {
	return builtinArgumentRole(namespace, name, argument) == argumentGXL
}

type argumentRole uint8

const (
	argumentPlain argumentRole = iota
	argumentGXL
	argumentRegex
)

func builtinArgumentRole(namespace, name string, argument int) argumentRole {
	if argument == 1 {
		if namespace == "list" && name == "order" {
			return argumentGXL
		}
		if namespace == "regex" && name == "match" {
			return argumentRegex
		}
	}
	return argumentPlain
}

// Comparator source is authored code, not data: validate it before dispatch,
// even when its input is empty or its containing expression is short-circuited.
func validateOrdering(node Node, comparator bool) error {
	switch n := node.(type) {
	case *PathRef:
		if comparator && n.Root != "left" && n.Root != "right" {
			return errkit.New("GXL-ORDER-001", "list.order comparators may reference only left and right")
		}
	case *Call:
		order := expressionArgument(n.Namespace, n.Name, 1)
		if comparator && (order || n.Namespace == "" && n.Name == "now") {
			return errkit.New("GXL-ORDER-001", "list.order comparators cannot use now() or nested list.order")
		}
		if order {
			if len(n.Args) != 2 {
				return errkit.New("GXL-TYPE-004", "list.order requires two arguments")
			}
			source, ok := n.Args[1].(*Literal)
			if !ok || source.Kind != LiteralString {
				return errkit.New("GXL-ORDER-001", "list.order requires a literal comparator expression string")
			}
			if _, err := parseExpression(source.Value.(string), true); err != nil {
				return err
			}
		}
		for _, arg := range n.Args {
			if err := validateOrdering(arg, comparator); err != nil {
				return err
			}
		}
	case *BinaryOp:
		if err := validateOrdering(n.Left, comparator); err != nil {
			return err
		}
		return validateOrdering(n.Right, comparator)
	case *UnaryOp:
		return validateOrdering(n.Expr, comparator)
	case *Group:
		return validateOrdering(n.Expr, comparator)
	}
	return nil
}
