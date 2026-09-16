package presentation

import (
	"context"
	"errors"

	internalparser "github.com/ormasoftchile/yawr/runtime/internal/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/pkgcatalog"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"gopkg.in/yaml.v3"
)

type dependencyMetadataParser struct{}

func needsDependencyClosure(root *yaml.Node, catalog *pkgcatalog.Catalog, entry string) bool {
	if hasMetadataIncludes(mapValue(root, "flow")) {
		return true
	}
	var refs []*schema.ToolRef
	if node := mapValue(root, "toolRefs"); node != nil {
		if err := node.Decode(&refs); err != nil {
			return false
		}
	}
	seen := make(map[string]bool, len(refs))
	for _, ref := range refs {
		if ref != nil {
			seen[ref.Name] = true
		}
	}
	collectMetadataToolCalls(mapValue(root, "flow"), &refs, seen)
	if catalog != nil {
		bindings, _ := pkgcatalog.BindFile(catalog, entry, refs, "")
		for _, binding := range bindings {
			for _, action := range binding.Def.Actions {
				if action != nil && action.Execute.IsSubstitution() {
					return true
				}
			}
		}
	}
	return false
}

func collectMetadataToolCalls(flow *yaml.Node, refs *[]*schema.ToolRef, seen map[string]bool) {
	if flow == nil || flow.Kind != yaml.SequenceNode {
		return
	}
	branches := func(node *yaml.Node) {
		if node != nil && node.Kind == yaml.SequenceNode {
			for _, branch := range node.Content {
				collectMetadataToolCalls(mapValue(branch, "steps"), refs, seen)
			}
		}
	}
	for _, node := range flow.Content {
		if step := mapValue(node, "step"); step != nil {
			if stringValue(mapValue(step, "type")) == "tool" {
				name := stringValue(mapValue(mapValue(step, "tool"), "name"))
				if name != "" && !seen[name] {
					seen[name] = true
					*refs = append(*refs, &schema.ToolRef{Name: name})
				}
			}
			branches(mapValue(step, "branches"))
			collectMetadataToolCalls(mapValue(mapValue(step, "compensate"), "steps"), refs, seen)
		}
		branches(mapValue(mapValue(node, "parallel"), "branches"))
		collectMetadataToolCalls(mapValue(mapValue(node, "iterate"), "steps"), refs, seen)
	}
}

func hasMetadataIncludes(flow *yaml.Node) bool {
	if flow == nil || flow.Kind != yaml.SequenceNode {
		return false
	}
	for _, node := range flow.Content {
		if step := mapValue(node, "step"); step != nil {
			if stringValue(mapValue(step, "type")) == "include" {
				return true
			}
			if hasMetadataBranches(mapValue(step, "branches")) ||
				hasMetadataIncludes(mapValue(mapValue(step, "compensate"), "steps")) {
				return true
			}
		}
		if hasMetadataIncludes(mapValue(mapValue(node, "iterate"), "steps")) ||
			hasMetadataBranches(mapValue(mapValue(node, "parallel"), "branches")) {
			return true
		}
	}
	return false
}

func hasMetadataBranches(branches *yaml.Node) bool {
	if branches == nil || branches.Kind != yaml.SequenceNode {
		return false
	}
	for _, branch := range branches.Content {
		if hasMetadataIncludes(mapValue(branch, "steps")) {
			return true
		}
	}
	return false
}

func (dependencyMetadataParser) Parse(context.Context, string) (*parser.ParsedRunbook, error) {
	return nil, errors.New("presentation: dependency metadata must use captured source")
}

func (dependencyMetadataParser) ParseBytes(ctx context.Context, source []byte) (*parser.ParsedRunbook, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, _ := parseSource(string(source))
	if root == nil || !uniqueMappingKeys(root) {
		return nil, errors.New("presentation: incomplete dependency identity")
	}
	data, err := yaml.Marshal(root)
	if err != nil {
		return nil, err
	}
	runbook, err := internalparser.DecodeDependencyMetadata(data)
	if err != nil {
		return nil, err
	}
	return &parser.ParsedRunbook{Runbook: runbook}, nil
}
