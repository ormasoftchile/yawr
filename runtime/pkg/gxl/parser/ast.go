// Package parser implements the YAWR Expression Language (GXL) parser.
package parser

// Pos identifies a source location in a GXL expression.
type Pos struct {
	Line   int
	Column int
	Offset int
}

// Span identifies the source range occupied by a node.
type Span struct {
	Start Pos
	End   Pos
}

// Expr is the root GXL expression AST node.
type Expr struct {
	Span Span
	Node Node
}

// Node is implemented by every concrete GXL AST variant.
type Node interface {
	NodeSpan() Span
	nodeKind() string
}

// BinaryOp is a left/right binary operator expression.
type BinaryOp struct {
	Span        Span
	Op          string
	Left, Right Node
}

// UnaryOp is a unary prefix operator expression.
type UnaryOp struct {
	Span Span
	Op   string
	Expr Node
}

// Call is a GXL built-in or namespace function call.
type Call struct {
	Span      Span
	Namespace string
	Name      string
	Args      []Node
}

// PathRef is a YAWR dotted path variable reference.
type PathRef struct {
	Span     Span
	Root     string
	Segments []PathSegment
}

// PathSegment is one dot or bracket segment in a PathRef.
type PathSegment struct {
	Span     Span
	Name     string
	Index    *int
	Optional bool
}

// LiteralKind names the type of a literal node.
type LiteralKind string

const (
	// LiteralString is a string literal.
	LiteralString LiteralKind = "string"
	// LiteralNumber is a numeric literal.
	LiteralNumber LiteralKind = "number"
	// LiteralBool is a boolean literal.
	LiteralBool LiteralKind = "bool"
	// LiteralNull is the null literal.
	LiteralNull LiteralKind = "null"
)

// Literal is a scalar GXL literal.
type Literal struct {
	Span  Span
	Kind  LiteralKind
	Text  string
	Value any
}

// Group is a parenthesized expression.
type Group struct {
	Span Span
	Expr Node
}

// NodeSpan returns the node source span.
func (n *BinaryOp) NodeSpan() Span   { return n.Span }
func (n *BinaryOp) nodeKind() string { return "binary" }

// NodeSpan returns the node source span.
func (n *UnaryOp) NodeSpan() Span   { return n.Span }
func (n *UnaryOp) nodeKind() string { return "unary" }

// NodeSpan returns the node source span.
func (n *Call) NodeSpan() Span   { return n.Span }
func (n *Call) nodeKind() string { return "call" }

// NodeSpan returns the node source span.
func (n *PathRef) NodeSpan() Span   { return n.Span }
func (n *PathRef) nodeKind() string { return "path" }

// NodeSpan returns the node source span.
func (n *Literal) NodeSpan() Span   { return n.Span }
func (n *Literal) nodeKind() string { return "literal" }

// NodeSpan returns the node source span.
func (n *Group) NodeSpan() Span   { return n.Span }
func (n *Group) nodeKind() string { return "group" }
