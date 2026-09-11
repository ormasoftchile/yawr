package replay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	internalexecutor "github.com/ormasoftchile/yawr/runtime/internal/executor"
	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/expr"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/tool"
	tracepkg "github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

// ReplayExecutor wraps a real executor and returns pre-recorded responses
// instead of executing real commands. Used when RunMode == "replay".
type ReplayExecutor struct {
	scenario  *Scenario
	kind      string
	evaluator expr.Evaluator
	tools     map[string]*schema.ToolDef
}

// NewReplayExecutor constructs a ReplayExecutor for the given kind, with
// no enum-check support (evaluator/tools unset -- see WithEnumChecks).
func NewReplayExecutor(kind string, scenario *Scenario) *ReplayExecutor {
	if scenario != nil {
		scenario.ensureMutex()
	}
	return &ReplayExecutor{kind: kind, scenario: scenario}
}

// WithEnumChecks attaches an expression evaluator and the plan's tool
// definitions so "tool" kind steps have their args rendered and
// enum-checked (ENUM-008) at the same moment the real ToolExecutor does
// (AR-ENUM-7, barbara-enum-mvp-implementation-gate.md R5), before any
// recorded fixture is consulted -- so a scenario cannot launder an
// off-enum value the real path would reject. Both arguments are optional
// (nil is safe: the check becomes a no-op, not a bypass of a known
// declaration -- see checkToolCallEnums). Returns e for chaining.
func (e *ReplayExecutor) WithEnumChecks(evaluator expr.Evaluator, tools map[string]*schema.ToolDef) *ReplayExecutor {
	e.evaluator = evaluator
	e.tools = tools
	return e
}

// Execute returns recorded outputs for the given step.
func (e *ReplayExecutor) Execute(ctx context.Context, step engine.ResolvedStep, vars map[string]any) (*engine.StepResult, error) {
	_ = ctx
	now := time.Now()

	switch e.kind {
	case "cli":
		return e.executeCLI(step, now)
	case "tool":
		return e.executeTool(step, vars, now)
	case "manual", "choice", "decision", "collector":
		return e.executeManual(step, now)
	default:
		return &engine.StepResult{
			StepID:      step.ID,
			Status:      engine.StepStatusCompleted,
			Outcome:     engine.StepOutcomeSuccess,
			StartedAt:   now,
			CompletedAt: now,
		}, nil
	}
}

func (e *ReplayExecutor) executeCLI(step engine.ResolvedStep, now time.Time) (*engine.StepResult, error) {
	argv := extractArgv(step)
	fixture, ok := e.scenario.MatchCommand(argv)
	if !ok {
		if e.scenario.AllowUnmatched {
			return &engine.StepResult{
				StepID:      step.ID,
				Status:      engine.StepStatusCompleted,
				Outcome:     engine.StepOutcomeSuccess,
				Output:      map[string]any{"stdout": "", "stderr": "", "exit_code": 0},
				StartedAt:   now,
				CompletedAt: now,
			}, nil
		}
		return &engine.StepResult{
			StepID:      step.ID,
			Status:      engine.StepStatusFailed,
			Outcome:     engine.StepOutcomeFailed,
			Error:       fmt.Errorf("replay: no matching command fixture for %v", argv),
			StartedAt:   now,
			CompletedAt: now,
		}, nil
	}

	status := engine.StepStatusCompleted
	outcome := engine.StepOutcomeSuccess
	if fixture.ExitCode != 0 {
		status = engine.StepStatusFailed
		outcome = engine.StepOutcomeFailed
	}

	return &engine.StepResult{
		StepID:  step.ID,
		Status:  status,
		Outcome: outcome,
		Output: map[string]any{
			"stdout":    fixture.Stdout,
			"stderr":    fixture.Stderr,
			"exit_code": fixture.ExitCode,
		},
		StartedAt:   now,
		CompletedAt: now,
	}, nil
}

func (e *ReplayExecutor) executeTool(step engine.ResolvedStep, vars map[string]any, now time.Time) (*engine.StepResult, error) {
	toolName := extractToolName(step)

	// ENUM-008 (AR-ENUM-7, barbara-enum-mvp-implementation-gate.md R5):
	// render this step's tool args exactly as the real ToolExecutor would
	// (GIS interpolation against vars) and reject an off-enum materialized
	// value before consulting any recorded fixture, so replay cannot
	// launder a value the real path would refuse to bind. Skipped only
	// when the tool definition or its action isn't known to this replay
	// registry (e.g. constructed without a plan's Tools map) -- in that
	// case there is no declaration to check against, not a bypass of a
	// known one.
	if aerr := e.checkToolCallEnums(step, vars); aerr != nil {
		return &engine.StepResult{
			StepID:      step.ID,
			Status:      engine.StepStatusFailed,
			Outcome:     engine.StepOutcomeFailed,
			Error:       aerr,
			StartedAt:   now,
			CompletedAt: now,
		}, nil
	}

	fixture, ok := e.scenario.Tools[toolName]
	if !ok {
		if e.scenario.AllowUnmatched {
			return &engine.StepResult{
				StepID:      step.ID,
				Status:      engine.StepStatusCompleted,
				Outcome:     engine.StepOutcomeSuccess,
				Output:      map[string]any{"response": "{}"},
				StartedAt:   now,
				CompletedAt: now,
			}, nil
		}
		return &engine.StepResult{
			StepID:      step.ID,
			Status:      engine.StepStatusFailed,
			Outcome:     engine.StepOutcomeFailed,
			Error:       fmt.Errorf("replay: no matching tool fixture for %s", toolName),
			StartedAt:   now,
			CompletedAt: now,
		}, nil
	}

	status := engine.StepStatusCompleted
	outcome := engine.StepOutcomeSuccess
	if fixture.ExitCode != 0 {
		status = engine.StepStatusFailed
		outcome = engine.StepOutcomeFailed
	}

	return &engine.StepResult{
		StepID:  step.ID,
		Status:  status,
		Outcome: outcome,
		Output: map[string]any{
			"response":  fixture.Response,
			"exit_code": fixture.ExitCode,
		},
		StartedAt:   now,
		CompletedAt: now,
	}, nil
}

func (e *ReplayExecutor) executeManual(step engine.ResolvedStep, now time.Time) (*engine.StepResult, error) {
	stepEvidence, ok := e.scenario.Evidence[step.ID]
	if !ok {
		return &engine.StepResult{
			StepID:      step.ID,
			Status:      engine.StepStatusCompleted,
			Outcome:     engine.StepOutcomeSuccess,
			Output:      map[string]any{"attestation": "replay_auto"},
			StartedAt:   now,
			CompletedAt: now,
		}, nil
	}

	output := make(map[string]any, len(stepEvidence))
	for name, fixture := range stepEvidence {
		switch fixture.Kind {
		case "text":
			output[name] = fixture.Value
		case "checklist":
			output["checklist"] = fixture.Items
		}
	}

	return &engine.StepResult{
		StepID:      step.ID,
		Status:      engine.StepStatusCompleted,
		Outcome:     engine.StepOutcomeSuccess,
		Output:      output,
		StartedAt:   now,
		CompletedAt: now,
	}, nil
}

func extractArgv(step engine.ResolvedStep) []string {
	spec, ok := step.Spec.(*schema.CLISpec)
	if !ok || spec == nil {
		return nil
	}
	argv := make([]string, 0, 1+len(spec.Args))
	if spec.Command != "" {
		argv = append(argv, spec.Command)
	}
	argv = append(argv, spec.Args...)
	return argv
}

func extractToolName(step engine.ResolvedStep) string {
	spec, ok := step.Spec.(*schema.ToolCallSpec)
	if !ok || spec == nil {
		return ""
	}
	if spec.Tool.Action != "" {
		return fmt.Sprintf("%s/%s", spec.Tool.Name, spec.Tool.Action)
	}
	return spec.Tool.Name
}

// checkToolCallEnums renders this tool-call step's args (GIS interpolation
// against vars, identical to internal/executor.ToolExecutor.Execute) and
// checks them against the declared action's enum-constrained args via the
// same internalexecutor.CheckArgEnums the real (non-replay) path uses
// (AR-ENUM-7, barbara-enum-mvp-implementation-gate.md R5). Returns nil
// (no-op, not a bypass) when the step isn't a tool call, or when this
// ReplayExecutor wasn't constructed with a tools map/evaluator, or when
// the referenced tool/action isn't present in it -- there is genuinely no
// declaration to check in those cases.
func (e *ReplayExecutor) checkToolCallEnums(step engine.ResolvedStep, vars map[string]any) error {
	spec, ok := step.Spec.(*schema.ToolCallSpec)
	if !ok || spec == nil || e.tools == nil {
		return nil
	}
	toolDef, ok := e.tools[spec.Tool.Name]
	if !ok || toolDef == nil {
		return nil
	}
	action := spec.Tool.Action
	if action == "" {
		action = "run"
	}
	schemaAction, ok := toolDef.Actions[action]
	if !ok || schemaAction == nil || len(schemaAction.Args) == 0 {
		return nil
	}

	args := make(map[string]any, len(spec.Tool.Args))
	for k, v := range spec.Tool.Args {
		if s, ok := v.(string); ok {
			resolved := s
			if e.evaluator != nil {
				if rendered, err := e.evaluator.Eval(s, vars); err == nil {
					resolved = rendered
				}
			}
			args[k] = resolved
			continue
		}
		args[k] = v
	}

	runtimeAction := &tool.ToolAction{Args: make(map[string]*tool.ArgDef, len(schemaAction.Args))}
	for name, argDef := range schemaAction.Args {
		if argDef == nil {
			continue
		}
		runtimeAction.Args[name] = &tool.ArgDef{
			Presentation: argDef.Presentation,
			Type:         argDef.Type,
			Required:     argDef.Required,
			Default:      argDef.Default,
			Enum:         argDef.Enum,
		}
	}
	return internalexecutor.CheckArgEnums(runtimeAction, args)
}

// ReplayExecutorRegistry wraps an existing registry, replacing all executors
// with ReplayExecutor instances that use scenario fixtures.
type ReplayExecutorRegistry struct {
	inner             engine.ExecutorRegistry
	scenario          *Scenario
	evaluator         expr.Evaluator
	condition         expr.ConditionEvaluator
	tools             map[string]*schema.ToolDef
	scheduler         *replayBoundaryScheduler
	evidenceHook      engine.EvidenceHook
	evidenceHookBound bool
	// dynamicIncludeExec, when non-nil, is returned for "include" lookups
	// instead of a plain ReplayExecutor. Used for pin-based dynamic include
	// re-binding during replay (Barbara §8.3).
	dynamicIncludeExec engine.StepExecutor
}

func (r *ReplayExecutorRegistry) WithEvidenceHook(hook engine.EvidenceHook) *ReplayExecutorRegistry {
	r.evidenceHook = hook
	r.evidenceHookBound = true
	return r
}

// NewReplayExecutorRegistry constructs a registry wrapper for replay, with
// no enum-check support (see NewReplayExecutorRegistryWithEnumChecks).
func NewReplayExecutorRegistry(inner engine.ExecutorRegistry, scenario *Scenario) *ReplayExecutorRegistry {
	if scenario != nil {
		scenario.PatternFixtures = true
		scenario.StrictExactFixtures = false
	}
	return &ReplayExecutorRegistry{inner: inner, scenario: scenario, scheduler: newReplayBoundaryScheduler(scenario)}
}

// NewReplayExecutorRegistryWithEnumChecks constructs a registry wrapper
// that also carries the plan's tool definitions and an evaluator through
// to every "tool" kind ReplayExecutor it produces (R5): every replayed
// tool-call step is enum-checked (ENUM-008) exactly as the real path would
// check it, at the moment its args are bound, before any fixture is
// consulted.
func NewReplayExecutorRegistryWithEnumChecks(inner engine.ExecutorRegistry, scenario *Scenario, evaluator expr.Evaluator, tools map[string]*schema.ToolDef) *ReplayExecutorRegistry {
	if scenario != nil {
		scenario.PatternFixtures = true
		scenario.StrictExactFixtures = false
	}
	return &ReplayExecutorRegistry{
		inner: inner, scenario: scenario, evaluator: evaluator, tools: tools,
		scheduler: newReplayBoundaryScheduler(scenario),
	}
}

// NewReplayExecutorRegistryWithPins is like NewReplayExecutorRegistryWithEnumChecks
// but intercepts "include" steps to re-bind dynamic includes from their
// pinned AbsPath instead of re-resolving against the current catalog (§8.3).
// When pins is empty, this behaves identically to NewReplayExecutorRegistryWithEnumChecks.
func NewReplayExecutorRegistryWithPins(
	inner engine.ExecutorRegistry,
	scenario *Scenario,
	evaluator expr.Evaluator,
	tools map[string]*schema.ToolDef,
	condition expr.ConditionEvaluator,
	pins []schema.LockedDynamicInclude,
	traceWriter tracepkg.TraceWriter,
	runID string,
) *ReplayExecutorRegistry {
	if scenario != nil && !scenario.StrictExactFixtures && len(scenario.StepResponses) == 0 &&
		len(scenario.HostActionResponses) == 0 && len(scenario.InteractionAnswers) == 0 &&
		len(scenario.Approvals) == 0 && len(scenario.WaitEvents) == 0 &&
		(len(scenario.Commands) > 0 || len(scenario.Tools) > 0 || scenario.AllowUnmatched) {
		scenario.PatternFixtures = true
	}
	r := &ReplayExecutorRegistry{
		inner: inner, scenario: scenario, evaluator: evaluator, tools: tools,
		scheduler: newReplayBoundaryScheduler(scenario),
	}
	r.condition = condition
	r.dynamicIncludeExec = &DynamicIncludeReplayExecutor{
		pins:        buildPinIndex(pins),
		innerExec:   inner.Lookup("include"),
		fallback:    NewReplayExecutor("include", scenario),
		traceWriter: traceWriter,
		runID:       runID,
		evaluator:   evaluator,
		registry:    r,
	}
	return r
}

func (r *ReplayExecutorRegistry) Register(kind string, exec engine.StepExecutor) {
	if r.inner != nil {
		r.inner.Register(kind, exec)
	}
}

func (r *ReplayExecutorRegistry) MaterializeLazyIncludes(ctx context.Context, plan *engine.ExecutionPlan) error {
	return internalexecutor.MaterializeLazyIncludes(ctx, r.inner, plan)
}

func (r *ReplayExecutorRegistry) Lookup(kind string) engine.StepExecutor {
	if r.inner == nil {
		return nil
	}
	inner := r.inner.Lookup(kind)
	if inner == nil {
		return nil
	}
	if r.scheduler != nil && r.scheduler.initErr != nil {
		return &replayErrorExecutor{err: r.scheduler.initErr}
	}
	if kind == "include" && r.dynamicIncludeExec != nil {
		return r.dynamicIncludeExec
	}
	if kind == "tool" {
		return &replayToolExecutor{registry: r, inner: inner}
	}
	switch kind {
	case "branch", "iterate", "compensate", "handoff", "assert", "noop", "end", "display":
		return &replayControlExecutor{inner: inner, registry: r}
	case "choice":
		return internalexecutor.NewChoiceExecutor(r.scheduler, r.evaluator)
	case "decision":
		return internalexecutor.NewDecisionExecutor(r.scheduler, r.evaluator)
	case "collector":
		return internalexecutor.NewCollectorExecutor(r.scheduler, r.evaluator, r.condition)
	case "host_action":
		return internalexecutor.NewHostActionExecutor(r.scheduler, r.evaluator)
	case "approve":
		if r.scheduler != nil && r.scheduler.hasSavedStepKind(kind) {
			return &replaySavedStepExecutor{kind: kind, scheduler: r.scheduler, inner: inner}
		}
		return internalexecutor.NewApproveExecutor(r.scheduler)
	}
	if r.scheduler != nil && r.scheduler.hasSavedStepKind(kind) {
		return &replaySavedStepExecutor{kind: kind, scheduler: r.scheduler, inner: inner}
	}
	switch kind {
	case "cli":
		if r.scheduler != nil && len(r.scheduler.steps) > 0 {
			return &replaySavedStepExecutor{kind: kind, scheduler: r.scheduler, inner: inner}
		}
	case "manual", "prompt":
		return &replaySavedStepExecutor{kind: kind, scheduler: r.scheduler, inner: inner}
	}
	if r.scenario != nil && r.scenario.PatternFixtures {
		return NewReplayExecutor(kind, r.scenario).WithEnumChecks(r.evaluator, r.tools)
	}
	return &replaySavedStepExecutor{kind: kind, scheduler: r.scheduler, inner: inner}
}

type replayToolExecutor struct {
	registry *ReplayExecutorRegistry
	inner    engine.StepExecutor
}

func (executor *replayToolExecutor) Execute(
	ctx context.Context,
	step engine.ResolvedStep,
	vars map[string]any,
) (*engine.StepResult, error) {
	if executor.registry.scenario != nil && executor.registry.scenario.PatternFixtures {
		if pure, ok := executor.inner.(interface {
			IsPureRunbookSubstitution(context.Context, engine.ResolvedStep, map[string]any) (bool, error)
		}); ok {
			isPure, err := pure.IsPureRunbookSubstitution(ctx, step, vars)
			if err != nil {
				return nil, fmt.Errorf("replay: resolve compatibility tool boundary: %w", err)
			}
			if isPure {
				return executor.inner.Execute(executor.registry.recursiveContext(ctx), step, vars)
			}
		}
		return NewReplayExecutor("tool", executor.registry.scenario).
			WithEnumChecks(executor.registry.evaluator, executor.registry.tools).
			Execute(ctx, step, vars)
	}
	definition, action, err := executor.frozenDefinition(step, vars)
	if err != nil {
		return nil, engine.NewReplayBoundaryError(err)
	}
	if action.Execute != nil && action.Execute.IsSubstitution() {
		if action.FrozenSubstitution == nil {
			return nil, engine.NewReplayBoundaryError(errors.New("replay: pure tool substitution has no frozen executable closure"))
		}
		frozen, ok := executor.inner.(interface {
			ExecuteFrozenSubstitution(context.Context, engine.ResolvedStep, map[string]any, *schema.ToolDef) (*engine.StepResult, error)
		})
		if !ok {
			return nil, engine.NewReplayBoundaryError(errors.New("replay: tool executor cannot execute frozen substitutions"))
		}
		return frozen.ExecuteFrozenSubstitution(executor.registry.recursiveContext(ctx), step, vars, definition)
	}
	if executor.registry.scheduler != nil && executor.registry.scheduler.hasSavedStepKind("tool") {
		result, fixtureErr := executor.registry.scheduler.savedStep(ctx, "tool", step)
		if fixtureErr != nil {
			return result, fixtureErr
		}
		validator, ok := executor.inner.(interface {
			ValidateFrozenSavedResult(context.Context, engine.ResolvedStep, map[string]any, *engine.StepResult, *schema.ToolDef) (*engine.StepResult, error)
		})
		if !ok {
			return nil, engine.NewReplayBoundaryError(errors.New("replay: tool executor cannot validate frozen results"))
		}
		return validator.ValidateFrozenSavedResult(ctx, step, vars, result, definition)
	}
	return nil, engine.NewReplayBoundaryError(errors.New("replay: no exact tool fixture"))
}

func (executor *replayToolExecutor) frozenDefinition(
	step engine.ResolvedStep,
	vars map[string]any,
) (*schema.ToolDef, *schema.ToolAction, error) {
	spec, ok := step.Spec.(*schema.ToolCallSpec)
	if !ok || spec == nil {
		return nil, nil, fmt.Errorf("replay: invalid tool spec for step %s", step.ID)
	}
	toolName := spec.Tool.Name
	var err error
	if executor.registry.evaluator != nil {
		toolName, err = executor.registry.evaluator.Eval(toolName, vars)
		if err != nil {
			return nil, nil, fmt.Errorf("replay: resolve frozen tool name: %w", err)
		}
	}
	definition := executor.registry.tools[toolName]
	if definition == nil {
		return nil, nil, fmt.Errorf("replay: frozen tool %q is unavailable", toolName)
	}
	actionName := spec.Tool.Action
	if actionName == "" {
		actionName = "run"
	}
	if executor.registry.evaluator != nil {
		actionName, err = executor.registry.evaluator.Eval(actionName, vars)
		if err != nil {
			return nil, nil, fmt.Errorf("replay: resolve frozen tool action: %w", err)
		}
	}
	action := definition.Actions[actionName]
	if action == nil {
		return nil, nil, fmt.Errorf("replay: frozen action %s#%s is unavailable", toolName, actionName)
	}
	return definition, action, nil
}

type replayErrorExecutor struct{ err error }

func (executor *replayErrorExecutor) Execute(context.Context, engine.ResolvedStep, map[string]any) (*engine.StepResult, error) {
	return nil, executor.err
}

type replaySavedStepExecutor struct {
	kind      string
	scheduler *replayBoundaryScheduler
	inner     engine.StepExecutor
}

func (executor *replaySavedStepExecutor) Execute(
	ctx context.Context,
	step engine.ResolvedStep,
	vars map[string]any,
) (*engine.StepResult, error) {
	result, err := executor.scheduler.savedStep(ctx, executor.kind, step)
	if err != nil || executor.kind != "tool" {
		return result, err
	}
	validator, ok := executor.inner.(interface {
		ValidateSavedResult(context.Context, engine.ResolvedStep, map[string]any, *engine.StepResult) (*engine.StepResult, error)
	})
	if !ok {
		return nil, errors.New("replay: tool executor has no saved-result validator")
	}
	return validator.ValidateSavedResult(ctx, step, vars, result)
}

type replayControlExecutor struct {
	inner    engine.StepExecutor
	registry *ReplayExecutorRegistry
}

func (executor *replayControlExecutor) Execute(
	ctx context.Context,
	step engine.ResolvedStep,
	vars map[string]any,
) (*engine.StepResult, error) {
	return executor.inner.Execute(executor.registry.recursiveContext(ctx), step, vars)
}

func (r *ReplayExecutorRegistry) recursiveContext(ctx context.Context) context.Context {
	ctx = internalexecutor.WithExecutorRegistry(ctx, r)
	ctx = internalexecutor.WithRunApprovalGate(ctx, r.scheduler)
	ctx = internalexecutor.WithRunEventDispatcher(ctx, r.scheduler)
	if r.evidenceHookBound {
		ctx = internalexecutor.WithRunEvidenceHook(ctx, r.evidenceHook)
	}
	return ctx
}

// DynamicIncludeReplayExecutor handles include steps during replay.
// For dynamic include steps (spec.Include.IsDynamic() == true) it:
//  1. Looks up the pin record by StepID (no catalog re-resolution).
//  2. Verifies the on-disk file digest and emits replay/dynamicIncludeDrift
//     (non-fatal) if the file has changed.
//  3. Transforms the step into a lazy-include (LazyRunbookPath = pin.AbsPath)
//     so the inner IncludeExecutor loads from the pinned path via its loader.
//
// For non-dynamic include steps (static / lazy), it falls back to the
// ReplayExecutor, returning recorded fixture data.
type DynamicIncludeReplayExecutor struct {
	pins         map[string][]schema.LockedDynamicInclude
	consumedPins map[string]map[int]bool
	invocations  map[string]int
	pinMu        sync.Mutex
	innerExec    engine.StepExecutor // real IncludeExecutor from inner registry
	fallback     *ReplayExecutor     // for non-dynamic includes
	traceWriter  tracepkg.TraceWriter
	runID        string
	evaluator    expr.Evaluator
	registry     *ReplayExecutorRegistry
}

func (e *DynamicIncludeReplayExecutor) Execute(ctx context.Context, step engine.ResolvedStep, vars map[string]any) (*engine.StepResult, error) {
	spec, ok := step.Spec.(*schema.IncludeSpec)
	if !ok || spec == nil {
		return nil, fmt.Errorf("replay: invalid include spec for step %s", step.ID)
	}
	if !spec.Include.IsDynamic() {
		if e.innerExec == nil {
			return nil, fmt.Errorf("replay: no include executor for static include step %s", step.ID)
		}
		if e.registry != nil {
			ctx = e.registry.recursiveContext(ctx)
		}
		return e.innerExec.Execute(ctx, step, vars)
	}

	invocation := e.nextInvocation(ctx, step)
	pin, pinFound, err := e.nextPin(ctx, step, invocation)
	if err != nil {
		return nil, engine.NewReplayBoundaryError(err)
	}
	if !pinFound {
		key := dynamicIncludeOccurrenceKey(ctx, step)
		structuralPath := engine.DynamicIncludeStructuralPathFromContext(ctx)
		if e.fallback != nil && e.fallback.scenario != nil &&
			len(e.fallback.scenario.DynamicIncludeNotFound) > 0 {
			renderedRef, renderErr := replayDynamicIncludeRef(e.evaluator, spec.Include.RunbookRef, vars)
			if renderErr != nil {
				return nil, engine.NewReplayBoundaryError(renderErr)
			}
			fixture, found, fixtureErr := e.fallback.scenario.MatchDynamicIncludeNotFound(
				step.ID, key, invocation, renderedRef, structuralPath,
			)
			if fixtureErr != nil {
				return nil, engine.NewReplayBoundaryError(fixtureErr)
			}
			if found {
				if spec.Include.OnNotFound != schema.OnNotFoundContinue {
					return nil, engine.NewReplayBoundaryError(errors.New("replay: dynamic include not-found policy changed"))
				}
				now := time.Now()
				warning := fmt.Sprintf(
					"dynamic include: step %s: runbook_ref %q was not found in the catalog; on_not_found: continue skipped the child runbook",
					step.ID, fixture.RenderedRef,
				)
				return &engine.StepResult{
					StepID: step.ID, Status: engine.StepStatusSkipped, Outcome: engine.StepOutcomeSkipped,
					Output: map[string]any{
						"warning": warning, "stderr": warning, "skip_reason": "include_not_found",
					},
					Vars: map[string]any{
						"runbook_found": false, "runbook_skipped_reason": fixture.Reason,
					},
					StartedAt: now, CompletedAt: now,
				}, nil
			}
		}
		if e.fallback == nil || e.fallback.scenario == nil || !e.fallback.scenario.AllowUnmatched {
			return nil, engine.NewReplayBoundaryError(errors.New("replay: dynamic include has no exact pinned occurrence"))
		}
		// Step was not reached in the original run — treat as skipped.
		now := time.Now()
		return &engine.StepResult{
			StepID:      step.ID,
			Status:      engine.StepStatusSkipped,
			Outcome:     engine.StepOutcomeSkipped,
			StartedAt:   now,
			CompletedAt: now,
		}, nil
	}

	modifiedSpec := &schema.IncludeSpec{
		Include: schema.IncludeConfig{
			With: spec.Include.With,
			Gate: spec.Include.Gate,
			When: spec.Include.When,
			// RunbookRef intentionally empty: IsDynamic() → false
		},
	}
	if len(pin.ExecutableClosure) > 0 {
		if pin.RunbookID == "" || pin.RunbookName == "" || len(pin.RunbookContentHash) != 64 {
			return nil, engine.NewReplayBoundaryError(errors.New("replay: pinned dynamic include has no authored runbook identity"))
		}
		flow, closureErr := plansnapshot.RestoreFlowClosure(pin.ExecutableClosure)
		if closureErr != nil {
			return nil, engine.NewReplayBoundaryError(fmt.Errorf("replay: restore pinned dynamic include %q: %w", pin.QualifiedID, closureErr))
		}
		modifiedSpec.ResolvedSteps = flow
		modifiedSpec.ResolvedRunbookPath = pin.AbsPath
		modifiedSpec.ResolvedRunbookID = pin.RunbookID
		modifiedSpec.ResolvedRunbookName = pin.RunbookName
		modifiedSpec.ResolvedRunbookContentHash = pin.RunbookContentHash
		modifiedSpec.ResolvedInputs = pin.ResolvedInputs
		modifiedSpec.ResolvedBindings = pin.ResolvedBindings
		modifiedSpec.ResolvedOutputs = pin.ResolvedOutputs
		modifiedSpec.ResolvedGovernance = pin.ResolvedGovernance
	} else {
		actual, digErr := fileDigestSHA256(pin.AbsPath)
		if digErr != nil {
			return nil, engine.NewReplayBoundaryError(fmt.Errorf("replay: read pinned dynamic include %q: %w", pin.QualifiedID, digErr))
		}
		if actual != pin.FileDigest {
			e.emitDrift(pin, actual)
			return nil, engine.NewReplayBoundaryError(fmt.Errorf("replay: pinned dynamic include %q digest changed", pin.QualifiedID))
		}
		modifiedSpec.LazyRunbookPath = pin.AbsPath
		modifiedSpec.LazyRunbookDigest = pin.FileDigest
	}
	modifiedStep := step
	modifiedStep.Spec = modifiedSpec

	if e.innerExec == nil {
		return nil, engine.NewReplayBoundaryError(fmt.Errorf("replay: no include executor for dynamic include step %s", step.ID))
	}
	if e.registry != nil {
		ctx = e.registry.recursiveContext(ctx)
	}
	return e.innerExec.Execute(ctx, modifiedStep, vars)
}

func replayDynamicIncludeRef(evaluator expr.Evaluator, template string, vars map[string]any) (string, error) {
	if evaluator != nil {
		rendered, err := evaluator.Eval(template, vars)
		if err != nil {
			return "", fmt.Errorf("replay: render dynamic include reference: %w", err)
		}
		return rendered, nil
	}
	if strings.Contains(template, "${") {
		return "", errors.New("replay: dynamic include reference requires an evaluator")
	}
	return template, nil
}

func (e *DynamicIncludeReplayExecutor) nextPin(
	ctx context.Context,
	step engine.ResolvedStep,
	invocation int,
) (schema.LockedDynamicInclude, bool, error) {
	key := dynamicIncludeOccurrenceKey(ctx, step)
	pins := e.pins[key]
	if len(pins) == 0 && key != step.ID {
		key = step.ID
		pins = e.pins[key]
	}
	if len(pins) == 0 {
		return schema.LockedDynamicInclude{}, false, nil
	}
	if len(pins) > 1 && pins[0].QualifiedNodeID == "" {
		return schema.LockedDynamicInclude{}, false, errors.New("replay: repeated dynamic include pins lack occurrence identity")
	}
	structuralPath := engine.DynamicIncludeStructuralPathFromContext(ctx)
	e.pinMu.Lock()
	defer e.pinMu.Unlock()
	if e.consumedPins == nil {
		e.consumedPins = make(map[string]map[int]bool)
	}
	if e.consumedPins[key] == nil {
		e.consumedPins[key] = make(map[int]bool)
	}
	for index, pin := range pins {
		if e.consumedPins[key][index] || pin.Invocation > 0 && pin.Invocation != invocation ||
			!sameDynamicStructuralPath(pin.StructuralPath, structuralPath) {
			continue
		}
		if pin.QualifiedNodeID != "" && pin.QualifiedNodeID != key {
			return schema.LockedDynamicInclude{}, false, errors.New("replay: dynamic include pin occurrence identity is invalid")
		}
		e.consumedPins[key][index] = true
		return pin, true, nil
	}
	return schema.LockedDynamicInclude{}, false, nil
}

func (e *DynamicIncludeReplayExecutor) nextInvocation(ctx context.Context, step engine.ResolvedStep) int {
	identity, _ := json.Marshal(struct {
		QualifiedNodeID string                               `json:"qualified_node_id"`
		StructuralPath  []schema.DynamicIncludeFrameIdentity `json:"structural_path,omitempty"`
	}{
		QualifiedNodeID: dynamicIncludeOccurrenceKey(ctx, step),
		StructuralPath:  engine.DynamicIncludeStructuralPathFromContext(ctx),
	})
	e.pinMu.Lock()
	defer e.pinMu.Unlock()
	if e.invocations == nil {
		e.invocations = make(map[string]int)
	}
	key := string(identity)
	e.invocations[key]++
	return e.invocations[key]
}

func dynamicIncludeOccurrenceKey(ctx context.Context, step engine.ResolvedStep) string {
	if boundary, found := engine.DispatchExecutionBoundaryFromContext(ctx); found && boundary.QualifiedNodeID != "" {
		return boundary.QualifiedNodeID
	}
	if callPath := engine.DebugCallPathFromContext(ctx); len(callPath) > 0 {
		return engine.DebugNodeID(callPath, step.ID)
	}
	return step.ID
}

func sameDynamicStructuralPath(
	left []schema.DynamicIncludeFrameIdentity,
	right []schema.DynamicIncludeFrameIdentity,
) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func (e *DynamicIncludeReplayExecutor) emitDrift(pin schema.LockedDynamicInclude, actualDigest string) {
	if e.traceWriter == nil {
		return
	}
	payload, err := json.Marshal(tracepkg.ReplayDynamicIncludeDriftPayload{
		StepID:         pin.StepID,
		QualifiedID:    pin.QualifiedID,
		ExpectedDigest: pin.FileDigest,
		ActualDigest:   actualDigest,
	})
	if err != nil {
		return
	}
	_ = e.traceWriter.Append(tracepkg.TraceEvent{
		RunID:     e.runID,
		Kind:      tracepkg.EventKindReplayDynamicIncludeDrift,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Payload:   payload,
	})
}
