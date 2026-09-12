package parser

import (
	"context"
	"sort"
)

// HighlightToken addresses UTF-8 byte boundaries in the scanner's input.
// It carries lexical presentation only, never an evaluated value.
type HighlightToken struct {
	Start int
	End   int
	Class string
}

// Highlight uses the strict parser's lexer, retaining its successfully scanned
// prefix on lexical errors. Literal arguments with a core-owned expression role
// are lexed recursively, with tokens mapped through the lexer's string escapes.
// It does not parse or evaluate an expression. Quotes and unscanned suffixes stay
// strings. Nested literal highlighting stops at 16 levels or 65536 scanned tokens.
func Highlight(ctx context.Context, src string) []HighlightToken {
	budget := 65536
	return highlight(ctx, src, 0, &budget)
}

func highlight(ctx context.Context, src string, depth int, budget *int) []HighlightToken {
	if ctx.Err() != nil || depth > 16 || *budget <= 0 {
		return nil
	}
	l := &lexer{src: src, line: 1, col: 1, ctx: ctx}
	_ = l.scan()
	if ctx.Err() != nil {
		return nil
	}
	scanned := len(l.tokens) + len(l.comments)
	if depth > 0 && scanned > *budget {
		return nil
	}
	*budget -= scanned
	out := append([]HighlightToken{}, l.comments...)
	ts := l.tokens
	expressions := highlightExpressionArguments(ts)
	at := func(i int) token {
		if i < 0 || i >= len(ts) {
			return token{kind: tokEOF}
		}
		return ts[i]
	}
	for i, t := range ts {
		if ctx.Err() != nil {
			return nil
		}
		if t.kind == tokEOF {
			continue
		}
		class := "delimiter"
		switch t.kind {
		case tokString:
			role := expressions[i]
			if role == argumentGXL || role == argumentRegex {
				var nested []HighlightToken
				if role == argumentRegex {
					nested = highlightRegex(ctx, t.value.(string), budget)
				} else {
					nested = highlight(ctx, t.value.(string), depth+1, budget)
				}
				if mapped := highlightLiteral(t, l.stringOffsets[t.start.Offset], nested); len(mapped) > 0 {
					out = append(out, mapped...)
					continue
				}
			}
			class = "string"
		case tokNumber:
			class = "number"
		case tokEq, tokNeq, tokLTE, tokGTE, tokLT, tokGT, tokPlus, tokMinus, tokStar, tokSlash, tokPercent:
			class = "operator"
		case tokIdent, tokKeyword:
			class = "variable"
			if t.kind == tokKeyword {
				class = "keyword"
			}
			switch t.lit {
			case "true", "false":
				class = "boolean"
			case "null":
				class = "null"
			case "and", "or", "not":
				class = "operator"
			}
			if at(i-1).kind == tokDot || at(i-1).kind == tokOptionalDot {
				class = "property"
				_, _, call := highlightNamespaceCall(ts, i+1)
				if call {
					class = "function"
				}
			} else if isNamespaceRoot(t.lit) {
				class = "variable"
				_, _, call := highlightNamespaceCall(ts, i+3)
				if call {
					class = "namespace"
				}
			} else if (t.lit == "len" || t.lit == "now") && at(i+1).kind == tokLParen {
				class = "function"
			}
		}
		out = append(out, HighlightToken{Start: t.start.Offset, End: t.end.Offset, Class: class})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out
}
