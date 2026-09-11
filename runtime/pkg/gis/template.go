// Package gis parses and evaluates YAWR Interpolated Strings.
package gis

import "github.com/ormasoftchile/yawr/runtime/pkg/gxl/parser"

// Pos identifies a byte-oriented source location in a GIS template.
type Pos struct {
	Line   int
	Column int
	Offset int
}

// Span identifies the source range occupied by a template segment.
type Span struct {
	Start Pos
	End   Pos
}

// Segment is implemented by every GIS template segment.
type Segment interface {
	SegmentSpan() Span
	segmentKind() string
}

// Template is a parsed interpolated string.
type Template struct {
	Segments []Segment
}

// Literal is a raw text segment after GIS escape processing.
type Literal struct {
	Span  Span
	Value string
}

// Expr is an embedded ${...} GXL expression segment.
type Expr struct {
	Span       Span
	Expression string
	Parsed     *parser.Expr
}

// SegmentSpan returns the segment source span.
func (l *Literal) SegmentSpan() Span   { return l.Span }
func (l *Literal) segmentKind() string { return "literal" }

// SegmentSpan returns the segment source span.
func (e *Expr) SegmentSpan() Span   { return e.Span }
func (e *Expr) segmentKind() string { return "expr" }
