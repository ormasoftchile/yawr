// Package planner implements the yawr runbook planner.
//
// The planner transforms a parsed runbook into a fully-resolved ExecutionPlan
// by resolving includes, discovering tools, and flattening the step tree.
//
// Schema traversal — flow-node dispatch, branch arms, parallel branches,
// iterate bodies, compensate bodies, include recursion, cycle detection, and
// max-depth enforcement — is delegated to pkg/flowwalk. The planner is a
// flowwalk.Visitor whose only job is to record one ResolvedStep per visited
// node with the right Depth / NestDepth / DisplayOrder / parent linkage.
package planner

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	internalgovernance "github.com/ormasoftchile/yawr/runtime/internal/governance"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/expand"
	"github.com/ormasoftchile/yawr/runtime/pkg/flowwalk"
	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	plannerPkg "github.com/ormasoftchile/yawr/runtime/pkg/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

var _ plannerPkg.Planner = (*impl)(nil)

const defaultMaxIncludeDepth = 10

// impl is the concrete Planner implementation.
type impl struct {
	loader   plannerPkg.RunbookLoader
	tools    plannerPkg.ToolRegistry
	baseDir  string
	maxDepth int
	policy   expand.Policy
	// profile is the optional Tier 0 preflight profile. Nil = no profile
	// checks; existing behavior is preserved exactly.
	profile *schema.RuntimeProfile
}

// New constructs a Planner with the given configuration.
// Panics if Loader or Tools is nil.
func New(cfg plannerPkg.Config) plannerPkg.Planner {
	if cfg.Loader == nil {
		panic("planner: Loader is required")
	}
	if cfg.Tools == nil {
		panic("planner: Tools is required")
	}
	maxDepth := cfg.MaxIncludeDepth
	if maxDepth == 0 {
		maxDepth = defaultMaxIncludeDepth
	}
	return &impl{
		loader:   cfg.Loader,
		tools:    cfg.Tools,
		baseDir:  cfg.BaseDir,
		maxDepth: maxDepth,
		policy:   cfg.ExpandPolicy,
		profile:  cfg.Profile,
	}
}

// loaderAdapter bridges plannerPkg.RunbookLoader to flowwalk.Loader.
// Both interfaces have the same method set; this is a typed-conversion adapter.
type loaderAdapter struct{ l plannerPkg.RunbookLoader }

func (a *loaderAdapter) Load(ctx context.Context, path string) (*parser.ParsedRunbook, error) {
	return a.l.Load(ctx, path)
}

// Plan transforms a parsed runbook into an execution plan.
// It resolves all include steps (recursively), discovers and validates tool
// references, and flattens the flow tree into an ordered ResolvedStep list.
func (p *impl) Plan(ctx context.Context, rb *parser.ParsedRunbook) (*engine.ExecutionPlan, error) {
	if rb == nil || rb.Runbook == nil {
		return nil, &plannerPkg.PlanError{
			Code:   plannerPkg.ErrNotImplemented,
			Detail: "nil runbook",
		}
	}

	// Do not dispatch a partially wired typed invocation. Remove this draft
	// capability guard only with durable root/frame publication integration.
	if err := typedRuntimeAvailability(rb.Runbook); err != nil {
		return nil, err
	}

	// Tier 0 plan-level preflight: attendance consistency check (PLAN-011).
	// This runs before walking so profile-level misconfigurations are reported
	// before any tool resolution work is attempted.
	if err := checkAttendancePreflight(p.profile); err != nil {
		return nil, err
	}

	v := &planVisitor{
		ctx:       ctx,
		p:         p,
		tools:     make(map[string]*schema.ToolDef),
		policy:    p.policy,
		lazyAt:    make(map[string]string),
		dynamicAt: make(map[string]bool),
	}

	w := &flowwalk.Walker{
		Loader:   &loaderAdapter{l: p.loader},
		BaseDir:  p.baseDir,
		MaxDepth: p.maxDepth,
	}

	if err := w.Walk(ctx, rb, v); err != nil {
		return nil, translateWalkError(err)
	}
	runbookContentHash, err := graphdoc.RunbookContentHash(rb.Runbook)
	if err != nil {
		return nil, err
	}

	plan := &engine.ExecutionPlan{
		RunbookPath:      rb.Source,
		Steps:            v.out,
		Tools:            v.tools,
		Providers:        make(map[string]*schema.ProviderDef),
		Inputs:           rb.Runbook.Inputs,
		Outputs:          rb.Runbook.Outputs,
		Bindings:         rb.Runbook.Bindings,
		Governance:       internalgovernance.BuildPolicy(rb.Runbook.Governance),
		GovernanceSource: rb.Runbook.Governance,
		Metadata: engine.PlanMetadata{
			PlannedAt: time.Now(), RunbookID: rb.Runbook.ID, RunbookName: rb.Runbook.Name,
			RunbookContentHash: runbookContentHash, Regions: rb.Runbook.Regions,
			Extensions: rb.Runbook.Extensions, Profile: p.profile,
		},
	}
	if _, err := validatePlan(plan); err != nil {
		return nil, err
	}
	return plan, nil
}

// translateWalkError maps flowwalk sentinel errors to planner.PlanError.
// Already-PlanError errors (returned by visitor methods) pass through.
func translateWalkError(err error) error {
	var pe *plannerPkg.PlanError
	if errors.As(err, &pe) {
		return err
	}
	switch {
	case errors.Is(err, flowwalk.ErrIncludeCycle):
		return &plannerPkg.PlanError{
			Code:   plannerPkg.ErrImportCycle,
			Detail: err.Error(),
		}
	case errors.Is(err, flowwalk.ErrMaxDepthExceeded):
		return &plannerPkg.PlanError{
			Code:   plannerPkg.ErrMaxDepthExceeded,
			Detail: err.Error(),
		}
	}
	return err
}

// planVisitor accumulates the flat list of ResolvedSteps as the walker
// traverses the runbook AST.
//
// Invariants:
//   - DisplayOrder is assigned pre-order (parent slot reserved before children).
//   - All composite parents (iterate / branch / parallel / compensate /
//     include) emit their ResolvedStep to out *before* their body is
//     visited; this happens in EnterIterate, EnterStep (for branch and
//     compensate), EnterParallel, and EnterInclude.
//   - Composite parents push BOTH execDepth and displayDepth on entry, so
//     their children appear at execDepth+1 (skipped by the engine's main
//     loop and dispatched via SubStepRunner) and displayDepth+1 (visual
//     nesting).
type planVisitor struct {
	flowwalk.Base

	// ctx is the request context, threaded into tool lookups.
	ctx context.Context
	// p is the parent planner instance (for tool registry).
	p *impl

	// out is the accumulating step list returned in ExecutionPlan.Steps.
	out []engine.ResolvedStep
	// tools accumulates resolved tool definitions for ExecutionPlan.Tools.
	tools map[string]*schema.ToolDef
	// displaySeq is the pre-order DisplayOrder counter.
	displaySeq int

	// execDepth is the engine-execution depth (ResolvedStep.Depth).
	// Pushed by iterate / branch-arm / parallel-branch / compensate /
	// include body entry.
	execDepth int
	// displayDepth is the visual nesting depth (ResolvedStep.NestDepth).
	// Pushed by every body entry.
	displayDepth int

	// policy controls expand resolution for include sites.
	policy expand.Policy
	// lazyAt records, by step.ID, the absolute path that should be
	// loaded at execution time for include sites the visitor decided
	// to defer. EnterInclude reads this to know whether to emit a
	// deferred (lazy) ResolvedStep instead of erroring on childRb=nil.
	lazyAt map[string]string
	// dynamicAt is the set of step IDs whose include target is resolved
	// at execution time (runbook_ref + resolve_from: catalog). Dynamic
	// sites are opaque at plan time — even more so than lazy sites,
	// which at least have a path. Like lazy sites, dynamic sites do NOT
	// push execDepth/displayDepth, so they must NOT pop them in
	// LeaveInclude either.
	dynamicAt map[string]bool
}

// nextDisplayOrder returns the next pre-order DisplayOrder index.
func (v *planVisitor) nextDisplayOrder() int {
	n := v.displaySeq
	v.displaySeq++
	return n
}

// EnterStep emits a ResolvedStep for every step kind except include.
// Include steps reserve a DisplayOrder slot in EnterInclude and emit in
// LeaveInclude. Branch and compensate parents emit here (their body is
// visited via EnterArm / EnterCompensate which manage depth pushes).
func (v *planVisitor) EnterStep(ctx flowwalk.Ctx, s *schema.Step) error {
	if s.Type == schema.StepTypeInclude {
		// Deferred to EnterInclude / LeaveInclude.
		return nil
	}
	if s.Type == schema.StepTypeTool {
		if err := v.resolveTool(s); err != nil {
			return err
		}
	}
	v.out = append(v.out, v.makeStep(ctx, s, ""))
	return nil
}

// EnterIterate emits the iterate parent ResolvedStep and pushes body depth.
func (v *planVisitor) EnterIterate(ctx flowwalk.Ctx, iter *schema.IterateNode) error {
	v.out = append(v.out, engine.ResolvedStep{
		ID:           iter.ID,
		Kind:         "iterate",
		Spec:         iter,
		Depth:        v.execDepth,
		NestDepth:    v.displayDepth,
		DisplayOrder: v.nextDisplayOrder(),
		Origin:       ctx.Origin,
		ParentID:     ctx.ParentID,
		ParentKind:   ctx.ParentKind,
		BranchLabel:  ctx.BranchLabel,
	})
	v.execDepth++
	v.displayDepth++
	return nil
}

// LeaveIterate pops body depth.
func (v *planVisitor) LeaveIterate(_ flowwalk.Ctx, _ *schema.IterateNode) error {
	v.execDepth--
	v.displayDepth--
	return nil
}

// EnterParallel emits the parallel parent ResolvedStep. Body depth is
// pushed per-branch in EnterParallelBranch, matching the original planner
// (which reset depth at each branch start via flattenNodes).
func (v *planVisitor) EnterParallel(ctx flowwalk.Ctx, par *schema.ParallelNode) error {
	v.out = append(v.out, engine.ResolvedStep{
		ID:           par.ID,
		Kind:         "parallel",
		Spec:         par,
		Depth:        v.execDepth,
		NestDepth:    v.displayDepth,
		DisplayOrder: v.nextDisplayOrder(),
		Origin:       ctx.Origin,
		ParentID:     ctx.ParentID,
		ParentKind:   ctx.ParentKind,
		BranchLabel:  ctx.BranchLabel,
	})
	return nil
}

// EnterParallelBranch pushes depth for one parallel branch's body.
func (v *planVisitor) EnterParallelBranch(_ flowwalk.Ctx, _ *schema.ParallelNode, _ int, _ *schema.ParallelBranch) error {
	v.execDepth++
	v.displayDepth++
	return nil
}

// LeaveParallelBranch pops depth.
func (v *planVisitor) LeaveParallelBranch(_ flowwalk.Ctx, _ *schema.ParallelNode, _ int, _ *schema.ParallelBranch) error {
	v.execDepth--
	v.displayDepth--
	return nil
}

// EnterArm pushes depth for a branch arm's body. The branch parent step
// itself was already emitted by EnterStep.
func (v *planVisitor) EnterArm(_ flowwalk.Ctx, _ *schema.Step, _ int, _ *schema.BranchArm) error {
	v.execDepth++
	v.displayDepth++
	return nil
}

// LeaveArm pops depth.
func (v *planVisitor) LeaveArm(_ flowwalk.Ctx, _ *schema.Step, _ int, _ *schema.BranchArm) error {
	v.execDepth--
	v.displayDepth--
	return nil
}

// EnterCompensate pushes depth for the compensate body. The compensate
// parent step itself was already emitted by EnterStep.
func (v *planVisitor) EnterCompensate(_ flowwalk.Ctx, _ *schema.Step) error {
	v.execDepth++
	v.displayDepth++
	return nil
}

// LeaveCompensate pops depth.
func (v *planVisitor) LeaveCompensate(_ flowwalk.Ctx, _ *schema.Step) error {
	v.execDepth--
	v.displayDepth--
	return nil
}

// BeforeInclude resolves the include site's expand mode and decides
// whether the walker should load the child runbook now (eager) or skip
// loading and let the IncludeExecutor materialize it at run time
// (lazy). The decision is recorded in v.lazyAt so EnterInclude knows
// to emit a deferred ResolvedStep instead of erroring on childRb=nil.
//
// Dynamic include sites (runbook_ref + resolve_from: catalog) are
// always opaque at plan time — the target is a GIS template that only
// renders at execution time. They are recorded in v.dynamicAt and
// handled in EnterInclude / LeaveInclude with the same no-push/no-pop
// discipline as lazy sites.
func (v *planVisitor) BeforeInclude(ctx flowwalk.Ctx, s *schema.Step) (bool, error) {
	if s.IncludeSpec == nil {
		return true, nil
	}
	if s.IncludeSpec.Include.IsDynamic() {
		v.dynamicAt[s.ID] = true
		return false, nil
	}
	siteMode, err := expand.Parse(s.IncludeSpec.Include.Expand)
	if err != nil {
		return false, &plannerPkg.PlanError{
			Code:   plannerPkg.ErrNotImplemented,
			StepID: s.ID,
			Detail: fmt.Sprintf("invalid include expand: %v", err),
		}
	}
	var rbMode expand.Mode
	if ctx.Runbook != nil && ctx.Runbook.Runbook != nil {
		rbMode, err = expand.Parse(ctx.Runbook.Runbook.Expand)
		if err != nil {
			return false, &plannerPkg.PlanError{
				Code:   plannerPkg.ErrNotImplemented,
				Path:   ctx.Origin,
				Detail: fmt.Sprintf("invalid runbook expand: %v", err),
			}
		}
	}
	resolved := v.policy.Resolve(siteMode, rbMode)
	// Auto materializes against the current runbook's direct include
	// count — a cheap measure that doesn't require loading children.
	if resolved == expand.ModeAuto {
		count := 0
		if ctx.Runbook != nil && ctx.Runbook.Runbook != nil {
			count = countDirectIncludes(ctx.Runbook.Runbook.Flow)
		}
		resolved = v.policy.Materialize(expand.ModeAuto, count)
	}
	if resolved != expand.ModeLazy {
		return true, nil
	}
	// Record the absolute path so EnterInclude can stamp it on the
	// emitted ResolvedStep. Mirrors flowwalk's own path resolution
	// (including imports: alias lookup).
	inclPath := s.IncludeSpec.Include.Runbook
	if aliased, ok := ctx.Imports[inclPath]; ok {
		inclPath = aliased
	}
	if !filepath.IsAbs(inclPath) {
		inclPath = filepath.Join(ctx.BaseDir, inclPath)
	}
	v.lazyAt[s.ID] = inclPath
	return false, nil
}

// EnterInclude emits the include parent ResolvedStep in pre-order with
// the loaded child runbook's flow nodes attached to its spec, then pushes
// both execDepth and displayDepth so children are emitted at depth+1.
// Children at depth+1 are skipped by the engine's main loop; the
// IncludeExecutor invokes SubStepRunner with the attached FlowNodes,
// matching how branch/iterate/parallel composites work.
//
// When BeforeInclude decided this site is lazy, childRb is nil and the
// step.ID appears in v.lazyAt; we emit a placeholder ResolvedStep
// carrying only the absolute path of the deferred runbook on its spec.
// The IncludeExecutor uses that path to load + run the child at
// execution time.
//
// When BeforeInclude decided this site is dynamic (runbook_ref), childRb
// is nil and the step.ID appears in v.dynamicAt. We emit a placeholder
// ResolvedStep whose IncludeSpec preserves the original Include config
// (including RunbookRef and ResolveFrom). No depths are pushed — dynamic
// sites have no static children, and the IncludeExecutor resolves them
// entirely at run time.
func (v *planVisitor) EnterInclude(ctx flowwalk.Ctx, s *schema.Step, childRb *parser.ParsedRunbook) (bool, error) {
	if childRb != nil {
		if err := typedRuntimeAvailability(childRb.Runbook); err != nil {
			return false, err
		}
	}
	if _, ok := v.dynamicAt[s.ID]; ok {
		rs := v.makeStep(ctx, s, "")
		rs.Spec = &schema.IncludeSpec{
			Include: s.IncludeSpec.Include,
		}
		v.out = append(v.out, rs)
		// No depth push: dynamic sites have no eager children.
		// Walker will not recurse because BeforeInclude returned load=false.
		return false, nil
	}
	if lazyPath, ok := v.lazyAt[s.ID]; ok {
		rs := v.makeStep(ctx, s, lookupIncludeAlias(ctx.Imports, s.IncludeSpec.Include.Runbook))
		rs.Spec = &schema.IncludeSpec{
			Include:         s.IncludeSpec.Include,
			LazyRunbookPath: lazyPath,
		}
		s.IncludeSpec.LazyRunbookPath = lazyPath
		v.out = append(v.out, rs)
		// Don't push depths: there are no eager children. Walker will
		// not recurse because BeforeInclude returned load=false.
		return false, nil
	}
	if childRb == nil {
		// No lazy mark and no loaded child: loader was misconfigured.
		return false, &plannerPkg.PlanError{
			Code:   plannerPkg.ErrRunbookNotFound,
			StepID: s.ID,
			Detail: "include runbook could not be resolved",
		}
	}
	childContentHash, err := graphdoc.RunbookContentHash(childRb.Runbook)
	if err != nil {
		return false, err
	}
	rs := v.makeStep(ctx, s, lookupIncludeAlias(ctx.Imports, s.IncludeSpec.Include.Runbook))
	// Attach the resolved child flow to the schema.Step's IncludeSpec so
	// that any downstream consumer that pulls the spec from the schema
	// (e.g. adapter.runSubStepsViaEngine when a parent composite dispatches
	// this include via SubStepRunner) sees the same children, not just the
	// engine-facing ResolvedStep.
	s.IncludeSpec.ResolvedSteps = childRb.Runbook.Flow
	s.IncludeSpec.ResolvedRunbookPath = childRb.Source
	s.IncludeSpec.ResolvedRunbookID = childRb.Runbook.ID
	s.IncludeSpec.ResolvedRunbookName = childRb.Runbook.Name
	s.IncludeSpec.ResolvedRunbookContentHash = childContentHash
	s.IncludeSpec.ResolvedInputs = childRb.Runbook.Inputs
	s.IncludeSpec.ResolvedBindings = childRb.Runbook.Bindings
	s.IncludeSpec.ResolvedOutputs = childRb.Runbook.Outputs
	s.IncludeSpec.ResolvedGovernance = childRb.Runbook.Governance
	// Replace the spec with one carrying the resolved child flow so the
	// IncludeExecutor can dispatch via SubStepRunner.
	rs.Spec = &schema.IncludeSpec{
		Include:             s.IncludeSpec.Include,
		ResolvedSteps:       childRb.Runbook.Flow,
		ResolvedRunbookPath: childRb.Source,
		ResolvedRunbookID:   childRb.Runbook.ID, ResolvedRunbookName: childRb.Runbook.Name,
		ResolvedRunbookContentHash: childContentHash,
		ResolvedInputs:             childRb.Runbook.Inputs,
		ResolvedBindings:           childRb.Runbook.Bindings,
		ResolvedOutputs:            childRb.Runbook.Outputs,
		ResolvedGovernance:         childRb.Runbook.Governance,
	}
	v.out = append(v.out, rs)
	v.execDepth++
	v.displayDepth++
	return true, nil
}

// countDirectIncludes returns the number of include steps directly
// reachable from nodes (not descending into includes themselves).
// Branches, iterate bodies, parallel branches and compensate bodies are
// counted into.
func countDirectIncludes(nodes []schema.FlowNode) int {
	n := 0
	for i := range nodes {
		fn := &nodes[i]
		switch {
		case fn.Step != nil:
			s := fn.Step
			if s.Type == schema.StepTypeInclude {
				n++
				continue
			}
			if s.BranchSpec != nil {
				for j := range s.BranchSpec.Branches {
					n += countDirectIncludes(s.BranchSpec.Branches[j].Steps)
				}
			}
			if s.CompensateSpec != nil {
				n += countDirectIncludes(s.CompensateSpec.Compensate.Steps)
			}
		case fn.Iterate != nil:
			n += countDirectIncludes(fn.Iterate.Steps)
		case fn.Parallel != nil:
			for j := range fn.Parallel.Branches {
				n += countDirectIncludes(fn.Parallel.Branches[j].Steps)
			}
		}
	}
	return n
}

// LeaveInclude pops both depths — but only if we pushed them in
// EnterInclude. Lazy and dynamic includes did not push (they emit no
// eager children), so we must not pop here either; popping would corrupt
// the depth counters and cause subsequent siblings to be emitted at
// the wrong execution depth (the engine's main loop would then
// re-dispatch steps that should have stayed nested under their
// composite parent).
func (v *planVisitor) LeaveInclude(_ flowwalk.Ctx, s *schema.Step, _ *parser.ParsedRunbook) error {
	if s != nil {
		if _, lazy := v.lazyAt[s.ID]; lazy {
			return nil
		}
		if _, dynamic := v.dynamicAt[s.ID]; dynamic {
			return nil
		}
	}
	v.execDepth--
	v.displayDepth--
	return nil
}

// makeStep builds a ResolvedStep using the visitor's current
// depth / displayDepth and the given ctx for parent linkage. includeAlias
// is filled only for include parent steps.
func (v *planVisitor) makeStep(ctx flowwalk.Ctx, s *schema.Step, includeAlias string) engine.ResolvedStep {
	return engine.ResolvedStep{
		ID:               s.ID,
		Name:             displayName(s),
		Subtitle:         s.Subtitle,
		Kind:             string(s.Type),
		Spec:             specForStep(s),
		Capture:          s.Capture,
		CaptureDefaults:  s.CaptureDefaults,
		When:             s.When,
		Retry:            s.Retry,
		Scope:            s.Scope,
		Export:           s.Export,
		Contract:         s.Contract,
		RequiredEvidence: s.RequiredEvidence,
		Depth:            v.execDepth,
		NestDepth:        v.displayDepth,
		DisplayOrder:     v.nextDisplayOrder(),
		Origin:           ctx.Origin,
		OnError:          s.OnError,
		Timeout:          s.Timeout,
		Delay:            s.Delay,
		ParentID:         ctx.ParentID,
		ParentKind:       ctx.ParentKind,
		BranchLabel:      ctx.BranchLabel,
		IncludeAlias:     includeAlias,
	}
}

// resolveTool looks up the tool definition for a tool step and caches it.
// Any lookup failure (unbound tool name / unbound action) is reported as
// PLAN-010 (§5.5 rule 1, §9 error catalog), wrapping the registry's own
// ErrToolNotFound/ErrActionNotFound sentinel as the cause so existing
// errors.Is(err, plannerPkg.ErrToolNotFound) call sites keep matching
// through the PlanError->errkit.Error->cause unwrap chain.
//
// After a successful lookup, Tier 0 static preflight is applied:
//   - AllowedEnvironments vs profile context (PLAN-010 / ErrContextMismatch)
//   - Test-context transport rules (PLAN-012 / ErrTestContextBinding)
//
// The tool is added to v.tools only if all checks pass; a failed preflight
// returns an error without mutating v.tools.
func (v *planVisitor) resolveTool(step *schema.Step) error {
	if step.ToolCall == nil {
		return nil
	}
	name := step.ToolCall.Tool.Name
	action := step.ToolCall.Tool.Action
	def, err := v.p.tools.Lookup(v.ctx, name, action)
	if err != nil {
		return &plannerPkg.PlanError{
			Code:   errkit.Wrap("PLAN-010", fmt.Sprintf("tool %q action %q not found", name, action), err),
			StepID: step.ID,
			Detail: fmt.Sprintf("tool %q action %q not found", name, action),
		}
	}
	// Tier 0 preflight: AllowedEnvironments and test-context transport rules.
	if preflightErr := checkToolEnvironmentPreflight(step.ID, def, v.p.profile); preflightErr != nil {
		return preflightErr
	}
	v.tools[name] = schema.CloneToolDef(def)
	return nil
}

// displayName returns the step's human-readable title, falling back to its ID.
func displayName(step *schema.Step) string {
	if step.Title != "" {
		return step.Title
	}
	if step.Type == schema.StepTypeResults {
		return "Results"
	}
	return step.ID
}

// lookupIncludeAlias reverse-looks-up the alias for an include path in an
// imports map. The map is alias→path; we want path→alias. Returns "" if
// no alias matches.
func lookupIncludeAlias(imports map[string]string, includePath string) string {
	for alias, path := range imports {
		if path == includePath {
			return alias
		}
	}
	return ""
}

// rawSpec is a fallback StepSpec for steps that lack a populated spec pointer.
type rawSpec struct{ kind string }

func (r *rawSpec) StepKind() string { return r.kind }

// specForStep returns the type-specific StepSpec for a step.
// Falls back to rawSpec when the concrete pointer is nil (e.g. malformed input).
func specForStep(step *schema.Step) engine.StepSpec {
	switch step.Type {
	case schema.StepTypeCLI:
		if step.CLI != nil {
			return step.CLI
		}
	case schema.StepTypeTool:
		if step.ToolCall != nil {
			return step.ToolCall
		}
	case schema.StepTypeInclude:
		if step.IncludeSpec != nil {
			return step.IncludeSpec
		}
	case schema.StepTypeChoice:
		if step.ChoiceSpec != nil {
			return step.ChoiceSpec
		}
	case schema.StepTypeDecision:
		if step.DecisionSpec != nil {
			return step.DecisionSpec
		}
	case schema.StepTypeCollector:
		if step.CollectorSpec != nil {
			return step.CollectorSpec
		}
	case schema.StepTypeHostAction:
		if step.HostActionSpec != nil {
			return step.HostActionSpec
		}
	case schema.StepTypeHandoff:
		if step.HandoffSpec != nil {
			return step.HandoffSpec
		}
	case schema.StepTypeBranch:
		if step.BranchSpec != nil {
			return step.BranchSpec
		}
	case schema.StepTypeParallel:
		if step.ParallelSpec != nil {
			return step.ParallelSpec
		}
	case schema.StepTypeApprove:
		if step.ApproveSpec != nil {
			return step.ApproveSpec
		}
	case schema.StepTypeAssert:
		if step.AssertSpec != nil {
			return step.AssertSpec
		}
	case schema.StepTypeCompensate:
		if step.CompensateSpec != nil {
			return step.CompensateSpec
		}
	case schema.StepTypeWaitForEvent:
		if step.WaitForEventSpec != nil {
			return step.WaitForEventSpec
		}
	case schema.StepTypeEnd:
		if step.EndSpec != nil {
			return step.EndSpec
		}
	case schema.StepTypeDisplay:
		if step.DisplaySpec != nil {
			return step.DisplaySpec
		}
	case schema.StepTypeNoop:
		if step.NoopSpec != nil {
			return step.NoopSpec
		}
		return &schema.NoopSpec{}
	case schema.StepTypeAssign:
		return step.AssignSpec
	case schema.StepTypeResults:
		return &schema.ResultsSpec{}
	}
	return &rawSpec{kind: string(step.Type)}
}
