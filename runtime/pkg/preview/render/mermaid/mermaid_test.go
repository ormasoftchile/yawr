package mermaid_test

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
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/render/mermaid"
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
	got := mermaid.Render(buildDoc(t, false))
	want := strings.TrimLeft(`
flowchart TD
  check_loop[/"over hosts as svc"/]
  check_host[["Check {{ .svc }}"]]
  accumulate>"Record {{ .svc }}"]
  show_result{"show_result"}
  go_ahead>"Go ahead"]
  no_go>"Check failures"]
  done((("Pre-install check complete")))
  check_loop -. body .-> check_host
  check_host --> accumulate
  check_loop --> show_result
  show_result -->|All checks passed| go_ahead
  show_result -->|Checks failed| no_go
  show_result --> done
`, "\n")
	if got != want {
		t.Errorf("opaque mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestRender_CollectHealth_Recurse(t *testing.T) {
	got := mermaid.Render(buildDoc(t, true))
	want := strings.TrimLeft(`
flowchart TD
  check_loop[/"over hosts as svc"/]
  check_host[["Check {{ .svc }}"]]
  accumulate>"Record {{ .svc }}"]
  show_result{"show_result"}
  go_ahead>"Go ahead"]
  no_go>"Check failures"]
  done((("Pre-install check complete")))
  subgraph frame_check_host [included: check-service]
    dns_lookup["DNS lookup {{ .service_host }}"]
    ping_host["Ping {{ .service_host }}"]
    summarize>"Summarize {{ .service_host }}"]
  end
  check_loop -. body .-> check_host
  check_host ==> dns_lookup
  dns_lookup --> ping_host
  ping_host --> summarize
  check_host --> accumulate
  check_loop --> show_result
  show_result -->|All checks passed| go_ahead
  show_result -->|Checks failed| no_go
  show_result --> done
`, "\n")
	if got != want {
		t.Errorf("recurse mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestRender_NilDoc(t *testing.T) {
	if got := mermaid.Render(nil); got != "flowchart TD\n" {
		t.Errorf("nil doc: got %q", got)
	}
}

func TestRenderWithState_EmitsStatusClasses(t *testing.T) {
	doc := buildDoc(t, false)
	st := runstate.New()
	st.Apply(engine.Event{Kind: "step/started", Sequence: 1, Payload: map[string]any{"step_id": "check_host"}})
	st.Apply(engine.Event{Kind: "step/completed", Sequence: 2, Payload: map[string]any{"step_id": "check_host"}})
	st.Apply(engine.Event{Kind: "step/started", Sequence: 3, Payload: map[string]any{"step_id": "accumulate"}})
	st.Apply(engine.Event{Kind: "step/failed", Sequence: 4, Payload: map[string]any{"step_id": "accumulate", "error": "x"}})

	got := mermaid.RenderWithState(doc, st)
	for _, sub := range []string{
		"class check_host status_completed",
		"class accumulate status_failed",
		"class show_result status_pending",
		"classDef status_running",
		"classDef status_failed",
	} {
		if !strings.Contains(got, sub) {
			t.Errorf("expected %q in output:\n%s", sub, got)
		}
	}
}

func TestRender_RepeatedIncludedStepIDsUseQualifiedIdentity(t *testing.T) {
	doc := &graphdoc.Document{
		Frames: []graphdoc.Frame{
			{ID: "frame:root", RunbookID: "root"},
			{ID: "frame:primary", QualifiedID: "frame:inspect_primary", RunbookID: "child", Depth: 1},
			{ID: "frame:secondary", QualifiedID: "frame:inspect_secondary", RunbookID: "child", Depth: 1},
		},
		Nodes: []graphdoc.Node{
			{ID: "inspect_primary", Kind: "include", FrameID: "frame:root"},
			{ID: "inspect_secondary", Kind: "include", FrameID: "frame:root"},
			{ID: "get_incident", QualifiedID: "inspect_primary/get_incident", Kind: "tool", FrameID: "frame:primary", QualifiedFrameID: "frame:inspect_primary"},
			{ID: "get_incident", QualifiedID: "inspect_secondary/get_incident", Kind: "tool", FrameID: "frame:secondary", QualifiedFrameID: "frame:inspect_secondary"},
		},
		Edges: []graphdoc.Edge{
			{From: "inspect_primary", To: "get_incident", QualifiedTo: "inspect_primary/get_incident", Kind: graphdoc.EdgeInclude},
			{From: "inspect_secondary", To: "get_incident", QualifiedTo: "inspect_secondary/get_incident", Kind: graphdoc.EdgeInclude},
		},
	}

	got := mermaid.Render(doc)
	for _, want := range []string{
		"inspect_primary_get_incident[\"get_incident\"]",
		"inspect_secondary_get_incident[\"get_incident\"]",
		"inspect_primary ==> inspect_primary_get_incident",
		"inspect_secondary ==> inspect_secondary_get_incident",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in output:\n%s", want, got)
		}
	}
}

func TestRender_SanitizedIdentifiersRemainCollisionFree(t *testing.T) {
	doc := &graphdoc.Document{
		Frames: []graphdoc.Frame{
			{ID: "frame:root", RunbookID: "root"},
			{ID: "frame:inspect-primary", RunbookID: "child-a", Depth: 1},
			{ID: "frame:inspect_primary", RunbookID: "child-b", Depth: 1},
		},
		Nodes: []graphdoc.Node{
			{ID: "inspect-primary", Kind: "include", FrameID: "frame:root"},
			{ID: "inspect_primary", Kind: "include", FrameID: "frame:root"},
			{ID: "leaf", QualifiedID: "inspect-primary/leaf", Kind: "tool", FrameID: "frame:inspect-primary"},
			{ID: "leaf", QualifiedID: "inspect_primary/leaf", Kind: "tool", FrameID: "frame:inspect_primary"},
		},
		Edges: []graphdoc.Edge{
			{From: "inspect-primary", To: "leaf", QualifiedTo: "inspect-primary/leaf", Kind: graphdoc.EdgeInclude},
			{From: "inspect_primary", To: "leaf", QualifiedTo: "inspect_primary/leaf", Kind: graphdoc.EdgeInclude},
		},
	}
	state := runstate.New()
	got := mermaid.RenderWithState(doc, state)
	for _, want := range []string{
		`inspect_primary[["inspect-primary"]]`,
		`inspect_primary_2[["inspect_primary"]]`,
		`inspect_primary_leaf["leaf"]`,
		`inspect_primary_leaf_2["leaf"]`,
		`subgraph frame_inspect_primary [included: child-a]`,
		`subgraph frame_inspect_primary_2 [included: child-b]`,
		`inspect_primary ==> inspect_primary_leaf`,
		`inspect_primary_2 ==> inspect_primary_leaf_2`,
		`class inspect_primary_leaf status_pending`,
		`class inspect_primary_leaf_2 status_pending`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in output:\n%s", want, got)
		}
	}
}
