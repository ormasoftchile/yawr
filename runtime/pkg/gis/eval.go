package gis

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/gdp"
	gxleval "github.com/ormasoftchile/yawr/runtime/pkg/gxl/eval"
	"github.com/ormasoftchile/yawr/runtime/pkg/gxl/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/pjvm"
)

// Interpolate evaluates every embedded GXL expression and returns the rendered string.
func Interpolate(template string, scope *gxleval.Scope) (string, error) {
	tmpl, err := Parse(template)
	if err != nil {
		return "", err
	}
	return tmpl.Interpolate(scope)
}

// Interpolate evaluates an already parsed template against scope.
func (t *Template) Interpolate(scope *gxleval.Scope) (string, error) {
	return t.interpolate(scope, nil)
}

func (t *Template) InterpolateBounded(scope *gxleval.Scope, budget *int) (string, error) {
	if budget == nil {
		return "", errkit.New("GIS-LIMIT-001", "missing evaluation budget")
	}
	return t.interpolate(scope.WithBudget(budget), budget)
}

func (t *Template) interpolate(scope *gxleval.Scope, budget *int) (string, error) {
	if t == nil {
		return "", nil
	}
	if scope == nil {
		scope = gxleval.NewScope(nil, nil)
	}
	e := evaluator{scope: scope, budget: budget}
	var out strings.Builder
	for _, segment := range t.Segments {
		switch s := segment.(type) {
		case *Literal:
			out.WriteString(s.Value)
		case *Expr:
			v, err := e.evalExpr(s.Parsed)
			if err != nil {
				return "", mapEvalError(err)
			}
			out.WriteString(v.String())
		default:
			return "", errkit.New("GIS-EVAL-001", fmt.Sprintf("unsupported segment %T", segment))
		}
	}
	return out.String(), nil
}

type evaluator struct {
	scope  *gxleval.Scope
	budget *int
}

func (e evaluator) evalExpr(expr *parser.Expr) (pjvm.Value, error) {
	if expr == nil || expr.Node == nil {
		return pjvm.Null(), errkit.New("GXL-EVAL-001", "nil expression")
	}
	return e.evalNode(expr.Node)
}

func (e evaluator) evalNode(n parser.Node) (pjvm.Value, error) {
	if e.budget != nil {
		*e.budget--
		if *e.budget < 0 {
			return pjvm.Null(), errkit.New("GIS-LIMIT-001", "typed expression evaluation budget exceeded")
		}
	}
	switch x := n.(type) {
	case *parser.Literal:
		return evalLiteral(x)
	case *parser.Group:
		return e.evalNode(x.Expr)
	case *parser.PathRef:
		return e.evalPath(x)
	case *parser.UnaryOp:
		return e.evalUnary(x)
	case *parser.BinaryOp:
		return e.evalBinary(x)
	case *parser.Call:
		return e.evalCall(x)
	default:
		return pjvm.Null(), errkit.New("GXL-EVAL-001", fmt.Sprintf("unsupported AST node %T", n))
	}
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

func (e evaluator) evalPath(ref *parser.PathRef) (pjvm.Value, error) {
	path := toGDPPath(ref)
	if pathHasOptional(path) {
		v, found, err := gdp.ResolveOptional(e.scope.Tree(), path)
		if err != nil {
			return pjvm.Null(), err
		}
		if !found || v.Kind() == pjvm.KindNull {
			return pjvm.NewString("")
		}
		return v, nil
	}
	return gdp.Resolve(e.scope.Tree(), path)
}

func (e evaluator) evalUnary(op *parser.UnaryOp) (pjvm.Value, error) {
	v, err := e.evalNode(op.Expr)
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

func (e evaluator) evalBinary(op *parser.BinaryOp) (pjvm.Value, error) {
	switch op.Op {
	case "and":
		left, err := e.evalNode(op.Left)
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
		right, err := e.evalNode(op.Right)
		if err != nil {
			return pjvm.Null(), err
		}
		rb, err := strictBool(right)
		if err != nil {
			return pjvm.Null(), err
		}
		return pjvm.Bool(rb), nil
	case "or":
		left, err := e.evalNode(op.Left)
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
		right, err := e.evalNode(op.Right)
		if err != nil {
			return pjvm.Null(), err
		}
		rb, err := strictBool(right)
		if err != nil {
			return pjvm.Null(), err
		}
		return pjvm.Bool(rb), nil
	}
	left, err := e.evalNode(op.Left)
	if err != nil {
		return pjvm.Null(), err
	}
	right, err := e.evalNode(op.Right)
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

func (e evaluator) evalCall(call *parser.Call) (pjvm.Value, error) {
	args := make([]pjvm.Value, len(call.Args))
	for i, node := range call.Args {
		v, err := e.evalNode(node)
		if err != nil {
			return pjvm.Null(), err
		}
		args[i] = v
	}
	if call.Namespace == "" {
		switch call.Name {
		case "len":
			return lengthValue(args[0])
		case "now":
			return gxleval.Eval(&parser.Expr{Node: call, Span: call.Span}, e.scope)
		}
	}
	switch call.Namespace {
	case "str":
		return callString(call.Name, args)
	case "list":
		if call.Name == "order" {
			return gxleval.CallBuiltin(call.Namespace, call.Name, args, e.scope)
		}
		return callList(call.Name, args)
	case "regex":
		return callRegex(call.Name, args)
	case "date":
		return gxleval.CallBuiltin(call.Namespace, call.Name, args, e.scope)
	default:
		return pjvm.Null(), errkit.New("GXL-PARSE-005", fmt.Sprintf("unknown namespace %q", call.Namespace))
	}
}

func callString(name string, args []pjvm.Value) (pjvm.Value, error) {
	switch name {
	case "startsWith":
		s, p, err := twoStrings(name, args)
		if err != nil {
			return pjvm.Null(), err
		}
		return pjvm.Bool(strings.HasPrefix(s, p)), nil
	case "endsWith":
		s, p, err := twoStrings(name, args)
		if err != nil {
			return pjvm.Null(), err
		}
		return pjvm.Bool(strings.HasSuffix(s, p)), nil
	case "contains":
		s, p, err := twoStrings(name, args)
		if err != nil {
			return pjvm.Null(), err
		}
		return pjvm.Bool(strings.Contains(s, p)), nil
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
		s, p, err := twoStrings(name, args)
		if err != nil {
			return pjvm.Null(), err
		}
		return pjvm.NewString(strings.TrimPrefix(s, p))
	case "trimSuffix":
		s, p, err := twoStrings(name, args)
		if err != nil {
			return pjvm.Null(), err
		}
		return pjvm.NewString(strings.TrimSuffix(s, p))
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
func pathHasOptional(path *gdp.Path) bool {
	for _, seg := range path.Segments {
		if seg.Optional {
			return true
		}
	}
	return false
}

func strictBool(v pjvm.Value) (bool, error) {
	b, ok := v.BoolValue()
	if !ok {
		return false, errkit.New("GXL-TYPE-002", fmt.Sprintf("non-bool value %s in boolean position", v.Kind()))
	}
	return b, nil
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
func lengthValue(v pjvm.Value) (pjvm.Value, error) {
	if s, ok := v.StringValue(); ok {
		return pjvm.Number(float64(utf8.RuneCountInString(s)))
	}
	if arr, ok := v.ArrayValue(); ok {
		return pjvm.Number(float64(len(arr)))
	}
	return pjvm.Null(), errkit.New("GXL-TYPE-003", fmt.Sprintf("len requires string or list, got %s", v.Kind()))
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
	if item.Kind() == pjvm.KindArray || item.Kind() == pjvm.KindObject {
		return 0, errkit.New("GXL-TYPE-003", fmt.Sprintf("%s requires scalar item, got %s", fn, item.Kind()))
	}
	for i, elem := range arr {
		if elem.Kind() == pjvm.KindNull || elem.Kind() == pjvm.KindArray || elem.Kind() == pjvm.KindObject || elem.Kind() != item.Kind() {
			continue
		}
		if elem.Equal(item) {
			return i, nil
		}
	}
	return -1, nil
}

func mapEvalError(err error) error {
	code := errorCode(err)
	switch {
	case strings.HasPrefix(code, "GXL-PATH-"):
		return errkit.Wrap("GIS-PATH-MISSING", "interpolation path did not resolve", err)
	case strings.HasPrefix(code, "GXL-TYPE-"):
		return errkit.Wrap("GIS-TYPE-002", "GXL type error in interpolation", err)
	case strings.HasPrefix(code, "GXL-PARSE-"):
		return errkit.Wrap("GIS-PARSE-003", "invalid GXL expression inside interpolation", err)
	case code == "":
		return err
	default:
		return err
	}
}

func errorCode(err error) string {
	if c, ok := err.(interface{ Code() string }); ok {
		return c.Code()
	}
	return ""
}
