package presentation

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

type authoringSource struct {
	original, text         string
	root                   *yaml.Node
	caret                  int
	repairAt, repairLength int
	incomplete             bool
	unsafe                 bool
	include                bool
	typed                  bool
}
type authoringTarget struct {
	site                       AuthoringSite
	node, key, tool, args, ref *yaml.Node
	mode                       ExpressionMode
	boolean, argumentColon     bool
	argumentRepair             bool
	includeSchema              *includeAuthoringSchema
	includeMapping             bool
	typedSchemaTarget          bool
}

func authoringDepthExceeded(n *yaml.Node, depth int) bool {
	if n == nil {
		return false
	}
	if depth > 128 {
		return true
	}
	for _, c := range n.Content {
		if authoringDepthExceeded(c, depth+1) {
			return true
		}
	}
	return false
}

func boundedSource(n *yaml.Node, depth int) bool {
	if n == nil {
		return true
	}
	if depth > 128 || n.Kind == yaml.AliasNode || n.Anchor != "" {
		return false
	}
	if n.Kind == yaml.MappingNode && !uniqueMappingKeys(n) {
		return false
	}
	for _, c := range n.Content {
		if !boundedSource(c, depth+1) {
			return false
		}
	}
	return true
}

// Unrelated value anchors are not ancestry. Do not expand aliases: global
// structural ambiguity is still rejected, and the selected path is checked
// separately before publishing any edits.
func authoringStructure(n *yaml.Node, depth int) bool {
	if n == nil {
		return true
	}
	if depth > 128 {
		return false
	}
	if n.Kind == yaml.MappingNode {
		copy := *n
		copy.Anchor = ""
		if !uniqueMappingKeys(&copy) {
			return false
		}
		for i := 0; i < len(n.Content); i += 2 {
			if n.Content[i].Anchor != "" {
				return false
			}
		}
	}
	for _, c := range n.Content {
		if !authoringStructure(c, depth+1) {
			return false
		}
	}
	return true
}

func (s *authoringSource) safeTarget(t *authoringTarget) bool {
	n := s.root
	for _, part := range strings.Split(strings.TrimPrefix(t.site.YAMLPath, "/"), "/") {
		if n == nil || n.Anchor != "" || n.Kind == yaml.AliasNode {
			return false
		}
		if n.Kind == yaml.MappingNode {
			if !uniqueMappingKeys(n) || !boundedSource(mapValue(n, "type"), 0) {
				return false
			}
			part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
			n = mapValue(n, part)
		} else if n.Kind == yaml.SequenceNode {
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(n.Content) {
				return false
			}
			n = n.Content[i]
		} else {
			return false
		}
	}
	return n != nil && boundedSource(n, 0) && boundedSource(t.tool, 0) && boundedSource(t.ref, 0)
}

func admittedAuthoringStructure(n *yaml.Node) bool {
	return authoringStructure(n, 0) && n.Anchor == "" &&
		boundedSource(mapValue(n, "toolRefs"), 0) && boundedSource(mapValue(n, "requires"), 0)
}

func parseAuthoringSource(text string, caret int, includes ...bool) (*authoringSource, string) {
	include := len(includes) > 0 && includes[0]
	s := &authoringSource{original: text, text: text, caret: caret, repairAt: -1, include: include}
	parse := func(t string) *yaml.Node {
		var doc yaml.Node
		if yaml.Unmarshal([]byte(t), &doc) != nil || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
			return nil
		}
		return doc.Content[0]
	}
	s.root = parse(text)
	if authoringDepthExceeded(s.root, 0) {
		return nil, "limit-exceeded"
	}
	lineStart := strings.LastIndexByte(text[:caret], '\n') + 1
	end := strings.IndexByte(text[caret:], '\n')
	if end < 0 {
		end = len(text)
	} else {
		end += caret
	}
	raw := strings.TrimSuffix(text[lineStart:end], "\r")
	trim := strings.TrimSpace(raw)
	// Only a bare block mapping key (or an empty insertion line) can be
	// repaired. The parsed ancestry, not indentation alone, admits the site.
	bare := !strings.ContainsAny(trim, ":#[]{}&*!|>'\"\\\t") && !strings.HasPrefix(trim, "-")
	if bare {
		at := lineStart + len(strings.TrimRight(raw, " "))
		insert := ": null"
		if trim == "" {
			at = caret
			insert = "__yawr_cursor__: null"
		}
		candidate := text[:at] + insert + text[at:]
		repaired := parse(candidate)
		if repaired == nil {
			if prefix, ok := authoringPrefix(candidate, caret); ok {
				repaired = parse(prefix)
			}
			if authoringDepthExceeded(repaired, 0) {
				return nil, "limit-exceeded"
			}
		}
		if repaired != nil && admittedAuthoringStructure(repaired) {
			probe := &authoringSource{original: text, text: candidate, root: repaired, caret: caret, repairAt: at, repairLength: len(insert), include: include}
			t := probe.target(context.Background(), "complete")
			if t != nil && !probe.unsafe && (t.site.Kind == "argument" || t.site.Kind == "include-key") && t.key != nil {
				return probe, ""
			}
		}
	}
	if s.root == nil {
		s.incomplete = true
		if !bare {
			if prefix, ok := authoringPrefix(text, caret); ok {
				s.root = parse(prefix)
			}
		}
	}
	if s.root == nil || !admittedAuthoringStructure(s.root) {
		return nil, "incomplete-source"
	}
	return s, ""
}

// A single prefix candidate may discard a later top-level flow entry. The
// resulting AST still has to prove all declarations and the current ancestry.
func authoringPrefix(text string, caret int) (string, bool) {
	offset, indent, cut := 0, -1, -1
	inFlow := false
	for _, line := range strings.SplitAfter(text, "\n") {
		if !inFlow {
			if strings.HasPrefix(line, "flow:") {
				inFlow = true
			}
			offset += len(line)
			continue
		}
		trim := strings.TrimLeft(line, " ")
		spaces := len(line) - len(trim)
		content := strings.TrimSpace(trim)
		// A discarded tail must stay inside flow. Otherwise a later root
		// declaration could change identity or duplicate an enclosing key.
		if content != "" && !strings.HasPrefix(content, "#") &&
			(spaces == 0 && !strings.HasPrefix(trim, "- ") || indent >= 0 && spaces < indent) {
			return "", false
		}
		if strings.HasPrefix(trim, "- ") {
			if indent < 0 {
				indent = spaces
			}
			if spaces == indent && offset > caret && cut < 0 {
				cut = offset
			}
		}
		offset += len(line)
	}
	if cut >= 0 {
		return text[:cut], true
	}
	return "", false
}
func (s *authoringSource) originalByte(i int) (int, bool) {
	if s.repairAt < 0 || i <= s.repairAt {
		return i, true
	}
	if i < s.repairAt+s.repairLength {
		return s.repairAt, false
	}
	return i - s.repairLength, true
}
func (s *authoringSource) rangeBytes(start, end int) (Range, bool) {
	a, ok := s.originalByte(start)
	b, ok2 := s.originalByte(end)
	if !ok || !ok2 || a < 0 || b < a || b > len(s.original) {
		return Range{}, false
	}
	return Range{Start: utf16Length(s.original[:a]), End: utf16Length(s.original[:b])}, true
}
func (s *authoringSource) nodeStart(n *yaml.Node) int {
	i, _ := bytePosition(s.text, n.Line, n.Column)
	return i
}
func (s *authoringSource) scalar(n *yaml.Node, column int) (Range, bool) {
	if n == nil {
		return Range{}, false
	}
	if n.Tag == "!!null" && n.Value == "" {
		i := s.nodeStart(n)
		if i <= s.caret && strings.Trim(s.text[i:s.caret], " \t") == "" {
			i = s.caret
		}
		return s.rangeBytes(i, i)
	}
	start := s.nodeStart(n)
	if s.repairAt == start && n.Value == "__yawr_cursor__" {
		return s.rangeBytes(start, start)
	}
	if n.Style == 0 && n.Value != "" && strings.HasPrefix(s.text[start:], n.Value) {
		end := start + len(n.Value)
		separator := end
		for separator < len(s.text) && (s.text[separator] == ' ' || s.text[separator] == '\t') {
			separator++
		}
		if separator < len(s.text) && s.text[separator] == ':' || n.Tag != "!!str" && (end == len(s.text) || strings.ContainsRune(" \t\r\n,}]", rune(s.text[end]))) {
			return s.rangeBytes(start, end)
		}
	}
	copy := *n
	copy.Tag = "!!str"
	r, ok := scalarRange(s.text, &copy, column)
	if !ok {
		return Range{}, false
	}
	start, _ = authoringByteOffset(s.text, r.Start)
	end, _ := authoringByteOffset(s.text, r.End)
	return s.rangeBytes(start, end)
}
func (s *authoringSource) mapping(n *yaml.Node) (Range, bool) {
	if n == nil {
		return Range{}, false
	}
	if n.Kind == yaml.ScalarNode {
		return s.scalar(n, n.Column)
	}
	start := s.nodeStart(n)
	if n.Style&yaml.FlowStyle != 0 {
		return Range{}, false
	}
	end := start
	var visit func(*yaml.Node)
	visit = func(c *yaml.Node) {
		if c.Kind == yaml.ScalarNode {
			copy := *c
			copy.Tag = "!!str"
			if r, ok := scalarRange(s.text, &copy, c.Column); ok {
				b, _ := authoringByteOffset(s.text, r.End)
				if b > end {
					end = b
				}
			} else if b := s.nodeStart(c); b > end {
				end = b
			}
		}
		for _, child := range c.Content {
			visit(child)
		}
	}
	visit(n)
	if end < len(s.text) {
		if nl := strings.IndexByte(s.text[end:], '\n'); nl >= 0 {
			end += nl + 1
		}
	}
	return s.rangeBytes(start, end)
}
func (s *authoringSource) at(r Range) bool {
	pos := utf16Length(s.original[:s.caret])
	return r.Start <= pos && pos <= r.End
}
func (s *authoringSource) keyLine(k *yaml.Node) bool {
	start := s.nodeStart(k)
	a, _ := s.originalByte(start)
	end := strings.IndexByte(s.original[a:], '\n')
	if end < 0 {
		end = len(s.original)
	} else {
		end += a
	}
	return a <= s.caret && s.caret <= end
}
func (s *authoringSource) target(ctx context.Context, operation string) *authoringTarget {
	var found *authoringTarget
	refs := mapValue(s.root, "toolRefs")
	if refs != nil && refs.Kind == yaml.SequenceNode {
		for i, ref := range refs.Content {
			if ctx.Err() != nil {
				return nil
			}
			for j := 0; j+1 < len(ref.Content); j += 2 {
				k, n := ref.Content[j], ref.Content[j+1]
				if k.Value == "name" {
					r, ok := s.scalar(n, k.Column)
					if ok && s.keyLine(k) && s.at(r) {
						found = &authoringTarget{site: AuthoringSite{Kind: "tool-reference", YAMLPath: fmtPath("/toolRefs", i) + "/name", Range: r}, node: n, key: k, ref: ref}
					}
				}
			}
		}
	}
	v := expressionSourceVisitor{ctx: ctx, text: s.text, typed: s.typed}
	if s.typed && operation == "complete" {
		set := func(n *yaml.Node, path, definition string) {
			if target := s.includeTarget(n, path, nil, typedAuthoringSchema(definition)); target != nil {
				target.typedSchemaTarget = true
				found = target
			}
		}
		eachSource(mapValue(s.root, "bindings"), "/bindings", func(n *yaml.Node, path string) { set(n, path, "Binding") })
		v.onStep = func(n *yaml.Node, path string) {
			set(n, path, "Step")
			eachSource(mapValue(n, "assign"), path+"/assign", func(write *yaml.Node, p string) { set(write, p, "Assignment") })
		}
	}
	if s.include && operation == "complete" {
		v.onInclude = func(n *yaml.Node, path string, key *yaml.Node) {
			if t := s.includeTarget(n, path, key, includeSchema()); t != nil {
				found = t
			}
		}
	}
	v.onAmbiguous = func(n *yaml.Node) {
		if r, ok := s.mapping(n); ok && s.at(r) {
			s.unsafe = true
		}
	}
	v.onTool = func(tool *yaml.Node, path string, toolKey *yaml.Node) {
		if ctx.Err() != nil {
			return
		}
		if !uniqueMappingKeys(tool) {
			return
		}
		args := mapValue(tool, "args")
		if operation == "required-arguments" {
			if r, ok := s.mapping(tool); ok && (s.at(r) || toolKey != nil && s.keyLine(toolKey)) {
				found = &authoringTarget{site: AuthoringSite{Kind: "argument", YAMLPath: path, Range: r}, tool: tool, args: args}
			}
		}
		for i := 0; i < len(tool.Content); i += 2 {
			k, n := tool.Content[i], tool.Content[i+1]
			if k.Value == "name" || k.Value == "action" {
				r, ok := s.scalar(n, k.Column)
				if ok && s.at(r) && s.keyLine(k) {
					kind := k.Value
					if kind == "name" {
						kind = "tool"
					}
					found = &authoringTarget{site: AuthoringSite{Kind: kind, YAMLPath: path + "/" + k.Value, Range: r}, node: n, key: k, tool: tool, args: args}
				}
			}
			if k.Value == "args" {
				r, ok := s.mapping(n)
				if !ok {
					if s.keyLine(k) {
						found = &authoringTarget{site: AuthoringSite{Kind: "argument", YAMLPath: path + "/args"}, tool: tool, args: n}
					}
					continue
				}
				if operation == "required-arguments" && (s.keyLine(k) || s.at(r)) {
					found = &authoringTarget{site: AuthoringSite{Kind: "argument", YAMLPath: path + "/args", Range: r}, tool: tool, args: n}
				}
				if n.Kind != yaml.MappingNode {
					continue
				}
				for j := 0; j < len(n.Content); j += 2 {
					key := n.Content[j]
					kr, valid := s.scalar(key, key.Column)
					if valid && s.at(kr) {
						a, _ := authoringByteOffset(s.original, kr.End)
						colon := s.argumentSeparator(a)
						found = &authoringTarget{site: AuthoringSite{Kind: "argument", YAMLPath: path + "/args", Range: r}, key: key, node: key, tool: tool, args: n, argumentColon: colon, argumentRepair: s.repairAt == a && s.repairLength > 0}
					}
				}
			}
		}
	}
	v.onScalar = func(n *yaml.Node, path string, column int, mode ExpressionMode, boolean bool) {
		if operation == "required-arguments" {
			return
		}
		r, ok := s.scalar(n, column)
		if ok && s.at(r) {
			// Identity scalar completion owns non-interpolated names.
			if found != nil && (found.site.Kind == "tool" || found.site.Kind == "action" || strings.HasPrefix(found.site.Kind, "include-")) && !strings.Contains(n.Value, "${") {
				return
			}
			found = &authoringTarget{site: AuthoringSite{Kind: "expression", YAMLPath: path, Range: r}, node: n, mode: mode, boolean: boolean}
		}
	}
	v.runbook(s.root)
	if found != nil && !s.safeTarget(found) {
		s.unsafe = true
		return nil
	}
	return found
}

func (s *authoringSource) argumentSeparator(at int) bool {
	// The AST proves this is a direct mapping key. Only YAML separation
	// may intervene before its original separator, never another token.
	for at < len(s.original) {
		switch s.original[at] {
		case ' ', '\t', '\r', '\n':
			at++
		case '#':
			for at < len(s.original) && s.original[at] != '\n' {
				at++
			}
		default:
			return s.original[at] == ':'
		}
	}
	return false
}

func fmtPath(path string, i int) string {
	// Paths are RFC6901 and sequence indices use decimal, independent of locale.
	return path + "/" + strconv.Itoa(i)
}

func yamlName(name string) string {
	n := yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name}
	if strings.ContainsAny(name, "\n\r\t") {
		n.Style = yaml.DoubleQuotedStyle
	}
	b, _ := yaml.Marshal(&n)
	return strings.TrimSuffix(string(b), "\n")
}
func (s *authoringSource) nameEdit(t *authoringTarget, name string) (AuthoringEdit, bool) {
	keyTarget := t.site.Kind == "argument" || t.site.Kind == "include-key"
	// Only the AST key owning the synthetic separator may acquire a colon.
	// A valid explicit key can have no separator: appending one there would
	// turn the scalar argument key into a mapping-valued key.
	if keyTarget && !t.argumentColon && !t.argumentRepair {
		return AuthoringEdit{}, false
	}
	r, ok := s.scalar(t.node, t.node.Column)
	if !ok {
		return AuthoringEdit{}, false
	}
	a, _ := authoringByteOffset(s.original, r.Start)
	b, _ := authoringByteOffset(s.original, r.End)
	if keyTarget && strings.ContainsAny(s.original[a:b], "\r\n") {
		return AuthoringEdit{}, false
	}
	text := yamlName(name)
	if b-a >= 2 && s.original[a] == '\'' && s.original[b-1] == '\'' {
		if keyTarget && !t.argumentColon {
			return AuthoringEdit{}, false
		}
		if strings.ContainsAny(name, "\r\n") {
			return AuthoringEdit{}, false
		}
		r.Start++
		r.End--
		text = strings.ReplaceAll(name, "'", "''")
	} else if b-a >= 2 && s.original[a] == '"' && s.original[b-1] == '"' {
		if keyTarget && !t.argumentColon {
			return AuthoringEdit{}, false
		}
		r.Start++
		r.End--
		text = quoteFragment(name)
	}
	if keyTarget && !t.argumentColon {
		text += ": "
	}
	if r.Start == r.End && !keyTarget {
		if a > 0 && s.original[a-1] == ':' {
			text = " " + text
		}
		if b < len(s.original) && s.original[b] == '#' {
			text += " "
		}
	}
	return AuthoringEdit{Range: r, NewText: text}, true
}

func quoteFragment(s string) string {
	// JSON string escapes are a subset of YAML double-quoted escapes.
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}

// scalarMap records decoded byte boundaries. -1 marks virtual folding bytes.
// Edits must fit in one contiguous physical run; envelopes may include escapes.
type scalarMap struct {
	offsets []int
	runs    []int
	quote   byte
}

func (s *authoringSource) decodeMap(t *authoringTarget) (scalarMap, bool) {
	a, _ := authoringByteOffset(s.original, t.site.Range.Start)
	b, _ := authoringByteOffset(s.original, t.site.Range.End)
	raw := s.original[a:b]
	value := t.node.Value
	m := scalarMap{offsets: make([]int, len(value)+1), runs: make([]int, len(value)+1)}
	for i := range m.offsets {
		m.offsets[i] = -1
	}
	if len(raw) == 0 {
		m.offsets[0] = a
		return m, true
	}
	pos := 0
	appendPart := func(decoded string, start, end, run int) bool {
		if pos+len(decoded) > len(value) || value[pos:pos+len(decoded)] != decoded {
			return false
		}
		m.offsets[pos] = a + start
		m.runs[pos] = run
		for i := 1; i < len(decoded); i++ {
			m.offsets[pos+i] = -1
			m.runs[pos+i] = run
		}
		pos += len(decoded)
		m.offsets[pos] = a + end
		m.runs[pos] = run
		return true
	}
	if raw[0] == '\'' || raw[0] == '"' {
		m.quote = raw[0]
		for i := 1; i < len(raw)-1; {
			start := i
			if raw[i] == '\r' || raw[i] == '\n' {
				return m, false
			}
			if m.quote == '\'' && i+1 < len(raw)-1 && raw[i:i+2] == "''" {
				i += 2
				if !appendPart("'", start, i, 0) {
					return m, false
				}
				continue
			}
			if m.quote == '"' && raw[i] == '\\' {
				i++
				if i >= len(raw)-1 {
					return m, false
				}
				length := 1
				switch raw[i] {
				case 'x':
					length = 3
				case 'u':
					length = 5
				case 'U':
					length = 9
				}
				i += length
				if i > len(raw)-1 {
					return m, false
				}
				var d string
				if yaml.Unmarshal([]byte("\""+raw[start:i]+"\""), &d) != nil || !appendPart(d, start, i, 0) {
					return m, false
				}
				continue
			}
			_, size := utf8.DecodeRuneInString(raw[i:])
			i += size
			if !appendPart(raw[start:i], start, i, 0) {
				return m, false
			}
		}
		if pos == 0 {
			m.offsets[0] = a + 1
		}
		return m, pos == len(value)
	}
	if raw[0] == '|' || raw[0] == '>' {
		nl := strings.IndexByte(raw, '\n')
		if nl < 0 {
			return m, false
		}
		body := nl + 1
		indent := -1
		lineNumber := 0
		for at := body; at < len(raw); {
			end := strings.IndexByte(raw[at:], '\n')
			if end < 0 {
				end = len(raw)
			} else {
				end += at
			}
			line := strings.TrimSuffix(raw[at:end], "\r")
			spaces := len(line) - len(strings.TrimLeft(line, " "))
			if strings.TrimSpace(line) != "" && indent < 0 {
				indent = spaces
			}
			if indent < 0 || len(line) < indent {
				line = ""
			} else {
				line = line[indent:]
			}
			if line != "" {
				// Locate only after YAML-generated whitespace. Never search past
				// decoded authored text, which could make repeated tokens ambiguous.
				for pos < len(value) && strings.ContainsRune(" \r\n\t", rune(value[pos])) && !strings.HasPrefix(value[pos:], line) {
					pos++
				}
				for i := 0; i < len(line); {
					_, size := utf8.DecodeRuneInString(line[i:])
					if !appendPart(line[i:i+size], at+indent+i, at+indent+i+size, lineNumber) {
						return m, false
					}
					i += size
				}
			}
			at = end + 1
			lineNumber++
		}
		return m, strings.TrimSpace(value[pos:]) == ""
	}
	if raw != value {
		return m, false
	}
	for i := 0; i <= len(value); i++ {
		if i == len(value) || utf8.RuneStart(value[i]) {
			m.offsets[i] = a + i
		}
	}
	return m, true
}
func (m scalarMap) mapped(start, end int, envelope bool) (int, int, bool) {
	if start < 0 || end < start || end >= len(m.offsets) || m.offsets[start] < 0 || m.offsets[end] < 0 {
		return 0, 0, false
	}
	if !envelope && m.runs[start] != m.runs[end] {
		return 0, 0, false
	}
	return m.offsets[start], m.offsets[end], true
}
func (m scalarMap) encode(text string) string {
	if m.quote == '\'' {
		return strings.ReplaceAll(text, "'", "''")
	}
	if m.quote == '"' {
		return quoteFragment(text)
	}
	return text
}
