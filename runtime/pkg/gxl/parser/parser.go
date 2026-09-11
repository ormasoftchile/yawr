package parser

import "strconv"

const maxDepth = 256

// Parse parses src as a complete GXL expression.
func Parse(src string) (*Expr, error) {
	return parseExpression(src, false)
}

func parseExpression(src string, comparator bool) (*Expr, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	return parseTokens(toks, comparator, false)
}

func parseTokens(toks []token, comparator, pure bool) (*Expr, error) {
	p := &parser{tokens: toks, pure: pure}
	n, err := p.expression(0)
	if err != nil {
		return nil, err
	}
	if tok := p.peek(); tok.kind != tokEOF {
		if tok.kind == tokRParen {
			return nil, parseErr("GXL-PARSE-008", tok.start, "unmatched parenthesis")
		}
		if isForbiddenInfix(tok) {
			return nil, parseErr("GXL-PARSE-007", tok.start, "forbidden infix operator %q", tok.lit)
		}
		return nil, parseErr("GXL-PARSE-001", tok.start, "unexpected token %s", tok.display())
	}
	if pure {
		if _, err := validatePureTree(n); err != nil {
			return nil, err
		}
	}
	if err := validateOrdering(n, comparator); err != nil {
		return nil, err
	}
	return &Expr{Span: n.NodeSpan(), Node: n}, nil
}

// Validate parses src and discards the resulting AST.
func Validate(src string) error {
	_, err := Parse(src)
	return err
}

type parser struct {
	tokens []token
	pos    int
	pure   bool
}

func (p *parser) peek() token { return p.tokens[p.pos] }
func (p *parser) peekN(n int) token {
	idx := p.pos + n
	if idx >= len(p.tokens) {
		return p.tokens[len(p.tokens)-1]
	}
	return p.tokens[idx]
}
func (p *parser) advance() token {
	t := p.peek()
	if p.pos < len(p.tokens)-1 {
		p.pos++
	}
	return t
}
func (p *parser) match(k tokenKind) (token, bool) {
	if p.peek().kind == k {
		return p.advance(), true
	}
	return token{}, false
}

func (p *parser) expression(depth int) (Node, error) { return p.orExpr(depth + 1) }

func (p *parser) checkDepth(depth int) error {
	limit := maxDepth
	if p.pure {
		limit = 128
	}
	if depth > limit {
		return parseErr("GXL-PARSE-001", p.peek().start, "maximum expression nesting depth exceeded")
	}
	return nil
}

func (p *parser) orExpr(depth int) (Node, error) {
	if err := p.checkDepth(depth); err != nil {
		return nil, err
	}
	left, err := p.andExpr(depth + 1)
	if err != nil {
		return nil, err
	}
	for isKeywordToken(p.peek(), "or") {
		op := p.advance()
		right, err := p.andExpr(depth + 1)
		if err != nil {
			return nil, err
		}
		left = &BinaryOp{Span: Span{Start: left.NodeSpan().Start, End: right.NodeSpan().End}, Op: op.lit, Left: left, Right: right}
	}
	return left, nil
}

func (p *parser) andExpr(depth int) (Node, error) {
	if err := p.checkDepth(depth); err != nil {
		return nil, err
	}
	left, err := p.notExpr(depth + 1)
	if err != nil {
		return nil, err
	}
	for isKeywordToken(p.peek(), "and") {
		op := p.advance()
		right, err := p.notExpr(depth + 1)
		if err != nil {
			return nil, err
		}
		left = &BinaryOp{Span: Span{Start: left.NodeSpan().Start, End: right.NodeSpan().End}, Op: op.lit, Left: left, Right: right}
	}
	return left, nil
}

func (p *parser) notExpr(depth int) (Node, error) {
	if err := p.checkDepth(depth); err != nil {
		return nil, err
	}
	if isKeywordToken(p.peek(), "not") {
		op := p.advance()
		expr, err := p.notExpr(depth + 1)
		if err != nil {
			return nil, err
		}
		return &UnaryOp{Span: Span{Start: op.start, End: expr.NodeSpan().End}, Op: op.lit, Expr: expr}, nil
	}
	return p.comparison(depth + 1)
}

func (p *parser) comparison(depth int) (Node, error) {
	if err := p.checkDepth(depth); err != nil {
		return nil, err
	}
	left, err := p.addition(depth + 1)
	if err != nil {
		return nil, err
	}
	if isComp(p.peek().kind) {
		op := p.advance()
		right, err := p.addition(depth + 1)
		if err != nil {
			return nil, err
		}
		left = &BinaryOp{Span: Span{Start: left.NodeSpan().Start, End: right.NodeSpan().End}, Op: op.lit, Left: left, Right: right}
		if isComp(p.peek().kind) {
			return nil, parseErr("GXL-PARSE-009", p.peek().start, "chained comparison is not allowed")
		}
	}
	return left, nil
}

func (p *parser) addition(depth int) (Node, error) {
	if err := p.checkDepth(depth); err != nil {
		return nil, err
	}
	left, err := p.multiply(depth + 1)
	if err != nil {
		return nil, err
	}
	for p.peek().kind == tokPlus || p.peek().kind == tokMinus {
		op := p.advance()
		right, err := p.multiply(depth + 1)
		if err != nil {
			return nil, err
		}
		left = &BinaryOp{Span: Span{Start: left.NodeSpan().Start, End: right.NodeSpan().End}, Op: op.lit, Left: left, Right: right}
	}
	return left, nil
}

func (p *parser) multiply(depth int) (Node, error) {
	if err := p.checkDepth(depth); err != nil {
		return nil, err
	}
	left, err := p.unaryMinus(depth + 1)
	if err != nil {
		return nil, err
	}
	for p.peek().kind == tokStar || p.peek().kind == tokSlash || p.peek().kind == tokPercent {
		op := p.advance()
		right, err := p.unaryMinus(depth + 1)
		if err != nil {
			return nil, err
		}
		left = &BinaryOp{Span: Span{Start: left.NodeSpan().Start, End: right.NodeSpan().End}, Op: op.lit, Left: left, Right: right}
	}
	return left, nil
}

func (p *parser) unaryMinus(depth int) (Node, error) {
	if err := p.checkDepth(depth); err != nil {
		return nil, err
	}
	if p.peek().kind == tokMinus {
		op := p.advance()
		expr, err := p.unaryMinus(depth + 1)
		if err != nil {
			return nil, err
		}
		return &UnaryOp{Span: Span{Start: op.start, End: expr.NodeSpan().End}, Op: op.lit, Expr: expr}, nil
	}
	return p.primary(depth + 1)
}

func (p *parser) primary(depth int) (Node, error) {
	if err := p.checkDepth(depth); err != nil {
		return nil, err
	}
	tok := p.peek()
	if p.looksLikeUnknownNamespaceCall() {
		return nil, parseErr("GXL-PARSE-005", tok.start, "unknown namespace %q", tok.lit)
	}
	if tok.kind == tokIdent && isNamespaceRoot(tok.lit) && p.peekN(1).kind == tokDot && p.peekN(3).kind == tokLParen {
		return p.namespaceCall(depth + 1)
	}
	if tok.kind == tokIdent && (tok.lit == "NaN" || tok.lit == "Infinity") {
		return nil, parseErr("GXL-PARSE-003", tok.start, "invalid number literal %q", tok.lit)
	}
	if tok.kind == tokIdent && p.peekN(1).kind == tokLParen {
		return nil, parseErr("GXL-PARSE-007", tok.start, "bare function call %q is forbidden", tok.lit)
	}
	if tok.kind == tokPlus {
		return nil, parseErr("GXL-PARSE-007", tok.start, "unary plus is forbidden")
	}
	if tok.kind == tokLBracket {
		return nil, parseErr("GXL-PARSE-007", tok.start, "array construction is forbidden")
	}
	if tok.kind == tokKeyword {
		switch tok.lit {
		case "now":
			return p.nowCall(depth + 1)
		case "len":
			return p.lenCall(depth + 1)
		case "str", "list", "regex":
			if p.peekN(1).kind == tokDot {
				return p.namespaceCall(depth + 1)
			}
			return p.pathRef(depth + 1)
		case "true", "false", "null":
			return p.literal()
		case "and", "or":
			return nil, parseErr("GXL-PARSE-010", tok.start, "keyword %q cannot be used as an identifier", tok.lit)
		case "math":
			if p.peekN(1).kind == tokDot {
				return nil, parseErr("GXL-PARSE-005", tok.start, "unknown namespace %q", tok.lit)
			}
			return nil, parseErr("GXL-PARSE-010", tok.start, "keyword %q cannot be used as an identifier", tok.lit)
		default:
			return nil, parseErr("GXL-PARSE-001", tok.start, "unexpected token %s", tok.display())
		}
	}
	if tok.kind == tokIdent {
		return p.pathRef(depth + 1)
	}
	if tok.kind == tokString || tok.kind == tokNumber {
		return p.literal()
	}
	if tok.kind == tokLParen {
		open := p.advance()
		expr, err := p.expression(depth + 1)
		if err != nil {
			return nil, err
		}
		close, ok := p.match(tokRParen)
		if !ok {
			return nil, parseErr("GXL-PARSE-008", open.start, "unmatched parenthesis: expected `)`, got %s", p.peek().display())
		}
		return &Group{Span: Span{Start: open.start, End: close.end}, Expr: expr}, nil
	}
	if tok.kind == tokEOF {
		return nil, parseErr("GXL-PARSE-001", tok.start, "unexpected end of expression")
	}
	return nil, parseErr("GXL-PARSE-001", tok.start, "unexpected token %s", tok.display())
}

func (p *parser) nowCall(depth int) (Node, error) {
	name := p.advance()
	if _, ok := p.match(tokLParen); !ok {
		return nil, parseErr("GXL-PARSE-001", name.start, "expected `(` after now")
	}
	args, err := p.argList(depth+1, true)
	if err != nil {
		return nil, err
	}
	close, ok := p.match(tokRParen)
	if !ok {
		return nil, parseErr("GXL-PARSE-008", name.start, "unmatched parenthesis: expected `)`, got %s", p.peek().display())
	}
	if len(args) != 0 {
		return nil, parseErr("GXL-TYPE-004", name.start, "now() takes zero arguments")
	}
	return &Call{Span: Span{Start: name.start, End: close.end}, Name: "now", Args: args}, nil
}

func (p *parser) lenCall(depth int) (Node, error) {
	name := p.advance()
	if _, ok := p.match(tokLParen); !ok {
		return nil, parseErr("GXL-PARSE-001", name.start, "expected `(` after len")
	}
	if p.peek().kind == tokRParen {
		return nil, parseErr("GXL-TYPE-004", name.start, "len() takes exactly one argument")
	}
	arg, err := p.expression(depth + 1)
	if err != nil {
		return nil, err
	}
	close, ok := p.match(tokRParen)
	if !ok {
		return nil, parseErr("GXL-PARSE-008", name.start, "unmatched parenthesis: expected `)`, got %s", p.peek().display())
	}
	return &Call{Span: Span{Start: name.start, End: close.end}, Name: "len", Args: []Node{arg}}, nil
}

func (p *parser) namespaceCall(depth int) (Node, error) {
	ns := p.advance()
	p.advance() // dot
	method := p.peek()
	if method.kind != tokIdent && method.kind != tokKeyword {
		return nil, parseErr("GXL-PARSE-001", method.start, "expected method name after namespace dot")
	}
	p.advance()
	if _, ok := p.match(tokLParen); !ok {
		return nil, parseErr("GXL-PARSE-001", method.start, "expected `(` after namespace method")
	}
	if !knownMethod(ns.lit, method.lit) {
		return nil, parseErr("GXL-PARSE-006", method.start, "unknown method %s.%s", ns.lit, method.lit)
	}
	args, err := p.argList(depth+1, true)
	if err != nil {
		return nil, err
	}
	close, ok := p.match(tokRParen)
	if !ok {
		return nil, parseErr("GXL-PARSE-008", ns.start, "unmatched parenthesis: expected `)`, got %s", p.peek().display())
	}
	return &Call{Span: Span{Start: ns.start, End: close.end}, Namespace: ns.lit, Name: method.lit, Args: args}, nil
}

func (p *parser) argList(depth int, allowEmpty bool) ([]Node, error) {
	if p.peek().kind == tokRParen {
		if allowEmpty {
			return nil, nil
		}
		return nil, parseErr("GXL-TYPE-004", p.peek().start, "missing function argument")
	}
	var args []Node
	for {
		expr, err := p.expression(depth + 1)
		if err != nil {
			return nil, err
		}
		args = append(args, expr)
		if _, ok := p.match(tokComma); !ok {
			break
		}
		if p.peek().kind == tokRParen {
			return nil, parseErr("GXL-PARSE-001", p.peek().start, "expected expression after comma")
		}
	}
	return args, nil
}

func (p *parser) pathRef(depth int) (Node, error) {
	root := p.peek()
	if root.lit == "vars" {
		return nil, parseErr("GXL-PARSE-010", root.start, "obsolete vars namespace is not supported; use bare variables")
	}
	if root.kind == tokKeyword && !isNamespaceRoot(root.lit) {
		return nil, parseErr("GXL-PARSE-010", root.start, "keyword %q cannot be used as an identifier", root.lit)
	}
	p.advance()
	path := &PathRef{Span: root.span(), Root: root.lit}
	for {
		switch p.peek().kind {
		case tokDot, tokOptionalDot:
			dot := p.advance()
			seg := p.peek()
			if seg.kind == tokKeyword {
				return nil, parseErr("GXL-PARSE-010", seg.start, "keyword %q cannot be used as an identifier", seg.lit)
			}
			if seg.kind != tokIdent {
				return nil, parseErr("GXL-PARSE-001", seg.start, "expected identifier after `.`, got %s", seg.display())
			}
			p.advance()
			path.Segments = append(path.Segments, PathSegment{Span: Span{Start: dot.start, End: seg.end}, Name: seg.lit, Optional: dot.kind == tokOptionalDot})
			path.Span.End = seg.end
		case tokLBracket, tokOptionalBracket:
			open := p.advance()
			if p.peek().kind == tokMinus {
				return nil, parseErr("GXL-PARSE-007", p.peek().start, "negative array index is forbidden")
			}
			idxTok := p.peek()
			if idxTok.kind != tokNumber {
				return nil, parseErr("GXL-PARSE-001", idxTok.start, "expected array index, got %s", idxTok.display())
			}
			if hasNumberFraction(idxTok.lit) {
				return nil, parseErr("GXL-PARSE-003", idxTok.start, "array index must be an integer")
			}
			p.advance()
			close, ok := p.match(tokRBracket)
			if !ok {
				return nil, parseErr("GXL-PARSE-001", open.start, "expected `]`, got %s", p.peek().display())
			}
			idx, _ := strconv.Atoi(idxTok.lit)
			path.Segments = append(path.Segments, PathSegment{Span: Span{Start: open.start, End: close.end}, Index: &idx, Optional: open.kind == tokOptionalBracket})
			path.Span.End = close.end
		default:
			return path, nil
		}
	}
}

func (p *parser) literal() (Node, error) {
	t := p.advance()
	switch {
	case t.kind == tokString:
		return &Literal{Span: t.span(), Kind: LiteralString, Text: t.lit, Value: t.value}, nil
	case t.kind == tokNumber:
		return &Literal{Span: t.span(), Kind: LiteralNumber, Text: t.lit, Value: t.value}, nil
	case isKeywordToken(t, "true"):
		return &Literal{Span: t.span(), Kind: LiteralBool, Text: t.lit, Value: true}, nil
	case isKeywordToken(t, "false"):
		return &Literal{Span: t.span(), Kind: LiteralBool, Text: t.lit, Value: false}, nil
	case isKeywordToken(t, "null"):
		return &Literal{Span: t.span(), Kind: LiteralNull, Text: t.lit, Value: nil}, nil
	default:
		return nil, parseErr("GXL-PARSE-001", t.start, "unexpected literal token %s", t.display())
	}
}

func (p *parser) looksLikeUnknownNamespaceCall() bool {
	if p.peek().kind != tokIdent && p.peek().kind != tokKeyword {
		return false
	}
	if p.peekN(1).kind != tokDot {
		return false
	}
	if p.peekN(2).kind != tokIdent && p.peekN(2).kind != tokKeyword {
		return false
	}
	if p.peekN(3).kind != tokLParen {
		return false
	}
	root := p.peek().lit
	return !isNamespaceRoot(root)
}

func knownMethod(ns, method string) bool {
	return registeredMethod(ns, method)
}

func isKeywordToken(t token, lit string) bool { return t.kind == tokKeyword && t.lit == lit }
func isComp(k tokenKind) bool {
	return k == tokEq || k == tokNeq || k == tokLT || k == tokLTE || k == tokGT || k == tokGTE
}
func hasNumberFraction(s string) bool {
	for _, r := range s {
		if r == '.' || r == 'e' || r == 'E' {
			return true
		}
	}
	return false
}
func isForbiddenInfix(t token) bool {
	return (t.kind == tokIdent || t.kind == tokKeyword) && (t.lit == "contains" || t.lit == "startsWith" || t.lit == "matches" || t.lit == "in")
}
