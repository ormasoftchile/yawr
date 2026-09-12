package flowwalk

import (
	"context"

	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// Loader resolves include paths to parsed runbooks.
//
// flowwalk declares its own loader interface (rather than depending on
// pkg/planner.RunbookLoader) so the package has no cycle risk and can be
// imported by the planner. Implementations satisfy both interfaces with a
// single method set.
type Loader interface {
	// Load returns the parsed runbook at the given absolute path.
	Load(ctx context.Context, path string) (*parser.ParsedRunbook, error)
}

// Ctx carries traversal context to visitor methods.
//
// The walker creates a fresh Ctx per descent; visitors must not retain
// pointers into Ctx.Path between calls (the walker may reuse the underlying
// slice). Make a copy if you need to keep one.
type Ctx struct {
	// Runbook is the parsed runbook currently being walked. Changes when
	// the walker descends into an included runbook.
	Runbook *parser.ParsedRunbook

	// BaseDir is the directory used to resolve relative include paths in
	// the current runbook.
	BaseDir string

	// Origin is the source path of the runbook currently being walked.
	Origin string

	// Path is the chain of step IDs from the current runbook root down to
	// (but not including) the node being visited. Iterate and parallel
	// nodes contribute their own ID; branch arms do not.
	Path []string

	// ParentID is the immediate enclosing iterate / branch / parallel /
	// include / compensate node, or empty at the top level.
	ParentID string

	// ParentKind is the kind of the immediate enclosing node:
	// "iterate" | "branch" | "parallel" | "include" | "compensate" | "".
	ParentKind string

	// BranchLabel is the label of the specific branch arm or parallel
	// branch this step belongs to (empty for non-branch parents).
	BranchLabel string

	// Imports is the imports map of the runbook currently being walked.
	Imports map[string]string

	// NestDepth is the include nesting level (0 = root runbook,
	// 1 = first include, …).
	NestDepth int
}

// Visitor is implemented by consumers of the walker.
//
// Methods are called in document order. Errors abort the walk and are
// returned from [Walker.Walk] unchanged.
//
// Recursion control: [Visitor.EnterInclude] returns a recurse flag. When
// recurse is true, the walker loads the child runbook (via [Loader]) and
// continues traversal inside it, with [Ctx.NestDepth] incremented and
// [Ctx.Imports]/[Ctx.BaseDir]/[Ctx.Origin] swapped to the child's values.
// On completion the walker calls [Visitor.LeaveInclude] in the original
// (parent) Ctx. When recurse is false the include is treated as opaque —
// LeaveInclude is still called, with childRb either the loaded runbook
// (if a Loader is configured) or nil.
//
// Embed [Base] to start with no-op defaults for every method.
type Visitor interface {
	EnterRunbook(ctx Ctx, rb *parser.ParsedRunbook) error
	LeaveRunbook(ctx Ctx, rb *parser.ParsedRunbook) error

	EnterStep(ctx Ctx, step *schema.Step) error
	LeaveStep(ctx Ctx, step *schema.Step) error

	EnterIterate(ctx Ctx, iter *schema.IterateNode) error
	LeaveIterate(ctx Ctx, iter *schema.IterateNode) error

	EnterParallel(ctx Ctx, par *schema.ParallelNode) error
	EnterParallelBranch(ctx Ctx, par *schema.ParallelNode, idx int, branch *schema.ParallelBranch) error
	LeaveParallelBranch(ctx Ctx, par *schema.ParallelNode, idx int, branch *schema.ParallelBranch) error
	LeaveParallel(ctx Ctx, par *schema.ParallelNode) error

	EnterBranch(ctx Ctx, step *schema.Step) error
	EnterArm(ctx Ctx, step *schema.Step, idx int, arm *schema.BranchArm) error
	LeaveArm(ctx Ctx, step *schema.Step, idx int, arm *schema.BranchArm) error
	LeaveBranch(ctx Ctx, step *schema.Step) error

	EnterCompensate(ctx Ctx, step *schema.Step) error
	LeaveCompensate(ctx Ctx, step *schema.Step) error

	// BeforeInclude is called just before the walker would load the
	// included child runbook. Return load=false to skip loading
	// entirely; the walker will then call EnterInclude with childRb=nil
	// and will not recurse, regardless of EnterInclude's recurse return
	// value. This lets a visitor implement "lazy" expansion that defers
	// parsing until execution time.
	BeforeInclude(ctx Ctx, step *schema.Step) (load bool, err error)

	EnterInclude(ctx Ctx, step *schema.Step, childRb *parser.ParsedRunbook) (recurse bool, err error)
	LeaveInclude(ctx Ctx, step *schema.Step, childRb *parser.ParsedRunbook) error
}

// Base is a Visitor with no-op methods. Embed it to override only the
// methods you care about:
//
//	type MyVisitor struct {
//	    flowwalk.Base
//	}
//
//	func (v *MyVisitor) EnterStep(ctx flowwalk.Ctx, s *schema.Step) error {
//	    // …
//	}
type Base struct{}

func (Base) EnterRunbook(Ctx, *parser.ParsedRunbook) error { return nil }
func (Base) LeaveRunbook(Ctx, *parser.ParsedRunbook) error { return nil }

func (Base) EnterStep(Ctx, *schema.Step) error { return nil }
func (Base) LeaveStep(Ctx, *schema.Step) error { return nil }

func (Base) EnterIterate(Ctx, *schema.IterateNode) error { return nil }
func (Base) LeaveIterate(Ctx, *schema.IterateNode) error { return nil }

func (Base) EnterParallel(Ctx, *schema.ParallelNode) error { return nil }
func (Base) EnterParallelBranch(Ctx, *schema.ParallelNode, int, *schema.ParallelBranch) error {
	return nil
}
func (Base) LeaveParallelBranch(Ctx, *schema.ParallelNode, int, *schema.ParallelBranch) error {
	return nil
}
func (Base) LeaveParallel(Ctx, *schema.ParallelNode) error { return nil }

func (Base) EnterBranch(Ctx, *schema.Step) error                      { return nil }
func (Base) EnterArm(Ctx, *schema.Step, int, *schema.BranchArm) error { return nil }
func (Base) LeaveArm(Ctx, *schema.Step, int, *schema.BranchArm) error { return nil }
func (Base) LeaveBranch(Ctx, *schema.Step) error                      { return nil }

func (Base) EnterCompensate(Ctx, *schema.Step) error { return nil }
func (Base) LeaveCompensate(Ctx, *schema.Step) error { return nil }

func (Base) BeforeInclude(Ctx, *schema.Step) (bool, error) { return true, nil }

func (Base) EnterInclude(Ctx, *schema.Step, *parser.ParsedRunbook) (bool, error) {
	return false, nil
}
func (Base) LeaveInclude(Ctx, *schema.Step, *parser.ParsedRunbook) error { return nil }
