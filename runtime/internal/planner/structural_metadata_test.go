package planner_test

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/parser"
	"github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/expand"
	pkgPlanner "github.com/ormasoftchile/yawr/runtime/pkg/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func collectHealthPath() string {
	_, thisFile, _, _ := runtime.Caller(0)
	pkgDir := filepath.Dir(thisFile)
	return filepath.Join(pkgDir, "testdata", "examples", "collect-health", "collect-health.runbook.yaml")
}

// TestStructuralMetadata_CollectHealth asserts that the planner records
// ParentID/ParentKind/IncludeAlias/BranchLabel for the canonical collect-health
// runbook (iterate → include → branch → end). The TUI relies on these fields
// to render hierarchy without walking DisplayOrder heuristics.
func TestStructuralMetadata_CollectHealth(t *testing.T) {
	ctx := context.Background()

	p, err := parser.New(platform.Real())
	if err != nil {
		t.Fatalf("parser.New: %v", err)
	}

	reg := &fakeRegistry{tools: map[string]*schema.ToolDef{
		"nslookup/lookup": {Name: "nslookup"},
		"ping/check":      {Name: "ping"},
	}}

	pl := planner.New(pkgPlanner.Config{
		Loader:       &fileLoader{p: p},
		Tools:        reg,
		ExpandPolicy: expand.Policy{Default: expand.ModeEager},
	})

	rb, err := p.Parse(ctx, collectHealthPath())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	plan, err := pl.Plan(ctx, rb)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// Index by ID for assertions.
	type meta struct {
		kind         string
		parentID     string
		parentKind   string
		includeAlias string
		branchLabel  string
	}
	got := make(map[string]meta)
	for _, s := range plan.Steps {
		got[s.ID] = meta{
			kind:         s.Kind,
			parentID:     s.ParentID,
			parentKind:   s.ParentKind,
			includeAlias: s.IncludeAlias,
			branchLabel:  s.BranchLabel,
		}
	}

	t.Log("All steps:")
	for _, s := range plan.Steps {
		t.Logf("  %-20s kind=%-10s parent=%-15s parentKind=%-10s alias=%-6s branch=%q",
			s.ID, s.Kind, s.ParentID, s.ParentKind, s.IncludeAlias, s.BranchLabel)
	}

	want := map[string]meta{
		// iterate node — top-level
		"check_loop": {kind: "iterate", parentID: "", parentKind: "", includeAlias: "", branchLabel: ""},

		// include step inside iterate body
		"check_host": {kind: "include", parentID: "check_loop", parentKind: "iterate", includeAlias: "check", branchLabel: ""},

		// children from the included check-service runbook → parent is the include step
		"dns_lookup": {kind: "tool", parentID: "check_host", parentKind: "include", includeAlias: "", branchLabel: ""},
		"ping_host":  {kind: "tool", parentID: "check_host", parentKind: "include", includeAlias: "", branchLabel: ""},
		"summarize":  {kind: "noop", parentID: "check_host", parentKind: "include", includeAlias: "", branchLabel: ""},

		// noop step inside iterate body, sibling of check_host
		"accumulate": {kind: "noop", parentID: "check_loop", parentKind: "iterate", includeAlias: "", branchLabel: ""},

		// branch parent — top-level
		"show_result": {kind: "branch", parentID: "", parentKind: "", includeAlias: "", branchLabel: ""},

		// branch arm bodies — direct children of show_result, BranchLabel from the arm
		"go_ahead": {kind: "display", parentID: "show_result", parentKind: "branch", includeAlias: "", branchLabel: "All checks passed"},
		"no_go":    {kind: "display", parentID: "show_result", parentKind: "branch", includeAlias: "", branchLabel: "Checks failed"},

		// end step — top-level
		"done": {kind: "end", parentID: "", parentKind: "", includeAlias: "", branchLabel: ""},
	}

	for id, w := range want {
		g, ok := got[id]
		if !ok {
			t.Errorf("step %q: missing from plan", id)
			continue
		}
		if g.kind != w.kind {
			t.Errorf("step %q: Kind got %q, want %q", id, g.kind, w.kind)
		}
		if g.parentID != w.parentID {
			t.Errorf("step %q: ParentID got %q, want %q", id, g.parentID, w.parentID)
		}
		if g.parentKind != w.parentKind {
			t.Errorf("step %q: ParentKind got %q, want %q", id, g.parentKind, w.parentKind)
		}
		if g.includeAlias != w.includeAlias {
			t.Errorf("step %q: IncludeAlias got %q, want %q", id, g.includeAlias, w.includeAlias)
		}
		if g.branchLabel != w.branchLabel {
			t.Errorf("step %q: BranchLabel got %q, want %q", id, g.branchLabel, w.branchLabel)
		}
	}
}
