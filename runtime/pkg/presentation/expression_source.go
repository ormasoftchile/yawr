package presentation

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

type expressionSourceVisitor struct {
	typed          bool
	ctx            context.Context
	text           string
	regions        []ExpressionRegion
	tokens         int
	limit, invalid bool
	onScalar       func(*yaml.Node, string, int, ExpressionMode, bool)
	onTool         func(*yaml.Node, string, *yaml.Node)
	onStep         func(*yaml.Node, string)
	onInclude      func(*yaml.Node, string, *yaml.Node)
	onAmbiguous    func(*yaml.Node)
}

func sortExpressionRegions(regions []ExpressionRegion) {
	sort.Slice(regions, func(i, j int) bool { return regions[i].Range.Start < regions[j].Range.Start })
}

func (v *expressionSourceVisitor) check(n *yaml.Node, depth int) bool {
	if depth > MaxExpressionDepth || v.ctx.Err() != nil {
		v.limit = true
		return false
	}
	if n == nil {
		return true
	}
	if n.Kind == yaml.AliasNode {
		return true
	}
	if n.Kind == yaml.MappingNode && n.Anchor == "" && !uniqueMappingKeys(n) {
		return false
	}
	for _, c := range n.Content {
		if !v.check(c, depth+1) {
			return false
		}
	}
	return true
}

func (v *expressionSourceVisitor) scalar(n *yaml.Node, path string, column int, mode ExpressionMode, boolean bool) {
	if n == nil || n.Kind != yaml.ScalarNode || n.Tag != "!!str" && !(v.onScalar != nil && mode == ExpressionGXL && n.Tag == "!!null" && n.Value == "") || n.Anchor != "" && mode != ExpressionRegex || v.limit {
		return
	}
	if v.ctx.Err() != nil {
		v.limit = true
		return
	}
	if v.onScalar != nil {
		v.onScalar(n, path, column, mode, boolean)
		return
	}
	if utf16Length(n.Value) > MaxExpressionValueUnits {
		return
	}
	r, ok := expressionScalarRange(v.text, n, column, mode)
	if !ok {
		v.invalid = true
		return
	}
	value, ok := HighlightExpression(v.ctx, n.Value, mode, boolean)
	if !ok {
		v.limit = true
		return
	}
	v.tokens += len(value.Tokens)
	if len(v.regions) >= MaxEntries || v.tokens > MaxExpressionTokens {
		v.limit = true
		return
	}
	v.regions = append(v.regions, ExpressionRegion{ExpressionValue: value, YAMLPath: path, Range: r})
}
func (v *expressionSourceVisitor) field(n *yaml.Node, path, key string, mode ExpressionMode, boolean bool) {
	if !uniqueMappingKeys(n) {
		return
	}
	for i := 0; i < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			v.scalar(n.Content[i+1], path+"/"+pointer(key), n.Content[i].Column, mode, boolean)
			return
		}
	}
}
func (v *expressionSourceVisitor) gis(n *yaml.Node, path string, keys ...string) {
	for _, key := range keys {
		v.field(n, path, key, ExpressionGIS, false)
	}
}
func (v *expressionSourceVisitor) leaves(n *yaml.Node, path string, column int) {
	if n == nil || n.Anchor != "" || v.limit {
		return
	}
	switch n.Kind {
	case yaml.MappingNode:
		if !uniqueMappingKeys(n) {
			return
		}
		for i := 0; i < len(n.Content); i += 2 {
			v.leaves(n.Content[i+1], path+"/"+pointer(n.Content[i].Value), n.Content[i].Column)
		}
	case yaml.SequenceNode:
		for i, c := range n.Content {
			v.leaves(c, fmt.Sprintf("%s/%d", path, i), c.Column)
		}
	default:
		v.scalar(n, path, column, ExpressionGIS, false)
	}
}
func (v *expressionSourceVisitor) leafField(n *yaml.Node, path, key string) {
	if !uniqueMappingKeys(n) {
		return
	}
	c := mapValue(n, key)
	if c != nil {
		v.leaves(c, path+"/"+key, c.Column)
	}
}
func eachSource(n *yaml.Node, path string, fn func(*yaml.Node, string)) {
	if n != nil && n.Kind == yaml.SequenceNode && n.Anchor == "" {
		for i, c := range n.Content {
			fn(c, fmt.Sprintf("%s/%d", path, i))
		}
	}
}
func (v *expressionSourceVisitor) runbook(n *yaml.Node) {
	if v.typed {
		eachSource(mapValue(n, "bindings"), "/bindings", func(binding *yaml.Node, path string) {
			v.leafField(binding, path, "value")
		})
	}
	outputs := mapValue(n, "outputs")
	if uniqueMappingKeys(outputs) {
		for i := 0; i < len(outputs.Content); i += 2 {
			path := "/outputs/" + pointer(outputs.Content[i].Value)
			v.field(outputs.Content[i+1], path, "value_expr", ExpressionGXL, false)
			v.gis(outputs.Content[i+1], path, "value")
			if v.typed {
				v.leafField(outputs.Content[i+1], path, "value_tree")
			}
		}
	}
	v.flow(mapValue(n, "flow"), "/flow")
}
func (v *expressionSourceVisitor) flow(n *yaml.Node, path string) {
	eachSource(n, path, func(item *yaml.Node, p string) {
		if !uniqueMappingKeys(item) {
			return
		}
		count := 0
		for _, key := range []string{"step", "iterate", "parallel"} {
			if mapValue(item, key) != nil {
				count++
			}
		}
		if count != 1 {
			if count > 1 && v.onAmbiguous != nil {
				v.onAmbiguous(item)
			}
			return
		}
		if step := mapValue(item, "step"); step != nil {
			v.step(step, p+"/step")
		}
		if it := mapValue(item, "iterate"); it != nil {
			if !uniqueMappingKeys(it) {
				return
			}
			ip := p + "/iterate"
			v.gis(it, ip, "over")
			v.field(it, ip, "until", ExpressionGXL, true)
			v.leafField(it, ip, "collect")
			v.leafField(it, ip, "collect_values")
			v.flow(mapValue(it, "steps"), ip+"/steps")
		}
		if par := mapValue(item, "parallel"); par != nil {
			v.parallel(par, p+"/parallel")
		}
	})
}
func (v *expressionSourceVisitor) parallel(n *yaml.Node, path string) {
	if !uniqueMappingKeys(n) {
		return
	}
	eachSource(mapValue(n, "branches"), path+"/branches", func(b *yaml.Node, p string) {
		if uniqueMappingKeys(b) {
			v.flow(mapValue(b, "steps"), p+"/steps")
		}
	})
}
func (v *expressionSourceVisitor) options(n *yaml.Node, path string) {
	eachSource(n, path, func(o *yaml.Node, p string) { v.gis(o, p, "label", "hint") })
}
func (v *expressionSourceVisitor) step(n *yaml.Node, path string) {
	if !uniqueMappingKeys(n) {
		return
	}
	if v.onStep != nil {
		v.onStep(n, path)
	}
	v.gis(n, path, "title")
	v.field(n, path, "when", ExpressionGXL, true)
	kind := stringValue(mapValue(n, "type"))
	switch kind {
	case "assign":
		if v.typed {
			eachSource(mapValue(n, "assign"), path+"/assign", func(write *yaml.Node, p string) {
				v.leafField(write, p, "value")
			})
		}
	case "cli":
		v.gis(n, path, "command", "stdin", "workdir", "shell")
		for _, key := range []string{"args", "run", "env"} {
			v.leafField(n, path, key)
		}
	case "tool":
		t := mapValue(n, "tool")
		p := path + "/tool"
		if v.onTool != nil {
			var key *yaml.Node
			for i := 0; i < len(n.Content); i += 2 {
				if n.Content[i].Value == "tool" {
					key = n.Content[i]
					break
				}
			}
			v.onTool(t, p, key)
		}
		v.gis(t, p, "name", "action")
		v.leafField(t, p, "args")
	case "host_action":
		v.leafField(mapValue(n, "host_action"), path+"/host_action", "request")
	case "include":
		t := mapValue(n, "include")
		p := path + "/include"
		if v.onInclude != nil {
			for i := 0; i < len(n.Content); i += 2 {
				if n.Content[i].Value == "include" {
					v.onInclude(t, p, n.Content[i])
				}
			}
		}
		v.field(t, p, "when", ExpressionGXL, true)
		v.gis(t, p, "runbook_ref")
		v.leafField(t, p, "with")
	case "handoff":
		t := mapValue(n, "handoff")
		p := path + "/handoff"
		v.leafField(t, p, "with")
		v.leafField(t, p, "facts")
	case "choice":
		v.gis(n, path, "prompt", "default")
		v.options(mapValue(n, "options"), path+"/options")
	case "decision":
		v.gis(n, path, "prompt")
		v.options(mapValue(n, "routes"), path+"/routes")
	case "collector":
		v.gis(n, path, "prompt")
		eachSource(mapValue(n, "fields"), path+"/fields", func(f *yaml.Node, p string) {
			if !uniqueMappingKeys(f) {
				return
			}
			v.gis(f, p, "label", "hint", "default")
			v.field(f, p, "when", ExpressionGXL, true)
			v.options(mapValue(f, "options"), p+"/options")
		})
	case "branch":
		eachSource(mapValue(n, "branches"), path+"/branches", func(b *yaml.Node, p string) {
			if !uniqueMappingKeys(b) {
				return
			}
			v.field(b, p, "condition", ExpressionGXL, true)
			v.flow(mapValue(b, "steps"), p+"/steps")
		})
	case "parallel":
		v.parallel(n, path)
	case "compensate":
		c := mapValue(n, "compensate")
		if uniqueMappingKeys(c) {
			v.flow(mapValue(c, "steps"), path+"/compensate/steps")
		}
	case "assert":
		eachSource(mapValue(n, "assert"), path+"/assert", func(a *yaml.Node, p string) {
			v.gis(a, p, "subject")
			mode := ExpressionGIS
			if uniqueMappingKeys(a) && stringValue(mapValue(a, "type")) == "matches" {
				mode = ExpressionRegex
			}
			v.field(a, p, "expected", mode, false)
		})
	case "display":
		v.gis(mapValue(n, "display"), path+"/display", "content")
	case "wait_for_event":
		e := mapValue(n, "event")
		p := path + "/event"
		v.gis(e, p, "id")
		v.leafField(e, p, "filter")
	}
	if kind == "noop" || kind == "include" {
		v.leafField(n, path, "capture")
	}
}

// Only established regex sites admit scalar properties; aliases remain YAML.
// yaml.v3 positions a node at its first property, unlike the editor's scalar CST.
func expressionScalarRange(text string, n *yaml.Node, column int, mode ExpressionMode) (Range, bool) {
	if mode != ExpressionRegex {
		return scalarRange(text, n, column)
	}
	start, ok := bytePosition(text, n.Line, n.Column)
	if !ok {
		return Range{}, false
	}
	copy := *n
	copy.Anchor = ""
	for start < len(text) && (text[start] == '&' || text[start] == '!') {
		if strings.HasPrefix(text[start:], "!<") {
			end := strings.IndexByte(text[start:], '>')
			if end < 0 {
				return Range{}, false
			}
			start += end + 1
		} else {
			for start < len(text) && !strings.ContainsRune(" \t\r\n,[]{}", rune(text[start])) {
				start++
			}
		}
		for start < len(text) {
			if strings.ContainsRune(" \t\r\n", rune(text[start])) {
				start++
			} else if text[start] == '#' {
				for start < len(text) && text[start] != '\n' {
					start++
				}
			} else {
				break
			}
		}
	}
	copy.Line = strings.Count(text[:start], "\n") + 1
	line := strings.LastIndexByte(text[:start], '\n') + 1
	copy.Column = utf8.RuneCountInString(text[line:start]) + 1
	return scalarRange(text, &copy, column)
}
