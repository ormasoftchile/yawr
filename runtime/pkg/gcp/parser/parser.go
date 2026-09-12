// Package parser implements the YAWR Capture Path parser.
package parser

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/gdp"
)

// Path is a parsed YAWR Capture Path.
type Path = gdp.Path

// Parse parses src as a complete GCP capture path.
func Parse(src string) (*Path, error) {
	p := &parser{src: src, line: 1, col: 1}
	path, err := p.capturePath()
	if err != nil {
		return nil, err
	}
	if !p.eof() {
		return nil, p.err("GCP-PARSE-001", p.pos(), "unexpected character %q", p.peek())
	}
	path.Span.End = p.pos()
	path.Dialect = gdp.DialectGCP
	return path, nil
}

// Validate parses src and discards the resulting AST.
func Validate(src string) error {
	_, err := Parse(src)
	return err
}

type parser struct {
	src       string
	off       int
	line, col int
}

func (p *parser) capturePath() (*Path, error) {
	start := p.pos()
	first, firstSpan, err := p.ident(false)
	if err != nil {
		return nil, err
	}
	path := &gdp.Path{Span: gdp.Span{Start: start, End: firstSpan.End}}
	switch first {
	case "step":
		return p.step(path)
	case "http":
		return p.http(path)
	case "event":
		return p.event(path)
	case "exit_code":
		if !p.eof() {
			return nil, p.err("GCP-PARSE-002", p.pos(), "exit_code does not permit a path suffix")
		}
		path.Source = gdp.Source{Kind: gdp.SourceLocal, Name: first, Field: first}
		return path, nil
	case "stdout", "stderr", "json", "yaml":
		path.Source = gdp.Source{Kind: gdp.SourceLocal, Name: first, Field: first}
		if !p.eof() {
			if err := p.gdpSuffix(path); err != nil {
				return nil, err
			}
		}
		return path, nil
	case "outputs":
		return p.outputs(path)
	default:
		return nil, p.err("GCP-PARSE-001", firstSpan.Start, "unknown source prefix %q", first)
	}
}

func (p *parser) step(path *gdp.Path) (*Path, error) {
	if err := p.dot(); err != nil {
		return nil, err
	}
	stepID, _, err := p.ident(false)
	if err != nil {
		return nil, err
	}
	if err := p.dot(); err != nil {
		return nil, err
	}
	field, _, err := p.ident(false)
	if err != nil {
		return nil, err
	}
	switch field {
	case "exit_code":
		if !p.eof() {
			return nil, p.err("GCP-PARSE-002", p.pos(), "step exit_code does not permit a path suffix")
		}
	case "stdout", "stderr", "json", "yaml":
		if !p.eof() {
			if err := p.gdpSuffix(path); err != nil {
				return nil, err
			}
		}
	default:
		return nil, p.err("GCP-PARSE-001", p.pos(), "unknown step output %q", field)
	}
	path.Source = gdp.Source{Kind: gdp.SourceStep, Name: field, StepID: stepID, Field: field}
	return path, nil
}

func (p *parser) http(path *gdp.Path) (*Path, error) {
	if err := p.dot(); err != nil {
		return nil, err
	}
	field, _, err := p.ident(false)
	if err != nil {
		return nil, err
	}
	switch field {
	case "status":
		if !p.eof() {
			return nil, p.err("GCP-PARSE-002", p.pos(), "http.status does not permit a path suffix")
		}
		path.Source = gdp.Source{Kind: gdp.SourceHTTP, Name: "http", Field: field}
	case "body":
		path.Source = gdp.Source{Kind: gdp.SourceHTTP, Name: "http", Field: field}
		if !p.eof() {
			if err := p.gdpSuffix(path); err != nil {
				return nil, err
			}
		}
	case "headers":
		if err := p.dot(); err != nil {
			return nil, err
		}
		header, err := p.headerName()
		if err != nil {
			return nil, err
		}
		if !p.eof() {
			return nil, p.err("GCP-PARSE-002", p.pos(), "headers do not permit a path suffix")
		}
		path.Source = gdp.Source{Kind: gdp.SourceHTTP, Name: "http", Field: field, Header: strings.ToLower(header)}
	default:
		return nil, p.err("GCP-PARSE-001", p.pos(), "unknown http field %q", field)
	}
	return path, nil
}

func (p *parser) outputs(path *gdp.Path) (*Path, error) {
	if p.eof() {
		path.Source = gdp.Source{Kind: gdp.SourceOutputs, Name: "outputs"}
		return path, nil
	}
	if err := p.dot(); err != nil {
		return nil, err
	}
	name, _, err := p.ident(false)
	if err != nil {
		return nil, err
	}
	path.Source = gdp.Source{Kind: gdp.SourceOutputs, Name: "outputs", Field: name}
	if !p.eof() {
		if err := p.gdpSuffix(path); err != nil {
			return nil, err
		}
	}
	return path, nil
}

func (p *parser) event(path *gdp.Path) (*Path, error) {
	if err := p.dot(); err != nil {
		return nil, err
	}
	field, _, err := p.ident(false)
	if err != nil {
		return nil, err
	}
	switch field {
	case "id":
		if !p.eof() {
			return nil, p.err("GCP-PARSE-002", p.pos(), "event.id does not permit a path suffix")
		}
		path.Source = gdp.Source{Kind: gdp.SourceEvent, Name: "event", Field: field}
	case "body":
		path.Source = gdp.Source{Kind: gdp.SourceEvent, Name: "event", Field: field}
		if !p.eof() {
			if err := p.gdpSuffix(path); err != nil {
				return nil, err
			}
		}
	case "headers":
		if err := p.dot(); err != nil {
			return nil, err
		}
		header, err := p.headerName()
		if err != nil {
			return nil, err
		}
		if !p.eof() {
			return nil, p.err("GCP-PARSE-002", p.pos(), "headers do not permit a path suffix")
		}
		path.Source = gdp.Source{Kind: gdp.SourceEvent, Name: "event", Field: field, Header: strings.ToLower(header)}
	default:
		return nil, p.err("GCP-PARSE-001", p.pos(), "unknown event field %q", field)
	}
	return path, nil
}

func (p *parser) gdpSuffix(path *gdp.Path) error {
	for !p.eof() {
		switch p.peek() {
		case '.':
			dotStart := p.pos()
			p.advance()
			if p.eof() || p.peek() == '.' {
				return p.err("GCP-PARSE-004", dotStart, "empty path segment")
			}
			name, span, err := p.ident(false)
			if err != nil {
				return err
			}
			path.Segments = append(path.Segments, gdp.Segment{Span: gdp.Span{Start: dotStart, End: span.End}, Name: name})
		case '[':
			seg, err := p.index(false)
			if err != nil {
				return err
			}
			path.Segments = append(path.Segments, seg)
		case '?':
			return p.err("GCP-PARSE-001", p.pos(), "optional chaining is not legal in GCP capture paths")
		default:
			return p.err("GCP-PARSE-001", p.pos(), "unexpected character %q", p.peek())
		}
	}
	return nil
}

func (p *parser) index(optional bool) (gdp.Segment, error) {
	open := p.pos()
	p.advance()
	if p.eof() {
		return gdp.Segment{}, p.err("GCP-PARSE-001", open, "unterminated index segment")
	}
	if p.peek() == '-' {
		return gdp.Segment{}, p.err("GCP-PARSE-003", p.pos(), "negative array index is forbidden")
	}
	start := p.pos()
	digits := p.readDigits()
	if digits == "" {
		return gdp.Segment{}, p.err("GCP-PARSE-001", start, "expected array index")
	}
	if p.eof() || p.peek() != ']' {
		return gdp.Segment{}, p.err("GCP-PARSE-001", p.pos(), "expected closing bracket")
	}
	idx, err := strconv.Atoi(digits)
	if err != nil {
		return gdp.Segment{}, p.err("GCP-PARSE-001", start, "array index is too large")
	}
	p.advance()
	return gdp.Segment{Span: gdp.Span{Start: open, End: p.pos()}, Index: &idx, Optional: optional}, nil
}

func (p *parser) dot() error {
	if p.eof() || p.peek() != '.' {
		return p.err("GCP-PARSE-004", p.pos(), "expected dot separator")
	}
	p.advance()
	if p.eof() || p.peek() == '.' {
		return p.err("GCP-PARSE-004", p.pos(), "empty path segment")
	}
	return nil
}

func (p *parser) ident(allowHyphen bool) (string, gdp.Span, error) {
	start := p.pos()
	if p.eof() || !isIdentStart(p.peek()) {
		return "", gdp.Span{}, p.err("GCP-PARSE-001", start, "expected identifier")
	}
	var b strings.Builder
	for !p.eof() {
		r := p.peek()
		if isIdentCont(r) || (allowHyphen && r == '-') {
			b.WriteRune(r)
			p.advance()
			continue
		}
		break
	}
	return b.String(), gdp.Span{Start: start, End: p.pos()}, nil
}

func (p *parser) headerName() (string, error) {
	start := p.pos()
	name, _, err := p.ident(true)
	if err != nil {
		return "", p.err("GCP-PARSE-005", start, "invalid header name")
	}
	if strings.Contains(name, "--") || strings.HasSuffix(name, "-") {
		return "", p.err("GCP-PARSE-005", start, "invalid header name %q", name)
	}
	if !p.eof() {
		r := p.peek()
		if r == ':' || r == ',' || r == ' ' || r == '\t' {
			return "", p.err("GCP-PARSE-005", p.pos(), "invalid header name character %q", r)
		}
	}
	return name, nil
}

func (p *parser) readDigits() string {
	start := p.off
	for !p.eof() {
		r := p.peek()
		if r < '0' || r > '9' {
			break
		}
		p.advance()
	}
	return p.src[start:p.off]
}

func (p *parser) eof() bool { return p.off >= len(p.src) }
func (p *parser) peek() rune {
	r, _ := utf8.DecodeRuneInString(p.src[p.off:])
	return r
}
func (p *parser) pos() gdp.Pos { return gdp.Pos{Line: p.line, Column: p.col, Offset: p.off} }
func (p *parser) advance() {
	r, size := utf8.DecodeRuneInString(p.src[p.off:])
	p.off += size
	if r == '\n' {
		p.line++
		p.col = 1
	} else {
		p.col++
	}
}

func (p *parser) err(code string, pos gdp.Pos, format string, args ...any) error {
	return errkit.New(code, fmt.Sprintf(format+" at line %d col %d", append(args, pos.Line, pos.Column)...))
}

func isIdentStart(r rune) bool { return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_' }
func isIdentCont(r rune) bool  { return isIdentStart(r) || (r >= '0' && r <= '9') }
