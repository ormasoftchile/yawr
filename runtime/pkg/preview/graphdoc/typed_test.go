package graphdoc_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
)

func TestTypedGraphMetadataAndLegacyIdentity(t *testing.T) {
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
	legacy, err := p.ParseBytes(context.Background(), []byte("apiVersion: yawr.runbook/v1\nid: old\nname: Old\nflow:\n  - step: {id: n, type: noop}\n"))
	if err != nil {
		t.Fatal(err)
	}
	old, err := (&graphdoc.Builder{}).Build(context.Background(), legacy)
	if err != nil {
		t.Fatal(err)
	}
	if old.SchemaVersion != "1" || old.Frames[0].Invocation != nil {
		t.Fatal("historical graph gained new metadata")
	}
	body, _ := json.Marshal(old)
	if len(body) == 0 {
		t.Fatal("legacy graph failed serialization")
	}
}
