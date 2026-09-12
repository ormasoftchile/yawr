// Package parser implements the yawr runbook parser.
//
// The parser performs two-phase validation:
//  1. Structural validation via JSON Schema (Draft 2020-12)
//  2. Semantic validation (cross-field rules, signal allow-list, nested parallel detection)
//
// Use New to construct a parser and Parse or ParseBytes to parse a runbook.
package parser

import (
	"context"
	"fmt"
	"os"

	parserPkg "github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// impl is the concrete Parser implementation.
type impl struct {
	plat   platform.Platform
	schema *jsonschema.Schema
}

// New constructs a Parser backed by the compiled JSON Schema and the given Platform.
// Returns an error if the embedded JSON Schema cannot be compiled.
func New(p platform.Platform) (parserPkg.Parser, error) {
	sch, err := compileSchema()
	if err != nil {
		return nil, fmt.Errorf("parser: compile schema: %w", err)
	}
	return &impl{plat: p, schema: sch}, nil
}

// Parse reads the runbook at path and returns a validated ParsedRunbook.
// Returns an error if the runbook is structurally or semantically invalid.
func (p *impl) Parse(_ context.Context, path string) (*parserPkg.ParsedRunbook, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("parser: read %s: %w", path, err)
	}
	pr, err := p.ParseBytes(context.Background(), data)
	if err != nil {
		return nil, err
	}
	pr.Source = path
	return pr, nil
}

// ParseBytes parses from an in-memory YAML source.
func (p *impl) ParseBytes(_ context.Context, src []byte) (*parserPkg.ParsedRunbook, error) {
	// Phase 0: pre-schema raw-document scan for keys that must be rejected
	// with a specific PKG-020/PKG-021 diagnostic rather than a generic
	// schema error (or, in PKG-020's case, silently accepted because
	// ToolRef/PackageRequirement's additionalProperties settings would
	// otherwise let it through).
	if pkgErrs := schema.ScanForbiddenPackageKeys(src); len(pkgErrs) > 0 {
		var ve ValidationErrors
		for _, e := range pkgErrs {
			ve = append(ve, verr(errCode(e), "", e.Error()))
		}
		return nil, ve
	}

	// Phase 1: structural validation via JSON Schema.
	structErrs := validateStructural(p.schema, src)
	if len(structErrs) > 0 {
		return nil, structErrs
	}

	// Unmarshal YAML into schema types using type-dispatch.
	rb, err := parseRunbook(src)
	if err != nil {
		return nil, fmt.Errorf("parser: unmarshal: %w", err)
	}

	// Phase 2: semantic validation.
	semErrs, semWarnings := validateSemantic(rb, p.plat)
	if len(semErrs) > 0 {
		return nil, semErrs
	}

	return &parserPkg.ParsedRunbook{Runbook: rb, Warnings: semWarnings}, nil
}

// errCode extracts a machine-readable error code from an errkit-typed error,
// falling back to the generic "schema/forbidden-key" code.
func errCode(err error) string {
	if c, ok := err.(interface{ Code() string }); ok {
		return c.Code()
	}
	return "schema/forbidden-key"
}
