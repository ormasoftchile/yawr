package graphdoc_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
)

func TestTypedGraphMetadataAndMinimalIdentity(t *testing.T) {
	p, err := parser.New(platform.Real())
	if err != nil {
		t.Fatal(err)
	}
	rb, err := p.ParseBytes(context.Background(), []byte(`apiVersion: yawr.runbook/v1
id: typed
name: Typed
bindings:
  - {name: value, type: any, mutable: true, value: null}
outputs:
  result: {type: object, value_tree: {value: '${value}'}}
flow:
  - step: {id: write, type: assign, assign: [{name: value, value: '${false}'}]}
  - step: {id: publish, type: results}
`))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := (&graphdoc.Builder{}).Build(context.Background(), rb)
	if err != nil {
		t.Fatal(err)
	}
	if doc.SchemaVersion != "3" || len(doc.Frames) != 1 || doc.Frames[0].Invocation == nil {
		t.Fatal("missing feature version/owner")
	}
	invocation := doc.Frames[0].Invocation
	if !invocation.Results || !invocation.Bindings[0].ValuePresent || invocation.Bindings[0].Value != nil || !invocation.Bindings[0].Mutable || !invocation.Outputs["result"].ValueTreePresent {
		t.Fatal("declaration or null presence lost")
	}
	if doc.Nodes[0].ID != "write" || doc.Nodes[1].ID != "publish" || doc.Nodes[1].Title != "Results" {
		t.Fatal("identity/title changed")
	}
	if doc.Nodes[0].Details.Role != "technical" || len(doc.Nodes[0].Details.Assign) != 1 || doc.Nodes[1].Details.Role != "operator" {
		t.Fatal("operation metadata missing")
	}
	if doc.Nodes[0].Details.ExpressionPresentation == nil {
		t.Fatal("typed tree expression descriptor missing")
	}
	first := doc.Hash
	doc2, err := (&graphdoc.Builder{}).Build(context.Background(), rb)
	if err != nil || doc2.Hash != first {
		t.Fatal("unstable authored graph hash")
	}
	minimal, err := p.ParseBytes(context.Background(), []byte("apiVersion: yawr.runbook/v1\nid: minimal\nname: Minimal\nflow:\n  - step: {id: n, type: noop}\n"))
	if err != nil {
		t.Fatal(err)
	}
	minimalDoc, err := (&graphdoc.Builder{}).Build(context.Background(), minimal)
	if err != nil {
		t.Fatal(err)
	}
	if minimalDoc.SchemaVersion != "1" || minimalDoc.Frames[0].Invocation != nil {
		t.Fatal("minimal graph gained typed metadata")
	}
	body, _ := json.Marshal(minimalDoc)
	if len(body) == 0 {
		t.Fatal("minimal graph failed serialization")
	}
}
