package gis

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/gxl/parser"
)

// Parse parses a GIS template into literal and expression segments.
func Parse(template string) (*Template, error) {
	p := &templateParser{src: template, line: 1, col: 1}
	return p.parse()
}

// ParsePure retains GIS escaping and segmentation while admitting its embedded
// expressions through the bounded GXL parser.
func ParsePure(template string) (*Template, error) {
	p := &templateParser{src: template, line: 1, col: 1, pure: true}
	return p.parse()
}

type templateParser struct {
	src            string
	off            int
	line           int
	col            int
	segments       []Segment
	litStart       Pos
	lit            strings.Builder
	litActive      bool
	ctx            context.Context
	blocks         []ExpressionBlock
	highlight      bool
	literalOffsets []int
	literals       []LiteralBlock
	pure           bool
}

func (p *templateParser) parse() (*Template, error) {
	for !p.eof() {
		if p.ctx != nil && p.ctx.Err() != nil {
			return nil, p.ctx.Err()
		}
		start := p.pos()
		if strings.HasPrefix(p.src[p.off:], `\${`) {
			p.writeLiteral(start, "${", 3)
			p.advanceN(3)
			continue
		}
		if strings.HasPrefix(p.src[p.off:], `$${`) {
			return nil, gisErr("GIS-PARSE-003", start, "$${ is not a GIS escape; use \\${ for a literal interpolation")
		}
		if strings.HasPrefix(p.src[p.off:], "${") {
			p.flushLiteral(start)
			if err := p.parseExpr(start); err != nil {
				return nil, err
			}
			continue
		}
		if p.src[p.off] == '\\' {
			if p.off+1 >= len(p.src) {
				p.writeLiteral(start, "\\", 1)
				p.advanceN(1)
				continue
			}
			next := p.src[p.off+1]
			switch next {
			case '\\':
				p.writeLiteral(start, "\\", 2)
				p.advanceN(2)
			case 'n':
				p.writeLiteral(start, "\n", 2)
				p.advanceN(2)
			case 'r':
				p.writeLiteral(start, "\r", 2)
				p.advanceN(2)
			case 't':
				p.writeLiteral(start, "\t", 2)
				p.advanceN(2)
			default:
				p.writeLiteral(start, p.src[p.off:p.off+2], 2)
				p.advanceN(2)
			}
			continue
		}
		r, size := utf8.DecodeRuneInString(p.src[p.off:])
		p.writeLiteral(start, string(r), size)
		p.advanceSize(size)
	}
	p.flushLiteral(p.pos())
	return &Template{Segments: p.segments}, nil
}

func (p *templateParser) parseExpr(start Pos) error {
	p.advanceN(2)
	exprStart := p.pos()
	end, err := findExprEndContext(p.ctx, p.src, p.off)
	if p.highlight {
		closed := err == nil
		if !closed {
			end = len(p.src)
		}
		p.blocks = append(p.blocks, ExpressionBlock{Start: start.Offset, ExpressionStart: p.off, ExpressionEnd: end, Closed: closed})
		p.off = end
		if closed {
			p.off++
		}
		return nil
	}
	if err != nil {
		return gisErr("GIS-PARSE-001", start, "unclosed interpolation block")
	}
	raw := p.src[p.off:end]
	if strings.TrimSpace(raw) == "" {
		return gisErr("GIS-PARSE-004", exprStart, "empty interpolation block")
	}
	parse := parser.Parse
	if p.pure {
		parse = parser.ParsePure
	}
	parsed, err := parse(raw)
	if err != nil {
		return gisErrWrap("GIS-PARSE-003", exprStart, "invalid GXL expression inside interpolation", err)
	}
	for p.off < end {
		p.advanceNext()
	}
	p.advanceN(1) // closing }
	p.segments = append(p.segments, &Expr{Span: Span{Start: start, End: p.pos()}, Expression: raw, Parsed: parsed})
	return nil
}

func findExprEnd(src string, start int) (int, error) {
	return findExprEndContext(nil, src, start)
}

func findExprEndContext(ctx context.Context, src string, start int) (int, error) {
	depth := 0
	for off := start; off < len(src); {
		if ctx != nil && ctx.Err() != nil {
			return 0, ctx.Err()
		}
		r, size := utf8.DecodeRuneInString(src[off:])
		if r == '"' || r == '\'' {
			next, err := skipQuotedContext(ctx, src, off, r)
			if err != nil {
				return 0, err
			}
			off = next
			continue
		}
		switch r {
		case '(', '[':
			depth++
		case ')', ']':
			if depth > 0 {
				depth--
			}
		case '}':
			if depth == 0 {
				return off, nil
			}
		}
		off += size
	}
	return 0, fmt.Errorf("missing closing brace")
}

func skipQuoted(src string, off int, quote rune) (int, error) {
	return skipQuotedContext(nil, src, off, quote)
}

func skipQuotedContext(ctx context.Context, src string, off int, quote rune) (int, error) {
	off += 1
	for off < len(src) {
		if ctx != nil && ctx.Err() != nil {
			return 0, ctx.Err()
		}
		r, size := utf8.DecodeRuneInString(src[off:])
		off += size
		if r == '\\' {
			if off >= len(src) {
				return 0, fmt.Errorf("unterminated string")
			}
			_, escSize := utf8.DecodeRuneInString(src[off:])
			off += escSize
			continue
		}
		if r == quote {
			return off, nil
		}
	}
	return 0, fmt.Errorf("unterminated string")
}

func (p *templateParser) writeLiteral(start Pos, s string, sourceBytes int) {
	if !p.litActive {
		p.litStart = start
		p.litActive = true
	}
	p.lit.WriteString(s)
	if p.highlight {
		// A multi-output escape (\${) has no independently paintable interior
		// boundary. Its final output token owns the whole authored escape.
		for i := range len(s) {
			offset := start.Offset + i
			if sourceBytes != len(s) {
				offset = start.Offset
			}
			p.literalOffsets = append(p.literalOffsets, offset)
		}
	}
}

func (p *templateParser) flushLiteral(end Pos) {
	if !p.litActive {
		return
	}
	p.segments = append(p.segments, &Literal{Span: Span{Start: p.litStart, End: end}, Value: p.lit.String()})
	if p.highlight {
		p.literals = append(p.literals, LiteralBlock{Text: p.lit.String(), Offsets: append(p.literalOffsets, end.Offset)})
		p.literalOffsets = nil
	}
	p.lit.Reset()
	p.litActive = false
}

func (p *templateParser) eof() bool { return p.off >= len(p.src) }
func (p *templateParser) pos() Pos  { return Pos{Line: p.line, Column: p.col, Offset: p.off} }

func (p *templateParser) advanceNext() {
	_, size := utf8.DecodeRuneInString(p.src[p.off:])
	p.advanceSize(size)
}
func (p *templateParser) advanceN(n int) {
	for i := 0; i < n; i++ {
		p.advanceSize(1)
	}
}
func (p *templateParser) advanceSize(size int) {
	if size <= 0 || p.off >= len(p.src) {
		return
	}
	if p.src[p.off] == '\r' {
		p.off += size
		if p.off < len(p.src) && p.src[p.off] == '\n' {
			p.off++
		}
		p.line++
		p.col = 1
		return
	}
	if p.src[p.off] == '\n' {
		p.off += size
		p.line++
		p.col = 1
		return
	}
	p.off += size
	p.col++
}

func gisErr(code string, pos Pos, msg string) error {
	return errkit.New(code, fmt.Sprintf("%s at line %d col %d", msg, pos.Line, pos.Column))
}

func gisErrWrap(code string, pos Pos, msg string, cause error) error {
	return errkit.Wrap(code, fmt.Sprintf("%s at line %d col %d", msg, pos.Line, pos.Column), cause)
}
