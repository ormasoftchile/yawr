package parser

import (
	"context"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestParserTerminalResults(t *testing.T) {
	parser, err := New(platform.NewFakePlatform())
	if err != nil {
		t.Fatal(err)
	}
	source := `apiVersion: yawr.runbook/v1
id: terminal
name: Terminal Results
outputs:
  result: {type: object, value_tree: {ok: true}}
flow:
  - step:
      id: branch
      type: branch
      branches:
        - else: true
          steps:
            - step: {id: end, type: end, publish_results: true, outcome: {category: no_action, code: exact-Code}}
            - step: {id: unreachable, type: noop}
  - step: {id: unreachable_root, type: noop}
`
	parsed, err := parser.ParseBytes(context.Background(), []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	end := parsed.Runbook.Flow[0].Step.BranchSpec.Branches[0].Steps[0].Step.EndSpec
	if !end.PublishResults || end.Outcome.Code != "exact-Code" || !schema.HasResults(parsed.Runbook.Flow) {
		t.Fatalf("terminal declaration lost: %#v", end)
	}
	for name, invalid := range map[string]string{
		"string flag":    strings.Replace(source, "publish_results: true", `publish_results: "true"`, 1),
		"null flag":      strings.Replace(source, "publish_results: true", "publish_results: null", 1),
		"wrong step":     strings.Replace(source, "type: end", "type: noop", 1),
		"retry":          strings.Replace(source, "publish_results: true", "publish_results: true, retry: {max: 2}", 1),
		"delay":          strings.Replace(source, "publish_results: true", "publish_results: true, delay: 1s", 1),
		"continue":       strings.Replace(source, "publish_results: true", "publish_results: true, on_error: continue", 1),
		"bad output":     strings.Replace(source, "type: object", "type: unsupported", 1),
		"branch results": strings.Replace(source, "type: end, publish_results: true, outcome: {category: no_action, code: exact-Code}", "type: results", 1),
		"outcome syntax": strings.Replace(source, "category: no_action", "category: '${broken'", 1),
		"outcome clock":  strings.Replace(source, "code: exact-Code", "code: '${now()}'", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parser.ParseBytes(context.Background(), []byte(invalid)); err == nil {
				t.Fatal("invalid terminal Results accepted")
			}
		})
	}
	legacy := strings.Replace(source, "publish_results: true", "publish_results: false", 1)
	parsed, err = parser.ParseBytes(context.Background(), []byte(legacy))
	if err != nil || schema.HasResults(parsed.Runbook.Flow) {
		t.Fatalf("legacy end changed: %v", err)
	}
}
