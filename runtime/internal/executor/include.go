package executor

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	internaldebugprotect "github.com/ormasoftchile/yawr/runtime/internal/debugprotect"
	igov "github.com/ormasoftchile/yawr/runtime/internal/governance"
	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/expr"
	govpkg "github.com/ormasoftchile/yawr/runtime/pkg/governance"
	"github.com/ormasoftchile/yawr/runtime/pkg/pkgcatalog"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	tracepkg "github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

// defaultMaxIncludeDepth is the per-run ceiling on nested dynamic include
// depth when RegistryConfig.MaxIncludeDepth is zero.
const defaultMaxIncludeDepth = 10

// LoadedRunbook is the execution payload for one deferred include.
type LoadedRunbook struct {
	Bindings    []schema.Binding
	Flow        []schema.FlowNode
	Inputs      map[string]*schema.Input
	Outputs     map[string]*schema.Output
	Governance  *schema.GovernanceConfig
	ID          string
	Name        string
	ContentHash string
}

// LazyRunbookLoader resolves a deferred include's child runbook at
// execution time, including metadata required by debugger protection.
type LazyRunbookLoader interface {
	// Load returns the runbook at the given
	// absolute path. Errors are surfaced to the engine as step
	// failures (e.g. broken include path, parse error, schema error).
	Load(ctx context.Context, absPath string) (*LoadedRunbook, error)
}

// IncludeExecutor runs an include step's child runbook via SubStepRunner.
// It treats include as a real composite parent (like branch / iterate /
// parallel): the planner attaches the resolved child runbook's flow nodes
// to the IncludeSpec, this executor renders `with:` template bindings into
// a child scope, runs the children, merges their output vars back into
// the parent scope, checks the optional outcome gate, and applies the
// `capture:` rename map only when the parent continues.
//
// When the planner deferred this include (expand=lazy), spec.ResolvedSteps
// is empty and spec.LazyRunbookPath holds the absolute path of the child.
// The executor then loads the child's flow via [LazyRunbookLoader] and
// proceeds identically to the eager path.
//
// When the planner marked this include as dynamic (spec.Include.IsDynamic()),
// the executor renders the runbook_ref template, resolves against the frozen
// catalog via DynamicIncludeResolver, validates inputs, composes governance,
// enforces runtime cycle/depth, emits trace events, and executes the child.
type IncludeExecutor struct {
	evaluator         expr.Evaluator
	runner            SubStepRunner
	loader            LazyRunbookLoader
	resolver          DynamicIncludeResolver
	maxIncludeDepth   int
	pinRecorder       PinRecorder
	presentationTools func(context.Context, []schema.FlowNode) map[string]*schema.ToolDef
	approvalGate      govpkg.ApprovalGate
}

// WithApprovalGate sets the approval gate used when effective governance
// requires approval at a dynamic include boundary.
func (e *IncludeExecutor) WithApprovalGate(gate govpkg.ApprovalGate) *IncludeExecutor {
	e.approvalGate = gate
	return e
}

// NewIncludeExecutor constructs an IncludeExecutor without dynamic include
// support. For production use (with dynamic includes), NewDefaultRegistry
// calls newIncludeExecutorFull.
func NewIncludeExecutor(eval expr.Evaluator, runner SubStepRunner, loader LazyRunbookLoader) *IncludeExecutor {
	return &IncludeExecutor{
		evaluator:       eval,
		runner:          runner,
		loader:          loader,
		maxIncludeDepth: defaultMaxIncludeDepth,
	}
}

// newIncludeExecutorFull constructs an IncludeExecutor with full dynamic
// include support. Used by NewDefaultRegistry.
func newIncludeExecutorFull(eval expr.Evaluator, runner SubStepRunner, loader LazyRunbookLoader,
	resolver DynamicIncludeResolver, maxDepth int, pin PinRecorder) *IncludeExecutor {
	if maxDepth <= 0 {
		maxDepth = defaultMaxIncludeDepth
	}
	return &IncludeExecutor{
		evaluator:       eval,
		runner:          runner,
		loader:          loader,
		resolver:        resolver,
		maxIncludeDepth: maxDepth,
		pinRecorder:     pin,
	}
}

func (e *IncludeExecutor) MaterializeLazyIncludes(ctx context.Context, plan *engine.ExecutionPlan) error {
	if plan == nil {
		return errors.New("include executor: execution plan is required")
	}
	stack := make(map[string]bool)
	for index := range plan.Steps {
		if err := e.materializeLazySpec(ctx, plan.Steps[index].Spec, plan.Steps[index].ID, stack); err != nil {
			return err
		}
	}
	return nil
}

func (e *IncludeExecutor) materializeLazySpec(
	ctx context.Context,
	spec engine.StepSpec,
	path string,
	stack map[string]bool,
) error {
	switch typed := spec.(type) {
	case *schema.IncludeSpec:
		if typed == nil || typed.Include.IsDynamic() {
			return nil
		}
		if typed.LazyRunbookPath != "" {
			loader, ok := e.loader.(LazyRunbookSnapshotLoader)
			if !ok {
				return fmt.Errorf("include executor: durable lazy include %s requires a snapshot loader", path)
			}
			absolutePath := filepath.Clean(typed.LazyRunbookPath)
			if stack[absolutePath] {
				return fmt.Errorf("include executor: lazy include cycle at %s", absolutePath)
			}
			source, err := os.ReadFile(absolutePath)
			if err != nil {
				return fmt.Errorf("include executor: capture lazy include %s: %w", absolutePath, err)
			}
			digest := fmt.Sprintf("sha256:%x", sha256.Sum256(source))
			if typed.LazyRunbookDigest != "" && typed.LazyRunbookDigest != digest {
				return fmt.Errorf("include executor: lazy include %s digest changed", absolutePath)
			}
			loaded, err := loader.LoadSnapshot(ctx, absolutePath, source)
			if err != nil {
				return fmt.Errorf("include executor: parse captured lazy include %s: %w", absolutePath, err)
			}
			if loaded == nil {
				return fmt.Errorf("include executor: captured lazy include %s loaded no definition", absolutePath)
			}
			typed.ResolvedSteps = loaded.Flow
			typed.ResolvedRunbookPath = absolutePath
			typed.ResolvedRunbookID = loaded.ID
			typed.ResolvedRunbookName = loaded.Name
			typed.ResolvedRunbookContentHash = loaded.ContentHash
			typed.ResolvedInputs = loaded.Inputs
			typed.ResolvedOutputs = loaded.Outputs
			typed.ResolvedBindings = loaded.Bindings
			typed.ResolvedGovernance = loaded.Governance
			typed.LazyRunbookDigest = digest
			typed.LazyRunbookPath = ""
			stack[absolutePath] = true
			defer delete(stack, absolutePath)
		}
		return e.materializeLazyFlow(ctx, typed.ResolvedSteps, path, stack)
	case *schema.BranchSpec:
		if typed != nil {
			for index := range typed.Branches {
				if err := e.materializeLazyFlow(ctx, typed.Branches[index].Steps, fmt.Sprintf("%s/branch:%d", path, index), stack); err != nil {
					return err
				}
			}
		}
	case *schema.IterateNode:
		if typed != nil {
			return e.materializeLazyFlow(ctx, typed.Steps, path+"/iterate", stack)
		}
	case *schema.ParallelNode:
		if typed != nil {
			for index := range typed.Branches {
				if err := e.materializeLazyFlow(ctx, typed.Branches[index].Steps, fmt.Sprintf("%s/parallel:%d", path, index), stack); err != nil {
					return err
				}
			}
		}
	case *schema.CompensateSpec:
		if typed != nil {
			return e.materializeLazyFlow(ctx, typed.Compensate.Steps, path+"/compensate", stack)
		}
	}
	return nil
}

func (e *IncludeExecutor) materializeLazyFlow(
	ctx context.Context,
	nodes []schema.FlowNode,
	path string,
	stack map[string]bool,
) error {
	for index := range nodes {
		nodePath := fmt.Sprintf("%s/node:%d", path, index)
		switch {
		case nodes[index].Step != nil:
			step := nodes[index].Step
			var spec engine.StepSpec
			switch step.Type {
			case schema.StepTypeInclude:
				spec = step.IncludeSpec
			case schema.StepTypeBranch:
				spec = step.BranchSpec
			case schema.StepTypeParallel:
				spec = step.ParallelSpec
			case schema.StepTypeCompensate:
				spec = step.CompensateSpec
			default:
				continue
			}
			if spec == nil {
				return fmt.Errorf("include executor: %s step %s has no spec", step.Type, step.ID)
			}
			if err := e.materializeLazySpec(ctx, spec, nodePath+"/"+step.ID, stack); err != nil {
				return err
			}
		case nodes[index].Iterate != nil:
			if err := e.materializeLazySpec(ctx, nodes[index].Iterate, nodePath+"/"+nodes[index].Iterate.ID, stack); err != nil {
				return err
			}
		case nodes[index].Parallel != nil:
			if err := e.materializeLazySpec(ctx, nodes[index].Parallel, nodePath+"/"+nodes[index].Parallel.ID, stack); err != nil {
				return err
			}
		}
	}
	return nil
}

// ResolveDebugProtection returns child protection metadata without executing
// the include. Lazy metadata is loaded only when a debug run reaches the step.
func (e *IncludeExecutor) ResolveDebugProtection(ctx context.Context, step engine.ResolvedStep, vars map[string]any) (engine.DebugProtection, bool) {
	spec, ok := step.Spec.(*schema.IncludeSpec)
	if !ok || spec == nil {
		return engine.DebugProtection{}, false
	}
	return e.resolveDebugProtection(ctx, spec, vars, make(map[string]bool), 0)
}

func (e *IncludeExecutor) resolveDebugProtection(ctx context.Context, spec *schema.IncludeSpec, vars map[string]any, stack map[string]bool, depth int) (engine.DebugProtection, bool) {
	if spec == nil || spec.Include.IsDynamic() || depth > e.maxIncludeDepth {
		return engine.DebugProtection{}, false
	}
	childVars := e.resolveChildVarsForDebug(spec.Include.With, vars)
	inputs := spec.ResolvedInputs
	outputs := spec.ResolvedOutputs
	governance := spec.ResolvedGovernance
	childFlow := spec.ResolvedSteps
	if spec.LazyRunbookPath != "" {
		if e.loader == nil {
			return engine.DebugProtection{}, false
		}
		path := filepath.Clean(spec.LazyRunbookPath)
		if stack[path] {
			return engine.DebugProtection{}, false
		}
		stack[path] = true
		defer delete(stack, path)
		loaded, err := e.loadLazyRunbook(ctx, spec)
		if err != nil || loaded == nil {
			return engine.DebugProtection{}, false
		}
		inputs = loaded.Inputs
		outputs = loaded.Outputs
		governance = loaded.Governance
		childFlow = loaded.Flow
	}
	childProtection := engine.ExtendDebugProtection(
		engine.DebugProtection{}, childVars, inputs, igov.BuildPolicy(governance),
	)
	childProtection = extendOutputDebugProtection(childProtection, childVars, outputs)
	childProtection = engine.MergeDebugProtection(childProtection, inheritedBindingDebugProtection(
		spec.Include.With, childVars, internaldebugprotect.ProtectionFromContext(ctx),
	))
	sourceProtection, sourceComplete := secretBindingDebugProtection(spec.Include.With, vars, inputs)
	protection := engine.MergeDebugProtection(childProtection, sourceProtection)
	nestedInherited := engine.MergeDebugProtection(internaldebugprotect.ProtectionFromContext(ctx), protection)
	nestedCtx := internaldebugprotect.WithProtection(ctx, nestedInherited)
	nestedProtection, complete := e.resolveNestedDebugProtection(nestedCtx, childFlow, childVars, stack, depth+1)
	return engine.MergeDebugProtection(protection, nestedProtection), sourceComplete && complete
}

func extendOutputDebugProtection(
	protection engine.DebugProtection,
	vars map[string]any,
	outputs map[string]*schema.Output,
) engine.DebugProtection {
	additional := engine.DebugProtection{}
	for name, declaration := range outputs {
		if declaration == nil || declaration.Type != "secret" {
			continue
		}
		additional.ProtectedVars = append(additional.ProtectedVars, name)
		if value, ok := vars[name].(string); ok && value != "" {
			additional.SecretValues = append(additional.SecretValues, value)
		}
	}
	return engine.MergeDebugProtection(protection, additional)
}

func (e *IncludeExecutor) resolveNestedDebugProtection(ctx context.Context, nodes []schema.FlowNode, vars map[string]any, stack map[string]bool, depth int) (engine.DebugProtection, bool) {
	protection := engine.DebugProtection{}
	complete := true
	mergeNodes := func(childNodes []schema.FlowNode) {
		childProtection, childComplete := e.resolveNestedDebugProtection(ctx, childNodes, vars, stack, depth)
		protection = engine.MergeDebugProtection(protection, childProtection)
		complete = complete && childComplete
	}
	for index := range nodes {
		node := &nodes[index]
		switch {
		case node.Step != nil:
			step := node.Step
			if step.Type == schema.StepTypeInclude {
				childProtection, childComplete := e.resolveDebugProtection(ctx, step.IncludeSpec, vars, stack, depth)
				protection = engine.MergeDebugProtection(protection, childProtection)
				complete = complete && childComplete
			}
			if step.BranchSpec != nil {
				for _, branch := range step.BranchSpec.Branches {
					mergeNodes(branch.Steps)
				}
			}
			if step.CompensateSpec != nil {
				mergeNodes(step.CompensateSpec.Compensate.Steps)
			}
		case node.Iterate != nil:
			mergeNodes(node.Iterate.Steps)
		case node.Parallel != nil:
			for _, branch := range node.Parallel.Branches {
				mergeNodes(branch.Steps)
			}
		}
	}
	return protection, complete
}

func (e *IncludeExecutor) Execute(ctx context.Context, step engine.ResolvedStep, vars map[string]any) (*engine.StepResult, error) {
	spec, ok := step.Spec.(*schema.IncludeSpec)
	if !ok || spec == nil {
		return nil, fmt.Errorf("include executor: invalid spec for step %s", step.ID)
	}
	if e.runner == nil {
		return nil, fmt.Errorf("include executor: SubStepRunner is required")
	}

	// Dispatch dynamic sites to the dedicated execution path.
	if spec.Include.IsDynamic() {
		return e.executeDynamic(ctx, step, vars, spec)
	}

	// Build the child scope: parent vars plus rendered with-bindings.
	childVars, err := e.resolveChildVars(spec.Include.With, vars)
	if err != nil {
		return nil, err
	}

	// Resolve the child's flow. Eager includes already have it on the
	// spec; lazy includes carry an absolute path that we load now.
	childFlow := spec.ResolvedSteps
	childRunbookPath := spec.ResolvedRunbookPath
	childInputs := spec.ResolvedInputs
	childOutputs := spec.ResolvedOutputs
	childBindings := spec.ResolvedBindings
	childGovernance := spec.ResolvedGovernance
	if len(childFlow) == 0 && spec.LazyRunbookPath != "" {
		childRunbookPath = filepath.Clean(spec.LazyRunbookPath)
		if e.loader == nil {
			return nil, fmt.Errorf("include executor: step %s is lazy (path=%s) but no LazyRunbookLoader is configured", step.ID, spec.LazyRunbookPath)
		}
		loaded, err := e.loadLazyRunbook(ctx, spec)
		if err != nil {
			return nil, fmt.Errorf("include executor: lazy load %s: %w", spec.LazyRunbookPath, err)
		}
		childFlow = loaded.Flow
		childInputs = loaded.Inputs
		childOutputs = loaded.Outputs
		childBindings = loaded.Bindings
		childGovernance = loaded.Governance
	}

	// Run the child runbook's flow nodes via SubStepRunner.
	childCtx := withChildDebugProtection(
		ctx, childVars, vars, spec.Include.With, childInputs, childOutputs, childGovernance,
	)
	invocationState := &RunbookInvocationState{}
	results, err := e.runner(childCtx, SubStepParent{
		RunbookInvocation: schema.InvocationForRunbook(childBindings, childOutputs, childFlow),
		InvocationState:   invocationState,
		ID:                step.ID,
		Kind:              "include",
		IncludeAlias:      step.IncludeAlias,
		RunbookPath:       childRunbookPath,
		NestDepth:         step.NestDepth + 1,
	}, childFlow, childVars)
	if err != nil {
		if errors.Is(err, ErrSubRunFailed) {
			// Child sub-run terminated as failed. Return a failed StepResult
			// so the parent engine applies resolveOnError on this container step.
			result := newResult(step, engine.StepStatusFailed)
			result.Error = err
			mergeChildVars(result, results)
			return result, nil
		}
		return nil, err
	}

	return e.aggregateResults(step, spec, results, vars, invocationState)
}

func (e *IncludeExecutor) loadLazyRunbook(ctx context.Context, spec *schema.IncludeSpec) (*LoadedRunbook, error) {
	if spec.LazyRunbookDigest == "" {
		return e.loader.Load(ctx, spec.LazyRunbookPath)
	}
	source, err := os.ReadFile(spec.LazyRunbookPath)
	if err != nil {
		return nil, err
	}
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(source))
	if digest != spec.LazyRunbookDigest {
		return nil, errors.New("lazy runbook digest changed")
	}
	loader, ok := e.loader.(LazyRunbookSnapshotLoader)
	if !ok {
		return nil, errors.New("digest-bound lazy include requires a snapshot loader")
	}
	return loader.LoadSnapshot(ctx, spec.LazyRunbookPath, source)
}

func (e *IncludeExecutor) resolveChildVars(bindings map[string]string, vars map[string]any) (map[string]any, error) {
	childVars := copyVars(vars)
	for destination, template := range bindings {
		rendered, err := resolveTemplate(e.evaluator, template, vars)
		if err != nil {
			return nil, fmt.Errorf("include executor: with[%q]: %w", destination, err)
		}
		childVars[destination] = rendered
	}
	return childVars, nil
}

func (e *IncludeExecutor) resolveChildVarsForDebug(bindings map[string]string, vars map[string]any) map[string]any {
	childVars := copyVars(vars)
	for destination, template := range bindings {
		if rendered, err := resolveTemplate(e.evaluator, template, vars); err == nil {
			childVars[destination] = rendered
		}
	}
	return childVars
}

// executeDynamic handles the full dynamic include execution path:
// render ref → validate → resolve → cycle/depth → inputs → governance →
// trace → run → pin → aggregate.
func (e *IncludeExecutor) executeDynamic(ctx context.Context, step engine.ResolvedStep, vars map[string]any, spec *schema.IncludeSpec) (*engine.StepResult, error) {
	inc := spec.Include

	// Render the runbook_ref template (B-3/B-4: GIS template semantics).
	renderedRef, err := resolveTemplate(e.evaluator, inc.RunbookRef, vars)
	if err != nil {
		return nil, errkit.Wrap("DINC-001",
			fmt.Sprintf("dynamic include: step %s: failed to render runbook_ref template %q: %v", step.ID, inc.RunbookRef, err), err)
	}

	// Validate the rendered ref before catalog lookup.
	// Catches runtime-rendered empty/invalid refs (e.g. variable that resolves
	// to ""). DINC-002 (empty) is routed through handleResolveError so
	// on_not_found: continue applies.
	if reason, kind := pkgcatalog.ValidateRenderedRef(renderedRef); kind != pkgcatalog.RefValidationOK {
		code := "DINC-001"
		if kind == pkgcatalog.RefValidationEmpty {
			code = "DINC-002"
		}
		return e.handleResolveError(ctx, step, inc, renderedRef,
			errkit.New(code, fmt.Sprintf("dynamic include: step %s: %s", step.ID, reason)))
	}

	var resolved *DynamicIncludeResult
	durableResolution, found, err := engine.LookupDynamicIncludeResolution(ctx, renderedRef)
	if err != nil {
		return nil, err
	}
	if found {
		resolved, err = e.loadPinnedDynamicInclude(ctx, durableResolution.Pin)
		if err != nil {
			return nil, err
		}
	} else {
		// Resolve against the frozen catalog. Served runs can supply a per-run
		// resolver via ctx while CLI runs keep using the executor-level resolver.
		resolver := resolverFromCtx(ctx)
		if resolver == nil {
			resolver = e.resolver
		}
		if resolver == nil {
			return nil, fmt.Errorf("dynamic include: step %s: no DynamicIncludeResolver configured", step.ID)
		}
		dispatch, prepareErr := engine.PrepareExternalDispatch(ctx, engine.DispatchRequest{
			Classification: "read-only", EndpointIdentity: "dynamic-include-resolver",
			RenderedRequest: map[string]any{"rendered_ref": renderedRef},
		})
		if prepareErr != nil {
			return nil, prepareErr
		}
		resolveCtx := engine.WithPreparedDispatch(ctx, dispatch)
		resolved, err = resolver.Resolve(resolveCtx, renderedRef)
		if err != nil {
			return e.handleResolveError(resolveCtx, step, inc, renderedRef, err)
		}
	}

	chain := includeChainFromCtx(ctx)
	if !found {
		if err := e.materializeLazyFlow(ctx, resolved.Flow, "dynamic:"+step.ID, make(map[string]bool)); err != nil {
			return nil, fmt.Errorf("dynamic include: capture executable closure for step %s: %w", step.ID, err)
		}
	}

	// Build child scope and validate inputs.
	childVars := copyVars(vars)
	for dest, tmpl := range inc.With {
		rendered, err := resolveTemplate(e.evaluator, tmpl, vars)
		if err != nil {
			return nil, fmt.Errorf("dynamic include: step %s: with[%q]: %w", step.ID, dest, err)
		}
		childVars[dest] = rendered
	}
	if err := e.validateChildInputs(ctx, step, inc, resolved.ChildInputs, childVars, vars); err != nil {
		return nil, err
	}

	// Compose effective governance.
	parentGov := dynGovFromCtx(ctx)
	effectiveGov := ComposeGovernance(parentGov, resolved.ChildGovernance)
	protectedChildCtx := withChildDebugProtection(
		ctx, childVars, vars, inc.With, resolved.ChildInputs, resolved.ChildOutputs, resolved.ChildGovernance,
	)
	if err := internaldebugprotect.ValidateHandoffFlow(
		internaldebugprotect.ProtectionFromContext(protectedChildCtx), resolved.Flow,
	); err != nil {
		return nil, errors.New("dynamic include: resolved handoff contains protected content")
	}
	pin := durableResolution.Pin
	if !found {
		var tools map[string]*schema.ToolDef
		if e.presentationTools != nil {
			tools = e.presentationTools(protectedChildCtx, resolved.Flow)
		}
		closure, closureErr := plansnapshot.EncodeInvocationFlowClosure(resolved.Flow, schema.InvocationForRunbook(resolved.ChildBindings, resolved.ChildOutputs, resolved.Flow), tools)
		if closureErr != nil {
			return nil, fmt.Errorf("dynamic include: encode executable closure for step %s: %w", step.ID, closureErr)
		}
		pin = schema.LockedDynamicInclude{
			StepID: step.ID, RenderedRef: renderedRef, QualifiedID: resolved.QualifiedID,
			RunbookID: resolved.RunbookID, RunbookName: resolved.RunbookName,
			RunbookContentHash: resolved.ContentHash,
			AbsPath:            resolved.AbsPath, PackageName: resolved.PackageName, PackageVersion: resolved.PackageVersion,
			FileDigest: resolved.FileDigest, PackageDigest: resolved.PackageDigest,
			ExecutableClosure: closure, ResolvedInputs: resolved.ChildInputs, ResolvedBindings: resolved.ChildBindings,
			ResolvedOutputs:    resolved.ChildOutputs,
			ResolvedGovernance: resolved.ChildGovernance,
		}
		committed, commitErr := engine.CommitDynamicIncludeResolution(protectedChildCtx, pin)
		if commitErr != nil {
			return nil, commitErr
		}
		if committed.ResolutionID != "" {
			pin = committed.Pin
		}
	}

	// The resolver result is authoritative before these deterministic checks:
	// commit its immutable closure first so a cycle/depth failure cannot leave
	// a settled resolver dispatch with no durable resolution provenance.
	for _, id := range chain {
		if id == resolved.QualifiedID {
			return nil, errkit.New("DINC-004",
				fmt.Sprintf("dynamic include: step %s: cycle detected — %q already in include chain %v",
					step.ID, resolved.QualifiedID, chain))
		}
	}
	if len(chain) >= e.maxIncludeDepth {
		return nil, errkit.New("DINC-005",
			fmt.Sprintf("dynamic include: step %s: max include depth %d exceeded",
				step.ID, e.maxIncludeDepth))
	}

	// Enforce require_approval from the composed governance before child steps run.
	if err := e.enforceApprovalPolicy(ctx, step.ID, effectiveGov); err != nil {
		return nil, err
	}

	// Emit include/resolved trace event.
	emitter := EmitterFromContext(ctx)
	if emitter != nil {
		emitter(string(tracepkg.EventKindIncludeResolved), map[string]any{
			"step_id":              step.ID,
			"rendered_ref":         renderedRef,
			"qualified_id":         resolved.QualifiedID,
			"abs_path":             resolved.AbsPath,
			"package_name":         resolved.PackageName,
			"package_version":      resolved.PackageVersion,
			"file_digest":          resolved.FileDigest,
			"package_digest":       resolved.PackageDigest,
			"depth":                len(chain) + 1,
			"effective_governance": effectiveGov,
		})
	}

	// Record the resolution pin for replay and resume.
	pinRecorder := pinRecorderFromCtx(ctx)
	if pinRecorder == nil {
		pinRecorder = e.pinRecorder
	}
	if pinRecorder != nil {
		pinRecorder(DynamicIncludePin{
			StepID: pin.StepID, QualifiedNodeID: pin.QualifiedNodeID,
			Invocation: pin.Invocation, Revision: pin.Revision,
			StructuralPath: append([]schema.DynamicIncludeFrameIdentity(nil), pin.StructuralPath...),
			RenderedRef:    pin.RenderedRef, QualifiedID: pin.QualifiedID,
			RunbookID: pin.RunbookID, RunbookName: pin.RunbookName, ContentHash: pin.RunbookContentHash,
			AbsPath: pin.AbsPath, PackageName: pin.PackageName, PackageVersion: pin.PackageVersion,
			FileDigest: pin.FileDigest, PackageDigest: pin.PackageDigest,
			ExecutableClosure: pin.ExecutableClosure, ResolvedInputs: pin.ResolvedInputs, ResolvedBindings: pin.ResolvedBindings,
			ResolvedOutputs:    pin.ResolvedOutputs,
			ResolvedGovernance: pin.ResolvedGovernance,
		})
	}

	// Execute child runbook with extended chain, composed governance, and
	// compiled policy in ctx so child CLI steps enforce deny_commands.
	extendedChain := append(append([]string{}, chain...), resolved.QualifiedID)
	childCtx := withIncludeChain(protectedChildCtx, extendedChain)
	if pin.Revision > 0 && pin.QualifiedNodeID != "" {
		tools, restoreErr := plansnapshot.RestoreFlowTools(pin.ExecutableClosure)
		if restoreErr != nil {
			return nil, restoreErr
		}
		childCtx = engine.WithCommittedPresentationTools(childCtx, tools)
	}
	childCtx = withDynGov(childCtx, effectiveGov)
	childCtx = withGovPolicy(childCtx, igov.BuildPolicy(effectiveGovToSchemaConfig(effectiveGov)))

	invocationState := &RunbookInvocationState{}
	results, err := e.runner(childCtx, SubStepParent{
		ID:                step.ID,
		RunbookInvocation: schema.InvocationForRunbook(resolved.ChildBindings, resolved.ChildOutputs, resolved.Flow),
		InvocationState:   invocationState,
		Kind:              "include",
		IncludeAlias:      step.IncludeAlias,
		RunbookPath:       resolved.AbsPath,
		NestDepth:         step.NestDepth + 1,
	}, resolved.Flow, childVars)
	if err != nil {
		if errors.Is(err, ErrSubRunFailed) {
			result := newResult(step, engine.StepStatusFailed)
			result.Error = err
			mergeChildVars(result, results)
			return result, nil
		}
		return nil, err
	}

	return e.aggregateResults(step, spec, results, vars, invocationState)
}

func (e *IncludeExecutor) loadPinnedDynamicInclude(
	ctx context.Context,
	pin schema.LockedDynamicInclude,
) (*DynamicIncludeResult, error) {
	if len(pin.ExecutableClosure) > 0 {
		flow, err := plansnapshot.RestoreFlowClosure(pin.ExecutableClosure)
		if err != nil {
			return nil, fmt.Errorf("dynamic include: restore pinned runbook %q: %w", pin.QualifiedID, err)
		}
		return &DynamicIncludeResult{
			Flow: flow, QualifiedID: pin.QualifiedID, AbsPath: pin.AbsPath,
			RunbookID: pin.RunbookID, RunbookName: pin.RunbookName, ContentHash: pin.RunbookContentHash,
			PackageName: pin.PackageName, PackageVersion: pin.PackageVersion,
			FileDigest: pin.FileDigest, PackageDigest: pin.PackageDigest,
			ChildInputs: pin.ResolvedInputs, ChildOutputs: pin.ResolvedOutputs, ChildBindings: pin.ResolvedBindings,
			ChildGovernance: pin.ResolvedGovernance,
		}, nil
	}
	if e.loader == nil {
		return nil, errors.New("dynamic include: durable resolution requires a runbook loader")
	}
	loaded, err := e.loadLazyRunbook(ctx, &schema.IncludeSpec{
		LazyRunbookPath: pin.AbsPath, LazyRunbookDigest: pin.FileDigest,
	})
	if err != nil {
		return nil, fmt.Errorf("dynamic include: load pinned runbook %q: %w", pin.QualifiedID, err)
	}
	if loaded == nil {
		return nil, fmt.Errorf("dynamic include: pinned runbook %q loaded no definition", pin.QualifiedID)
	}
	return &DynamicIncludeResult{
		Flow: loaded.Flow, QualifiedID: pin.QualifiedID, AbsPath: pin.AbsPath,
		RunbookID: loaded.ID, RunbookName: loaded.Name, ContentHash: loaded.ContentHash,
		PackageName: pin.PackageName, PackageVersion: pin.PackageVersion,
		FileDigest: pin.FileDigest, PackageDigest: pin.PackageDigest,
		ChildInputs: loaded.Inputs, ChildOutputs: loaded.Outputs, ChildGovernance: loaded.Governance, ChildBindings: loaded.Bindings,
	}, nil
}

func withChildDebugProtection(
	ctx context.Context,
	childVars, parentVars map[string]any,
	bindings map[string]string,
	inputs map[string]*schema.Input,
	outputs map[string]*schema.Output,
	governance *schema.GovernanceConfig,
) context.Context {
	additional := engine.ExtendDebugProtection(
		engine.DebugProtection{}, childVars, inputs, igov.BuildPolicy(governance),
	)
	additional = extendOutputDebugProtection(additional, childVars, outputs)
	additional = engine.MergeDebugProtection(additional, inheritedBindingDebugProtection(
		bindings, childVars, internaldebugprotect.ProtectionFromContext(ctx),
	))
	sourceProtection, _ := secretBindingDebugProtection(bindings, parentVars, inputs)
	additional = engine.MergeDebugProtection(additional, sourceProtection)
	internaldebugprotect.PublishProtection(ctx, additional)
	protection := engine.MergeDebugProtection(internaldebugprotect.ProtectionFromContext(ctx), additional)
	return internaldebugprotect.WithProtection(ctx, protection)
}

// enforceApprovalPolicy checks whether the composed governance mandates
// approval before the child runbook may execute. If the
// approval gate is nil and approval is required, the include fails with
// DYN-015.
func (e *IncludeExecutor) enforceApprovalPolicy(ctx context.Context, stepID string, gov *tracepkg.EffectiveGovernancePayload) error {
	if gov == nil || !gov.RequireApproval {
		return nil
	}
	gate := approvalGateFromContext(ctx)
	if gate == nil {
		gate = e.approvalGate
	}
	if gate == nil {
		return fmt.Errorf("dynamic include: step %s: effective governance requires approval but no approval gate is configured [DYN-015]", stepID)
	}
	if _, err := gate.RequestApproval(ctx, stepID, "dynamic include: effective governance requires approval"); err != nil {
		return fmt.Errorf("dynamic include: step %s: governance approval denied [DYN-015]: %w", stepID, err)
	}
	return nil
}

// handleResolveError applies the on_not_found policy (B-5/B-6):
// only DINC-002 may take the continue path; all other codes are always fatal.
func (e *IncludeExecutor) handleResolveError(ctx context.Context, step engine.ResolvedStep, inc schema.IncludeConfig, renderedRef string, resolveErr error) (*engine.StepResult, error) {
	emitter := EmitterFromContext(ctx)
	if emitter != nil {
		code := ""
		if c, ok := resolveErr.(errkit.Coder); ok {
			code = c.Code()
		}
		qualifiedNodeID := engine.DebugNodeID(engine.DebugCallPathFromContext(ctx), step.ID)
		invocation := 1
		if boundary, ok := engine.DispatchExecutionBoundaryFromContext(ctx); ok {
			if boundary.QualifiedNodeID != "" {
				qualifiedNodeID = boundary.QualifiedNodeID
			}
			if boundary.Invocation > 0 {
				invocation = boundary.Invocation
			}
		}
		if dispatch, ok := engine.PreparedDispatchFromContext(ctx); ok && dispatch.Invocation > 0 {
			invocation = dispatch.Invocation
		}
		emitter(string(tracepkg.EventKindIncludeNotFound), map[string]any{
			"step_id":           step.ID,
			"qualified_node_id": qualifiedNodeID,
			"structural_path":   engine.DynamicIncludeStructuralPathFromContext(ctx),
			"invocation":        invocation,
			"rendered_ref":      renderedRef,
			"error_code":        code,
			"reason":            resolveErr.Error(),
			"continued":         inc.OnNotFound == schema.OnNotFoundContinue && code == "DINC-002",
		})
	}

	// on_not_found: continue covers ONLY DINC-002 (B-5).
	if inc.OnNotFound == schema.OnNotFoundContinue {
		if c, ok := resolveErr.(errkit.Coder); ok && c.Code() == "DINC-002" {
			warning := fmt.Sprintf("dynamic include: step %s: runbook_ref %q was not found in the catalog; on_not_found: continue skipped the child runbook", step.ID, renderedRef)
			result := newResult(step, engine.StepStatusSkipped)
			result.Output["warning"] = warning
			result.Output["stderr"] = warning
			result.Output["skip_reason"] = "include_not_found"
			result.Vars["runbook_found"] = false
			result.Vars["runbook_skipped_reason"] = resolveErr.Error()
			return result, nil
		}
	}

	return nil, resolveErr
}

// validateChildInputs checks that the supplied with-bindings satisfy the
// child runbook's declared inputs. DINC-W007 (unknown with-key) is non-fatal
// per §11 table (emitted as a trace note); all others are fatal.
func (e *IncludeExecutor) validateChildInputs(ctx context.Context, step engine.ResolvedStep, inc schema.IncludeConfig, childInputs map[string]*schema.Input, childVars map[string]any, parentVars map[string]any) error {
	emitter := EmitterFromContext(ctx)

	// DINC-W007: with-key not declared in child inputs (non-fatal per B-15).
	for k := range inc.With {
		if _, ok := childInputs[k]; !ok {
			if emitter != nil {
				emitter("step/output", map[string]any{
					"step_id": step.ID,
					"code":    "DINC-W007",
					"message": fmt.Sprintf("dynamic include: with key %q is not declared in child inputs", k),
				})
			}
		}
	}

	for name, inp := range childInputs {
		val, bound := childVars[name]

		// DINC-006: required input not provided (no binding, no default, not in parent scope).
		if inp.Required && inp.Default == nil {
			if !bound {
				if _, inParent := parentVars[name]; !inParent {
					return errkit.New("DINC-006",
						fmt.Sprintf("dynamic include: step %s: required input %q not provided", step.ID, name))
				}
			}
		}

		if !bound {
			continue
		}

		valStr, isStr := val.(string)

		// DINC-008: enum constraint violation.
		if len(inp.Enum) > 0 && isStr {
			found := false
			for _, allowed := range inp.Enum {
				if allowed == valStr {
					found = true
					break
				}
			}
			if !found {
				return errkit.New("DINC-008",
					fmt.Sprintf("dynamic include: step %s: input %q value %q violates enum constraint (allowed: %v)",
						step.ID, name, valStr, []string(inp.Enum)))
			}
		}

		// DINC-009: type mismatch.
		if inp.Type != "" && inp.Type != "string" {
			switch inp.Type {
			case "boolean":
				if _, ok := val.(bool); !ok {
					return errkit.New("DINC-009",
						fmt.Sprintf("dynamic include: step %s: input %q expects boolean, got %T", step.ID, name, val))
				}
			case "number", "integer":
				switch val.(type) {
				case int, int64, float64:
					// ok
				default:
					return errkit.New("DINC-009",
						fmt.Sprintf("dynamic include: step %s: input %q expects %s, got %T", step.ID, name, inp.Type, val))
				}
			}
		}
	}
	return nil
}

// aggregateResults is the shared post-run aggregation used by both eager/lazy
// and dynamic execution paths.
func (e *IncludeExecutor) aggregateResults(step engine.ResolvedStep, spec *schema.IncludeSpec, results []*engine.StepResult, parentVars map[string]any, invocationStates ...*RunbookInvocationState) (*engine.StepResult, error) {
	mergedVars := map[string]any{}
	for _, state := range invocationStates {
		if state != nil {
			for name, value := range state.Bindings {
				mergedVars[name] = value
			}
		}
	}
	for _, res := range results {
		if res == nil {
			continue
		}
		for k, v := range res.Vars {
			mergedVars[k] = v
		}
	}

	// If runSubStepsViaEngine returned without error, the sub-engine
	// completed its flow — any individual step failures were tolerated
	// (continue_on_fail / on_error: continue). The include is successful.
	status := engine.StepStatusCompleted

	result := newResult(step, status)
	var requiredFailure *engine.StepResult
	for _, child := range results {
		if child == nil {
			continue
		}
		result.RequiredFailure = result.RequiredFailure || child.RequiredFailure ||
			child.Status == engine.StepStatusFailed || child.Status == engine.StepStatusDenied ||
			child.Status == engine.StepStatusIndeterminate
		if child.RequiredFailure || child.Error != nil ||
			child.Status == engine.StepStatusFailed || child.Status == engine.StepStatusDenied ||
			child.Status == engine.StepStatusIndeterminate || child.Status == engine.StepStatusWaiting ||
			child.Status == engine.StepStatusRunning || child.Status == engine.StepStatusPending {
			if requiredFailure == nil || requiredFailure.Error == nil {
				requiredFailure = child
			}
		}
		if child.Results != nil {
			result.PublicOutputs = make(map[string]any, len(child.Results.Outputs))
			for name, output := range child.Results.Outputs {
				result.PublicOutputs[name] = output.Value
			}
		}
	}
	// Bubble all child vars up so the parent runbook sees them, matching
	// the prior inline-flattening behavior where child steps mutated the
	// parent vars map directly.
	for k, v := range mergedVars {
		result.Vars[k] = v
	}

	// A matching gate is a control-flow return, not a capture continuation.
	// Decide from the existing declared outcome before reading any public
	// output or writing any capture alias, including when the child published.
	if spec.Include.Gate != nil {
		if outcomeCategory, ok := mergedVars["__run_outcome_category"].(string); ok {
			for _, stopCat := range spec.Include.Gate.StopIf {
				if outcomeCategory != stopCat {
					continue
				}
				if requiredFailure != nil {
					result.Status, result.Outcome = engine.StepStatusFailed, engine.StepOutcomeFailed
					result.RequiredFailure, result.PublicOutputs = true, nil
					result.Error = fmt.Errorf("include %s: required child work is not successfully settled", step.ID)
					if requiredFailure.Error != nil {
						result.Error = fmt.Errorf("include %s: %w", step.ID, requiredFailure.Error)
					}
					return result, nil
				}
				result.Output["terminal"] = true
				result.Output["outcome_category"] = outcomeCategory
				if outcomeCode, ok := mergedVars["__run_outcome_code"].(string); ok {
					result.Output["outcome_code"] = outcomeCode
				}
				return result, nil
			}
		}
	}

	// Apply the include step's `capture:` rename map: copies vars[srcName]
	// (from the merged child scope OR the parent scope as a fallback) to
	// result.Vars[destName], overriding any same-named entry from the
	// implicit propagation above.
	for destName, srcName := range step.Capture {
		if strings.HasPrefix(srcName, "outputs.") {
			name := strings.TrimPrefix(srcName, "outputs.")
			value, ok := result.PublicOutputs[name]
			if !ok {
				return nil, fmt.Errorf("include %s: public output %q was not published", step.ID, name)
			}
			result.Vars[destName] = value
			continue
		}
		if val, ok := mergedVars[srcName]; ok {
			result.Vars[destName] = val
		} else if val, ok := parentVars[srcName]; ok {
			result.Vars[destName] = val
		}
	}

	return result, nil
}
