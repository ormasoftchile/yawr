// Package parser defines the interface for parsing runbook files.
// The Parser reads YAML source, validates structure, and produces a ParsedRunbook
// that the Planner can consume without re-reading the source.
package parser

import (
	"context"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// ParsedRunbook is the validated, normalized form of a runbook source file.
// It is the output of Parser.Parse and the input to Planner.Plan.
type ParsedRunbook struct {
	// Source is the original file path or URI.
	Source string
	// Runbook is the fully parsed and structurally validated runbook.
	Runbook *schema.Runbook
	// ResolvedIncludes contains any included runbooks, recursively resolved.
	ResolvedIncludes []*ParsedRunbook
	// Warnings are non-fatal parse warnings (e.g., deprecated fields used).
	Warnings []ParseWarning
}

// ParseWarning is a non-fatal warning emitted during parsing.
type ParseWarning struct {
	Field   string
	Message string
}

// Parser reads runbook source files and produces ParsedRunbook values.
// Implementations handle YAML parsing, JSON Schema structural validation, and
// semantic validation (cross-field rules, signal allow-lists, nested parallel detection).
// The Planner consumes the fully-validated ParsedRunbook without re-reading the source.
type Parser interface {
	// Parse reads the runbook at path (or from source bytes if path is "-")
	// and returns a validated ParsedRunbook.
	// Returns an error if the runbook is structurally invalid.
	Parse(ctx context.Context, path string) (*ParsedRunbook, error)
	// ParseBytes parses from an in-memory source (useful in tests).
	ParseBytes(ctx context.Context, source []byte) (*ParsedRunbook, error)
}
