// Package eval evaluates parsed YAWR Expression Language ASTs.
package eval

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/gdp"
	"github.com/ormasoftchile/yawr/runtime/pkg/gxl/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/pjvm"
)

// Clock returns the current time for now().
type Clock func() time.Time

// Scope contains variable bindings visible to an evaluation.
type Scope struct {
	vars   map[string]pjvm.Value
	parent *Scope
	clock  Clock
	budget *int
}

// NewScope creates a Scope from PJVM variable bindings.
func NewScope(vars map[string]pjvm.Value, parent *Scope) *Scope {
	out := make(map[string]pjvm.Value, len(vars))
	for k, v := range vars {
		out[k] = v
	}
	return &Scope{vars: out, parent: parent}
}

// FromAny creates a Scope by converting vars into PJVM values.
func FromAny(vars map[string]any) (*Scope, error) {
	converted := make(map[string]pjvm.Value, len(vars))
	for k, raw := range vars {
		v, err := pjvm.FromAny(raw)
		if err != nil {
			return nil, fmt.Errorf("scope variable %q: %w", k, err)
		}
		converted[k] = v
	}
	return NewScope(converted, nil), nil
}

// WithClock returns a child scope that uses clock for now().
func (s *Scope) WithClock(clock Clock) *Scope {
	if s == nil {
		return &Scope{clock: clock}
	}

	return &Scope{vars: s.vars, parent: s.parent, clock: clock}
}

func (s *Scope) WithBudget(budget *int) *Scope {
	copy := *normalizeScope(s)
	copy.budget = budget
	return &copy
}

// Eval evaluates expr against scope using strict GXL semantics.
func Eval(expr *parser.Expr, scope *Scope) (pjvm.Value, error) {
	if expr == nil || expr.Node == nil {
		return pjvm.Null(), errkit.New("GXL-EVAL-001", "nil expression")
	}
	return evalNode(expr.Node, normalizeScope(scope))
}

func normalizeScope(scope *Scope) *Scope {
	if scope == nil {
		return NewScope(nil, nil)
	}
	return scope
}

func evalNode(n parser.Node, scope *Scope) (pjvm.Value, error) {
	if scope.budget != nil {
		*scope.budget--
		if *scope.budget < 0 {
			return pjvm.Null(), errkit.New("GXL-LIMIT-001", "typed expression evaluation budget exceeded")
		}
	}
	switch x := n.(type) {
	case *parser.Literal:
		return evalLiteral(x)
	case *parser.Group:
		return evalNode(x.Expr, scope)
	case *parser.PathRef:
		return evalPath(x, scope)
	case *parser.UnaryOp:
		return evalUnary(x, scope)
	case *parser.BinaryOp:
		return evalBinary(x, scope)
	case *parser.Call:
		return evalCall(x, scope)
	default:
		return pjvm.Null(), errkit.New("GXL-EVAL-001", fmt.Sprintf("unsupported AST node %T", n))
	}

}

// EvalBounded shares one construction budget across all leaves and comparator
// evaluations without changing legacy evaluator limits.
func EvalBounded(expression *parser.Expr, scope *Scope, budget *int) (pjvm.Value, error) {
	if budget == nil {
		return pjvm.Null(), errkit.New("GXL-LIMIT-001", "missing evaluation budget")
	}
	owned := *normalizeScope(scope)
	owned.budget = budget
	return Eval(expression, &owned)
}

func evalLiteral(lit *parser.Literal) (pjvm.Value, error) {
	switch lit.Kind {
	case parser.LiteralNull:
		return pjvm.Null(), nil
	case parser.LiteralBool:
		b, _ := lit.Value.(bool)
		return pjvm.Bool(b), nil
	case parser.LiteralString:
		s, _ := lit.Value.(string)
		return pjvm.NewString(s)
	case parser.LiteralNumber:
		n, _ := lit.Value.(float64)
		return pjvm.Number(n)
	default:
		return pjvm.Null(), errkit.New("GXL-EVAL-001", "unknown literal kind")
	}
}

func evalPath(ref *parser.PathRef, scope *Scope) (pjvm.Value, error) {
	return gdp.Resolve(scope.Tree(), toGDPPath(ref))
}

func evalUnary(op *parser.UnaryOp, scope *Scope) (pjvm.Value, error) {
	v, err := evalNode(op.Expr, scope)
	if err != nil {
		return pjvm.Null(), err
	}
	switch op.Op {
	case "not":
		b, err := strictBool(v)
		if err != nil {
			return pjvm.Null(), err
		}
		return pjvm.Bool(!b), nil
	case "-":
		n, ok := v.NumberValue()
		if !ok {
			return pjvm.Null(), errkit.New("GXL-TYPE-003", fmt.Sprintf("unary - requires number, got %s", v.Kind()))
		}
		return pjvm.Number(-n)
	default:
		return pjvm.Null(), errkit.New("GXL-EVAL-001", fmt.Sprintf("unsupported unary operator %q", op.Op))
	}
}

func evalBinary(op *parser.BinaryOp, scope *Scope) (pjvm.Value, error) {
	switch op.Op {
	case "and":
		left, err := evalNode(op.Left, scope)
		if err != nil {
			return pjvm.Null(), err
		}
		b, err := strictBool(left)
		if err != nil {
			return pjvm.Null(), err
		}
		if !b {
			return pjvm.Bool(false), nil
		}
		right, err := evalNode(op.Right, scope)
		if err != nil {
			return pjvm.Null(), err
		}
		rb, err := strictBool(right)
		if err != nil {
			return pjvm.Null(), err
		}
		return pjvm.Bool(rb), nil
	case "or":
		left, err := evalNode(op.Left, scope)
		if err != nil {
			return pjvm.Null(), err
		}
		b, err := strictBool(left)
		if err != nil {
			return pjvm.Null(), err
		}
		if b {
			return pjvm.Bool(true), nil
		}
		right, err := evalNode(op.Right, scope)
		if err != nil {
			return pjvm.Null(), err
		}
		rb, err := strictBool(right)
		if err != nil {
			return pjvm.Null(), err
		}
		return pjvm.Bool(rb), nil
	}

	left, err := evalNode(op.Left, scope)
	if err != nil {
		return pjvm.Null(), err
	}
	right, err := evalNode(op.Right, scope)
	if err != nil {
		return pjvm.Null(), err
	}
	switch op.Op {
	case "+", "-", "*", "/", "%":
		return evalArithmetic(op.Op, left, right)
	case "==", "!=", "<", "<=", ">", ">=":
		return evalComparison(op.Op, left, right)
	default:
		return pjvm.Null(), errkit.New("GXL-EVAL-001", fmt.Sprintf("unsupported binary operator %q", op.Op))
	}
}

func evalArithmetic(op string, left, right pjvm.Value) (pjvm.Value, error) {
	l, ok := left.NumberValue()
	if !ok {
		return pjvm.Null(), errkit.New("GXL-TYPE-003", fmt.Sprintf("arithmetic requires number, got %s", left.Kind()))
	}
	r, ok := right.NumberValue()
	if !ok {
		return pjvm.Null(), errkit.New("GXL-TYPE-003", fmt.Sprintf("arithmetic requires number, got %s", right.Kind()))
	}
	var out float64
	switch op {
	case "+":
		out = l + r
	case "-":
		out = l - r
	case "*":
		out = l * r
	case "/":
		if r == 0 {
			return pjvm.Null(), errkit.New("GXL-EVAL-002", "division by zero")
		}
		out = l / r
	case "%":
		if r == 0 {
			return pjvm.Null(), errkit.New("GXL-EVAL-002", "modulo by zero")
		}
		out = math.Mod(l, r)
	}
	v, err := pjvm.Number(out)
	if err != nil {
		return pjvm.Null(), errkit.Wrap("GXL-EVAL-001", "arithmetic produced non-finite number", err)
	}
	return v, nil
}

func evalComparison(op string, left, right pjvm.Value) (pjvm.Value, error) {
	if op == "==" || op == "!=" {
		eq, err := equalScalars(left, right)
		if err != nil {
			return pjvm.Null(), err
		}
		if op == "!=" {
			eq = !eq
		}
		return pjvm.Bool(eq), nil
	}
	if left.Kind() == pjvm.KindNull || right.Kind() == pjvm.KindNull {
		return pjvm.Null(), errkit.New("GXL-EVAL-004", "null in ordered comparison")
	}
	if left.Kind() != right.Kind() {
		return pjvm.Null(), errkit.New("GXL-TYPE-001", fmt.Sprintf("cannot compare %s to %s", left.Kind(), right.Kind()))
	}
	switch left.Kind() {
	case pjvm.KindNumber:
		l, _ := left.NumberValue()
		r, _ := right.NumberValue()
		return pjvm.Bool(compareFloat(op, l, r)), nil
	case pjvm.KindString:
		l, _ := left.StringValue()
		r, _ := right.StringValue()
		return pjvm.Bool(compareString(op, l, r)), nil
	case pjvm.KindBool:
		return pjvm.Null(), errkit.New("GXL-TYPE-005", "ordered comparison on boolean operands")
	default:
		return pjvm.Null(), errkit.New("GXL-TYPE-001", fmt.Sprintf("ordered comparison unsupported for %s", left.Kind()))
	}
}

func equalScalars(left, right pjvm.Value) (bool, error) {
	if left.Kind() == pjvm.KindArray || left.Kind() == pjvm.KindObject || right.Kind() == pjvm.KindArray || right.Kind() == pjvm.KindObject {
		return false, errkit.New("GXL-TYPE-001", "equality is restricted to scalars and null")
	}
	if left.Kind() == pjvm.KindNull || right.Kind() == pjvm.KindNull {
		return left.Kind() == pjvm.KindNull && right.Kind() == pjvm.KindNull, nil
	}
	if left.Kind() != right.Kind() {
		return false, errkit.New("GXL-TYPE-001", fmt.Sprintf("cannot compare %s to %s", left.Kind(), right.Kind()))
	}
	return left.Equal(right), nil
}

func compareFloat(op string, l, r float64) bool {
	switch op {
	case "<":
		return l < r
	case "<=":
		return l <= r
	case ">":
		return l > r
	case ">=":
		return l >= r
	default:
		return false
	}
}

func compareString(op, l, r string) bool {
	switch op {
	case "<":
		return l < r
	case "<=":
		return l <= r
	case ">":
		return l > r
	case ">=":
		return l >= r
	default:
		return false
	}
}

func evalCall(call *parser.Call, scope *Scope) (pjvm.Value, error) {
	args, err := evalArgs(call.Args, scope)
	if err != nil {
		return pjvm.Null(), err
	}
	return CallBuiltin(call.Namespace, call.Name, args, scope)
}

// CallBuiltin evaluates a built-in using already-evaluated arguments.
// GIS uses this boundary to share typed primitives without re-evaluating inputs.
func CallBuiltin(namespace, name string, args []pjvm.Value, scope *Scope) (pjvm.Value, error) {
	if namespace == "" {
		switch name {
		case "len":
			return callLen(args)
		case "now":
			return callNow(args, scope)
		}
	}
	switch namespace {
	case "str":
		return callString(name, args)
	case "list":
		if name == "order" {
			return callOrder(args, scope.budget)
		}
		return callList(name, args)
	case "regex":
		return callRegex(name, args)
	case "date":
		return callDate(name, args)
	default:
		return pjvm.Null(), errkit.New("GXL-PARSE-005", fmt.Sprintf("unknown namespace %q", namespace))
	}
}

func evalArgs(nodes []parser.Node, scope *Scope) ([]pjvm.Value, error) {
	args := make([]pjvm.Value, len(nodes))
	for i, node := range nodes {
		v, err := evalNode(node, scope)
		if err != nil {
			return nil, err
		}
		args[i] = v
	}
	return args, nil
}

func callLen(args []pjvm.Value) (pjvm.Value, error) {
	if len(args) != 1 {
		return pjvm.Null(), errkit.New("GXL-TYPE-004", fmt.Sprintf("len takes 1 argument, got %d", len(args)))
	}
	return lengthValue(args[0])
}

func callNow(args []pjvm.Value, scope *Scope) (pjvm.Value, error) {
	if len(args) != 0 {
		return pjvm.Null(), errkit.New("GXL-TYPE-004", fmt.Sprintf("now takes 0 arguments, got %d", len(args)))
	}
	clock := scope.resolveClock()
	return pjvm.NewString(clock().UTC().Format("2006-01-02T15:04:05Z"))
}

func callString(name string, args []pjvm.Value) (pjvm.Value, error) {
	switch name {
	case "startsWith":
		s, prefix, err := twoStrings(name, args)
		if err != nil {
			return pjvm.Null(), err
		}
		return pjvm.Bool(strings.HasPrefix(s, prefix)), nil
	case "endsWith":
		s, suffix, err := twoStrings(name, args)
		if err != nil {
			return pjvm.Null(), err
		}
		return pjvm.Bool(strings.HasSuffix(s, suffix)), nil
	case "contains":
		s, substr, err := twoStrings(name, args)
		if err != nil {
			return pjvm.Null(), err
		}
		return pjvm.Bool(strings.Contains(s, substr)), nil
	case "toLower":
		s, err := oneString(name, args)
		if err != nil {
			return pjvm.Null(), err
		}
		return pjvm.NewString(strings.ToLower(s))
	case "toUpper":
		s, err := oneString(name, args)
		if err != nil {
			return pjvm.Null(), err
		}
		return pjvm.NewString(strings.ToUpper(s))
	case "trim":
		s, err := oneString(name, args)
		if err != nil {
			return pjvm.Null(), err
		}
		return pjvm.NewString(strings.TrimSpace(s))
	case "length":
		if len(args) != 1 {
			return pjvm.Null(), wrongArity("str.length", 1, len(args))
		}
		return lengthValue(args[0])
	case "trimPrefix":
		s, prefix, err := twoStrings(name, args)
		if err != nil {
			return pjvm.Null(), err
		}
		return pjvm.NewString(strings.TrimPrefix(s, prefix))
	case "trimSuffix":
		s, suffix, err := twoStrings(name, args)
		if err != nil {
			return pjvm.Null(), err
		}
		return pjvm.NewString(strings.TrimSuffix(s, suffix))
	default:
		return pjvm.Null(), errkit.New("GXL-PARSE-006", fmt.Sprintf("unknown str method %q", name))
	}
}

func callList(name string, args []pjvm.Value) (pjvm.Value, error) {
	switch name {
	case "contains":
		idx, err := listIndex("list.contains", args)
		if err != nil {
			return pjvm.Null(), err
		}
		return pjvm.Bool(idx >= 0), nil
	case "indexOf":
		idx, err := listIndex("list.indexOf", args)
		if err != nil {
			return pjvm.Null(), err
		}
		return pjvm.Number(float64(idx))
	case "length":
		if len(args) != 1 {
			return pjvm.Null(), wrongArity("list.length", 1, len(args))
		}
		arr, ok := args[0].ArrayValue()
		if !ok {
			return pjvm.Null(), errkit.New("GXL-TYPE-003", fmt.Sprintf("list.length requires list, got %s", args[0].Kind()))
		}
		return pjvm.Number(float64(len(arr)))
	default:
		return pjvm.Null(), errkit.New("GXL-PARSE-006", fmt.Sprintf("unknown list method %q", name))
	}
}

func callRegex(name string, args []pjvm.Value) (pjvm.Value, error) {
	if name != "match" {
		return pjvm.Null(), errkit.New("GXL-PARSE-006", fmt.Sprintf("unknown regex method %q", name))
	}
	s, pattern, err := twoStrings("regex.match", args)
	if err != nil {
		return pjvm.Null(), err
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return pjvm.Null(), errkit.Wrap("GXL-EVAL-003", "invalid regex pattern", err)
	}
	return pjvm.Bool(re.MatchString(s)), nil
}

func lengthValue(v pjvm.Value) (pjvm.Value, error) {
	if s, ok := v.StringValue(); ok {
		return pjvm.Number(float64(utf8.RuneCountInString(s)))
	}
	if arr, ok := v.ArrayValue(); ok {
		return pjvm.Number(float64(len(arr)))
	}
	return pjvm.Null(), errkit.New("GXL-TYPE-003", fmt.Sprintf("len requires string or list, got %s", v.Kind()))
}

func listIndex(fn string, args []pjvm.Value) (int, error) {
	if len(args) != 2 {
		return 0, wrongArity(fn, 2, len(args))
	}
	arr, ok := args[0].ArrayValue()
	if !ok {
		return 0, errkit.New("GXL-TYPE-003", fmt.Sprintf("%s requires list first argument, got %s", fn, args[0].Kind()))
	}
	item := args[1]
	if item.Kind() == pjvm.KindNull {
		return -1, nil
	}
	if !isScalar(item) {
		return 0, errkit.New("GXL-TYPE-003", fmt.Sprintf("%s requires scalar item, got %s", fn, item.Kind()))
	}
	for i, elem := range arr {
		if elem.Kind() == pjvm.KindNull || elem.Kind() == pjvm.KindArray || elem.Kind() == pjvm.KindObject {
			continue
		}
		if elem.Kind() != item.Kind() {
			continue
		}
		if elem.Equal(item) {
			return i, nil
		}
	}
	return -1, nil
}

func oneString(fn string, args []pjvm.Value) (string, error) {
	if len(args) != 1 {
		return "", wrongArity("str."+fn, 1, len(args))
	}
	s, ok := args[0].StringValue()
	if !ok {
		return "", errkit.New("GXL-TYPE-003", fmt.Sprintf("str.%s requires string, got %s", fn, args[0].Kind()))
	}
	return s, nil
}

func twoStrings(fn string, args []pjvm.Value) (string, string, error) {
	if len(args) != 2 {
		return "", "", wrongArity("str."+fn, 2, len(args))
	}
	first, ok := args[0].StringValue()
	if !ok {
		return "", "", errkit.New("GXL-TYPE-003", fmt.Sprintf("str.%s requires string first argument, got %s", fn, args[0].Kind()))
	}
	second, ok := args[1].StringValue()
	if !ok {
		return "", "", errkit.New("GXL-TYPE-003", fmt.Sprintf("str.%s requires string second argument, got %s", fn, args[1].Kind()))
	}
	return first, second, nil
}

func wrongArity(fn string, want, got int) error {
	return errkit.New("GXL-TYPE-004", fmt.Sprintf("%s takes %d arguments, got %d", fn, want, got))
}

func strictBool(v pjvm.Value) (bool, error) {
	b, ok := v.BoolValue()
	if !ok {
		return false, errkit.New("GXL-TYPE-002", fmt.Sprintf("non-bool value %s in boolean position", v.Kind()))
	}
	return b, nil
}

func isScalar(v pjvm.Value) bool {
	switch v.Kind() {
	case pjvm.KindBool, pjvm.KindNumber, pjvm.KindString:
		return true
	default:
		return false
	}
}

// Tree returns the merged PJVM object visible to this scope.
func (s *Scope) Tree() pjvm.Value {
	bindings := map[string]pjvm.Value{}
	var fill func(*Scope)
	fill = func(cur *Scope) {
		if cur == nil {
			return
		}
		fill(cur.parent)
		for k, v := range cur.vars {
			bindings[k] = v
		}
	}
	fill(s)
	return pjvm.Object(bindings)
}

func (s *Scope) resolveClock() Clock {
	for cur := s; cur != nil; cur = cur.parent {
		if cur.clock != nil {
			return cur.clock
		}
	}
	return time.Now
}

func toGDPPath(ref *parser.PathRef) *gdp.Path {
	path := &gdp.Path{Dialect: gdp.DialectGXL, Root: ref.Root, Span: toGDPSpan(ref.Span)}
	for _, seg := range ref.Segments {
		path.Segments = append(path.Segments, gdp.Segment{Span: toGDPSpan(seg.Span), Name: seg.Name, Index: seg.Index, Optional: seg.Optional})
	}
	return path
}

func toGDPSpan(s parser.Span) gdp.Span {
	return gdp.Span{Start: gdp.Pos{Line: s.Start.Line, Column: s.Start.Column, Offset: s.Start.Offset}, End: gdp.Pos{Line: s.End.Line, Column: s.End.Column, Offset: s.End.Offset}}
}
