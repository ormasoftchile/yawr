package graphjson_test

import (
	"context"
	"encoding/json"
	"testing"

	parserPkg "github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/render/graphjson"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// nilDynamicLoader satisfies flowwalk.Loader but must never be called for
// dynamic include sites.
type nilDynamicLoader struct{ t *testing.T }

func (n *nilDynamicLoader) Load(_ context.Context, path string) (*parserPkg.ParsedRunbook, error) {
	n.t.Errorf("nilDynamicLoader.Load called for %q; dynamic includes must not trigger loads", path)
	return nil, nil
}

// TestRender_DynamicInclude_DataField verifies that a dynamic include node is
// rendered to JSON with the "dynamic": true field in its data object.
func TestRender_DynamicInclude_DataField(t *testing.T) {
	rb := &parserPkg.ParsedRunbook{
		Source: "/abs/parent.yaml",
		Runbook: &schema.Runbook{
			ID:   "parent",
			Name: "parent",
			Flow: []schema.FlowNode{
				{Step: &schema.Step{
					ID:   "dyn-step",
					Type: schema.StepTypeInclude,
					IncludeSpec: &schema.IncludeSpec{
						Include: schema.IncludeConfig{
							RunbookRef:  "${suggested_tsg_id}",
							ResolveFrom: schema.ResolveFromCatalog,
						},
					},
				}},
			},
		},
	}

	b := &graphdoc.Builder{Loader: &nilDynamicLoader{t: t}}
	doc, err := b.Build(context.Background(), rb)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	out, err := graphjson.Render(doc)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	var rendered graphjson.Document
	if err := json.Unmarshal(out, &rendered); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(rendered.Nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(rendered.Nodes))
	}
	n := rendered.Nodes[0]

	dynVal, exists := n.Data["dynamic"]
	if !exists {
		t.Error(`graphjson node data must contain "dynamic" key for dynamic include nodes`)
	} else if dynVal != true {
		t.Errorf(`graphjson node data["dynamic"] must be true, got %v`, dynVal)
	}
}

// TestRender_StaticInclude_NoDynamicField verifies that a static (non-dynamic)
// include node does NOT carry the "dynamic" field in its JSON data.
func TestRender_StaticInclude_NoDynamicField(t *testing.T) {
	rb := &parserPkg.ParsedRunbook{
		Source: "/abs/parent.yaml",
		Runbook: &schema.Runbook{
			ID:   "parent",
			Name: "parent",
			Flow: []schema.FlowNode{
				{Step: &schema.Step{
					ID:   "static-inc",
					Type: schema.StepTypeInclude,
					IncludeSpec: &schema.IncludeSpec{
						Include: schema.IncludeConfig{
							Runbook: "./child.yaml",
						},
					},
				}},
			},
		},
	}

	b := &graphdoc.Builder{Loader: &nilDynamicLoader{t: t}}
	doc, err := b.Build(context.Background(), rb)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	out, err := graphjson.Render(doc)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	var rendered graphjson.Document
	if err := json.Unmarshal(out, &rendered); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(rendered.Nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(rendered.Nodes))
	}
	n := rendered.Nodes[0]

	if _, exists := n.Data["dynamic"]; exists {
		t.Errorf(`static include node must NOT carry "dynamic" field in data, got %v`, n.Data["dynamic"])
	}
}
