package flowwalk

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// DefaultMaxIncludeDepth is the default ceiling on transitive include depth
// when [Walker.MaxDepth] is zero. Matches the planner's historical default.
const DefaultMaxIncludeDepth = 10

// Sentinel errors returned by Walk for traversal-level failures. Wrap with
// fmt.Errorf when adding context; consumers detect these via errors.Is.
var (
	// ErrIncludeCycle is returned when a runbook is reached transitively
	// through itself.
	ErrIncludeCycle = errors.New("flowwalk: include cycle")
	// ErrMaxDepthExceeded is returned when include nesting exceeds the
	// walker's MaxDepth.
	ErrMaxDepthExceeded = errors.New("flowwalk: max include depth exceeded")
)

// Walker traverses a parsed runbook in document order, dispatching to a
// [Visitor]. It is safe to reuse a single Walker for many Walk calls;
// each Walk allocates its own traversal state.
type Walker struct {
	// Loader resolves include paths to parsed runbooks. May be nil — in
	// that case includes are reported as opaque (childRb=nil) and the
	// walker never recurses into them, regardless of what the visitor
	// returns.
	Loader Loader

	// BaseDir overrides the initial directory used to resolve relative includes.
	BaseDir string

	// MaxDepth caps include nesting. Zero means [DefaultMaxIncludeDepth].
	MaxDepth int
}

// Walk traverses rb and dispatches to v in document order.
//
// Returns the first error encountered (visitor error, loader error, cycle
// detection, or max-depth violation).
func (w *Walker) Walk(ctx context.Context, rb *parser.ParsedRunbook, v Visitor) error {
	if rb == nil || rb.Runbook == nil {
		return errors.New("flowwalk: nil runbook")
	}
	maxDepth := w.MaxDepth
	if maxDepth == 0 {
		maxDepth = DefaultMaxIncludeDepth
	}
	st := &walkState{
		ctx:    ctx,
		loader: w.Loader,
		max:    maxDepth,
		seen:   map[string]bool{},
	}
	if rb.Source != "" {
		st.seen[rb.Source] = true
	}
	baseDir := w.BaseDir
	if baseDir == "" && rb.Source != "" {
		baseDir = filepath.Dir(rb.Source)
	}
	baseDir = normalizeBaseDir(baseDir)
	root := Ctx{
		Runbook: rb,
		BaseDir: baseDir,
		Origin:  rb.Source,
		Imports: rb.Runbook.Imports,
	}
	if err := v.EnterRunbook(root, rb); err != nil {
		return err
	}
	if err := st.walkNodes(v, rb.Runbook.Flow, root); err != nil {
		return err
	}
	return v.LeaveRunbook(root, rb)
}

// walkState is the per-Walk mutable state.
type walkState struct {
	ctx    context.Context
	loader Loader
	max    int
	seen   map[string]bool // include cycle detection (DFS-style: enter sets, leave clears)
}

func (s *walkState) walkNodes(v Visitor, nodes []schema.FlowNode, ctx Ctx) error {
	for i := range nodes {
		n := &nodes[i]
		switch {
		case n.Step != nil:
			if err := s.walkStep(v, n.Step, ctx); err != nil {
				return err
			}
		case n.Iterate != nil:
			if err := s.walkIterate(v, n.Iterate, ctx); err != nil {
				return err
			}
		case n.Parallel != nil:
			if err := s.walkParallel(v, n.Parallel, ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *walkState) walkStep(v Visitor, step *schema.Step, ctx Ctx) error {
	switch step.Type {
	case schema.StepTypeBranch:
		return s.walkBranch(v, step, ctx)
	case schema.StepTypeCompensate:
		return s.walkCompensate(v, step, ctx)
	case schema.StepTypeInclude:
		return s.walkInclude(v, step, ctx)
	case schema.StepTypeParallel:
		return s.walkStepParallel(v, step, ctx)
	}
	if err := v.EnterStep(ctx, step); err != nil {
		return err
	}
	return v.LeaveStep(ctx, step)
}

func (s *walkState) walkStepParallel(v Visitor, step *schema.Step, ctx Ctx) error {
	if err := v.EnterStep(ctx, step); err != nil {
		return err
	}
	if step.ParallelSpec != nil {
		for index := range step.ParallelSpec.Branches {
			branch := &step.ParallelSpec.Branches[index]
			if err := v.EnterParallelBranch(ctx, step.ParallelSpec, index, branch); err != nil {
				return err
			}
			branchCtx := childCtx(ctx, step.ID, "parallel", branch.Label)
			if err := s.walkNodes(v, branch.Steps, branchCtx); err != nil {
				return err
			}
			if err := v.LeaveParallelBranch(ctx, step.ParallelSpec, index, branch); err != nil {
				return err
			}
		}
	}
	return v.LeaveStep(ctx, step)
}

func (s *walkState) walkIterate(v Visitor, iter *schema.IterateNode, ctx Ctx) error {
	if err := v.EnterIterate(ctx, iter); err != nil {
		return err
	}
	body := childCtx(ctx, iter.ID, "iterate", "")
	if err := s.walkNodes(v, iter.Steps, body); err != nil {
		return err
	}
	return v.LeaveIterate(ctx, iter)
}

func (s *walkState) walkParallel(v Visitor, par *schema.ParallelNode, ctx Ctx) error {
	if err := v.EnterParallel(ctx, par); err != nil {
		return err
	}
	for i := range par.Branches {
		branch := &par.Branches[i]
		if err := v.EnterParallelBranch(ctx, par, i, branch); err != nil {
			return err
		}
		bctx := childCtx(ctx, par.ID, "parallel", branch.Label)
		if err := s.walkNodes(v, branch.Steps, bctx); err != nil {
			return err
		}
		if err := v.LeaveParallelBranch(ctx, par, i, branch); err != nil {
			return err
		}
	}
	return v.LeaveParallel(ctx, par)
}

func (s *walkState) walkBranch(v Visitor, step *schema.Step, ctx Ctx) error {
	if err := v.EnterStep(ctx, step); err != nil {
		return err
	}
	if err := v.EnterBranch(ctx, step); err != nil {
		return err
	}
	if step.BranchSpec != nil {
		for i := range step.BranchSpec.Branches {
			arm := &step.BranchSpec.Branches[i]
			if err := v.EnterArm(ctx, step, i, arm); err != nil {
				return err
			}
			armCtx := childCtx(ctx, step.ID, "branch", arm.Label)
			if err := s.walkNodes(v, arm.Steps, armCtx); err != nil {
				return err
			}
			if err := v.LeaveArm(ctx, step, i, arm); err != nil {
				return err
			}
		}
	}
	if err := v.LeaveBranch(ctx, step); err != nil {
		return err
	}
	return v.LeaveStep(ctx, step)
}

func (s *walkState) walkCompensate(v Visitor, step *schema.Step, ctx Ctx) error {
	if err := v.EnterStep(ctx, step); err != nil {
		return err
	}
	if err := v.EnterCompensate(ctx, step); err != nil {
		return err
	}
	if step.CompensateSpec != nil {
		cctx := childCtx(ctx, step.ID, "compensate", "")
		if err := s.walkNodes(v, step.CompensateSpec.Compensate.Steps, cctx); err != nil {
			return err
		}
	}
	if err := v.LeaveCompensate(ctx, step); err != nil {
		return err
	}
	return v.LeaveStep(ctx, step)
}

func (s *walkState) walkInclude(v Visitor, step *schema.Step, ctx Ctx) error {
	if err := v.EnterStep(ctx, step); err != nil {
		return err
	}
	if step.IncludeSpec == nil {
		return v.LeaveStep(ctx, step)
	}

	inclPath := step.IncludeSpec.Include.Runbook
	// imports: is an alias registry (alias -> path); an include site may
	// reference either a literal relative path or a declared alias name.
	// Resolve the alias first, falling back to treating the value as a
	// literal path (the historical, still-supported idiom) when no
	// matching alias exists.
	if aliased, ok := ctx.Imports[inclPath]; ok {
		inclPath = aliased
	}
	if !filepath.IsAbs(inclPath) {
		inclPath = filepath.Join(ctx.BaseDir, inclPath)
	}

	// Ask the visitor whether we should even load this include. A lazy
	// visitor returns load=false to defer parsing until execution time;
	// in that case we skip the cycle and depth checks too (they only
	// apply to actually-traversed includes — a deferred include is
	// effectively opaque to the walk).
	load, err := v.BeforeInclude(ctx, step)
	if err != nil {
		return err
	}

	var childRb *parser.ParsedRunbook
	if load {
		if s.seen[inclPath] {
			return fmt.Errorf("%w: %s", ErrIncludeCycle, inclPath)
		}
		if ctx.NestDepth+1 > s.max {
			return fmt.Errorf("%w: limit=%d at %s", ErrMaxDepthExceeded, s.max, inclPath)
		}
		if s.loader != nil {
			var lerr error
			childRb, lerr = s.loader.Load(s.ctx, inclPath)
			if lerr != nil {
				return lerr
			}
		}
	}

	recurse, err := v.EnterInclude(ctx, step, childRb)
	if err != nil {
		return err
	}

	if load && recurse && childRb != nil {
		s.seen[inclPath] = true
		childCtxVal := Ctx{
			Runbook:    childRb,
			BaseDir:    filepath.Dir(inclPath),
			Origin:     inclPath,
			Path:       appendPath(ctx.Path, step.ID),
			ParentID:   step.ID,
			ParentKind: "include",
			Imports:    childRb.Runbook.Imports,
			NestDepth:  ctx.NestDepth + 1,
		}
		if err := s.walkNodes(v, childRb.Runbook.Flow, childCtxVal); err != nil {
			return err
		}
		delete(s.seen, inclPath)
	}

	if err := v.LeaveInclude(ctx, step, childRb); err != nil {
		return err
	}
	return v.LeaveStep(ctx, step)
}

// childCtx returns a Ctx for traversing into a structural parent's body.
// It preserves the runbook-level fields (Origin, BaseDir, Imports, NestDepth)
// and updates the parent linkage and Path.
func normalizeBaseDir(dir string) string {
	if dir == "" {
		return ""
	}
	if filepath.IsAbs(dir) {
		return filepath.Clean(dir)
	}
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return filepath.Clean(dir)
}

func childCtx(parent Ctx, parentID, parentKind, branchLabel string) Ctx {
	c := parent
	c.ParentID = parentID
	c.ParentKind = parentKind
	c.BranchLabel = branchLabel
	c.Path = appendPath(parent.Path, parentID)
	return c
}

// appendPath returns a copy of path with id appended. It allocates a fresh
// backing array so the returned slice does not alias the caller's.
func appendPath(path []string, id string) []string {
	out := make([]string, len(path), len(path)+1)
	copy(out, path)
	return append(out, id)
}
