package graphjson_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	parserPkg "github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/render/graphjson"
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
	b := &graphdoc.Builder{Loader: &fileLoader{p: p}, Recurse: recurse}
	doc, err := b.Build(context.Background(), rb)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return doc
}

func TestRender_StaticShape(t *testing.T) {
	doc := buildDoc(t, false)
	b, err := graphjson.Render(doc)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var got graphjson.Document
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Hash == "" || got.SchemaVersion == "" {
		t.Errorf("missing top-level fields: %+v", got)
	}
	if len(got.Nodes) != len(doc.Nodes) {
		t.Errorf("node count: got %d want %d", len(got.Nodes), len(doc.Nodes))
	}
	if len(got.Edges) != len(doc.Edges) {
		t.Errorf("edge count: got %d want %d", len(got.Edges), len(doc.Edges))
	}
	// Branch nodes must map to the "decision" type.
	for _, n := range got.Nodes {
		if n.ID == "show_result" && n.Type != "decision" {
			t.Errorf("show_result type: got %q want decision", n.Type)
		}
		if n.ID == "done" && n.Type != "terminal" {
			t.Errorf("done type: got %q want terminal", n.Type)
		}
	}
	// go_ahead is inside a branch-arm group, so it should have parentNode set.
	for _, n := range got.Nodes {
		if n.ID == "go_ahead" {
			if n.ParentNode == "" || n.Extent != "parent" {
				t.Errorf("go_ahead should be in a parent group: %+v", n)
			}
		}
		if n.ID == "show_result" && n.ParentNode != "" {
			t.Errorf("show_result is top-level, parentNode should be empty")
		}
	}
}

func TestRender_V1QualifiedIdentityCompatibility(t *testing.T) {
	doc := &graphdoc.Document{
		SchemaVersion: "1",
		Runbook:       graphdoc.RunbookRef{ID: "root"},
		Frames: []graphdoc.Frame{{
			ID: "frame:child", QualifiedID: "frame:include/child", RunbookID: "child",
		}},
		Groups: []graphdoc.Group{{
			ID: "group:branch:0", QualifiedID: "group:include/branch:0", Kind: graphdoc.GroupBranchArm,
			ParentNodeID: "branch", QualifiedParentNodeID: "include/branch",
			FrameID: "frame:child", QualifiedFrameID: "frame:include/child",
		}},
		Nodes: []graphdoc.Node{{
			ID: "leaf", QualifiedID: "include/leaf", StepID: "leaf", CallPath: []string{"include"},
			Kind: "noop", FrameID: "frame:child", QualifiedFrameID: "frame:include/child",
			GroupID: "group:branch:0", QualifiedGroupID: "group:include/branch:0",
		}},
		Edges: []graphdoc.Edge{{
			From: "branch", QualifiedFrom: "include/branch", To: "leaf", QualifiedTo: "include/leaf", Kind: graphdoc.EdgeSequence,
		}},
	}

	encoded, err := graphjson.Render(doc)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var rendered graphjson.Document
	if err := json.Unmarshal(encoded, &rendered); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rendered.SchemaVersion != "1" || rendered.Frames[0].ID != "frame:include/child" || rendered.Groups[0].ID != "group:include/branch:0" {
		t.Fatalf("v1 container identities drifted: %#v", rendered)
	}
	node := rendered.Nodes[0]
	if node.ID != "include/leaf" || node.ParentNode != "group:include/branch:0" || node.Data["id"] != "include/leaf" || node.Data["step_id"] != "leaf" {
		t.Fatalf("v1 node identity drifted: %#v", node)
	}
	if rendered.Edges[0].Source != "include/branch" || rendered.Edges[0].Target != "include/leaf" {
		t.Fatalf("v1 edge identity drifted: %#v", rendered.Edges[0])
	}
}

func TestRenderWithState_AddsStatus(t *testing.T) {
	doc := buildDoc(t, false)
	st := runstate.New()
	st.Apply(engine.Event{Kind: "step/started", Sequence: 1, Payload: map[string]any{"step_id": "check_host"}})
	st.Apply(engine.Event{Kind: "step/completed", Sequence: 2, Payload: map[string]any{
		"step_id": "check_host", "duration_ms": int64(42),
	}})

	b, err := graphjson.RenderWithState(doc, st)
	if err != nil {
		t.Fatalf("RenderWithState: %v", err)
	}
	var got graphjson.Document
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, n := range got.Nodes {
		if n.ID == "check_host" {
			if got := n.Data["status"]; got != "completed" {
				t.Errorf("check_host.status: got %v want completed", got)
			}
			if _, ok := n.Data["duration_ms"]; !ok {
				t.Errorf("check_host missing duration_ms")
			}
		}
		if n.ID == "show_result" {
			if got := n.Data["status"]; got != "pending" {
				t.Errorf("show_result.status: got %v want pending", got)
			}
		}
	}
}

// TestRender_Inputs_DeclaredOrderSurvives is the F-2 regression test
// (barbara-client-enum-parity-gate-review.md, B-1): the graphdoc.Document
// Inputs field must be copied verbatim by RenderWithState -- the field was
// previously dropped entirely by pkg/preview/render/graphjson.Document,
// so `yawr preview --format graphjson` (and serve's /preview/document)
// emitted no `inputs` key at all. Covers CE-D-01/02/03 at the render
// layer (see cmd/yawr/preview_graphjson_inputs_test.go for the CLI-level
// assertion of the same behaviour).
func TestRender_Inputs_DeclaredOrderSurvives(t *testing.T) {
	doc := &graphdoc.Document{
		SchemaVersion: graphdoc.SchemaVersion,
		Inputs: []graphdoc.InputDecl{
			{Name: "env_name", Type: "string", Required: true, Enum: []string{"staging", "prod"}},
			{Name: "free_text", Type: "string"},
			{Name: "secret_env", Type: "string", Required: true, EnumRedacted: true, EnumMemberCount: 3},
		},
	}

	b, err := graphjson.Render(doc)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var got graphjson.Document
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Inputs) != 3 {
		t.Fatalf("Inputs count: got %d want 3; raw: %s", len(got.Inputs), b)
	}

	// Declared order survives verbatim -- no re-sort of the list, no
	// reorder of enum members.
	if got.Inputs[0].Name != "env_name" || len(got.Inputs[0].Enum) != 2 ||
		got.Inputs[0].Enum[0] != "staging" || got.Inputs[0].Enum[1] != "prod" {
		t.Fatalf("env_name decl not preserved verbatim: %+v", got.Inputs[0])
	}

	// Unconstrained input emits no enum key at all (absent, not []).
	if got.Inputs[1].Enum != nil {
		t.Fatalf("free_text.Enum = %v, want nil (absent)", got.Inputs[1].Enum)
	}
	if strings := string(b); jsonHasKeyOnObject(strings, `"free_text"`, `"enum"`) {
		t.Fatalf("free_text object must omit \"enum\" entirely, got: %s", b)
	}

	// Redacted input emits enumRedacted+enumMemberCount and no members.
	if !got.Inputs[2].EnumRedacted || got.Inputs[2].EnumMemberCount != 3 {
		t.Fatalf("secret_env redaction fields not preserved: %+v", got.Inputs[2])
	}
	if got.Inputs[2].Enum != nil {
		t.Fatalf("secret_env.Enum = %v, want nil under redaction", got.Inputs[2].Enum)
	}
}

// jsonHasKeyOnObject is a coarse textual check: true if needle appears
// anywhere after the object's own name key in the raw JSON, up to the next
// top-level array element boundary. Good enough for this small fixture's
// flat shape; not intended as a general JSON scanner.
func jsonHasKeyOnObject(raw, objectMarker, key string) bool {
	idx := indexOf(raw, objectMarker)
	if idx < 0 {
		return false
	}
	end := indexOf(raw[idx:], "}")
	if end < 0 {
		end = len(raw) - idx
	}
	return indexOf(raw[idx:idx+end], key) >= 0
}

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

func TestRender_Nil(t *testing.T) {
	b, err := graphjson.Render(nil)
	if err != nil {
		t.Fatalf("Render(nil): %v", err)
	}
	if len(b) == 0 {
		t.Errorf("expected JSON output")
	}
}

// TestRender_Regions verifies the regions manifest from the runbook
// roundtrips through the graphjson output. Uses a DRI compiler golden
// that embeds a regions: block.
func TestRender_Regions(t *testing.T) {
	p, err := parser.New(platform.Real())
	if err != nil {
		t.Fatalf("parser.New: %v", err)
	}
	rb, err := p.Parse(context.Background(), driChangeRequestPath(t))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	b := &graphdoc.Builder{Loader: &fileLoader{p: p}}
	doc, err := b.Build(context.Background(), rb)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if doc.Regions == nil {
		t.Fatalf("doc.Regions is nil; expected manifest from runbook")
	}
	out, err := graphjson.Render(doc)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var got graphjson.Document
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Regions == nil {
		t.Fatalf("graphjson.Document.Regions missing")
	}
	if got.Regions.SchemaVersion != "1" {
		t.Errorf("regions schema_version: got %q want 1", got.Regions.SchemaVersion)
	}
	if len(got.Regions.Regions) != 1 {
		t.Fatalf("regions count: got %d want 1", len(got.Regions.Regions))
	}
	r := got.Regions.Regions[0]
	if r.OpType != "ops.change-request" {
		t.Errorf("region op_type: got %q want ops.change-request", r.OpType)
	}
	if len(r.Members) == 0 || len(r.Entries) == 0 || len(r.Exits) == 0 {
		t.Errorf("region missing members/entries/exits: %+v", r)
	}
}
