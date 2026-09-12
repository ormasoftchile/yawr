package graphdoc_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/parser"
	parserPkg "github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func collectHealthPath() string {
	_, thisFile, _, _ := runtime.Caller(0)
	pkgDir := filepath.Dir(thisFile)
	return filepath.Join(pkgDir, "testdata", "examples", "collect-health", "collect-health.runbook.yaml")
}

// fileLoader satisfies flowwalk.Loader by reusing the real parser.
type fileLoader struct{ p parserPkg.Parser }

func (fl *fileLoader) Load(ctx context.Context, path string) (*parserPkg.ParsedRunbook, error) {
	return fl.p.Parse(ctx, path)
}

func parseCollectHealth(t *testing.T) (parserPkg.Parser, *parserPkg.ParsedRunbook) {
	t.Helper()
	p, err := parser.New(platform.Real())
	if err != nil {
		t.Fatalf("parser.New: %v", err)
	}
	rb, err := p.Parse(context.Background(), collectHealthPath())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return p, rb
}

// edgeLine renders an edge in a stable, comparable form.
func edgeLine(e graphdoc.Edge) string {
	if e.Label != "" {
		return fmt.Sprintf("%s --[%s:%s]--> %s", e.From, e.Kind, e.Label, e.To)
	}
	return fmt.Sprintf("%s --[%s]--> %s", e.From, e.Kind, e.To)
}

func sortedEdgeLines(doc *graphdoc.Document) []string {
	lines := make([]string, len(doc.Edges))
	for i, e := range doc.Edges {
		lines[i] = edgeLine(e)
	}
	sort.Strings(lines)
	return lines
}

func nodeByID(doc *graphdoc.Document, id string) *graphdoc.Node {
	for i := range doc.Nodes {
		if doc.Nodes[i].ID == id {
			return &doc.Nodes[i]
		}
	}
	return nil
}

func TestBuild_ToolNodeCarriesLogicalIdentity(t *testing.T) {
	dir := t.TempDir()
	runbookPath := filepath.Join(dir, "tool-identity.runbook.yaml")
	content := `apiVersion: yawr.runbook/v1
id: tool-identity
name: tool-identity
kind: reference
flow:
  - step:
      id: inspect
      type: tool
      tool:
        name: icm
        action: get-incident
        args:
          secret: must-not-enter-graph
`
	if err := os.WriteFile(runbookPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write runbook: %v", err)
	}
	p, err := parser.New(platform.Real())
	if err != nil {
		t.Fatalf("parser.New: %v", err)
	}
	runbook, err := p.Parse(context.Background(), runbookPath)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	doc, err := (&graphdoc.Builder{Loader: &fileLoader{p: p}}).Build(context.Background(), runbook)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	node := nodeByID(doc, "inspect")
	if node == nil || node.ToolName != "icm" || node.ToolAction != "get-incident" {
		t.Fatalf("tool identity = %#v", node)
	}
	if node.Details == nil || node.Details.Kind != "tool" || node.Details.Tool != "icm" || node.Details.Action != "get-incident" {
		t.Fatalf("tool details = %#v", node.Details)
	}
}

// TestBuild_CollectHealth_Opaque asserts the structural document for the
// collect-health runbook with includes left opaque (default).
func TestBuild_CollectHealth_Opaque(t *testing.T) {
	p, rb := parseCollectHealth(t)

	b := &graphdoc.Builder{Loader: &fileLoader{p: p}}
	doc, err := b.Build(context.Background(), rb)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Single (root) frame.
	if got := len(doc.Frames); got != 1 {
		t.Fatalf("frames: got %d want 1", got)
	}
	if doc.Frames[0].ID != "frame:root" || doc.Frames[0].Depth != 0 {
		t.Errorf("root frame unexpected: %+v", doc.Frames[0])
	}

	// Node IDs (pre-order). check-service children must NOT appear.
	wantNodes := []string{
		"check_loop", "check_host", "accumulate",
		"show_result", "go_ahead", "no_go", "done",
	}
	gotNodes := make([]string, len(doc.Nodes))
	for i, n := range doc.Nodes {
		gotNodes[i] = n.ID
	}
	if strings.Join(gotNodes, ",") != strings.Join(wantNodes, ",") {
		t.Errorf("nodes (in order):\n  got:  %v\n  want: %v", gotNodes, wantNodes)
	}

	// Groups: iterate-body + 2 branch arms.
	if got := len(doc.Groups); got != 3 {
		t.Errorf("groups: got %d want 3 (%v)", got, doc.Groups)
	}

	// Edges (sorted for stable comparison).
	wantEdges := []string{
		"check_host --[sequence]--> accumulate",
		"check_loop --[iterate-body]--> check_host",
		"check_loop --[sequence]--> show_result",
		"show_result --[branch-arm:All checks passed]--> go_ahead",
		"show_result --[branch-arm:Checks failed]--> no_go",
		"show_result --[sequence]--> done",
	}
	gotEdges := sortedEdgeLines(doc)
	if strings.Join(gotEdges, "\n") != strings.Join(wantEdges, "\n") {
		t.Errorf("edges:\n  got:\n    %s\n  want:\n    %s",
			strings.Join(gotEdges, "\n    "),
			strings.Join(wantEdges, "\n    "))
	}

	// Hash is stable across rebuilds and 64-hex.
	if len(doc.Hash) != 64 {
		t.Errorf("hash should be 64-hex, got %q", doc.Hash)
	}
	doc2, err := b.Build(context.Background(), rb)
	if err != nil {
		t.Fatalf("Build (2nd): %v", err)
	}
	if doc.Hash != doc2.Hash {
		t.Errorf("hash not stable: %s vs %s", doc.Hash, doc2.Hash)
	}
}

func TestBuild_BranchFallbackMetadata(t *testing.T) {
	rb := &parserPkg.ParsedRunbook{Source: "fallback.runbook.yaml", Runbook: &schema.Runbook{
		ID: "fallback",
		Flow: []schema.FlowNode{{Step: &schema.Step{
			ID: "branch", Type: schema.StepTypeBranch,
			BranchSpec: &schema.BranchSpec{Branches: []schema.BranchArm{
				{Condition: `status == "ok"`, Label: "Ready", Steps: []schema.FlowNode{{Step: &schema.Step{ID: "ready", Type: schema.StepTypeNoop}}}},
				{Else: true, Label: "Blocked", Steps: []schema.FlowNode{{Step: &schema.Step{ID: "fallback", Type: schema.StepTypeNoop}}}},
			}},
		}}},
	}}
	doc, err := (&graphdoc.Builder{}).Build(context.Background(), rb)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	fallbacks := 0
	for _, group := range doc.Groups {
		if group.Kind == graphdoc.GroupBranchArm && group.Fallback {
			fallbacks++
			if group.Label != "Blocked" {
				t.Errorf("fallback arm label = %q, want Blocked", group.Label)
			}
		}
	}
	if fallbacks != 1 {
		t.Fatalf("fallback branch-arm groups: got %d want 1", fallbacks)
	}
	foundFallbackEdge := false
	for _, edge := range doc.Edges {
		if edge.From == "branch" && edge.To == "fallback" {
			foundFallbackEdge = true
			if edge.Label != "Otherwise — Blocked" {
				t.Errorf("fallback edge label = %q, want Otherwise — Blocked", edge.Label)
			}
		}
	}
	if !foundFallbackEdge {
		t.Fatal("fallback edge not found")
	}
}

func TestBuild_EmptyNestedIncludePreservesQualifiedFrameID(t *testing.T) {
	dir := t.TempDir()
	rootPath := filepath.Join(dir, "root.runbook.yaml")
	childPath := filepath.Join(dir, "child.runbook.yaml")
	emptyPath := filepath.Join(dir, "empty.runbook.yaml")
	if err := os.WriteFile(rootPath, []byte("apiVersion: yawr.runbook/v1\nid: root\nname: Root\nkind: mitigation\nflow:\n  - step:\n      id: child\n      type: include\n      include: { runbook: child.runbook.yaml }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(childPath, []byte("apiVersion: yawr.runbook/v1\nid: child\nname: Child\nkind: composable\nflow:\n  - step:\n      id: empty\n      type: include\n      include: { runbook: empty.runbook.yaml }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(emptyPath, []byte("apiVersion: yawr.runbook/v1\nid: empty\nname: Empty\nkind: composable\nflow: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	parserImpl, err := parser.New(platform.Real())
	if err != nil {
		t.Fatal(err)
	}
	runbook, err := parserImpl.Parse(context.Background(), rootPath)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := (&graphdoc.Builder{Loader: &fileLoader{p: parserImpl}, Recurse: true}).Build(context.Background(), runbook)
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range doc.Groups {
		if group.Kind != graphdoc.GroupIncludeFrame || group.QualifiedParentNodeID == "" {
			continue
		}
		if group.QualifiedFrameID == "" || group.QualifiedFrameID == group.FrameID {
			t.Fatalf("empty include group lost qualified frame identity: %#v", group)
		}
		return
	}
	t.Fatal("missing include-frame group")
}

func TestBuild_RepeatedChildRunbookUsesCallPathQualifiedNodeIDs(t *testing.T) {
	dir := t.TempDir()
	childPath := filepath.Join(dir, "child.runbook.yaml")
	rootPath := filepath.Join(dir, "root.runbook.yaml")
	if err := os.WriteFile(childPath, []byte(`apiVersion: yawr.runbook/v1
id: child
name: child
flow:
  - step:
      id: get_incident
      type: noop
`), 0o600); err != nil {
		t.Fatalf("write child: %v", err)
	}
	if err := os.WriteFile(rootPath, []byte(`apiVersion: yawr.runbook/v1
id: root
name: root
flow:
  - step:
      id: inspect_primary_icm
      type: include
      include:
        runbook: child.runbook.yaml
  - step:
      id: inspect_secondary_icm
      type: include
      include:
        runbook: child.runbook.yaml
`), 0o600); err != nil {
		t.Fatalf("write root: %v", err)
	}
	p, err := parser.New(platform.Real())
	if err != nil {
		t.Fatalf("parser.New: %v", err)
	}
	root, err := p.Parse(context.Background(), rootPath)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	doc, err := (&graphdoc.Builder{Loader: &fileLoader{p: p}, Recurse: true}).Build(context.Background(), root)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	var primary, secondary *graphdoc.Node
	for i := range doc.Nodes {
		node := &doc.Nodes[i]
		switch node.QualifiedID {
		case "inspect_primary_icm/get_incident":
			primary = node
		case "inspect_secondary_icm/get_incident":
			secondary = node
		}
	}
	if primary == nil || secondary == nil {
		t.Fatalf("qualified children missing: nodes=%#v", doc.Nodes)
	}
	if primary.StepID != "get_incident" || secondary.StepID != "get_incident" {
		t.Fatalf("raw step IDs = %q/%q", primary.StepID, secondary.StepID)
	}
	if strings.Join(primary.CallPath, "/") != "inspect_primary_icm" ||
		strings.Join(secondary.CallPath, "/") != "inspect_secondary_icm" {
		t.Fatalf("call paths = %#v/%#v", primary.CallPath, secondary.CallPath)
	}
	if primary.FrameID == secondary.FrameID {
		t.Fatalf("repeated child frames collapsed to %q", primary.FrameID)
	}
}

// TestBuild_CollectHealth_Recurse asserts that with Recurse=true the
// included runbook's steps appear under a child frame attached to the
// include node.
func TestBuild_CollectHealth_Recurse(t *testing.T) {
	p, rb := parseCollectHealth(t)

	b := &graphdoc.Builder{Loader: &fileLoader{p: p}, Recurse: true}
	doc, err := b.Build(context.Background(), rb)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Two frames: root + the include's child frame.
	if got := len(doc.Frames); got != 2 {
		t.Fatalf("frames: got %d want 2 (%v)", got, doc.Frames)
	}
	child := doc.Frames[1]
	if child.ParentIncludeNodeID != "check_host" || child.Depth != 1 {
		t.Errorf("child frame unexpected: %+v", child)
	}

	// Pre-order nodes: include children appear between check_host and accumulate.
	wantNodes := []string{
		"check_loop", "check_host",
		"dns_lookup", "ping_host", "summarize",
		"accumulate",
		"show_result", "go_ahead", "no_go", "done",
	}
	gotNodes := make([]string, len(doc.Nodes))
	for i, n := range doc.Nodes {
		gotNodes[i] = n.ID
	}
	if strings.Join(gotNodes, ",") != strings.Join(wantNodes, ",") {
		t.Errorf("nodes (in order):\n  got:  %v\n  want: %v", gotNodes, wantNodes)
	}

	// dns_lookup / ping_host / summarize must live in the child frame.
	for _, id := range []string{"dns_lookup", "ping_host", "summarize"} {
		n := nodeByID(doc, id)
		if n == nil {
			t.Errorf("missing node %q", id)
			continue
		}
		if n.FrameID != child.ID {
			t.Errorf("%s.FrameID = %q want %q", id, n.FrameID, child.ID)
		}
	}

	wantEdges := []string{
		"check_host --[include]--> dns_lookup",
		"check_host --[sequence]--> accumulate",
		"check_loop --[iterate-body]--> check_host",
		"check_loop --[sequence]--> show_result",
		"dns_lookup --[sequence]--> ping_host",
		"ping_host --[sequence]--> summarize",
		"show_result --[branch-arm:All checks passed]--> go_ahead",
		"show_result --[branch-arm:Checks failed]--> no_go",
		"show_result --[sequence]--> done",
	}
	gotEdges := sortedEdgeLines(doc)
	if strings.Join(gotEdges, "\n") != strings.Join(wantEdges, "\n") {
		t.Errorf("edges:\n  got:\n    %s\n  want:\n    %s",
			strings.Join(gotEdges, "\n    "),
			strings.Join(wantEdges, "\n    "))
	}

	// Recurse hash differs from opaque.
	bo := &graphdoc.Builder{Loader: &fileLoader{p: p}, Recurse: false}
	opaque, err := bo.Build(context.Background(), rb)
	if err != nil {
		t.Fatalf("Build (opaque): %v", err)
	}
	if opaque.Hash == doc.Hash {
		t.Errorf("opaque and recurse hashes should differ")
	}
}

// TestBuild_NilRunbook errors cleanly.
func TestBuild_NilRunbook(t *testing.T) {
	b := &graphdoc.Builder{}
	if _, err := b.Build(context.Background(), nil); err == nil {
		t.Error("expected error for nil runbook")
	}
}
