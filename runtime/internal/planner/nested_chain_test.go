package planner_test

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/parser"
	"github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/expand"
	parserPkg "github.com/ormasoftchile/yawr/runtime/pkg/parser"
	pkgPlanner "github.com/ormasoftchile/yawr/runtime/pkg/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func nestedChainPath() string {
	_, thisFile, _, _ := runtime.Caller(0)
	pkgDir := filepath.Dir(thisFile)
	return filepath.Join(pkgDir, "testdata", "examples", "nested-chain", "chain-level-1.runbook.yaml")
}

// fileLoader loads runbooks from disk using the real parser.
type fileLoader struct {
	p parserPkg.Parser
}

func (fl *fileLoader) Load(ctx context.Context, path string) (*parserPkg.ParsedRunbook, error) {
	return fl.p.Parse(ctx, path)
}

func TestNestedChain_DisplayOrderAndNestDepth(t *testing.T) {
	ctx := context.Background()

	p, err := parser.New(platform.Real())
	if err != nil {
		t.Fatalf("parser.New: %v", err)
	}

	// nested-chain has no tool steps, so an empty fakeRegistry is sufficient.
	reg := &fakeRegistry{tools: make(map[string]*schema.ToolDef)}

	pl := planner.New(pkgPlanner.Config{
		Loader:       &fileLoader{p: p},
		Tools:        reg,
		ExpandPolicy: expand.Policy{Default: expand.ModeEager},
	})

	rb, err := p.Parse(ctx, nestedChainPath())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	plan, err := pl.Plan(ctx, rb)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	t.Log("All steps (execution order):")
	for i, s := range plan.Steps {
		t.Logf("  [%d] %-25s DO=%d ND=%d", i, s.ID, s.DisplayOrder, s.NestDepth)
	}

	type stepInfo struct {
		displayOrder int
		nestDepth    int
	}
	infos := make(map[string]stepInfo)
	for _, s := range plan.Steps {
		infos[s.ID] = stepInfo{displayOrder: s.DisplayOrder, nestDepth: s.NestDepth}
	}

	fmt.Printf("\nStep count: %d\n", len(plan.Steps))
	for id, info := range infos {
		fmt.Printf("  %-25s DO=%d ND=%d\n", id, info.displayOrder, info.nestDepth)
	}

	// Assert total step count: invoke_level_2, invoke_level_3, invoke_level_4, invoke_level_5, done = 5
	if got, want := len(plan.Steps), 5; got != want {
		t.Errorf("step count: got %d, want %d", got, want)
	}

	type want struct {
		do int
		nd int
	}
	expectations := map[string]want{
		"invoke_level_2": {do: 0, nd: 0},
		"invoke_level_3": {do: 1, nd: 1},
		"invoke_level_4": {do: 2, nd: 2},
		"invoke_level_5": {do: 3, nd: 3},
		"done":           {do: 4, nd: 4},
	}

	for id, exp := range expectations {
		info, ok := infos[id]
		if !ok {
			t.Errorf("step %q not found in plan", id)
			continue
		}
		if info.displayOrder != exp.do {
			t.Errorf("step %q: DisplayOrder got %d, want %d", id, info.displayOrder, exp.do)
		}
		if info.nestDepth != exp.nd {
			t.Errorf("step %q: NestDepth got %d, want %d", id, info.nestDepth, exp.nd)
		}
	}
}
