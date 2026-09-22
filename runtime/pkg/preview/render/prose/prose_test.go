package prose_test

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	parserPkg "github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/render/prose"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/runstate"
)

func collectHealthPath() string {
	_, thisFile, _, _ := runtime.Caller(0)
	pkgDir := filepath.Dir(thisFile)
	return filepath.Join(pkgDir, "testdata", "examples", "collect-health", "collect-health.runbook.yaml")
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
	b := &graphdoc.Builder{Loader: &fileLoader{p: p}, Recurse: recurse}
	doc, err := b.Build(context.Background(), rb)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return doc
}

func TestRender_CollectHealth_Opaque(t *testing.T) {
	doc := buildDoc(t, false)
	got := prose.Render(doc)
	want := strings.TrimLeft(`
# collect-health

1. **Loop** over hosts as svc (`+"`check_loop`"+`)
    1. **Include** Check {{ .svc }} (`+"`check_host`"+`)
    2. _Record {{ .svc }} (`+"`accumulate`"+`)_
2. **Decide** `+"`show_result`"+`
    - If All checks passed:
        1. Go ahead (`+"`go_ahead`"+`)
    - If Checks failed:
        1. Check failures (`+"`no_go`"+`)
3. **End** Pre-install check complete (`+"`done`"+`)
`, "\n")
	if got != want {
		t.Errorf("opaque mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestRender_CollectHealth_Recurse(t *testing.T) {
	doc := buildDoc(t, true)
	got := prose.Render(doc)
	want := strings.TrimLeft(`
# collect-health

1. **Loop** over hosts as svc (`+"`check_loop`"+`)
    1. **Include** Check {{ .svc }} (`+"`check_host`"+`)
        1. **Tool** DNS lookup {{ .service_host }} (`+"`dns_lookup`"+`)
        2. **Tool** Ping {{ .service_host }} (`+"`ping_host`"+`)
        3. _Summarize {{ .service_host }} (`+"`summarize`"+`)_
    2. _Record {{ .svc }} (`+"`accumulate`"+`)_
2. **Decide** `+"`show_result`"+`
    - If All checks passed:
        1. Go ahead (`+"`go_ahead`"+`)
    - If Checks failed:
        1. Check failures (`+"`no_go`"+`)
3. **End** Pre-install check complete (`+"`done`"+`)
`, "\n")
	if got != want {
		t.Errorf("recurse mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestRender_NilOrEmpty(t *testing.T) {
	if got := prose.Render(nil); got != "" {
		t.Errorf("nil: got %q want empty", got)
	}
	empty := &graphdoc.Document{Runbook: graphdoc.RunbookRef{ID: "x"}}
	got := prose.Render(empty)
	if got != "# x\n" {
		t.Errorf("empty: got %q want %q", got, "# x\n")
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
			FrameID: "frame:root", Label: "External view did not open", Fallback: true,
		}},
	}
	got := prose.Render(doc)
	if !strings.Contains(got, "Otherwise — External view did not open:") {
		t.Fatalf("fallback arm missing Otherwise label:\n%s", got)
	}
	if strings.Contains(got, "If External view did not open:") {
		t.Fatalf("fallback arm rendered as a condition:\n%s", got)
	}
}

func TestRenderWithState_AnnotatesStatus(t *testing.T) {
	doc := buildDoc(t, false)
	st := runstate.New()
	st.Apply(engine.Event{Kind: "step/started", Sequence: 1, Payload: map[string]any{"step_id": "check_loop"}})
	st.Apply(engine.Event{Kind: "iterate/iteration_started", Sequence: 2, Payload: map[string]any{
		"step_id": "check_loop", "iteration_index": int64(2), "iteration_total": int64(3),
	}})
	st.Apply(engine.Event{Kind: "step/started", Sequence: 3, Payload: map[string]any{"step_id": "check_host"}})
	st.Apply(engine.Event{Kind: "step/completed", Sequence: 4, Payload: map[string]any{
		"step_id": "check_host", "duration_ms": int64(120),
	}})
	st.Apply(engine.Event{Kind: "step/failed", Sequence: 5, Payload: map[string]any{
		"step_id": "accumulate", "error": "disk full", "duration_ms": int64(7),
	}})

	got := prose.RenderWithState(doc, st)
	for _, sub := range []string{
		"▶ **Loop**", // running
		"iter 2/3",
		"✓ **Include**",
		"120ms",
		"✗ _Record",
		"error: disk full",
		"· **Decide**", // pending
	} {
		if !strings.Contains(got, sub) {
			t.Errorf("expected %q in:\n%s", sub, got)
		}
	}
}

func TestRender_RepeatedIncludedBranchesKeepQualifiedGroupsSeparate(t *testing.T) {
	doc := repeatedBranchDocument()
	got := prose.Render(doc)
	if count := strings.Count(got, "_Leaf (`leaf`)_"); count != 2 {
		t.Fatalf("repeated branch leaves = %d, want 2:\n%s", count, got)
	}
	for _, label := range []string{"If Primary arm:", "If Secondary arm:"} {
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
