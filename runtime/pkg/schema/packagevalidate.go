package schema

import (
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"gopkg.in/yaml.v3"
)

// ScanForbiddenPackageKeys performs the mandated pre-schema raw-document scan
// Parse-gate pre-check.
// for PKG-020/PKG-021) that must run BEFORE JSON Schema validation, because
// a rejected key like "toolPackages" or "toolRefs[].alias" should receive a
// specific diagnostic rather than only a generic schema error.
//
//   - A top-level "toolPackages" key anywhere in the document is PKG-020.
//   - Any "toolRefs[].alias" key is PKG-021.
//
// It operates on raw YAML bytes and returns every violation found (not just
// the first), so all offending sites are reported in one pass.
func ScanForbiddenPackageKeys(data []byte) []error {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil // malformed YAML is reported elsewhere; nothing to scan
	}
	if doc.Kind == 0 || len(doc.Content) == 0 {
		return nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil
	}

	var errs []error
	for i := 0; i+1 < len(root.Content); i += 2 {
		key := root.Content[i]
		val := root.Content[i+1]
		if key.Value == "toolPackages" {
			errs = append(errs, errkit.New("PKG-020", fmt.Sprintf(
				"line %d: unsupported key 'toolPackages:' — use 'requires:' instead", key.Line)))
		}
		if key.Value == "toolRefs" && val.Kind == yaml.SequenceNode {
			errs = append(errs, scanToolRefsForAlias(val)...)
		}
	}
	return errs
}

func scanToolRefsForAlias(seq *yaml.Node) []error {
	var errs []error
	for _, item := range seq.Content {
		if item.Kind != yaml.MappingNode {
			continue
		}
		for i := 0; i+1 < len(item.Content); i += 2 {
			if item.Content[i].Value == "alias" {
				errs = append(errs, errkit.New("PKG-021", fmt.Sprintf(
					"line %d: 'toolRefs[].alias' is removed; aliases are out of MVP scope", item.Content[i].Line)))
			}
		}
	}
	return errs
}
