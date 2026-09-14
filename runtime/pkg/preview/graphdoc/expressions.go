package graphdoc

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/pkg/presentation"
)

func expressionPointer(key string) string {
	return strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
}

func expressionSafe(text string) bool {
	lower := strings.ToLower(text)
	for _, marker := range []string{"<redacted>", "[redacted]", "<truncated>", "[truncated]"} {
		if strings.Contains(lower, marker) {
			return false
		}
	}
	return true
}

// projectExpressions is called only while projecting an authored typed step.
// These destinations are the retained safe fields, not arbitrary graph data.
func (d *StepDetails) projectExpressions() {
	d.ProjectExpressions()
}

func (d *StepDetails) ProjectExpressions() {
	if d == nil {
		return
	}
	e := &presentation.ExpressionPresentation{Version: 1, GrammarVersion: presentation.ExpressionGrammarVersion, Values: []presentation.ExpressionDetailValue{}}
	tokens := 0
	overflow := false
	add := func(path, text string, mode presentation.ExpressionMode) {
		if overflow || text == "" || !expressionSafe(text) {
			return
		}
		value, ok := presentation.HighlightExpression(context.Background(), text, mode, mode == presentation.ExpressionGXL)
		if !ok || len(value.Tokens) == 0 {
			return
		}
		tokens += len(value.Tokens)
		if len(e.Values) >= presentation.MaxEntries || tokens > presentation.MaxExpressionTokens {
			overflow = true
			return
		}
		e.Values = append(e.Values, presentation.ExpressionDetailValue{ExpressionValue: value, Path: path})
	}
	gis := func(path, text string) { add(path, text, presentation.ExpressionGIS) }
	gxl := func(path, text string) { add(path, text, presentation.ExpressionGXL) }
	var leaves func(string, any, int)
	leaves = func(path string, value any, depth int) {
		if depth > presentation.MaxExpressionDepth {
			overflow = true
			return
		}
		switch v := value.(type) {
		case string:
			gis(path, v)
		case map[string]any:
			keys := make([]string, 0, len(v))
			for k := range v {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				leaves(path+"/"+expressionPointer(k), v[k], depth+1)
			}
		case map[string]string:
			keys := make([]string, 0, len(v))
			for k := range v {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				leaves(path+"/"+expressionPointer(k), v[k], depth+1)
			}
		case []any:
			for i, c := range v {
				leaves(fmt.Sprintf("%s/%d", path, i), c, depth+1)
			}
		case []string:
			for i, c := range v {
				leaves(fmt.Sprintf("%s/%d", path, i), c, depth+1)
			}
		}
	}
	named := func(path string, values []NamedDetailValue) {
		for i, v := range values {
			if !v.Redacted {
				leaves(fmt.Sprintf("%s/%d/value", path, i), v.Value, 0)
			}
		}
	}
	options := func(path string, values []OptionDetails) {
		for i, v := range values {
			p := fmt.Sprintf("%s/%d", path, i)
			gis(p+"/label", v.Label)
			gis(p+"/hint", v.Hint)
		}
	}
	if d.Common != nil {
		gxl("/common/when", d.Common.When)
		if d.Kind == "noop" || d.Kind == "include" {
			for i, c := range d.Common.Captures {
				gis(fmt.Sprintf("/common/captures/%d/source", i), c.Source)
			}
		}
	}
	switch d.Kind {
	case "end":
		if d.PublishResults {
			gis("/category", d.Category)
			gis("/code", d.Code)
		}
	case "assign":
		for i, write := range d.Assign {
			leaves(fmt.Sprintf("/assign/%d/value", i), write.Value, 0)
		}
	case "cli":
		gis("/command", d.Command)
		leaves("/args", d.Args, 0)
		leaves("/script", d.Script, 0)
		gis("/workdir", d.Workdir)
		gis("/shell", d.Shell)
	case "tool":
		gis("/tool", d.Tool)
		gis("/action", d.Action)
		named("/arguments", d.Arguments)
	case "host_action":
		named("/request", d.Request)
	case "include":
		named("/bindings", d.Bindings)
		if d.Dynamic {
			gis("/reference", d.Reference)
		}
	case "choice":
		gis("/prompt", d.Prompt)
		if value, ok := d.Default.(string); ok {
			gis("/default", value)
		}
		options("/options", d.Options)
	case "decision":
		gis("/prompt", d.Prompt)
		for i, r := range d.Routes {
			p := fmt.Sprintf("/routes/%d", i)
			gis(p+"/label", r.Label)
			gis(p+"/hint", r.Hint)
		}
	case "collector":
		gis("/prompt", d.Prompt)
		for i, f := range d.Fields {
			p := fmt.Sprintf("/fields/%d", i)
			gis(p+"/label", f.Label)
			gis(p+"/hint", f.Hint)
			gxl(p+"/when", f.When)
			if value, ok := f.Default.(string); ok {
				gis(p+"/default", value)
			}
			options(p+"/options", f.Options)
		}
	case "branch":
		for i, a := range d.Arms {
			gxl(fmt.Sprintf("/arms/%d/condition", i), a.Condition)
		}
	case "iterate":
		gis("/over", d.Over)
		gxl("/until", d.Until)
		named("/collect", d.Collect)
	case "assert":
		for i, a := range d.Assertions {
			p := fmt.Sprintf("/assertions/%d", i)
			gis(p+"/subject", a.Subject)
			if a.Type == "matches" {
				add(p+"/expected", a.Expected, presentation.ExpressionRegex)
			} else {
				gis(p+"/expected", a.Expected)
			}
		}
	case "display":
		gis("/content", d.Content)
	case "wait_for_event":
		gis("/event_id", d.EventID)
		named("/filter", d.Filter)
	}
	d.ExpressionPresentation = nil
	if overflow || len(e.Values) == 0 {
		return
	}
	data, err := json.Marshal(e)
	if err == nil && len(data) <= presentation.MaxBytes {
		d.ExpressionPresentation = e
	}
}

type plainStepDetails StepDetails

func validExpressionPresentation(e *presentation.ExpressionPresentation, details any) *presentation.ExpressionPresentation {
	if e == nil || e.Version != 1 || e.GrammarVersion != presentation.ExpressionGrammarVersion || len(e.Values) > presentation.MaxEntries {
		return nil
	}
	result := *e
	result.Values = []presentation.ExpressionDetailValue{}
	tokens := 0
	seen := map[string]bool{}
	for _, v := range e.Values {
		tokens += len(v.Tokens)
		if tokens > presentation.MaxExpressionTokens {
			return nil
		}
		text, ok := expressionDetailText(details, v.Path)
		if !ok || !expressionSafe(text) || seen[v.Path] {
			continue
		}
		if v.Mode == presentation.ExpressionRegex {
			parts := strings.Split(v.Path, "/")
			kind, _ := expressionDetailText(details, "/kind")
			if kind != "assert" || len(parts) != 4 || parts[1] != "assertions" || parts[3] != "expected" {
				continue
			}
			assertionType, _ := expressionDetailText(details, strings.TrimSuffix(v.Path, "/expected")+"/type")
			if assertionType != "matches" {
				continue
			}
		}
		expected, ok := presentation.HighlightExpression(context.Background(), text, v.Mode, v.Mode == presentation.ExpressionGXL)
		matches := func(expected presentation.ExpressionValue, ok bool) bool {
			if !ok || expected.TextDigest != v.TextDigest || expected.TextLength != v.TextLength || len(expected.Tokens) != len(v.Tokens) {
				return false
			}
			for i, token := range expected.Tokens {
				if token != v.Tokens[i] {
					return false
				}
			}
			return true
		}
		if !matches(expected, ok) {
			continue
		}
		seen[v.Path] = true
		result.Values = append(result.Values, v)
	}
	if len(result.Values) == 0 {
		return nil
	}
	data, err := json.Marshal(result)
	if err != nil || len(data) > presentation.MaxBytes {
		return nil
	}
	return &result
}

func expressionDetailText(value any, path string) (string, bool) {
	if path == "" || !strings.HasPrefix(path, "/") {
		return "", false
	}
	parts := strings.Split(path[1:], "/")
	if len(parts) > presentation.MaxExpressionDepth {
		return "", false
	}
	for index, raw := range parts {
		key := strings.ReplaceAll(strings.ReplaceAll(raw, "~1", "/"), "~0", "~")
		switch v := value.(type) {
		case map[string]any:
			if index == 2 {
				switch parts[0] {
				case "arguments", "bindings", "request", "filter", "collect":
					if redacted, _ := v["redacted"].(bool); redacted {
						return "", false
					}
				}
			}
			value = v[key]
		case []any:
			i, err := strconv.Atoi(key)
			if err != nil || i < 0 || i >= len(v) || strconv.Itoa(i) != key {
				return "", false
			}
			value = v[i]
		default:
			return "", false
		}
	}
	s, ok := value.(string)
	return s, ok
}

// PruneExpressionPresentation invalidates metadata after a later protection
// boundary changes a displayed value. It never reconstructs discarded text.
func (d *StepDetails) PruneExpressionPresentation() {
	if d == nil || d.ExpressionPresentation == nil {
		return
	}
	copy := plainStepDetails(*d)
	copy.ExpressionPresentation = nil
	data, err := json.Marshal(copy)
	if err != nil {
		d.ExpressionPresentation = nil
		return
	}
	var value any
	if json.Unmarshal(data, &value) != nil {
		d.ExpressionPresentation = nil
		return
	}
	d.ExpressionPresentation = validExpressionPresentation(d.ExpressionPresentation, value)
}

// ForExpressionRendering preserves the original bound document while dropping
// stale display spans. Hashing/persistence itself never excludes this metadata.
func (d *Document) ForExpressionRendering() *Document {
	copy := *d
	copy.Nodes = append([]Node(nil), d.Nodes...)
	changed := false
	safeDetails := func(details *StepDetails) *StepDetails {
		if details == nil || details.ExpressionPresentation == nil {
			return details
		}
		result := *details
		before, _ := json.Marshal(result.ExpressionPresentation)
		result.PruneExpressionPresentation()
		after, _ := json.Marshal(result.ExpressionPresentation)
		if string(before) != string(after) {
			changed = true
		}
		return &result
	}
	for i := range copy.Nodes {
		copy.Nodes[i].Details = safeDetails(copy.Nodes[i].Details)
	}
	if d.PresentationState != nil {
		state := *d.PresentationState
		state.Occurrences = append([]PresentationOccurrence(nil), state.Occurrences...)
		for i := range state.Occurrences {
			state.Occurrences[i].Details = safeDetails(state.Occurrences[i].Details)
		}
		copy.PresentationState = &state
	}
	if changed {
		if hash, err := copy.ContentHash(); err == nil {
			copy.Hash = hash
		}
	}
	return &copy
}
