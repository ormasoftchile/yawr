package parser

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
)

func TestParserTypedBindingsAndResultsAdmission(t *testing.T) {
	parser, err := New(platform.NewFakePlatform())
	if err != nil {
		t.Fatal(err)
	}
	source := `apiVersion: yawr.runbook/v1
id: bindings
name: Bindings
inputs:
  seed: {type: string}
bindings:
  - {name: status, type: string, mutable: true, value: '${seed}'}
  - {name: native, type: any, value: null}
  - {name: rows, type: array, mutable: true, value: []}
outputs:
  result: {type: any, value_tree: null}
flow:
  - step:
      id: update
      type: assign
      assign:
        - {name: status, value: observed}
  - step: {id: publish, type: results}
`
	parsed, err := parser.ParseBytes(context.Background(), []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.Runbook.Bindings[1].ValuePresent || parsed.Runbook.Bindings[1].Value != nil ||
		!parsed.Runbook.Outputs["result"].ValueTreePresent {
		t.Fatal("explicit null presence lost")
	}
	data, err := json.Marshal(parsed.Runbook.Bindings)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"value_present":true`) {
		t.Fatalf("snapshot presence lost: %s", data)
	}
	for name, invalid := range map[string]string{
		"missing value":  strings.Replace(source, ", value: null", "", 1),
		"wrong type":     strings.Replace(source, "type: any", "type: nonsense", 1),
		"forward":        strings.Replace(source, "${seed}", "${rows}", 1),
		"self":           strings.Replace(source, "${seed}", "${status}", 1),
		"undeclared":     strings.Replace(source, "${seed}", "${unknown}", 1),
		"clock":          strings.Replace(source, "${seed}", "${now()}", 1),
		"immutable":      strings.Replace(source, "mutable: true", "mutable: false", 1),
		"absent assign":  strings.Replace(source, "name: status, value: observed", "name: missing, value: observed", 1),
		"dual output":    strings.Replace(source, "value_tree: null", "value_tree: null, value_expr: native", 1),
		"delay":          strings.Replace(source, "id: publish, type: results", "id: publish, type: results, delay: 1s", 1),
		"payload":        strings.Replace(source, "id: publish, type: results", "id: publish, type: results, command: forbidden", 1),
		"following work": source + "  - step: {id: later, type: noop}\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parser.ParseBytes(context.Background(), []byte(invalid)); err == nil {
				t.Fatalf("invalid declaration accepted: %s", invalid)
			}
		})
	}
}

func TestParserTypedCollectionAndOutputExpressions(t *testing.T) {
	parser, err := New(platform.NewFakePlatform())
	if err != nil {
		t.Fatal(err)
	}
	source := `apiVersion: yawr.runbook/v1
id: typed
name: Typed values
outputs:
  records: {type: array, value_expr: records}
flow:
  - iterate:
      id: loop
      over: items
      collect: {labels: '${item.name}'}
      collect_values:
        records: {configuration: '${item}', rows: '${rows}', active: true}
      steps:
        - step: {id: noop, type: noop}
`
	parsed, err := parser.ParseBytes(context.Background(), []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Runbook.Outputs["records"].ValueExpr != "records" ||
		len(parsed.Runbook.Flow[0].Iterate.CollectValues) != 1 {
		t.Fatalf("typed declarations lost: %#v", parsed.Runbook)
	}
	for _, invalid := range []string{
		strings.Replace(source, "value_expr: records", `value: literal, value_expr: records`, 1),
		strings.Replace(source, "value_expr: records", `value_expr: ""`, 1),
		strings.Replace(source, "value_expr: records", `value_expr: "records +"`, 1),
		strings.Replace(source, "collect: {labels:", "collect: {records:", 1),
	} {
		if _, err := parser.ParseBytes(context.Background(), []byte(invalid)); err == nil {
			t.Fatalf("invalid typed declaration accepted:\n%s", invalid)
		}
	}
}
