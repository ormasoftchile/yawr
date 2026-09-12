package gis

import "context"

// ExpressionBlock contains byte boundaries established by the GIS scanner.
// ExpressionEnd addresses the closing brace, or EOF when Closed is false.
type ExpressionBlock struct {
	Start           int
	ExpressionStart int
	ExpressionEnd   int
	Closed          bool
}

// LiteralBlock maps decoded GIS literal byte boundaries back to authored input.
type LiteralBlock struct {
	Text    string
	Offsets []int
}

// ScanPresentation shares the runtime scanner, including its forbidden-escape
// stopping point. Literal segments never span an unknown interpolation value.
func ScanPresentation(ctx context.Context, src string) ([]LiteralBlock, []ExpressionBlock) {
	p := &templateParser{src: src, line: 1, col: 1, ctx: ctx, highlight: true}
	_, _ = p.parse()
	p.flushLiteral(p.pos())
	if ctx.Err() != nil {
		return nil, nil
	}
	return p.literals, p.blocks
}

// ScanExpressions reuses GIS escape and quoted-brace processing without parsing
// or evaluating GXL. An unterminated block retains the bounded remaining input;
// forbidden GIS escapes stop the scanner just as they do in Parse.
func ScanExpressions(ctx context.Context, src string) []ExpressionBlock {
	p := &templateParser{src: src, line: 1, col: 1, ctx: ctx, highlight: true}
	_, _ = p.parse()
	if ctx.Err() != nil {
		return nil
	}
	return p.blocks
}
