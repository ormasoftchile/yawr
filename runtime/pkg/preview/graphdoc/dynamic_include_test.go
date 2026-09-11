package graphdoc_test

import (
	"context"
	"strings"
	"testing"

	parserPkg "github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// nilLoader satisfies flowwalk.Loader by always returning an error. Used to
// confirm that dynamic include sites never trigger a load.
type nilLoader struct{ t *testing.T }

func (n *nilLoader) Load(_ context.Context, path string) (*parserPkg.ParsedRunbook, error) {
	n.t.Errorf("nilLoader.Load called for %q; dynamic includes must never trigger a load", path)
	return nil, nil
}

// dynamicIncludeRunbook builds a minimal ParsedRunbook containing a single
// dynamic include step (runbook_ref + resolve_from: catalog).
func dynamicIncludeRunbook() *parserPkg.ParsedRunbook {
	return &parserPkg.ParsedRunbook{
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
}

// TestBuild_DynamicInclude_NodeMarked verifies that a dynamic include step
// produces a Node with Dynamic=true and a title prefixed with ⟨dynamic⟩.
func TestBuild_DynamicInclude_NodeMarked(t *testing.T) {
	rb := dynamicIncludeRunbook()
	b := &graphdoc.Builder{Loader: &nilLoader{t: t}}
	doc, err := b.Build(context.Background(), rb)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(doc.Nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(doc.Nodes))
	}
	n := doc.Nodes[0]
	if n.ID != "dyn-step" {
		t.Errorf("node ID: got %q, want %q", n.ID, "dyn-step")
	}
	if !n.Dynamic {
		t.Error("Node.Dynamic must be true for dynamic include nodes")
	}
	if !strings.HasPrefix(n.Title, "⟨dynamic⟩") {
		t.Errorf("Node.Title must start with ⟨dynamic⟩, got %q", n.Title)
	}
	if !strings.Contains(n.Title, "${suggested_tsg_id}") {
		t.Errorf("Node.Title must contain the runbook_ref template, got %q", n.Title)
	}
}

// TestBuild_DynamicInclude_NestedInBranch verifies that a dynamic include
// inside a branch arm is marked Dynamic in the document, satisfying the
// requirement that dynamic markers must be unmistakable in nested containers.
func TestBuild_DynamicInclude_NestedInBranch(t *testing.T) {
	rb := &parserPkg.ParsedRunbook{
		Source: "/abs/parent.yaml",
		Runbook: &schema.Runbook{
			ID:   "parent",
			Name: "parent",
			Flow: []schema.FlowNode{
				{Step: &schema.Step{
					ID:   "branch0",
					Type: schema.StepTypeBranch,
					BranchSpec: &schema.BranchSpec{
						Branches: []schema.BranchArm{
							{
								Condition: "${x}",
								Steps: []schema.FlowNode{
									{Step: &schema.Step{
										ID:   "dyn-in-arm",
										Type: schema.StepTypeInclude,
										IncludeSpec: &schema.IncludeSpec{
											Include: schema.IncludeConfig{
												RunbookRef:  "${tsg_id}",
												ResolveFrom: schema.ResolveFromCatalog,
											},
										},
									}},
								},
							},
						},
					},
				}},
			},
		},
	}

	b := &graphdoc.Builder{Loader: &nilLoader{t: t}}
	doc, err := b.Build(context.Background(), rb)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	for i := range doc.Nodes {
		if doc.Nodes[i].ID == "dyn-in-arm" {
			if !doc.Nodes[i].Dynamic {
				t.Error("dynamic include inside branch arm must have Dynamic=true")
			}
			if !strings.HasPrefix(doc.Nodes[i].Title, "⟨dynamic⟩") {
				t.Errorf("title must start with ⟨dynamic⟩, got %q", doc.Nodes[i].Title)
			}
			return
		}
	}
	t.Error("node 'dyn-in-arm' not found in document")
}

// TestBuild_DynamicInclude_NestedInIterate verifies dynamic marker inside iterate body.
func TestBuild_DynamicInclude_NestedInIterate(t *testing.T) {
	rb := &parserPkg.ParsedRunbook{
		Source: "/abs/parent.yaml",
		Runbook: &schema.Runbook{
			ID:   "parent",
			Name: "parent",
			Flow: []schema.FlowNode{
				{Iterate: &schema.IterateNode{
					ID:   "iter0",
					Over: "${items}",
					As:   "item",
					Steps: []schema.FlowNode{
						{Step: &schema.Step{
							ID:   "dyn-in-iter",
							Type: schema.StepTypeInclude,
							IncludeSpec: &schema.IncludeSpec{
								Include: schema.IncludeConfig{
									RunbookRef:  "${item}",
									ResolveFrom: schema.ResolveFromCatalog,
								},
							},
						}},
					},
				}},
			},
		},
	}

	b := &graphdoc.Builder{Loader: &nilLoader{t: t}}
	doc, err := b.Build(context.Background(), rb)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	for i := range doc.Nodes {
		if doc.Nodes[i].ID == "dyn-in-iter" {
			if !doc.Nodes[i].Dynamic {
				t.Error("dynamic include inside iterate body must have Dynamic=true")
			}
			return
		}
	}
	t.Error("node 'dyn-in-iter' not found in document")
}

// TestBuild_DynamicInclude_NoChildFrame verifies that a dynamic include does
// not produce any child Frame or sub-Group (it is always a leaf node in preview).
func TestBuild_DynamicInclude_NoChildFrame(t *testing.T) {
	rb := dynamicIncludeRunbook()
	b := &graphdoc.Builder{Loader: &nilLoader{t: t}, Recurse: true}
	doc, err := b.Build(context.Background(), rb)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// Only root frame must exist.
	if len(doc.Frames) != 1 {
		t.Errorf("expected 1 frame (root only), got %d", len(doc.Frames))
	}
}
