package tool

import (
	"errors"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"gopkg.in/yaml.v3"
)

// Decode only binding identity and declared field contracts. Transport/auth,
// argv, defaults and result-parser configuration are not authoring prerequisites.
func parsePresentationMetadata(data []byte) (*schema.ToolDef, error) {
	var doc yaml.Node
	if yaml.Unmarshal(data, &doc) != nil || len(doc.Content) != 1 {
		return nil, errors.New("invalid tool source")
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode || !uniqueMetadataNodes(root) {
		return nil, errors.New("invalid tool identity")
	}
	filter := func(n *yaml.Node, keys ...string) *yaml.Node {
		if n == nil || n.Kind != yaml.MappingNode {
			return n
		}
		out := *n
		out.Content = nil
		for i := 0; i < len(n.Content); i += 2 {
			for _, key := range keys {
				if n.Content[i].Value == key {
					out.Content = append(out.Content, n.Content[i], n.Content[i+1])
					break
				}
			}
		}
		return &out
	}
	clean := filter(root, "apiVersion", "meta", "name", "version", "actions")
	for i := 0; i < len(clean.Content); i += 2 {
		switch clean.Content[i].Value {
		case "meta":
			clean.Content[i+1] = filter(clean.Content[i+1], "name", "version")
		case "actions":
			actions := *clean.Content[i+1]
			actions.Content = append([]*yaml.Node(nil), actions.Content...)
			start, increment := 0, 1
			if actions.Kind == yaml.MappingNode {
				start, increment = 1, 2
			}
			for j := start; j < len(actions.Content); j += increment {
				a := filter(actions.Content[j], "name", "args", "outputs", "execute")
				if a == nil || a.Kind != yaml.MappingNode {
					continue
				}
				for k := 0; k < len(a.Content); k += 2 {
					if a.Content[k].Value != "args" && a.Content[k].Value != "outputs" {
						continue
					}
					fields := *a.Content[k+1]
					fields.Content = append([]*yaml.Node(nil), fields.Content...)
					if fields.Kind != yaml.MappingNode {
						continue
					}
					for n := 1; n < len(fields.Content); n += 2 {
						fields.Content[n] = filter(fields.Content[n], "type", "presentation", "optional", "from")
					}
					a.Content[k+1] = &fields
				}
				actions.Content[j] = a
			}
			clean.Content[i+1] = &actions
		}
	}
	var def schema.ToolDef
	if err := clean.Decode(&def); err != nil {
		return nil, err
	}
	if def.Name == "" {
		return nil, errors.New("incomplete tool identity")
	}
	return &def, nil
}

func uniqueMetadataNodes(n *yaml.Node) bool {
	if n.Kind == yaml.AliasNode || n.Anchor != "" {
		return false
	}
	if n.Kind == yaml.MappingNode {
		seen := map[string]bool{}
		for i := 0; i < len(n.Content); i += 2 {
			k := n.Content[i]
			if k.Kind != yaml.ScalarNode || k.Tag != "!!str" || seen[k.Value] {
				return false
			}
			seen[k.Value] = true
		}
	}
	for _, child := range n.Content {
		if !uniqueMetadataNodes(child) {
			return false
		}
	}
	return true
}
