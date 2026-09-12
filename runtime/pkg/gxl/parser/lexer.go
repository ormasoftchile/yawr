package parser

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
)

type lexer struct {
	src        string
	off        int
	line       int
	col        int
	tokens     []token
	ctx        context.Context
	comments   []HighlightToken
	tokenLimit int
	// Only highlighting records decoded string byte boundaries in source bytes.
	stringOffsets map[int][]int
}

func lex(src string) ([]token, error) {
	l := &lexer{src: src, line: 1, col: 1}
	err := l.scan()
	if err != nil {
		return nil, err
	}
	return l.tokens, nil
}

func (l *lexer) scan() error {
	for {
		if l.tokenLimit > 0 && len(l.tokens)+len(l.comments) >= l.tokenLimit {
			return fmt.Errorf("limit-exceeded")
		}
		if l.ctx != nil && l.ctx.Err() != nil {
			return l.ctx.Err()
		}
		l.skipSpaceAndComments()
		start := l.pos()
		if l.eof() {
			l.tokens = append(l.tokens, token{kind: tokEOF, start: start, end: start})
			return nil
		}
		r, _ := l.peekRune()
		switch {
		case isIdentStart(r):
			l.lexIdent()
		case r >= '0' && r <= '9':
			if err := l.lexNumber(); err != nil {
				return err
			}
		case r == '"' || r == '\'':
			if err := l.lexString(r); err != nil {
				return err
			}
		default:
			if err := l.lexSymbol(); err != nil {
				return err
			}
		}
	}
}

func (l *lexer) pos() Pos  { return Pos{Line: l.line, Column: l.col, Offset: l.off} }
func (l *lexer) eof() bool { return l.off >= len(l.src) || (l.ctx != nil && l.ctx.Err() != nil) }

func (l *lexer) peekRune() (rune, int) {
	if l.eof() {
		return -1, 0
	}
	return utf8.DecodeRuneInString(l.src[l.off:])
}

func (l *lexer) advance() (rune, Pos, Pos) {
	start := l.pos()
	r, size := l.peekRune()
	l.off += size
	if r == '\n' {
		l.line++
		l.col = 1
	} else if r == '\r' {
		if l.off < len(l.src) && l.src[l.off] == '\n' {
			l.off++
		}
		l.line++
		l.col = 1
	} else {
		l.col++
	}
	return r, start, l.pos()
}

func (l *lexer) skipSpaceAndComments() {
	for !l.eof() {
		r, _ := l.peekRune()
		switch r {
		case ' ', '\t', '\n', '\r':
			l.advance()
		case '#':
			start := l.off
			for !l.eof() {
				r, _ := l.peekRune()
				if r == '\n' || r == '\r' {
					break
				}
				l.advance()
			}
			if l.ctx != nil {
				l.comments = append(l.comments, HighlightToken{Start: start, End: l.off, Class: "comment"})
			}
		default:
			return
		}
	}
}

func (l *lexer) lexIdent() {
	start := l.pos()
	var b strings.Builder
	for !l.eof() {
		r, _ := l.peekRune()
		if !isIdentCont(r) {
			break
		}
		l.advance()
		b.WriteRune(r)
	}
	lit := b.String()
	kind := tokIdent
	if isKeyword(lit) {
		kind = tokKeyword
	}
	l.tokens = append(l.tokens, token{kind: kind, lit: lit, start: start, end: l.pos()})
}

func (l *lexer) lexNumber() error {
	start := l.pos()
	startOff := l.off
	r, _, _ := l.advance()
	if r == '0' {
		if !l.eof() {
			next, _ := l.peekRune()
			if next >= '0' && next <= '9' {
				for !l.eof() {
					n, _ := l.peekRune()
					if n < '0' || n > '9' {
						break
					}
					l.advance()
				}
				return parseErr("GXL-PARSE-003", start, "invalid number literal with leading zero")
			}
			if next == 'x' || next == 'X' || next == 'o' || next == 'O' || next == 'b' || next == 'B' {
				for !l.eof() {
					n, _ := l.peekRune()
					if !isIdentCont(n) {
						break
					}
					l.advance()
				}
				return parseErr("GXL-PARSE-003", start, "invalid number literal %q", l.src[startOff:l.off])
			}
		}
	} else {
		for !l.eof() {
			n, _ := l.peekRune()
			if n < '0' || n > '9' {
				break
			}
			l.advance()
		}
	}
	if l.matchByte('.') {
		l.advance()
		if l.eof() {
			return parseErr("GXL-PARSE-003", start, "invalid number literal: fraction requires a digit")
		}
		n, _ := l.peekRune()
		if n < '0' || n > '9' {
			return parseErr("GXL-PARSE-003", start, "invalid number literal: fraction requires a digit")
		}
		for !l.eof() {
			n, _ := l.peekRune()
			if n < '0' || n > '9' {
				break
			}
			l.advance()
		}
	}
	if !l.eof() {
		n, _ := l.peekRune()
		if n == 'e' || n == 'E' {
			l.advance()
			if !l.eof() {
				s, _ := l.peekRune()
				if s == '+' || s == '-' {
					l.advance()
				}
			}
			if l.eof() {
				return parseErr("GXL-PARSE-003", start, "invalid number literal: exponent requires a digit")
			}
			d, _ := l.peekRune()
			if d < '0' || d > '9' {
				return parseErr("GXL-PARSE-003", start, "invalid number literal: exponent requires a digit")
			}
			for !l.eof() {
				d, _ := l.peekRune()
				if d < '0' || d > '9' {
					break
				}
				l.advance()
			}
		}
	}
	lit := l.src[startOff:l.off]
	val, err := strconv.ParseFloat(lit, 64)
	if err != nil {
		return parseErr("GXL-PARSE-003", start, "invalid number literal %q", lit)
	}
	l.tokens = append(l.tokens, token{kind: tokNumber, lit: lit, value: val, start: start, end: l.pos()})
	return nil
}

func (l *lexer) lexString(quote rune) error {
	start := l.pos()
	l.advance()
	var b strings.Builder
	var offsets []int
	if l.ctx != nil {
		offsets = []int{l.off}
	}
	writeRune := func(r rune) {
		before := b.Len()
		b.WriteRune(r)
		if offsets != nil {
			for i := before + 1; i < b.Len(); i++ {
				offsets = append(offsets, -1)
			}
			offsets = append(offsets, l.off)
		}
	}
	for !l.eof() {
		r, rStart, _ := l.advance()
		if r == quote {
			lit := l.src[start.Offset:l.off]
			l.tokens = append(l.tokens, token{kind: tokString, lit: lit, value: b.String(), start: start, end: l.pos()})
			if offsets != nil {
				if l.stringOffsets == nil {
					l.stringOffsets = make(map[int][]int)
				}
				l.stringOffsets[start.Offset] = offsets
			}
			return nil
		}
		if r == '\n' || r == '\r' {
			return parseErr("GXL-PARSE-002", rStart, "unterminated string literal")
		}
		if r != '\\' {
			writeRune(r)
			continue
		}
		if l.eof() {
			return parseErr("GXL-PARSE-002", start, "unterminated string literal")
		}
		esc, escStart, _ := l.advance()
		switch esc {
		case '\\', '"', '\'':
			writeRune(esc)
		case 'n':
			writeRune('\n')
		case 'r':
			writeRune('\r')
		case 't':
			writeRune('\t')
		case '0':
			writeRune(0)
		case 'u':
			rn, err := l.readHexRune(4, escStart)
			if err != nil {
				return err
			}
			writeRune(rn)
		case 'U':
			rn, err := l.readHexRune(8, escStart)
			if err != nil {
				return err
			}
			writeRune(rn)
		default:
			return parseErr("GXL-PARSE-004", escStart, "invalid escape sequence \\%c", esc)
		}
	}
	return parseErr("GXL-PARSE-002", start, "unterminated string literal")
}

func (l *lexer) readHexRune(n int, pos Pos) (rune, error) {
	start := l.off
	for i := 0; i < n; i++ {
		if l.eof() {
			return 0, parseErr("GXL-PARSE-004", pos, "invalid unicode escape: expected %d hex digits", n)
		}
		r, _ := l.peekRune()
		if !isHex(r) {
			return 0, parseErr("GXL-PARSE-004", pos, "invalid unicode escape: expected %d hex digits", n)
		}
		l.advance()
	}
	v, err := strconv.ParseInt(l.src[start:l.off], 16, 32)
	if err != nil || !utf8.ValidRune(rune(v)) {
		return 0, parseErr("GXL-PARSE-004", pos, "invalid unicode escape")
	}
	return rune(v), nil
}

func (l *lexer) lexSymbol() error {
	start := l.pos()
	if strings.HasPrefix(l.src[l.off:], "?.[") {
		l.advance()
		l.advance()
		l.advance()
		l.tokens = append(l.tokens, token{kind: tokOptionalBracket, lit: "?.[", start: start, end: l.pos()})
		return nil
	}
	if strings.HasPrefix(l.src[l.off:], "?.") {
		l.advance()
		l.advance()
		l.tokens = append(l.tokens, token{kind: tokOptionalDot, lit: "?.", start: start, end: l.pos()})
		return nil
	}
	if strings.HasPrefix(l.src[l.off:], "&&") || strings.HasPrefix(l.src[l.off:], "||") || strings.HasPrefix(l.src[l.off:], ":=") {
		lit := l.src[l.off : l.off+2]
		l.advance()
		l.advance()
		return parseErr("GXL-PARSE-007", start, "forbidden syntax %q", lit)
	}
	if strings.HasPrefix(l.src[l.off:], "==") || strings.HasPrefix(l.src[l.off:], "!=") || strings.HasPrefix(l.src[l.off:], "<=") || strings.HasPrefix(l.src[l.off:], ">=") {
		lit := l.src[l.off : l.off+2]
		l.advance()
		l.advance()
		kind := map[string]tokenKind{"==": tokEq, "!=": tokNeq, "<=": tokLTE, ">=": tokGTE}[lit]
		l.tokens = append(l.tokens, token{kind: kind, lit: lit, start: start, end: l.pos()})
		return nil
	}
	r, _, _ := l.advance()
	single := map[rune]tokenKind{'(': tokLParen, ')': tokRParen, ',': tokComma, '.': tokDot, '[': tokLBracket, ']': tokRBracket, '<': tokLT, '>': tokGT, '+': tokPlus, '-': tokMinus, '*': tokStar, '/': tokSlash, '%': tokPercent}
	if k, ok := single[r]; ok {
		l.tokens = append(l.tokens, token{kind: k, lit: string(r), start: start, end: l.pos()})
		return nil
	}
	if r == '!' || r == '|' || r == '?' || r == ':' || r == '=' || r == '{' || r == '}' {
		return parseErr("GXL-PARSE-007", start, "forbidden syntax %q", string(r))
	}
	return parseErr("GXL-PARSE-001", start, "unexpected character %q", r)
}

func (l *lexer) matchByte(b byte) bool { return l.off < len(l.src) && l.src[l.off] == b }

func isIdentStart(r rune) bool { return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_' }
func isIdentCont(r rune) bool  { return isIdentStart(r) || (r >= '0' && r <= '9') }
func isHex(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
}

func parseErr(code string, pos Pos, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	msg = fmt.Sprintf("%s at line %d col %d", msg, pos.Line, pos.Column)
	return errkit.New(code, msg)
}
