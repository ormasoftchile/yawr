package flowwalk_test

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/flowwalk"
	parserPkg "github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// fileLoader satisfies flowwalk.Loader by reusing the real parser to load
// included runbooks from disk.
type fileLoader struct{ p parserPkg.Parser }

func (fl *fileLoader) Load(ctx context.Context, path string) (*parserPkg.ParsedRunbook, error) {
	return fl.p.Parse(ctx, path)
}

// traceVisitor records every visitor callback as a single line. It indents
// using two spaces per nesting level so the trace doubles as a structural
// snapshot.
type traceVisitor struct {
	flowwalk.Base
	depth int
	lines []string
}

func (t *traceVisitor) emit(format string, args ...any) {
	t.lines = append(t.lines, strings.Repeat("  ", t.depth)+fmt.Sprintf(format, args...))
}
func (t *traceVisitor) push(format string, args ...any) { t.emit(format, args...); t.depth++ }
func (t *traceVisitor) pop(format string, args ...any)  { t.depth--; t.emit(format, args...) }

func (t *traceVisitor) EnterRunbook(_ flowwalk.Ctx, rb *parserPkg.ParsedRunbook) error {
	t.push("EnterRunbook %s", rb.Runbook.ID)
	return nil
}
func (t *traceVisitor) LeaveRunbook(_ flowwalk.Ctx, rb *parserPkg.ParsedRunbook) error {
	t.pop("LeaveRunbook %s", rb.Runbook.ID)
	return nil
}

func (t *traceVisitor) EnterStep(_ flowwalk.Ctx, s *schema.Step) error {
	t.push("EnterStep %s [%s]", s.ID, s.Type)
	return nil
}
func (t *traceVisitor) LeaveStep(_ flowwalk.Ctx, s *schema.Step) error {
	t.pop("LeaveStep %s", s.ID)
	return nil
}

func (t *traceVisitor) EnterIterate(_ flowwalk.Ctx, n *schema.IterateNode) error {
	t.push("EnterIterate %s (over=%s as=%s)", n.ID, n.Over, n.As)
	return nil
}
func (t *traceVisitor) LeaveIterate(_ flowwalk.Ctx, n *schema.IterateNode) error {
	t.pop("LeaveIterate %s", n.ID)
	return nil
}

func (t *traceVisitor) EnterBranch(_ flowwalk.Ctx, s *schema.Step) error {
	t.push("EnterBranch %s", s.ID)
	return nil
}
func (t *traceVisitor) LeaveBranch(_ flowwalk.Ctx, s *schema.Step) error {
	t.pop("LeaveBranch %s", s.ID)
	return nil
}
func (t *traceVisitor) EnterArm(_ flowwalk.Ctx, _ *schema.Step, idx int, arm *schema.BranchArm) error {
	t.push("EnterArm[%d] %q", idx, arm.Label)
	return nil
}
func (t *traceVisitor) LeaveArm(_ flowwalk.Ctx, _ *schema.Step, idx int, _ *schema.BranchArm) error {
	t.pop("LeaveArm[%d]", idx)
	return nil
}

func (t *traceVisitor) EnterParallel(_ flowwalk.Ctx, p *schema.ParallelNode) error {
	t.push("EnterParallel %s", p.ID)
	return nil
}
func (t *traceVisitor) LeaveParallel(_ flowwalk.Ctx, p *schema.ParallelNode) error {
	t.pop("LeaveParallel %s", p.ID)
	return nil
}
func (t *traceVisitor) EnterParallelBranch(_ flowwalk.Ctx, _ *schema.ParallelNode, idx int, b *schema.ParallelBranch) error {
	t.push("EnterParallelBranch[%d] %q", idx, b.Label)
	return nil
}
func (t *traceVisitor) LeaveParallelBranch(_ flowwalk.Ctx, _ *schema.ParallelNode, idx int, _ *schema.ParallelBranch) error {
	t.pop("LeaveParallelBranch[%d]", idx)
	return nil
}

// recurseIncludes is a knob set per test.
type traceVisitorWithRecurse struct {
	traceVisitor
	recurse bool
}

func (t *traceVisitorWithRecurse) EnterInclude(_ flowwalk.Ctx, s *schema.Step, child *parserPkg.ParsedRunbook) (bool, error) {
	childID := "<nil>"
	if child != nil {
		childID = child.Runbook.ID
	}
	t.push("EnterInclude %s -> %s (recurse=%v)", s.ID, childID, t.recurse)
	return t.recurse, nil
}
func (t *traceVisitorWithRecurse) LeaveInclude(_ flowwalk.Ctx, s *schema.Step, _ *parserPkg.ParsedRunbook) error {
	t.pop("LeaveInclude %s", s.ID)
	return nil
}

func collectHealthPath() string {
	_, thisFile, _, _ := runtime.Caller(0)
	pkgDir := filepath.Dir(thisFile)
	return filepath.Join(pkgDir, "testdata", "examples", "collect-health", "collect-health.runbook.yaml")
}

// TestWalk_CollectHealth_Opaque asserts the traversal trace when the visitor
// keeps includes opaque (recurse=false). The walker MUST report the include
// step but NOT descend into the child runbook.
func TestWalk_CollectHealth_Opaque(t *testing.T) {
	ctx := context.Background()
	p, err := parser.New(platform.Real())
	if err != nil {
		t.Fatalf("parser.New: %v", err)
	}
	rb, err := p.Parse(ctx, collectHealthPath())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	v := &traceVisitorWithRecurse{recurse: false}
	w := &flowwalk.Walker{Loader: &fileLoader{p: p}}
	if err := w.Walk(ctx, rb, v); err != nil {
		t.Fatalf("Walk: %v", err)
	}

	got := strings.Join(v.lines, "\n")
	want := strings.TrimSpace(`
EnterRunbook collect-health
  EnterIterate check_loop (over=hosts as=svc)
    EnterStep check_host [include]
      EnterInclude check_host -> check-service (recurse=false)
      LeaveInclude check_host
    LeaveStep check_host
    EnterStep accumulate [noop]
    LeaveStep accumulate
  LeaveIterate check_loop
  EnterStep show_result [branch]
    EnterBranch show_result
      EnterArm[0] "All checks passed"
        EnterStep go_ahead [display]
        LeaveStep go_ahead
      LeaveArm[0]
      EnterArm[1] "Checks failed"
        EnterStep no_go [display]
        LeaveStep no_go
      LeaveArm[1]
    LeaveBranch show_result
  LeaveStep show_result
  EnterStep done [end]
  LeaveStep done
LeaveRunbook collect-health
`)
	if got != want {
		t.Errorf("opaque trace mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestWalk_CollectHealth_Recurse asserts the trace when the visitor descends
// into included runbooks. The check-service runbook's three steps must appear
// nested under EnterInclude / LeaveInclude.
func TestWalk_CollectHealth_Recurse(t *testing.T) {
	ctx := context.Background()
	p, err := parser.New(platform.Real())
	if err != nil {
		t.Fatalf("parser.New: %v", err)
	}
	rb, err := p.Parse(ctx, collectHealthPath())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	v := &traceVisitorWithRecurse{recurse: true}
	w := &flowwalk.Walker{Loader: &fileLoader{p: p}}
	if err := w.Walk(ctx, rb, v); err != nil {
		t.Fatalf("Walk: %v", err)
	}

	got := strings.Join(v.lines, "\n")
	want := strings.TrimSpace(`
EnterRunbook collect-health
  EnterIterate check_loop (over=hosts as=svc)
    EnterStep check_host [include]
      EnterInclude check_host -> check-service (recurse=true)
        EnterStep dns_lookup [tool]
        LeaveStep dns_lookup
        EnterStep ping_host [tool]
        LeaveStep ping_host
        EnterStep summarize [noop]
        LeaveStep summarize
      LeaveInclude check_host
    LeaveStep check_host
    EnterStep accumulate [noop]
    LeaveStep accumulate
  LeaveIterate check_loop
  EnterStep show_result [branch]
    EnterBranch show_result
      EnterArm[0] "All checks passed"
        EnterStep go_ahead [display]
        LeaveStep go_ahead
      LeaveArm[0]
      EnterArm[1] "Checks failed"
        EnterStep no_go [display]
        LeaveStep no_go
      LeaveArm[1]
    LeaveBranch show_result
  LeaveStep show_result
  EnterStep done [end]
  LeaveStep done
LeaveRunbook collect-health
`)
	if got != want {
		t.Errorf("recurse trace mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestWalk_CtxParentLinkage asserts the Ctx fields the walker exposes to
// visitors mirror the structural parent — this is what the planner / preview
// document builder will use to record ParentID/ParentKind/BranchLabel/Imports.
func TestWalk_CtxParentLinkage(t *testing.T) {
	ctx := context.Background()
	p, err := parser.New(platform.Real())
	if err != nil {
		t.Fatalf("parser.New: %v", err)
	}
	rb, err := p.Parse(ctx, collectHealthPath())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	type entry struct {
		parentID    string
		parentKind  string
		branchLabel string
		nestDepth   int
	}
	got := map[string]entry{}

	v := &linkageVisitor{recurse: true}
	w := &flowwalk.Walker{Loader: &fileLoader{p: p}}
	if err := w.Walk(ctx, rb, v); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	for k, e := range v.got {
		got[k] = entry(e)
	}

	want := map[string]entry{
		"check_host":  {parentID: "check_loop", parentKind: "iterate"},               // child of iterate
		"accumulate":  {parentID: "check_loop", parentKind: "iterate"},               // child of iterate
		"dns_lookup":  {parentID: "check_host", parentKind: "include", nestDepth: 1}, // inside included runbook
		"ping_host":   {parentID: "check_host", parentKind: "include", nestDepth: 1},
		"summarize":   {parentID: "check_host", parentKind: "include", nestDepth: 1},
		"show_result": {}, // top-level
		"go_ahead":    {parentID: "show_result", parentKind: "branch", branchLabel: "All checks passed"},
		"no_go":       {parentID: "show_result", parentKind: "branch", branchLabel: "Checks failed"},
		"done":        {}, // top-level
	}

	for id, w := range want {
		g, ok := got[id]
		if !ok {
			t.Errorf("step %q: not visited", id)
			continue
		}
		if g != w {
			t.Errorf("step %q: got %+v, want %+v", id, g, w)
		}
	}
}

type linkageVisitor struct {
	flowwalk.Base
	got     map[string]entry
	recurse bool
}

type entry struct {
	parentID    string
	parentKind  string
	branchLabel string
	nestDepth   int
}

func (v *linkageVisitor) EnterStep(ctx flowwalk.Ctx, s *schema.Step) error {
	if v.got == nil {
		v.got = map[string]entry{}
	}
	v.got[s.ID] = entry{
		parentID:    ctx.ParentID,
		parentKind:  ctx.ParentKind,
		branchLabel: ctx.BranchLabel,
		nestDepth:   ctx.NestDepth,
	}
	return nil
}

func (v *linkageVisitor) EnterIterate(ctx flowwalk.Ctx, n *schema.IterateNode) error {
	if v.got == nil {
		v.got = map[string]entry{}
	}
	v.got[n.ID] = entry{
		parentID:    ctx.ParentID,
		parentKind:  ctx.ParentKind,
		branchLabel: ctx.BranchLabel,
		nestDepth:   ctx.NestDepth,
	}
	return nil
}

func (v *linkageVisitor) EnterInclude(_ flowwalk.Ctx, _ *schema.Step, _ *parserPkg.ParsedRunbook) (bool, error) {
	return v.recurse, nil
}
