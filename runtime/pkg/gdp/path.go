// Package gdp resolves YAWR Dotted Paths against PJVM value trees.
package gdp

import (
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/pjvm"
)

// Dialect selects the conformance error-code family used during resolution.
type Dialect string

const (
	// DialectGXL reports GXL-PATH errors.
	DialectGXL Dialect = "gxl"
	// DialectGCP reports GCP-RESOLVE errors.
	DialectGCP Dialect = "gcp"
)

// Pos identifies a source location in a dotted path.
type Pos struct {
	Line   int
	Column int
	Offset int
}

// Span identifies the source range occupied by a path or segment.
type Span struct {
	Start Pos
	End   Pos
}

// SourceKind identifies the source prefix of a GCP path.
type SourceKind string

const (
	// SourceLocal identifies current-step stdout/stderr/exit_code/json/yaml captures.
	SourceLocal SourceKind = "local"
	// SourceStep identifies step-qualified captures.
	SourceStep SourceKind = "step"
	// SourceHTTP identifies http.* captures.
	SourceHTTP SourceKind = "http"
	// SourceEvent identifies event.* captures.
	SourceEvent SourceKind = "event"
	// SourceOutputs identifies outputs.<name> captures: the ratified
	// LocalOutputs capture root (spec/grammar/gcp.ebnf §3.4a) for a
	// substituted (execute.kind: runbook) action's own declared outputs,
	// capturable only in the owning step's own capture: block.
	SourceOutputs SourceKind = "outputs"
)

// Source describes the non-GDP prefix of a capture path.
type Source struct {
	Kind   SourceKind
	Name   string
	StepID string
	Field  string
	Header string
}

// Path is a parsed YAWR Dotted Path with optional GCP source metadata.
type Path struct {
	Span     Span
	Dialect  Dialect
	Root     string
	Segments []Segment
	Source   Source
}

// Segment is one field or index hop in a YAWR Dotted Path.
type Segment struct {
	Span     Span
	Name     string
	Index    *int
	Optional bool
}

// Resolve resolves path against tree and returns a hard error on misses.
func Resolve(tree pjvm.Value, path *Path) (pjvm.Value, error) {
	value, found, err := resolve(tree, path, false)
	if err != nil {
		return pjvm.Null(), err
	}
	if !found {
		return pjvm.Null(), missError(path, "optional path did not resolve")
	}
	return value, nil
}

// ResolveOptional resolves path and reports false when an optional segment misses.
func ResolveOptional(tree pjvm.Value, path *Path) (pjvm.Value, bool, error) {
	return resolve(tree, path, true)
}

func resolve(tree pjvm.Value, path *Path, allowOptional bool) (pjvm.Value, bool, error) {
	if path == nil {
		return pjvm.Null(), false, errkit.New(resolveCode(DialectGXL, "field-missing"), "nil path")
	}
	cur := tree
	start := 0
	if path.Root != "" {
		obj, ok := cur.ObjectValue()
		if !ok {
			return pjvm.Null(), false, fieldTypeError(path, path.Root, cur.Kind())
		}
		val, ok := obj[path.Root]
		if !ok {
			if allowOptional && len(path.Segments) > 0 && path.Segments[0].Optional {
				return pjvm.Null(), false, nil
			}
			return pjvm.Null(), false, missError(path, fmt.Sprintf("path root %q not found", path.Root))
		}
		cur = val
		start = 0
	}
	for i := start; i < len(path.Segments); i++ {
		seg := path.Segments[i]
		next, err := resolveSegment(cur, seg, path)
		if err != nil {
			if allowOptional && seg.Optional && isOptionalMiss(err) {
				return pjvm.Null(), false, nil
			}
			return pjvm.Null(), false, err
		}
		cur = next
	}
	return cur, true, nil
}

func resolveSegment(cur pjvm.Value, seg Segment, path *Path) (pjvm.Value, error) {
	if seg.Index != nil {
		arr, ok := cur.ArrayValue()
		if !ok {
			return pjvm.Null(), indexError(path, *seg.Index, cur.Kind())
		}
		if *seg.Index < 0 || *seg.Index >= len(arr) {
			return pjvm.Null(), indexError(path, *seg.Index, cur.Kind())
		}
		return arr[*seg.Index], nil
	}
	if cur.Kind() == pjvm.KindNull && seg.Optional {
		return pjvm.Null(), errkit.New(resolveCode(pathDialect(path), "field-missing"), fmt.Sprintf("optional field %q reached null", seg.Name))
	}
	obj, ok := cur.ObjectValue()
	if !ok {
		return pjvm.Null(), fieldTypeError(path, seg.Name, cur.Kind())
	}
	val, ok := obj[seg.Name]
	if !ok {
		return pjvm.Null(), missError(path, fmt.Sprintf("path segment %q not found", seg.Name))
	}
	return val, nil
}

func isOptionalMiss(err error) bool {
	code := ""
	if c, ok := err.(interface{ Code() string }); ok {
		code = c.Code()
	}
	switch code {
	case "GXL-PATH-001", "GXL-PATH-002", "GXL-PATH-003", "GCP-RESOLVE-002", "GCP-RESOLVE-003":
		return true
	default:
		return false
	}
}

func missError(path *Path, msg string) error {
	return &MissingPathError{Err: errkit.New(resolveCode(pathDialect(path), "field-missing"), msg)}
}

// MissingPathError classifies absence without confusing null or a type error
// with an omitted optional result.
type MissingPathError struct{ Err error }

func (e *MissingPathError) Error() string { return e.Err.Error() }
func (e *MissingPathError) Unwrap() error { return e.Err }
func (e *MissingPathError) Code() string {
	if coded, ok := e.Err.(interface{ Code() string }); ok {
		return coded.Code()
	}
	return ""
}

func indexError(path *Path, idx int, kind pjvm.Kind) error {
	kindName := "index"
	if pathDialect(path) == DialectGXL && kind != pjvm.KindArray {
		kindName = "index-type"
	}
	err := errkit.New(resolveCode(pathDialect(path), kindName), fmt.Sprintf("array index %d unavailable on %s", idx, kind))
	if kind == pjvm.KindArray {
		return &MissingPathError{Err: err}
	}
	return err
}

func fieldTypeError(path *Path, name string, kind pjvm.Kind) error {
	return errkit.New(resolveCode(pathDialect(path), "field-type"), fmt.Sprintf("field %q cannot be read from %s", name, kind))
}

func pathDialect(path *Path) Dialect {
	if path != nil && path.Dialect != "" {
		return path.Dialect
	}
	return DialectGXL
}

func resolveCode(d Dialect, kind string) string {
	if d == DialectGCP {
		switch kind {
		case "index":
			return "GCP-RESOLVE-003"
		default:
			return "GCP-RESOLVE-002"
		}
	}
	switch kind {
	case "index":
		return "GXL-PATH-002"
	case "index-type":
		return "GXL-PATH-003"
	case "field-type":
		return "GXL-PATH-004"
	default:
		return "GXL-PATH-001"
	}
}
