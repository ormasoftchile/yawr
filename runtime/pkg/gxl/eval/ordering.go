package eval

import (
	"fmt"
	"regexp"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/gxl/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/pjvm"
)

const maxOrderItems = 256

var rfc3339Timestamp = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?(Z|[+-](0[0-9]|1[0-9]|2[0-3]):[0-5][0-9])$`)

func callDate(name string, args []pjvm.Value) (pjvm.Value, error) {
	if name != "compare" && name != "diffSeconds" {
		return pjvm.Null(), errkit.New("GXL-PARSE-006", fmt.Sprintf("unknown date method %q", name))
	}
	if len(args) != 2 {
		return pjvm.Null(), wrongArity("date."+name, 2, len(args))
	}
	var dates [2]time.Time
	for index, argument := range args {
		text, ok := argument.StringValue()
		if !ok {
			return pjvm.Null(), errkit.New("GXL-TYPE-003", "date."+name+" requires string arguments")
		}
		layout := time.RFC3339Nano
		if len(text) == 19 && text[10] == ' ' {
			layout = "2006-01-02 15:04:05"
		} else if !rfc3339Timestamp.MatchString(text) {
			return pjvm.Null(), invalidDate()
		}
		parsed, err := time.Parse(layout, text)
		if err != nil || parsed.Year() < 1 {
			return pjvm.Null(), invalidDate()
		}
		dates[index] = parsed
	}
	if name == "compare" {
		return pjvm.Number(float64(dates[0].Compare(dates[1])))
	}
	// time.Sub saturates outside approximately 290 years.
	seconds := float64(dates[0].Unix()-dates[1].Unix()) +
		float64(dates[0].Nanosecond()-dates[1].Nanosecond())/1e9
	return pjvm.Number(seconds)
}

func invalidDate() error {
	return errkit.New("GXL-DATE-001", "expected a valid RFC3339 timestamp or UTC YYYY-MM-DD HH:MM:SS, with year 0001 or later")
}

func callOrder(args []pjvm.Value, budgets ...*int) (pjvm.Value, error) {
	if len(args) != 2 {
		return pjvm.Null(), wrongArity("list.order", 2, len(args))
	}
	items, ok := args[0].ArrayValue()
	if !ok {
		return pjvm.Null(), errkit.New("GXL-TYPE-003", "list.order requires a list")
	}
	if len(items) > maxOrderItems {
		return pjvm.Null(), errkit.New("GXL-ORDER-002", fmt.Sprintf("list.order is limited to %d items", maxOrderItems))
	}
	source, ok := args[1].StringValue()
	if !ok {
		return pjvm.Null(), errkit.New("GXL-TYPE-003", "list.order requires a comparator expression string")
	}
	comparator, err := parser.Parse(source)
	if err != nil {
		return pjvm.Null(), err
	}
	if err := validateOrderComparator(comparator.Node); err != nil {
		return pjvm.Null(), err
	}
	less := func(left, right pjvm.Value) (bool, error) {
		scope := NewScope(map[string]pjvm.Value{"left": left, "right": right}, nil)
		if len(budgets) > 0 {
			scope.budget = budgets[0]
		}
		value, err := Eval(comparator, scope)
		if err != nil {
			return false, err
		}
		return strictBool(value)
	}
	ranks := make([]int, len(items))
	edges := make([][]int, len(items))
	ambiguous, inconsistent := false, false
	for left := range items {
		for right := left + 1; right < len(items); right++ {
			before, err := less(items[left], items[right])
			if err != nil {
				return pjvm.Null(), err
			}
			after, err := less(items[right], items[left])
			if err != nil {
				return pjvm.Null(), err
			}
			switch {
			case before && after:
				inconsistent = true
			case !before && !after:
				ambiguous = true
			case before:
				ranks[right]++
				edges[left] = append(edges[left], right)
			default:
				ranks[left]++
				edges[right] = append(edges[right], left)
			}
		}
	}
	if inconsistent {
		return orderResult("inconsistent", items), nil
	}
	queue := make([]int, 0, len(items))
	for index, rank := range ranks {
		if rank == 0 {
			queue = append(queue, index)
		}
	}
	ordered := make([]pjvm.Value, 0, len(items))
	for index := 0; index < len(queue); index++ {
		current := queue[index]
		ordered = append(ordered, items[current])
		for _, next := range edges[current] {
			ranks[next]--
			if ranks[next] == 0 {
				queue = append(queue, next)
			}
		}
	}
	if len(ordered) != len(items) {
		return orderResult("inconsistent", items), nil
	}
	if ambiguous {
		return orderResult("ambiguous", items), nil
	}
	return orderResult("ordered", ordered), nil
}

func orderResult(status string, items []pjvm.Value) pjvm.Value {
	value, _ := pjvm.NewString(status)
	return pjvm.Object(map[string]pjvm.Value{"status": value, "items": pjvm.Array(items)})
}

func validateOrderComparator(node parser.Node) error {
	switch typed := node.(type) {
	case *parser.PathRef:
		if typed.Root != "left" && typed.Root != "right" {
			return errkit.New("GXL-ORDER-001", "list.order comparators may reference only left and right")
		}
	case *parser.Call:
		if typed.Namespace == "" && typed.Name == "now" || typed.Namespace == "list" && typed.Name == "order" {
			return errkit.New("GXL-ORDER-001", "list.order comparators cannot use now() or nested list.order")
		}
		for _, argument := range typed.Args {
			if err := validateOrderComparator(argument); err != nil {
				return err
			}
		}
	case *parser.BinaryOp:
		if err := validateOrderComparator(typed.Left); err != nil {
			return err
		}
		return validateOrderComparator(typed.Right)
	case *parser.UnaryOp:
		return validateOrderComparator(typed.Expr)
	case *parser.Group:
		return validateOrderComparator(typed.Expr)
	}
	return nil
}
