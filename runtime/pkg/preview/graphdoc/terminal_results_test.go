package graphdoc_test

import (
	"context"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
)

func TestTerminalResultsGraphPreservesEndAndDeclarations(t *testing.T) {
	p, err := parser.New(platform.Real())
	if err != nil {
		t.Fatal(err)
	}
	rb, err := p.ParseBytes(context.Background(), []byte(`apiVersion: yawr.runbook/v1
id: terminal
name: Terminal
outputs:
  status: {type: string, value: escalated}
flow:
  - step:
      id: branch
      type: branch
      branches:
        - else: true
          steps:
            - step: {id: end, type: end, publish_results: true, outcome: {category: escalated, code: Exact_Code}}
`))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := (&graphdoc.Builder{}).Build(context.Background(), rb)
	if err != nil {
		t.Fatal(err)
	}
	if doc.SchemaVersion != "3" || doc.Frames[0].Invocation == nil || !doc.Frames[0].Invocation.Results {
		t.Fatal("terminal-only graph lost declarations")
	}
	found := false
	for _, node := range doc.Nodes {
		if node.Kind == "end" {
			found = true
			if !node.Details.PublishResults || node.Details.Category != "escalated" || node.Details.Code != "Exact_Code" {
				t.Fatal("end publication/outcome details changed")
			}
		}
	}
	if !found {
		t.Fatal("terminal end replaced by synthetic Results")
	}
}
