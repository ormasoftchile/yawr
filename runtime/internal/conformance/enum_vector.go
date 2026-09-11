package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

// EnumVector is one tv-enum.yaml conformance vector (AR-ENUM-1..15,
// barbara-enum-mvp-implementation-gate.md R2). Its shape differs from the
// GXL/GIS/GCP Vector type: variables models a synthetic workspace
// filesystem (a flat map from workspace-relative POSIX path to file
// content string) rather than a PJVM evaluation scope, and input is the
// entry-point file within that workspace rather than a raw expression.
type EnumVector struct {
	ID          string            `yaml:"id"`
	Category    string            `yaml:"category"`
	Description string            `yaml:"description"`
	Input       string            `yaml:"input"`
	Variables   map[string]string `yaml:"variables"`
	Expected    EnumExpected      `yaml:"expected"`
	Note        string            `yaml:"note,omitempty"`
	Tags        []string          `yaml:"tags,omitempty"`
}

// EnumExpected is the union of every expected: shape used across
// tv-enum.yaml's 58 vectors: an error (either error_class/error_code or a
// nested error.code, used by the ENUM-SUBST vectors that report a PKG-*
// contract violation), a success value (with optional warnings, e.g.
// ENUM-W001), or a package catalog snapshot (the ENUM-SUBST vectors that
// assert on pkgcatalog.Catalog contents rather than an error).
type EnumExpected struct {
	ErrorClass string                                 `yaml:"error_class,omitempty"`
	ErrorCode  string                                 `yaml:"error_code,omitempty"`
	Error      *EnumExpectedErrorRef                  `yaml:"error,omitempty"`
	Value      any                                    `yaml:"value,omitempty"`
	Warnings   []string                               `yaml:"warnings,omitempty"`
	Catalog    map[string]EnumCatalogEntryExpectation `yaml:"catalog,omitempty"`
	HasValue   bool                                   `yaml:"-"`
}

// EnumExpectedErrorRef is the {code: "PKG-013"} shape used by the
// ENUM-SUBST vectors.
type EnumExpectedErrorRef struct {
	Code string `yaml:"code"`
}

// EnumCatalogEntryExpectation is one expected catalog entry (by bare
// name), asserting on pkgcatalog.Entry.Qualified/Tier.
type EnumCatalogEntryExpectation struct {
	QualifiedName string `yaml:"qualifiedName"`
	Tier          int    `yaml:"tier"`
}

// UnmarshalYAML decodes EnumExpected while preserving explicit value:null
// (TV-ENUM-DEFAULT-005) as distinct from "no value: key at all".
func (e *EnumExpected) UnmarshalYAML(node *yaml.Node) error {
	type expected EnumExpected
	var decoded expected
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == "value" {
			decoded.HasValue = true
			break
		}
	}
	*e = EnumExpected(decoded)
	return nil
}

// WantsError reports whether the vector expects a failure (either
// error_class/error_code or the ENUM-SUBST error.code shape).
func (e EnumExpected) WantsError() bool {
	return e.ErrorClass != "" || e.ErrorCode != "" || e.Error != nil
}

// WantsCatalog reports whether the vector asserts on catalog contents
// rather than an error or a value.
func (e EnumExpected) WantsCatalog() bool { return len(e.Catalog) > 0 }

// EnumVectorFile is tv-enum.yaml's decoded top-level shape.
type EnumVectorFile struct {
	Vectors []EnumVector `yaml:"vectors"`
}

// LoadEnumSuite loads and schema-validates the vendored tv-enum.yaml
// corpus from dir (see internal/conformance/enumdata/README.md for the
// vendoring source and sync instructions).
func LoadEnumSuite(ctx context.Context, dir string) ([]EnumVector, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	schema, err := compileSchema(filepath.Join(dir, "vector.schema.json"))
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "tv-enum.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read tv-enum.yaml: %w", err)
	}
	if err := validateEnumYAML(schema, data); err != nil {
		return nil, fmt.Errorf("schema-validate tv-enum.yaml: %w", err)
	}
	var file EnumVectorFile
	if err := yaml.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("decode tv-enum.yaml: %w", err)
	}
	return file.Vectors, nil
}

// validateEnumYAML mirrors loader.go's validateYAML, duplicated here
// because EnumExpected's richer shape (nested error.code, catalog) is not
// itself schema-validated by the same struct as GXL/GIS/GCP Expected --
// only the raw YAML-as-JSON instance is, against the shared schema file.
func validateEnumYAML(schema *jsonschema.Schema, data []byte) error {
	var raw any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parse YAML: %w", err)
	}
	jsonBytes, err := json.Marshal(normalizeYAML(raw))
	if err != nil {
		return fmt.Errorf("marshal YAML as JSON: %w", err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(jsonBytes))
	if err != nil {
		return fmt.Errorf("unmarshal JSON instance: %w", err)
	}
	return schema.Validate(inst)
}
