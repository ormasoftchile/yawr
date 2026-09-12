package asciigraph_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	parserPkg "github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/render/asciigraph"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/runstate"
)

func collectHealthPath() string {
	_, thisFile, _, _ := runtime.Caller(0)
	pkgDir := filepath.Dir(thisFile)
	return filepath.Join(pkgDir, "testdata", "examples", "collect-health", "collect-health.runbook.yaml")
}

func driChangeRequestPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	pkgDir := filepath.Dir(thisFile)
	path := filepath.Join(pkgDir, "testdata", "yawr-domain-dri", "pkg", "compiler", "testdata", "change-request.golden.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Skip("fixture missing: yawr-domain-dri/pkg/compiler/testdata/change-request.golden.yaml")
	}
	return path
}

type fileLoader struct{ p parserPkg.Parser }

func (fl *fileLoader) Load(ctx context.Context, path string) (*parserPkg.ParsedRunbook, error) {
	return fl.p.Parse(ctx, path)
}

func buildDoc(t *testing.T, recurse bool) *graphdoc.Document {
	t.Helper()
	p, err := parser.New(platform.Real())
	if err != nil {
		t.Fatalf("parser.New: %v", err)
	}
	rb, err := p.Parse(context.Background(), collectHealthPath())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	doc, err := (&graphdoc.Builder{Loader: &fileLoader{p: p}, Recurse: recurse}).Build(context.Background(), rb)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return doc
}

func TestRender_Tree(t *testing.T) {
	got := asciigraph.Render(buildDoc(t, false))
	for _, sub := range []string{
		"collect-health",
		"├─ ",
		"└─ ",
		"if All checks passed",
		"if Checks failed",
	} {
		if !strings.Contains(got, sub) {
			t.Errorf("expected %q in:\n%s", sub, got)
		}
	}
}

func TestRender_FallbackArmUsesOtherwise(t *testing.T) {
	doc := &graphdoc.Document{
		Runbook: graphdoc.RunbookRef{ID: "fallback"},
		Frames:  []graphdoc.Frame{{ID: "frame:root"}},
		Nodes: []graphdoc.Node{
			{ID: "branch", Kind: "branch", FrameID: "frame:root", Order: 0},
			{ID: "blocked", Kind: "display", FrameID: "frame:root", GroupID: "group:fallback", Order: 1},
		},
		Groups: []graphdoc.Group{{
			ID: "group:fallback", Kind: graphdoc.GroupBranchArm, ParentNodeID: "branch",
			FrameID: "frame:root", Label: "XTS view did not open", Fallback: true,
		}},
	}
	got := asciigraph.Render(doc)
	if !strings.Contains(got, "otherwise — XTS view did not open") {
		t.Fatalf("fallback arm missing otherwise label:\n%s", got)
	}
	if strings.Contains(got, "if XTS view did not open") {
		t.Fatalf("fallback arm rendered as a condition:\n%s", got)
	}
}

func TestRender_State(t *testing.T) {
	doc := buildDoc(t, false)
	st := runstate.New()
	st.Apply(engine.Event{Kind: "step/started", Sequence: 1, Payload: map[string]any{"step_id": "check_loop"}})
	st.Apply(engine.Event{Kind: "step/completed", Sequence: 2, Payload: map[string]any{
		"step_id": "check_loop", "duration_ms": int64(50),
	}})
	got := asciigraph.RenderWithState(doc, st)
	if !strings.Contains(got, "✓") {
		t.Errorf("expected completed glyph in output:\n%s", got)
	}
	if !strings.Contains(got, "50ms") {
		t.Errorf("expected duration in output:\n%s", got)
	}
}

func TestRender_RepeatedIncludedBranchesKeepQualifiedGroupsSeparate(t *testing.T) {
	doc := repeatedBranchDocument()
	got := asciigraph.Render(doc)
	if count := strings.Count(got, "Leaf [noop]"); count != 2 {
		t.Fatalf("repeated branch leaves = %d, want 2:\n%s", count, got)
	}
	for _, label := range []string{"if Primary arm", "if Secondary arm"} {
		if count := strings.Count(got, label); count != 1 {
			t.Fatalf("%q occurrences = %d, want 1:\n%s", label, count, got)
		}
	}
}

func repeatedBranchDocument() *graphdoc.Document {
	return &graphdoc.Document{
		Runbook: graphdoc.RunbookRef{ID: "root"},
		Frames:  []graphdoc.Frame{{ID: "frame:root"}},
		Nodes: []graphdoc.Node{
			{ID: "primary", Kind: "include", FrameID: "frame:root", Order: 0},
			{ID: "route", QualifiedID: "primary/route", Kind: "branch", FrameID: "frame:primary", GroupID: "group:primary:include-frame:0", Order: 1},
			{ID: "leaf", QualifiedID: "primary/route/leaf", Kind: "noop", Title: "Leaf", FrameID: "frame:primary", GroupID: "group:route:branch-arm:0", QualifiedGroupID: "group:primary/route:branch-arm:0", Order: 2},
			{ID: "secondary", Kind: "include", FrameID: "frame:root", Order: 3},
			{ID: "route", QualifiedID: "secondary/route", Kind: "branch", FrameID: "frame:secondary", GroupID: "group:secondary:include-frame:0", Order: 4},
			{ID: "leaf", QualifiedID: "secondary/route/leaf", Kind: "noop", Title: "Leaf", FrameID: "frame:secondary", GroupID: "group:route:branch-arm:0", QualifiedGroupID: "group:secondary/route:branch-arm:0", Order: 5},
		},
		Groups: []graphdoc.Group{
			{ID: "group:primary:include-frame:0", Kind: graphdoc.GroupIncludeFrame, ParentNodeID: "primary", FrameID: "frame:primary"},
			{ID: "group:route:branch-arm:0", QualifiedID: "group:primary/route:branch-arm:0", Kind: graphdoc.GroupBranchArm, ParentNodeID: "route", QualifiedParentNodeID: "primary/route", FrameID: "frame:primary", Label: "Primary arm"},
			{ID: "group:secondary:include-frame:0", Kind: graphdoc.GroupIncludeFrame, ParentNodeID: "secondary", FrameID: "frame:secondary"},
			{ID: "group:route:branch-arm:0", QualifiedID: "group:secondary/route:branch-arm:0", Kind: graphdoc.GroupBranchArm, ParentNodeID: "route", QualifiedParentNodeID: "secondary/route", FrameID: "frame:secondary", Label: "Secondary arm"},
		},
	}
}

// TestRender_Regions verifies kit-declared regions render as a labelled
// tree block wrapping their member nodes.
func TestRender_Regions(t *testing.T) {
	p, err := parser.New(platform.Real())
	if err != nil {
		t.Fatalf("parser.New: %v", err)
	}
	rb, err := p.Parse(context.Background(), driChangeRequestPath(t))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	doc, err := (&graphdoc.Builder{Loader: &fileLoader{p: p}}).Build(context.Background(), rb)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	got := asciigraph.Render(doc)
	for _, sub := range []string{
		"◇ ops.change-request: Deploy service",
		"ops.change-request.deploy.pre-approval",
		"ops.change-request.deploy.rollback-branch",
	} {
		if !strings.Contains(got, sub) {
			t.Errorf("expected %q in:\n%s", sub, got)
		}
	}
	// The region header must precede its first member.
	headIdx := strings.Index(got, "◇ ops.change-request")
	memIdx := strings.Index(got, "ops.change-request.deploy.pre-approval")
	if headIdx < 0 || memIdx < 0 || headIdx >= memIdx {
		t.Errorf("region header must precede members; output:\n%s", got)
	}
}
