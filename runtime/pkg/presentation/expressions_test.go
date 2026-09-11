package presentation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf16"

	"gopkg.in/yaml.v3"
)

func expressionRequest(t *testing.T, text string) Request {
	t.Helper()
	root, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "unsaved.runbook.yaml")
	return Request{SchemaVersion: ExpressionSchemaVersion, RequestID: "expressions", Context: Context{ProjectRoot: root, Generation: 3, PackageMapPath: filepath.Join(root, "nonexistent.package-map.yaml")}, Document: Buffer{URI: FileURI(path), Path: path, Version: 7, Text: text}, Overlays: []Buffer{}}
}

func TestExpressionCanonicalValues(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "expression-values.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Text     string
		Expected ExpressionValue
	}
	if err = json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		got, ok := HighlightExpression(context.Background(), c.Text, c.Expected.Mode, false)
		if !ok || !reflect.DeepEqual(got, c.Expected) {
			t.Fatalf("%q: got %#v, expected %#v", c.Text, got, c.Expected)
		}
	}
}

func TestExpressionLexicalToleranceAndUTF16(t *testing.T) {
	for _, text := range []string{
		"vars.count >= 2", `str.contains(vars.name, "🚀}") and true or null == false`,
		"vars?.[0]?.name + # comment 🚀\r\n 2", `vars.name + "unfinished`,
		`date.compare("a", "b")`, `list.order(vars.items, "name")`, "len(vars.x) > 0",
	} {
		value, ok := HighlightExpression(context.Background(), text, ExpressionGXL, false)
		if !ok || len(value.Tokens) == 0 {
			t.Fatal(text, value)
		}
		units := utf16.Encode([]rune(text))
		last := 0
		for _, token := range value.Tokens {
			if token.Start < last || token.End <= token.Start || token.End > len(units) {
				t.Fatal(text, token)
			}
			for _, pos := range []int{token.Start, token.End} {
				if pos > 0 && pos < len(units) && units[pos] >= 0xdc00 && units[pos] <= 0xdfff {
					t.Fatal("split surrogate", token)
				}
			}
			last = token.End
		}
	}
	value, _ := HighlightExpression(context.Background(), `🚀 ${vars.name + }`, ExpressionGIS, false)
	if value.Tokens[0].Start != 3 || value.Tokens[0].End != 5 || value.Tokens[len(value.Tokens)-1].Class != "interpolation" {
		t.Fatal(value)
	}
	wrapped, _ := HighlightExpression(context.Background(), " {{ vars.x }} ", ExpressionGXL, true)
	raw, _ := HighlightExpression(context.Background(), " {{ vars.x }} ", ExpressionGXL, false)
	if len(wrapped.Tokens) != 5 || len(raw.Tokens) != 0 {
		t.Fatal(wrapped, raw)
	}
}

func TestExpressionStructuralInventory(t *testing.T) {
	text := `apiVersion: yawr.runbook/v1
outputs:
  expr: {value_expr: 'vars.count >= 2'}
  template: {value: '${vars.name}'}
vars: {condition: '${vars.no}', when: vars.no}
inputs: {x: {default: '${vars.no}'}}
flow:
  - step:
      id: cli
      type: cli
      title: 'Hi ${vars.name}'
      when: vars.count >= 2
      command: '${vars.command}'
      args: ['${vars.arg}']
      run: {windows: '${vars.script}'}
      stdin: '${vars.in}'
      env: {KEY: '${vars.env}'}
      shell: '${vars.shell}'
      workdir: '${vars.dir}'
      capture: {ordinary: '${vars.no}'}
      subtitle: '${vars.no}'
  - step:
      type: tool
      tool:
        name: '${vars.tool}'
        action: '${vars.action}'
        args: {nested: [{a/b~c: '${vars.value}'}], condition: '${vars.yes}'}
  - step:
      type: include
      when: '{{ vars.ready }}'
      include:
        when: vars.ready
        runbook_ref: '${vars.book}'
        with: {x: '${vars.x}'}
        gate: {stop_if: ['${vars.no}']}
      capture: {value: '${vars.x}'}
      capture_defaults: {value: '${vars.no}'}
  - step:
      type: choice
      prompt: '${vars.prompt}'
      default: '${vars.default}'
      options: [{label: '${vars.label}', hint: '${vars.hint}', value: '${vars.no}'}]
  - step:
      type: decision
      prompt: '${vars.prompt}'
      routes: [{label: '${vars.label}', hint: '${vars.hint}', runbook: '${vars.no}'}]
  - step:
      type: collector
      prompt: '${vars.prompt}'
      fields:
        - name: a
          when: vars.ready
          label: '${vars.label}'
          hint: '${vars.hint}'
          default: '${vars.default}'
          value_expr: vars.no
          options: [{label: '${vars.label}', hint: '${vars.hint}', value: '${vars.no}'}]
        - name: b
          default: {condition: '${vars.no}'}
  - step:
      type: host_action
      host_action: {request: {data: [{condition: '${vars.value}'}]}}
  - step:
      type: handoff
      handoff: {with: {x: '${vars.x}'}, facts: {x: '${vars.x}'}, reason: {summary: '${vars.no}'}}
  - step:
      type: branch
      branches:
        - condition: vars.ready
          steps:
            - iterate:
                over: '${vars.items}'
                until: vars.done
                collect: {x: '${vars.x}'}
                collect_values: {x: ['${vars.x}']}
                steps:
                  - parallel:
                      branches:
                        - steps:
                            - step:
                                type: compensate
                                compensate:
                                  steps:
                                    - step: {type: noop, capture: {x: '${vars.x}'}}
  - step:
      type: assert
      assert: [{subject: '${vars.a}', expected: '${vars.b}'}]
  - step: {type: display, display: {content: '${vars.text}'}}
  - step: {type: wait_for_event, event: {id: '${vars.id}', filter: {x: '${vars.x}'}}}
`
	reply := ResolveExpressions(context.Background(), expressionRequest(t, text))
	if reply.Status != "resolved" {
		t.Fatalf("%+v", reply)
	}
	got := map[string]ExpressionMode{}
	for _, r := range reply.Regions {
		got[r.YAMLPath] = r.Mode
	}
	want := map[string]ExpressionMode{}
	gxl := func(paths ...string) {
		for _, p := range paths {
			want[p] = ExpressionGXL
		}
	}
	gis := func(paths ...string) {
		for _, p := range paths {
			want[p] = ExpressionGIS
		}
	}
	gxl("/outputs/expr/value_expr", "/flow/0/step/when", "/flow/2/step/when", "/flow/2/step/include/when", "/flow/5/step/fields/0/when", "/flow/8/step/branches/0/condition", "/flow/8/step/branches/0/steps/0/iterate/until")
	gis("/outputs/template/value")
	for _, s := range []string{"title", "command", "args/0", "run/windows", "stdin", "env/KEY", "shell", "workdir"} {
		gis("/flow/0/step/" + s)
	}
	for _, s := range []string{"name", "action", "args/nested/0/a~1b~0c", "args/condition"} {
		gis("/flow/1/step/tool/" + s)
	}
	gis("/flow/2/step/include/runbook_ref", "/flow/2/step/include/with/x", "/flow/2/step/capture/value")
	for _, s := range []string{"prompt", "default", "options/0/label", "options/0/hint"} {
		gis("/flow/3/step/" + s)
	}
	for _, s := range []string{"prompt", "routes/0/label", "routes/0/hint"} {
		gis("/flow/4/step/" + s)
	}
	for _, s := range []string{"prompt", "fields/0/label", "fields/0/hint", "fields/0/default", "fields/0/options/0/label", "fields/0/options/0/hint"} {
		gis("/flow/5/step/" + s)
	}
	gis("/flow/6/step/host_action/request/data/0/condition", "/flow/7/step/handoff/with/x", "/flow/7/step/handoff/facts/x")
	base := "/flow/8/step/branches/0/steps/0/iterate"
	gis(base+"/over", base+"/collect/x", base+"/collect_values/x/0", base+"/steps/0/parallel/branches/0/steps/0/step/compensate/steps/0/step/capture/x")
	gis("/flow/9/step/assert/0/subject", "/flow/9/step/assert/0/expected", "/flow/10/step/display/content", "/flow/11/step/event/id", "/flow/11/step/event/filter/x")
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %d expected %d", len(got), len(want))
		for p, m := range want {
			if got[p] != m {
				t.Errorf("missing %s %s", p, m)
			}
		}
		for p := range got {
			if _, ok := want[p]; !ok {
				t.Errorf("unexpected %s", p)
			}
		}
	}
}

func TestExpressionScalarOwnership(t *testing.T) {
	for _, eol := range []string{"\n", "\r\n"} {
		for _, scalar := range []string{`${vars.name}`, `'Hi ${vars.name} 🚀'`, `"Hi ${vars.name} \U0001F680"`, ">-\n        Hi ${vars.name}\n        🚀\n", "|+\n        Hi ${vars.name}\n        🚀\n\n", "|2-\n        ${vars.name}\n"} {
			scalar = strings.ReplaceAll(scalar, "\n        ", "\n          ")
			text := strings.ReplaceAll("flow:\n  - step:\n      type: display\n      display:\n        content: "+scalar+"\n", "\n", eol)
			reply := ResolveExpressions(context.Background(), expressionRequest(t, text))
			if reply.Status != "resolved" || len(reply.Regions) != 1 {
				t.Fatalf("%q: %+v", text, reply)
			}
			var doc yaml.Node
			if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
				t.Fatal(err)
			}
			value := mapValue(mapValue(mapValue(doc.Content[0], "flow").Content[0], "step"), "display")
			content := mapValue(value, "content")
			r := reply.Regions[0]
			if r.TextDigest != Digest([]byte(content.Value)) || r.TextLength != utf16Length(content.Value) || r.Range.End > utf16Length(text) {
				t.Fatal(r)
			}
		}
	}
}

func TestExpressionProtocolLimitsAndRecovery(t *testing.T) {
	req := expressionRequest(t, "apiVersion: yawr.runbook/v1\nflow:\n  - step: {type: noop, when: 'vars.x +'}\n")
	data, _ := json.Marshal(req)
	if _, err := DecodeExpressionRequest(strings.NewReader(string(data))); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeRequest(strings.NewReader(string(data))); err == nil {
		t.Fatal("old protocol accepted expression schema")
	}
	req.SchemaVersion = "expression-resolve/v2"
	if got := ResolveExpressions(context.Background(), req); got.Reason != "invalid-request" {
		t.Fatal(got)
	}
	req.SchemaVersion = ExpressionSchemaVersion
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := ResolveExpressions(ctx, req); got.Reason != "limit-exceeded" || len(got.Regions) != 0 {
		t.Fatal(got)
	}
	req.Document.Text += "  - step: {type: display, display: [\n"
	if got := ResolveExpressions(context.Background(), req); got.Status != "resolved" || len(got.Regions) != 1 {
		t.Fatal(got)
	}
	for _, text := range []string{
		"flow: [{step: {type: noop, when: x, when: y}}]",
	} {
		if got := ResolveExpressions(context.Background(), expressionRequest(t, text)); got.Reason != "incomplete-source" {
			t.Fatal(got)
		}
		aliases := "inputs: {x: &text {type: string}, y: *text}\nflow: [{step: {type: noop, when: vars.x}}, {step: &s {type: noop, when: vars.y}}, {step: *s}]"
		if got := ResolveExpressions(context.Background(), expressionRequest(t, aliases)); got.Status != "resolved" || len(got.Regions) != 1 {
			t.Fatal("unrelated aliases or ambiguous source sites", got)
		}
	}
	large := "flow:\n"
	for i := 0; i < MaxEntries+1; i++ {
		large += fmt.Sprintf("  - step: {type: noop, when: 'vars.x', id: n%d}\n", i)
	}
	if got := ResolveExpressions(context.Background(), expressionRequest(t, large)); got.Reason != "limit-exceeded" || len(got.Regions) != 0 {
		t.Fatal("region cap", got.Status, got.Reason, len(got.Regions))
	}
	large = "flow:\n"
	for i := 0; i < 5; i++ {
		large += "  - step: {type: noop, when: '" + strings.Repeat("+ ", 16000) + "'}\n"
	}
	if got := ResolveExpressions(context.Background(), expressionRequest(t, large)); got.Reason != "limit-exceeded" {
		t.Fatal("token cap", got.Status, got.Reason)
	}
	large = "flow: [{step: {type: noop, when: '" + strings.Repeat("x", MaxExpressionValueUnits+1) + "'}}]"
	if got := ResolveExpressions(context.Background(), expressionRequest(t, large)); got.Status != "resolved" || len(got.Regions) != 0 {
		t.Fatal("value cap", got.Status, got.Reason)
	}
	deep := "flow: [{step: {type: tool, tool: {args: {x: " + strings.Repeat("[", 130) + "'${vars.x}'" + strings.Repeat("]", 130) + "}}}}]"
	if got := ResolveExpressions(context.Background(), expressionRequest(t, deep)); got.Reason != "limit-exceeded" {
		t.Fatal("depth cap", got.Status, got.Reason)
	}
}

func TestExpressionClosedRequestAndMetadataIndependence(t *testing.T) {
	req := expressionRequest(t, "toolRefs: [{name: missing, path: missing.tool.yaml}]\nflow: [{step: {type: tool, tool: {name: missing, action: query, args: {query: '${vars.x}'}}}}]")
	tool := filepath.Join(req.Context.ProjectRoot, "missing.tool.yaml")
	req.Overlays = []Buffer{{URI: FileURI(tool), Path: tool, Version: 99, Text: "broken: ["}}
	reply := ResolveExpressions(context.Background(), req)
	if reply.Status != "resolved" || len(reply.Regions) != 3 {
		t.Fatal("unavailable tool metadata blocked expressions", reply)
	}
	raw, _ := json.Marshal(req)
	for _, bad := range []string{
		strings.Replace(string(raw), `"request_id":"expressions"`, `"request_id":"expressions","request_id":"duplicate"`, 1),
		strings.Replace(string(raw), `"generation":3`, `"generation":3,"unknown":true`, 1),
		strings.Replace(string(raw), `"overlays":[`, `"unknown":true,"overlays":[`, 1),
		string(raw) + " {}",
		strings.Repeat(" ", MaxBytes+1),
	} {
		if _, err := DecodeExpressionRequest(strings.NewReader(bad)); err == nil {
			t.Fatal("invalid request accepted")
		}
	}
	req.Overlays = make([]Buffer, 129)
	if got := ResolveExpressions(context.Background(), req); got.Reason != "invalid-request" {
		t.Fatal("overlay cap")
	}
}

func TestExpressionAnchoredAncestorsDoNotOwnScalars(t *testing.T) {
	text := `flow:
  - step:
      type: tool
      tool: &tool {name: literal, action: query, args: {text: '${vars.no}'}}
  - step:
      type: branch
      branches:
        - &arm
          condition: vars.no
          steps: [{step: {type: noop, when: vars.no}}]
  - step: {type: noop, when: vars.yes}
`
	got := ResolveExpressions(context.Background(), expressionRequest(t, text))
	if got.Status != "resolved" || len(got.Regions) != 1 || got.Regions[0].YAMLPath != "/flow/2/step/when" {
		t.Fatal(got)
	}
}
