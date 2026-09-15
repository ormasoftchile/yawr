package executor

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/expr"
	"github.com/ormasoftchile/yawr/runtime/pkg/governance"
	"github.com/ormasoftchile/yawr/runtime/pkg/hostaction"
	"github.com/ormasoftchile/yawr/runtime/pkg/input"
	parserpkg "github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

// ErrSubRunFailed is a sentinel indicating that a sub-engine run reached a
// terminal failed state (on_error=stop fired inside the child). It is wrapped
// by runSubStepsViaEngine and detected by include/branch executors so they can
// return a failed StepResult (not an infrastructure error) to the parent
// engine, which then applies resolveOnError on the container step.
var ErrSubRunFailed = errors.New("sub-run failed")

// SubStepParent carries structural-parent context to a SubStepRunner so that
// sub-engine ResolvedSteps can be tagged with parent metadata for trace events.
type SubStepParent struct {
	RunbookInvocation *schema.RunbookInvocation
	InvocationState   *RunbookInvocationState
	ID                string // ID of the iterate/branch/compensate/include parent
	Kind              string // "iterate" | "branch" | "compensate" | "include"
	BranchLabel       string // for "branch" kind: the matched arm's label
	IncludeAlias      string // for "include" kind: the alias under which the runbook was imported
	RunbookPath       string // immutable declaring runbook path for include children
	RootScopeID       string // immutable lexical owner of the child flow
	IterationIndex    int    // 1-based for iterate frames; zero outside iteration
	NestDepth         int    // visual nesting depth to apply to child ResolvedSteps
}

type RunbookInvocationState struct {
	Bindings map[string]any
}

// SubStepRunner executes nested steps for branch/iterate/compensate flows.
// The parent argument carries structural context so emitted step/started events
// can record parent_step_id/parent_kind/branch_label.
type SubStepRunner func(ctx context.Context, parent SubStepParent, steps []schema.FlowNode, vars map[string]any) ([]*engine.StepResult, error)

// MapRegistry is a concrete ExecutorRegistry implementation backed by a map.
type MapRegistry struct {
	mu    sync.RWMutex
	execs map[string]engine.StepExecutor
}

// Ensure MapRegistry implements engine.ExecutorRegistry.
var _ engine.ExecutorRegistry = (*MapRegistry)(nil)

// NewMapRegistry constructs an empty registry.
func NewMapRegistry() *MapRegistry {
	return &MapRegistry{execs: make(map[string]engine.StepExecutor)}
}

// Register associates a step kind with an executor.
func (r *MapRegistry) Register(kind string, exec engine.StepExecutor) {
	r.mu.Lock()
	r.execs[kind] = exec
	r.mu.Unlock()
}

// Lookup returns the executor for a step kind.
func (r *MapRegistry) Lookup(kind string) engine.StepExecutor {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.execs[kind]
}

// RegistryConfig bundles dependencies for the default executor set.
type RegistryConfig struct {
	Platform             platform.Platform
	Evaluator            expr.Evaluator
	ConditionEvaluator   expr.ConditionEvaluator
	PromptProvider       input.PromptProvider
	HostActionProvider   hostaction.Provider
	InputProvider        input.InputProvider
	ApprovalGate         governance.ApprovalGate
	SubStepRunner        SubStepRunner
	IterateSubStepRunner SubStepRunner // if set, used by iterate executor (supports include expansion)
	ToolRuntime          tool.ToolRuntime
	Output               io.Writer         // output target for display steps; defaults to os.Stdout if nil
	LazyRunbookLoader    LazyRunbookLoader // optional: required only if any included runbook is loaded lazily
	// DynamicIncludeResolver resolves dynamic include runbook_ref values at
	// execution time against the frozen catalog. Optional; dynamic includes
	// fail with a clear step error when nil.
	DynamicIncludeResolver DynamicIncludeResolver
	// MaxIncludeDepth caps the runtime dynamic include depth. Defaults to
	// defaultMaxIncludeDepth (10) when zero.
	MaxIncludeDepth int
	// PinRecorder is called after each successful dynamic include resolution
	// so the engine can record the pin for replay/resume.
	// Nil is safe: pinning is skipped.
	PinRecorder PinRecorder
	// SubstitutionParser, when non-nil, enables the tool executor to run
	// execute.kind: runbook (substituted) actions: it parses the
	// substitute runbook file via pkg/pkgsubst.Plan's validation path. When
	// nil, the tool executor falls back to plain process invocation and a
	// substituted action encountered at run time fails with a clear step
	// error, matching the LazyRunbookLoader==nil convention above.
	SubstitutionParser parserpkg.Parser
}

// NewDefaultRegistry constructs a MapRegistry pre-loaded with all Phase 5 executors.
// NOTE: "parallel" and "wait_for_event" are intentionally NOT registered.
func NewDefaultRegistry(cfg RegistryConfig) *MapRegistry {
	r := NewMapRegistry()
	r.Register("cli", NewCLIExecutor(cfg.Platform, cfg.Evaluator))
	r.Register("tool", NewToolExecutorWithSubstitution(cfg.ToolRuntime, cfg.Evaluator, cfg.SubStepRunner, cfg.SubstitutionParser, cfg.ApprovalGate))
	include := newIncludeExecutorFull(cfg.Evaluator, cfg.SubStepRunner, cfg.LazyRunbookLoader, cfg.DynamicIncludeResolver, cfg.MaxIncludeDepth, cfg.PinRecorder).WithApprovalGate(cfg.ApprovalGate)
	if executor, ok := r.Lookup("tool").(*ToolExecutor); ok {
		include.presentationTools = executor.freezeDynamicPresentationTools
	}
	r.Register("include", include)
	r.Register("choice", NewChoiceExecutor(cfg.PromptProvider, cfg.Evaluator))
	r.Register("decision", NewDecisionExecutor(cfg.PromptProvider, cfg.Evaluator))
	r.Register("collector", NewCollectorExecutor(cfg.PromptProvider, cfg.Evaluator, cfg.ConditionEvaluator))
	r.Register("host_action", NewHostActionExecutor(cfg.HostActionProvider, cfg.Evaluator))
	r.Register("handoff", NewHandoffExecutor(cfg.Evaluator))
	r.Register("prompt", NewPromptExecutor(cfg.InputProvider))
	r.Register("branch", NewBranchExecutor(cfg.ConditionEvaluator, cfg.SubStepRunner))
	iterRunner := cfg.SubStepRunner
	if cfg.IterateSubStepRunner != nil {
		iterRunner = cfg.IterateSubStepRunner
	}
	r.Register("iterate", NewIterateExecutor(cfg.Evaluator, cfg.ConditionEvaluator, iterRunner))
	r.Register("approve", NewApproveExecutor(cfg.ApprovalGate))
	r.Register("assert", NewAssertExecutor(cfg.Evaluator))
	r.Register("compensate", NewCompensateExecutor())
	r.Register("end", NewEndExecutor())
	r.Register("noop", NewNoopExecutor(cfg.Evaluator))
	r.Register("assign", &AssignExecutor{})
	r.Register("results", &ResultsExecutor{})
	out := cfg.Output
	if out == nil {
		out = os.Stdout
	}
	r.Register("display", NewDisplayExecutor(cfg.Evaluator, out))
	return r
}
