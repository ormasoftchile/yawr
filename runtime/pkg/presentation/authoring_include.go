package presentation

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"sync"

	"github.com/ormasoftchile/yawr/runtime/schemas"
	"gopkg.in/yaml.v3"
)

// Read the same embedded schema used by runtime validation. In particular,
// oneOf's closed arms, not a second editor inventory, govern eligible fields.
type includeAuthoringSchema struct {
	Type        string                             `json:"type"`
	Description string                             `json:"description"`
	Properties  map[string]*includeAuthoringSchema `json:"properties"`
	Required    []string                           `json:"required"`
	Enum        []string                           `json:"enum"`
	OneOf       []*includeAuthoringSchema          `json:"oneOf"`
}

var includeSchema = sync.OnceValue(func() *includeAuthoringSchema {
	var root struct {
		Defs map[string]json.RawMessage `json:"$defs"`
	}
	if json.Unmarshal(schemas.RunbookSchema, &root) != nil {
		return nil
	}
	var spec includeAuthoringSchema
	if json.Unmarshal(root.Defs["IncludeConfig"], &spec) != nil {
		return nil
	}
	return &spec
})

func includeProperties(spec *includeAuthoringSchema, mapping, current *yaml.Node) map[string]*includeAuthoringSchema {
	out := map[string]*includeAuthoringSchema{}
	if spec == nil {
		return out
	}
	arms := spec.OneOf
	if len(arms) == 0 {
		arms = []*includeAuthoringSchema{spec}
	}
	for _, arm := range arms {
		compatible := true
		if mapping != nil && mapping.Kind == yaml.MappingNode {
			for i := 0; i < len(mapping.Content); i += 2 {
				key := mapping.Content[i]
				if key != current && arm.Properties[key.Value] == nil {
					compatible = false
				}
			}
		}
		if compatible {
			for name, property := range arm.Properties {
				out[name] = property
			}
		}
	}
	return out
}

func (s *authoringSource) includeTarget(n *yaml.Node, path string, key *yaml.Node, spec *includeAuthoringSchema) *authoringTarget {
	if n == nil || spec == nil {
		return nil
	}
	if n.Kind == yaml.ScalarNode && n.Tag == "!!null" && n.Value == "" && key != nil && s.keyLine(key) {
		kr, keyOK := s.scalar(key, key.Column)
		end, _ := authoringByteOffset(s.original, kr.End)
		if r, ok := s.scalar(n, key.Column); keyOK && s.argumentSeparator(end) && ok && s.at(r) && spec.Type == "object" {
			return &authoringTarget{site: AuthoringSite{Kind: "include-mapping", YAMLPath: path, Range: r},
				node: n, key: key, includeSchema: spec}
		}
	}
	if !uniqueMappingKeys(n) {
		return nil
	}
	r, ok := s.includeMappingRange(n)
	if !ok {
		return nil
	}
	properties := includeProperties(spec, n, nil)
	for i := 0; i < len(n.Content); i += 2 {
		k, value := n.Content[i], n.Content[i+1]
		if kr, valid := s.scalar(k, k.Column); valid && s.at(kr) {
			end, _ := authoringByteOffset(s.original, kr.End)
			return &authoringTarget{site: AuthoringSite{Kind: "include-key", YAMLPath: path, Range: r},
				node: k, key: k, args: n, includeSchema: spec, argumentColon: s.argumentSeparator(end),
				argumentRepair: s.repairAt == end && s.repairLength > 0}
		}
		property := properties[k.Value]
		if property == nil {
			continue
		}
		p := path + "/" + pointer(k.Value)
		if property.Type == "object" {
			if t := s.includeTarget(value, p, k, property); t != nil {
				return t
			}
		}
		if len(property.Enum) > 0 && value.Kind == yaml.ScalarNode && !dynamicName(value) {
			if vr, valid := s.scalar(value, k.Column); valid && s.at(vr) && s.keyLine(k) {
				return &authoringTarget{site: AuthoringSite{Kind: "include-value", YAMLPath: p, Range: vr},
					node: value, key: k, includeSchema: property}
			}
		}
	}
	// Empty inline mappings are already valid YAML. Replace only a whitespace-
	// only, single-line container; never swallow interior comments or anchors.
	if len(n.Content) == 0 && n.Style&yaml.FlowStyle != 0 && s.at(r) {
		a, _ := authoringByteOffset(s.original, r.Start)
		b, _ := authoringByteOffset(s.original, r.End)
		if strings.Trim(s.original[a+1:b-1], " \t") == "" {
			return &authoringTarget{site: AuthoringSite{Kind: "include-mapping", YAMLPath: path, Range: r},
				node: n, key: key, includeSchema: spec, includeMapping: true}
		}
	}
	return nil
}

func (s *authoringSource) includeMappingRange(n *yaml.Node) (Range, bool) {
	if n.Style&yaml.FlowStyle == 0 {
		return s.mapping(n)
	}
	start := s.nodeStart(n)
	depth, quote, comment := 0, byte(0), false
	for i := start; i < len(s.text); i++ {
		c := s.text[i]
		if comment {
			if c == '\n' {
				comment = false
			}
			continue
		}
		if quote != 0 {
			if quote == '"' && c == '\\' {
				i++
			} else if c == quote {
				if quote == '\'' && i+1 < len(s.text) && s.text[i+1] == '\'' {
					i++
				} else {
					quote = 0
				}
			}
			continue
		}
		switch c {
		case '#':
			comment = true
		case '\'', '"':
			quote = c
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				// Prove the lexical envelope belongs to this exact AST before
				// returning it, including plain scalars with quote characters.
				var doc yaml.Node
				if yaml.Unmarshal([]byte(s.text[start:i+1]), &doc) != nil || len(doc.Content) != 1 {
					return Range{}, false
				}
				b, _ := yaml.Marshal(doc.Content[0])
				// Exterior comments belong to the surrounding source, not the
				// flow container. They need not be present in the local parse.
				copy := *n
				copy.HeadComment, copy.LineComment, copy.FootComment = "", "", ""
				a, _ := yaml.Marshal(&copy)
				if !bytes.Equal(a, b) {
					return Range{}, false
				}
				return s.rangeBytes(start, i+1)
			}
		}
	}
	return Range{}, false
}

func includePlaceholder(spec *includeAuthoringSchema) string {
	if len(spec.Enum) == 1 {
		return yamlName(spec.Enum[0])
	}
	switch spec.Type {
	case "object":
		return "{}"
	case "array":
		return "[]"
	default:
		return `""`
	}
}

func completeInclude(s *authoringSource, t *authoringTarget, reply *AuthoringReply) {
	if t.site.Kind == "include-value" {
		for _, value := range t.includeSchema.Enum {
			// Empty expand means inheritance, not a useful named suggestion.
			if value == "" {
				continue
			}
			edit, ok := s.nameEdit(t, value)
			if !ok {
				reply.unavailable("invalid-source-range")
				return
			}
			reply.Items = append(reply.Items, AuthoringItem{ID: "include-value:" + value, Kind: "include-value", Name: value, Edit: edit})
		}
		return
	}
	if t.site.Kind == "include-key" {
		for name, property := range includeProperties(t.includeSchema, t.args, t.key) {
			if mapValue(t.args, name) != nil && name != t.key.Value {
				continue
			}
			edit, ok := s.nameEdit(t, name)
			if !ok {
				reply.unavailable("invalid-source-range")
				return
			}
			reply.Items = append(reply.Items, AuthoringItem{ID: "include-key:" + name, Kind: "include-key", Name: name,
				ValueType: property.Type, Description: property.Description, Edit: edit})
		}
		return
	}
	arms := t.includeSchema.OneOf
	if len(arms) == 0 {
		arms = []*includeAuthoringSchema{t.includeSchema}
	}
	for _, arm := range arms {
		keys := arm.Required
		if len(keys) == 0 {
			for name := range arm.Properties {
				keys = append(keys, name)
			}
			sort.Strings(keys)
		}
		if len(keys) == 0 {
			continue
		}
		parts := []string{}
		for _, key := range keys {
			parts = append(parts, yamlName(key)+": "+includePlaceholder(arm.Properties[key]))
		}
		text := "{" + strings.Join(parts, ", ") + "}"
		r := t.site.Range
		if !t.includeMapping {
			at, _ := authoringByteOffset(s.original, r.Start)
			if at > 0 && s.original[at-1] == ':' {
				text = " " + text
			}
			if at < len(s.original) && s.original[at] == '#' {
				text += " "
			}
		}
		reply.Items = append(reply.Items, AuthoringItem{ID: "include-mapping:" + keys[0], Kind: "include-mapping", Name: keys[0],
			Description: "Insert an include mapping. Empty values are authoring placeholders; fill the target explicitly.",
			Edit:        AuthoringEdit{Range: r, NewText: text}})
	}
}
