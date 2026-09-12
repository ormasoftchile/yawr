package presentation

import (
	"fmt"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

func mapValue(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}
func stringValue(n *yaml.Node) string {
	if n != nil && n.Kind == yaml.ScalarNode && n.Tag == "!!str" {
		return n.Value
	}
	return ""
}
func uniqueNodes(n *yaml.Node) bool {
	if n == nil {
		return true
	}

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
	for _, c := range n.Content {
		if !uniqueNodes(c) {
			return false
		}
	}
	return true
}
func uniqueMappingKeys(n *yaml.Node) bool {
	if n == nil || n.Kind != yaml.MappingNode || n.Anchor != "" {
		return false
	}
	seen := map[string]bool{}
	for i := 0; i < len(n.Content); i += 2 {
		key := n.Content[i]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || seen[key.Value] {
			return false
		}
		seen[key.Value] = true
	}
	return true
}

func parseSource(text string) (*yaml.Node, bool) {
	var doc yaml.Node
	if yaml.Unmarshal([]byte(text), &doc) == nil && len(doc.Content) == 1 && doc.Content[0].Kind == yaml.MappingNode {
		return doc.Content[0], true
	}
	// Only complete top-level prefixes are recoverable. No recovery through
	// an identity block, or through an invalid toolRefs declaration.
	offset := 0
	for _, line := range strings.SplitAfter(text, "\n") {
		if strings.HasPrefix(line, "flow:") {
			var prefix yaml.Node
			if yaml.Unmarshal([]byte(text[:offset]), &prefix) == nil && len(prefix.Content) == 1 && uniqueNodes(prefix.Content[0]) {
				cuts := []int{}
				position := offset + len(line)
				indent := -1
				for _, bodyLine := range strings.SplitAfter(text[position:], "\n") {
					trimmed := strings.TrimLeft(bodyLine, " ")
					n := len(bodyLine) - len(trimmed)
					if strings.HasPrefix(trimmed, "- ") {
						if indent < 0 {
							indent = n
						}
						if n == indent {
							cuts = append(cuts, position)
						}
					}
					position += len(bodyLine)
				}
				for i := len(cuts) - 1; i >= 0; i-- {
					var partial yaml.Node
					if yaml.Unmarshal([]byte(text[:cuts[i]]), &partial) == nil && len(partial.Content) == 1 && uniqueNodes(partial.Content[0]) {
						return partial.Content[0], false
					}
				}
				return prefix.Content[0], false
			}
			break
		}
		offset += len(line)
	}
	return nil, false
}
func bytePosition(text string, line, column int) (int, bool) {
	if line < 1 || column < 1 {
		return 0, false
	}
	i := 0
	for l := 1; l < line; l++ {
		j := strings.IndexByte(text[i:], '\n')
		if j < 0 {
			return 0, false
		}
		i += j + 1
	}
	for c := 1; c < column; c++ {
		if i >= len(text) {
			return 0, false
		}
		_, n := utf8.DecodeRuneInString(text[i:])
		i += n
	}
	return i, i <= len(text)
}
func scalarRange(text string, n *yaml.Node, keyColumn int) (Range, bool) {
	r, ok := scalarRangeStyle(text, n, keyColumn, false)
	if ok {
		return r, true
	}
	return scalarRangeStyle(text, n, keyColumn, true)
}
func scalarRangeStyle(text string, n *yaml.Node, keyColumn int, flow bool) (Range, bool) {
	start, ok := bytePosition(text, n.Line, n.Column)
	if !ok || start >= len(text) || n.Kind != yaml.ScalarNode || n.Tag != "!!str" || n.Anchor != "" {
		return Range{}, false
	}
	end := start
	switch text[start] {
	case '\'', '"':
		quote := text[start]
		end++
		closed := false
		for end < len(text) {
			if quote == '"' && text[end] == '\\' {
				end++
				if end < len(text) {
					_, size := utf8.DecodeRuneInString(text[end:])
					end += size
				}
				continue
			}
			if text[end] == quote {
				end++
				if quote == '\'' && end < len(text) && text[end] == '\'' {
					end++
					continue
				}
				closed = true
				break
			}
			_, size := utf8.DecodeRuneInString(text[end:])
			end += size
		}
		if !closed {
			return Range{}, false
		}
	case '|', '>':
		j := strings.IndexByte(text[start:], '\n')
		if j < 0 {
			end = len(text)
			break
		}
		end = start + j + 1
		bodyStart := end
		contentIndent := -1
		keep := strings.Contains(strings.Fields(text[start : start+j])[0], "+")
		for end < len(text) {
			lineEnd := strings.IndexByte(text[end:], '\n')
			if lineEnd < 0 {
				lineEnd = len(text) - end
			} else {
				lineEnd++
			}
			line := text[end : end+lineEnd]
			trim := strings.TrimSpace(line)
			indent := len(line) - len(strings.TrimLeft(line, " "))
			if trim != "" && indent < keyColumn {
				break
			}
			if trim != "" && contentIndent < 0 {
				contentIndent = indent
			}
			end += lineEnd
		}
		// Clip/strip scalars do not own trailing empty lines at or below
		// their content indentation. Keep scalars do own those lines.
		if !keep {
			if contentIndent < 0 {
				contentIndent = 0
			}
			for end > bodyStart {
				last := end
				if last > bodyStart && text[last-1] == '\n' {
					last--
				}
				if last > bodyStart && text[last-1] == '\r' {
					last--
				}
				lineStart := strings.LastIndexByte(text[:last], '\n') + 1
				if lineStart <= bodyStart || strings.Trim(text[lineStart:last], " ") != "" || last-lineStart > contentIndent {
					break
				}
				end = lineStart
			}
		}
	default:
		for end < len(text) && text[end] != '\r' && text[end] != '\n' && (!flow || (text[end] != ',' && text[end] != ']' && text[end] != '}')) {
			if text[end] == '#' && (end == start || text[end-1] == ' ' || text[end-1] == '\t') {
				break
			}
			end++
		}
		for end > start && (text[end-1] == ' ' || text[end-1] == '\t') {
			end--
		}
		if n.Style == 0 && strings.Contains(n.Value, " ") {
			lineEnd := strings.IndexByte(text[end:], '\n')
			if lineEnd >= 0 {
				next := end + lineEnd + 1
				for next < len(text) {
					nl := strings.IndexByte(text[next:], '\n')
					finish := len(text)
					if nl >= 0 {
						finish = next + nl
					}
					line := strings.TrimSuffix(text[next:finish], "\r")
					trim := strings.TrimSpace(line)
					indent := len(line) - len(strings.TrimLeft(line, " "))
					if trim == "" {
						next = finish + 1
						continue
					}
					if indent < keyColumn {
						break
					}
					end = next + len(strings.TrimRight(line, " \t"))
					next = finish + 1
				}
			}
		}
	}
	if end <= start || !utf8.ValidString(text[start:end]) {
		return Range{}, false
	}
	var check yaml.Node
	candidate := text[start:end]
	block := text[start] == '|' || text[start] == '>'
	if block {
		candidate = strings.Repeat(" ", keyColumn-1) + "value: " + candidate
	}
	if yaml.Unmarshal([]byte(candidate), &check) != nil || len(check.Content) != 1 {
		return Range{}, false
	}
	value := check.Content[0]
	if block {
		value = mapValue(value, "value")
	}
	if value == nil || value.Kind != yaml.ScalarNode || value.Tag != "!!str" || value.Value != n.Value {
		return Range{}, false
	}
	return Range{Start: len(utf16.Encode([]rune(text[:start]))), End: len(utf16.Encode([]rune(text[:end])))}, true
}
func pointer(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1") }
func collectRegions(text string, root *yaml.Node, bindings []Binding) ([]Region, bool) {
	out := []Region{}
	invalidRange := false
	byName := map[string]Binding{}
	for _, b := range bindings {
		byName[b.Name] = b
	}
	var walk func(*yaml.Node, string)
	walk = func(n *yaml.Node, path string) {
		if n == nil {
			return
		}
		if n.Kind == yaml.MappingNode {
			if !uniqueMappingKeys(n) {
				return
			}
			if stringValue(mapValue(n, "type")) == "tool" {
				tool := mapValue(n, "tool")
				name := stringValue(mapValue(tool, "name"))
				action := stringValue(mapValue(tool, "action"))
				b, ok := byName[name]
				if ok && b.Status == "resolved" {
					for _, a := range b.Actions {
						if a.Name != action {
							continue
						}
						args := mapValue(tool, "args")
						if args != nil && args.Kind == yaml.MappingNode && uniqueNodes(args) {
							for i := 0; i < len(args.Content); i += 2 {
								key, value := args.Content[i], args.Content[i+1]
								for _, f := range a.Arguments {
									if f.Name != key.Value || f.ValueType != "string" || value.Tag != "!!str" || value.Kind != yaml.ScalarNode {
										continue
									}
									r, ok := scalarRange(text, value, key.Column)
									if !ok {
										invalidRange = true
										continue
									}
									out = append(out, Region{BindingID: b.ID, Action: action, Direction: "input", Field: f.Name, YAMLPath: path + "/tool/args/" + pointer(key.Value), Range: r, Status: f.Status, Reason: f.Reason})
								}
							}
						}
					}
				}
			}
			for i := 0; i < len(n.Content); i += 2 {
				walk(n.Content[i+1], path+"/"+pointer(n.Content[i].Value))
			}
		} else if n.Kind == yaml.SequenceNode {
			for i, c := range n.Content {
				walk(c, fmt.Sprintf("%s/%d", path, i))
			}
		}
	}
	walk(root, "")
	return out, invalidRange
}
