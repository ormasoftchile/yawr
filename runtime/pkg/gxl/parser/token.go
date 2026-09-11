package parser

import "github.com/ormasoftchile/yawr/runtime/pkg/gxl/functions"

type tokenKind int

const (
	tokEOF tokenKind = iota
	tokIdent
	tokKeyword
	tokString
	tokNumber
	tokLParen
	tokRParen
	tokComma
	tokDot
	tokOptionalDot
	tokOptionalBracket
	tokLBracket
	tokRBracket
	tokEq
	tokNeq
	tokLTE
	tokGTE
	tokLT
	tokGT
	tokPlus
	tokMinus
	tokStar
	tokSlash
	tokPercent
)

type token struct {
	kind  tokenKind
	lit   string
	value any
	start Pos
	end   Pos
}

func (t token) span() Span { return Span{Start: t.start, End: t.end} }

func (t token) display() string {
	if t.kind == tokEOF {
		return "EOF"
	}
	if t.lit != "" {
		return "`" + t.lit + "`"
	}
	return t.kind.String()
}

func (k tokenKind) String() string {
	switch k {
	case tokEOF:
		return "EOF"
	case tokIdent:
		return "identifier"
	case tokKeyword:
		return "keyword"
	case tokString:
		return "string"
	case tokNumber:
		return "number"
	case tokLParen:
		return "`(`"
	case tokRParen:
		return "`)`"
	case tokComma:
		return "`,`"
	case tokDot:
		return "`.`"
	case tokOptionalDot:
		return "`?.`"
	case tokOptionalBracket:
		return "`?.[`"
	case tokLBracket:
		return "`[`"
	case tokRBracket:
		return "`]`"
	case tokEq:
		return "`==`"
	case tokNeq:
		return "`!=`"
	case tokLTE:
		return "`<=`"
	case tokGTE:
		return "`>=`"
	case tokLT:
		return "`<`"
	case tokGT:
		return "`>`"
	case tokPlus:
		return "`+`"
	case tokMinus:
		return "`-`"
	case tokStar:
		return "`*`"
	case tokSlash:
		return "`/`"
	case tokPercent:
		return "`%`"
	default:
		return "token"
	}
}

func isKeyword(s string) bool {
	switch s {
	case "true", "false", "null", "and", "or", "not", "len", "now", "str", "list", "regex", "math":
		return true
	default:
		return false
	}
}

func isNamespaceRoot(s string) bool {
	return functions.Namespace(s)
}
