package adapter

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	internalengine "github.com/ormasoftchile/yawr/runtime/internal/engine"
	internaleventbus "github.com/ormasoftchile/yawr/runtime/internal/eventbus"
	internalevidence "github.com/ormasoftchile/yawr/runtime/internal/evidence"
	internalexecutor "github.com/ormasoftchile/yawr/runtime/internal/executor"
	internalexpr "github.com/ormasoftchile/yawr/runtime/internal/expr"
	internalgovernance "github.com/ormasoftchile/yawr/runtime/internal/governance"
	internalinput "github.com/ormasoftchile/yawr/runtime/internal/input"
	internalplanner "github.com/ormasoftchile/yawr/runtime/internal/planner"
	internalreplay "github.com/ormasoftchile/yawr/runtime/internal/replay"
	internalrunstore "github.com/ormasoftchile/yawr/runtime/internal/runstore"
	internaltool "github.com/ormasoftchile/yawr/runtime/internal/tool"
	internaltrace "github.com/ormasoftchile/yawr/runtime/internal/trace"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	eventbuspkg "github.com/ormasoftchile/yawr/runtime/pkg/eventbus"
	exprpkg "github.com/ormasoftchile/yawr/runtime/pkg/expr"
	"github.com/ormasoftchile/yawr/runtime/pkg/governance"
	"github.com/ormasoftchile/yawr/runtime/pkg/hostaction"
	inputpkg "github.com/ormasoftchile/yawr/runtime/pkg/input"
	otelPkg "github.com/ormasoftchile/yawr/runtime/pkg/otel"
	otlpadapter "github.com/ormasoftchile/yawr/runtime/pkg/otel/adapter"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
	tracepkg "github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

// BuildEngineConfig constructs a production engine configuration.
// The returned shutdown func must be called when the engine is done to flush
// traces, close run-store writers, and flush any pending OTel spans. It is
// always non-nil and safe to defer.
func BuildEngineConfig(ctx context.Context, opts WireOptions) (engine.EngineConfig, func(), error) {
	opts = opts.withDefaults()
	if opts.Mode == string(engine.RunModeRouteTest) && opts.RouteTestScheduler == nil {
		return engine.EngineConfig{}, func() {}, fmt.Errorf("adapter: route-test scheduler is required")
	}

	tracePath := opts.TraceFile
	if tracePath == "" {
		tracePath = filepath.Join(opts.TraceDir, "trace.jsonl")
	}
	traceWriter, err := internaltrace.NewJSONLWriter(tracePath)
	if err != nil {
		return engine.EngineConfig{}, func() {}, err
	}
	store := internalrunstore.NewDirRunStore(opts.RunDir)

	toolRegistry, err := buildToolRegistry(opts.ToolScanDir, opts.ExcludeTestTools)
	if err != nil {
		return engine.EngineConfig{}, func() {}, err
	}
	// Apply extra tool-scan paths supplied by --package-map. Scan each path
	// AFTER the base scan and override any same-named tool so the explicitly
	// declared binding wins, independent of filesystem walk order.
	for _, extraPath := range opts.ExtraToolScanPaths {
		extraDefs, scanErr := internaltool.ScanDirWithoutTests(extraPath)
		if scanErr != nil {
			continue // non-existent or unreadable paths are silently skipped
		}
		for _, d := range extraDefs {
			toolRegistry.Override(d)
		}
	}
	toolRuntime := internaltool.NewDefaultToolRuntime(toolRegistry)
	if opts.Profile != nil {
		toolRuntime.SetProfile(opts.Profile)
	}

	evaluator := &internalexpr.TemplateEvaluator{}
	conditionEvaluator := internalexpr.NewSimpleConditionEvaluator(evaluator)

	promptProvider := buildPromptProvider(opts)
	if opts.PromptProviderOverride != nil {
		promptProvider = opts.PromptProviderOverride
	}
	if opts.RouteTestScheduler != nil {
		promptProvider = opts.RouteTestScheduler
		opts.HostActionProvider = opts.RouteTestScheduler
	}
	inputProvider := buildInputProvider(opts)
	approvalGate := buildApprovalGate(opts)
	if opts.ApprovalGateOverride != nil {
		approvalGate = opts.ApprovalGateOverride
	}
	if opts.RouteTestScheduler != nil {
		approvalGate = opts.RouteTestScheduler
	}
	if opts.Mode == string(engine.RunModeReal) {
		promptProvider = internalinput.NewDurablePromptProvider(promptProvider)
		approvalGate = internalgovernance.NewDurableApprovalGate(approvalGate)
	}

	dispatcher := internaleventbus.NewDispatcher()
	if opts.WebhookAddr != "" {
		listener := internaleventbus.NewWebhookListener(opts.WebhookAddr, dispatcher)
		go func() { _ = listener.Start(ctx) }()
	}
	eventBus := internaleventbus.NewBus()

	plat := platform.Real()

	var execRegistry engine.ExecutorRegistry
	subRunner := func(ctx context.Context, parent internalexecutor.SubStepParent, steps []schema.FlowNode, vars map[string]any) ([]*engine.StepResult, error) {
		return runSubStepsViaEngine(
			ctx,
			execRegistry,
			parent,
			steps,
			vars,
			modeFromString(opts.Mode),
			traceWriter,
			dispatcher,
			plat,
			inputProvider,
			promptProvider,
			toolRuntime,
			approvalGate,
			evaluator,
			conditionEvaluator,
			eventBus,
		)
	}

	baseRegistry := internalexecutor.NewDefaultRegistry(internalexecutor.RegistryConfig{
		Platform:               plat,
		Evaluator:              evaluator,
		ConditionEvaluator:     conditionEvaluator,
		PromptProvider:         promptProvider,
		HostActionProvider:     opts.HostActionProvider,
		InputProvider:          inputProvider,
		ApprovalGate:           approvalGate,
		SubStepRunner:          subRunner,
		ToolRuntime:            toolRuntime,
		LazyRunbookLoader:      opts.LazyRunbookLoader,
		SubstitutionParser:     opts.SubstitutionParser,
		DynamicIncludeResolver: versionedIncludeResolver{legacy: opts.DynamicIncludeResolver},
		PinRecorder:            opts.PinRecorder,
		MaxIncludeDepth:        opts.MaxIncludeDepth,
		Output:                 opts.Output,
	})
	execRegistry = baseRegistry
	if opts.Mode == "dry-run" {
		execRegistry = &DryRunExecutorRegistry{inner: execRegistry}
	}
	if opts.Mode == string(engine.RunModeRouteTest) {
		execRegistry = opts.RouteTestScheduler.WrapRegistry(execRegistry)
	}
	if opts.Mode == "replay" {
		if opts.ScenarioFile == "" {
			return engine.EngineConfig{}, func() {}, fmt.Errorf("adapter: scenario file is required for replay")
		}
		scenario, err := internalreplay.LoadScenario(opts.ScenarioFile)
		if err != nil {
			return engine.EngineConfig{}, func() {}, fmt.Errorf("adapter: load scenario: %w", err)
		}
		// R5 (barbara-enum-mvp-implementation-gate.md): this
		// wiring-time construction runs before any runbook is
		// parsed/planned, so the plan's tool definitions are not yet
		// available here to enum-check replayed tool-call args
		// against -- unlike internal/replay.ReplayEngine.ReplayFromTrace,
		// which builds its ReplayExecutorRegistry after planning and
		// does pass them through. No CLI command currently reaches this
		// branch (opts.Mode is only ever "real"/"dry-run" from
		// cmd/yawr), so it carries no live ENUM-008 exposure today; if a
		// caller starts constructing engines this way with a live plan
		// in hand, pass it through NewReplayExecutorRegistryWithEnumChecks
		// instead of this call, the same way ReplayFromTrace does.
		execRegistry = internalreplay.NewReplayExecutorRegistry(execRegistry, scenario)
	}

	tracerProvider, shutdown := buildTracerProvider(opts)
	shutdownAll := func() {
		_ = traceWriter.Close()
		_ = store.Close()
		shutdown()
	}
	return engine.EngineConfig{
		Executors:           execRegistry,
		Dispatcher:          dispatcher,
		TraceWriter:         traceWriter,
		Platform:            plat,
		EventBus:            eventBus,
		Store:               store,
		EvidenceHook:        internalevidence.NewDefaultCollector(),
		Evaluator:           evaluator,
		ConditionEvaluator:  conditionEvaluator,
		PromptProvider:      promptProvider,
		InputProvider:       inputProvider,
		ToolRuntime:         toolRuntime,
		ApprovalGate:        approvalGate,
		TracerProvider:      tracerProvider,
		GovernanceEvaluator: internalgovernance.BuildEvaluator(approvalGate),
	}, shutdownAll, nil
}

// buildTracerProvider returns a TracerProvider and shutdown func based on WireOptions.
// When OTelEndpoint is set, an OTLP gRPC provider is created; on failure it
// logs a warning and falls back to noop. When OTelStdout is set, spans are
// written to stderr as JSON for debugging.
func buildTracerProvider(opts WireOptions) (otelPkg.TracerProvider, func()) {
	if opts.OTelEndpoint != "" {
		provider, shutdown, err := otlpadapter.NewOTLPTracerProvider(
			opts.OTelEndpoint,
			otlpadapter.WithServiceName(opts.OTelServiceName),
			otlpadapter.WithInsecure(),
		)
		if err != nil {
			log.Printf("yawr: failed to create OTLP tracer provider: %v (falling back to noop)", err)
			return nil, func() {}
		}
		return provider, shutdown
	}
	if opts.OTelStdout {
		return &stdoutTracerProvider{serviceName: opts.OTelServiceName}, func() {}
	}
	return nil, func() {}
}

// stdoutTracerProvider is a minimal debug provider that writes span summaries to stderr.
type stdoutTracerProvider struct {
	serviceName string
}

func (p *stdoutTracerProvider) Tracer(name string, _ ...otelPkg.TracerOption) otelPkg.Tracer {
	return &stdoutTracer{serviceName: p.serviceName, name: name}
}

type stdoutTracer struct {
	serviceName string
	name        string
}

func (t *stdoutTracer) Start(ctx context.Context, spanName string, opts ...otelPkg.SpanStartOption) (context.Context, otelPkg.Span) {
	span := &stdoutSpan{tracer: t, name: spanName, startTime: time.Now()}
	return otelPkg.ContextWithSpan(ctx, span), span
}

type stdoutSpan struct {
	tracer    *stdoutTracer
	name      string
	startTime time.Time
	status    otelPkg.StatusCode
	statusMsg string
}

func (s *stdoutSpan) End(_ ...otelPkg.SpanEndOption) {
	dur := time.Since(s.startTime)
	statusStr := "unset"
	switch s.status {
	case otelPkg.StatusOK:
		statusStr = "ok"
	case otelPkg.StatusError:
		statusStr = "error"
	}
	fmt.Fprintf(os.Stderr, `{"span":%q,"service":%q,"duration_ms":%d,"status":%q,"status_msg":%q}`+"\n",
		s.name, s.tracer.serviceName, dur.Milliseconds(), statusStr, s.statusMsg)
}

func (s *stdoutSpan) SetAttributes(_ ...otelPkg.Attribute)          {}
func (s *stdoutSpan) RecordError(_ error, _ ...otelPkg.EventOption) {}
func (s *stdoutSpan) SetStatus(code otelPkg.StatusCode, msg string) {
	s.status = code
	s.statusMsg = msg
}

// DryRunExecutorRegistry wraps executors to simulate dry-run mode.
type DryRunExecutorRegistry struct {
	inner engine.ExecutorRegistry
}

func (r *DryRunExecutorRegistry) MaterializeLazyIncludes(ctx context.Context, plan *engine.ExecutionPlan) error {
	return internalexecutor.MaterializeLazyIncludes(ctx, r.inner, plan)
}

func (r *DryRunExecutorRegistry) Register(kind string, exec engine.StepExecutor) {
	if r.inner != nil {
		r.inner.Register(kind, exec)
	}
}

func (r *DryRunExecutorRegistry) Lookup(kind string) engine.StepExecutor {
	if r.inner == nil {
		return nil
	}
	exec := r.inner.Lookup(kind)
	if exec == nil {
		return nil
	}
	return &DryRunExecutor{kind: kind}
}

// DryRunExecutor returns a simulated StepResult without side effects.
type DryRunExecutor struct {
	kind string
}

func (d *DryRunExecutor) Execute(_ context.Context, step engine.ResolvedStep, _ map[string]any) (*engine.StepResult, error) {
	now := time.Now()
	output := map[string]any{"dry_run": true, "would_execute": d.kind}
	if d.kind == "host_action" {
		output["status"] = string(hostaction.StatusUnsupported)
		output["result"] = map[string]any{"status": string(hostaction.StatusUnsupported)}
	}
	return &engine.StepResult{
		StepID:      step.ID,
		Status:      engine.StepStatusCompleted,
		Outcome:     engine.StepOutcomeSuccess,
		Output:      output,
		StartedAt:   now,
		CompletedAt: now,
		DurationMs:  0,
		Vars:        map[string]any{},
	}, nil
}

func buildToolRegistry(scanDir string, excludeTestTools bool) (*internaltool.OverlayRegistry, error) {
	var (
		toolDefs []toolpkg.ToolDef
		err      error
	)
	if excludeTestTools {
		toolDefs, err = internaltool.ScanDirWithoutTests(scanDir)
	} else {
		toolDefs, err = internaltool.ScanDir(scanDir)
	}
	if err != nil {
		return nil, err
	}
	registry := internaltool.NewMapRegistry(toolDefs)
	builtin := internaltool.NewBuiltinRegistry()
	for _, def := range builtin.All() {
		if _, ok := registry.Lookup(def.Name); ok {
			continue
		}
		_ = registry.Register(def)
	}
	// Wrap in an overlay so callers that resolve an authoritative binding
	// after wiring (e.g. cmd/yawr's Tool Packages MVP requires:/toolRefs:
	// catalog resolution) can force it to win over whatever this directory
	// scan happened to find first, via Override — see OverlayRegistry.
	return internaltool.NewOverlayRegistry(registry), nil
}

func buildInputProvider(opts WireOptions) *internalinput.ChainProvider {
	envProvider := internalinput.NewEnvProviderWithPrefix(opts.InputEnvPrefix)
	var vaultProvider inputpkg.InputProvider
	if opts.VaultAddr != "" {
		vaultProvider = internalinput.NewVaultProvider()
	}
	var promptInput inputpkg.InputProvider
	if opts.TTYOutput {
		promptInput = internalinput.NewPromptProvider(os.Stdin, os.Stdout)
	}
	return internalinput.NewChainProvider(envProvider, vaultProvider, promptInput)
}

func buildPromptProvider(opts WireOptions) inputpkg.PromptProvider {
	if !opts.TTYOutput {
		return nil
	}
	return internalinput.NewTerminalInputProvider(os.Stdin, os.Stdout)
}

func buildApprovalGate(opts WireOptions) governance.ApprovalGate {
	// Resolution order for attendance:
	//   1. opts.Attended != nil → profile-declared attendance wins.
	//   2. opts.Attended == nil → fall back to TTYOutput (no regression).
	attended := opts.TTYOutput
	if opts.Attended != nil {
		attended = *opts.Attended
	}
	if attended {
		return internalgovernance.NewTerminalApprovalGate(os.Stdin, os.Stdout)
	}
	return internalgovernance.NewNoOpApprovalGate()
}

type subEngineTraceSink struct{}

func (subEngineTraceSink) Append(tracepkg.TraceEvent) error { return nil }
func (subEngineTraceSink) Close() error                     { return nil }

func ExecuteSubSteps(ctx context.Context, cfg engine.EngineConfig, parent internalexecutor.SubStepParent, nodes []schema.FlowNode, vars map[string]any) ([]*engine.StepResult, error) {
	return runSubStepsViaEngine(ctx, cfg.Executors, parent, nodes, vars, engine.RunModeReal,
		cfg.TraceWriter, cfg.Dispatcher, cfg.Platform, cfg.InputProvider, cfg.PromptProvider,
		cfg.ToolRuntime, cfg.ApprovalGate, cfg.Evaluator, cfg.ConditionEvaluator, cfg.EventBus, cfg.OnEvent)
}

func runSubStepsViaEngine(
	ctx context.Context,
	registry engine.ExecutorRegistry,
	parent internalexecutor.SubStepParent,
	nodes []schema.FlowNode,
	vars map[string]any,
	mode engine.RunMode,
	traceWriter tracepkg.TraceWriter,
	dispatcher eventbuspkg.EventDispatcher,
	plat platform.Platform,
	inputProvider inputpkg.InputProvider,
	promptProvider inputpkg.PromptProvider,
	toolRuntime toolpkg.ToolRuntime,
	approvalGate governance.ApprovalGate,
	evaluator exprpkg.Evaluator,
	conditionEvaluator exprpkg.ConditionEvaluator,
	eventBus eventbuspkg.EventBus,
	onEvents ...func(engine.Event),
) ([]*engine.StepResult, error) {
	if replayRegistry := internalexecutor.ExecutorRegistryFromContext(ctx); replayRegistry != nil {
		registry = replayRegistry
		mode = engine.RunModeReplay
	}
	if replayApprovalGate := internalexecutor.RunApprovalGateFromContext(ctx); replayApprovalGate != nil {
		approvalGate = replayApprovalGate
	}
	if replayDispatcher := internalexecutor.RunEventDispatcherFromContext(ctx); replayDispatcher != nil {
		dispatcher = replayDispatcher
	}
	steps := make([]engine.ResolvedStep, 0, len(nodes))
	for _, node := range nodes {
		step, ok := resolveFlowNode(node)
		if !ok {
			continue
		}
		step.ParentID = parent.ID
		step.ParentKind = parent.Kind
		step.BranchLabel = parent.BranchLabel
		if step.LexicalScopeID == "" {
			step.LexicalScopeID = parent.RootScopeID
		}
		step.NestDepth = parent.NestDepth
		engine.ApplyPlanStepPresentation(ctx, &step)
		steps = append(steps, step)
	}
	if len(steps) == 0 && (parent.RunbookInvocation == nil || len(parent.RunbookInvocation.Bindings) == 0) {
		return nil, nil
	}
	var declarations []schema.Binding
	if parent.RunbookInvocation != nil {
		declarations = parent.RunbookInvocation.Bindings
	} else if scope := engine.BindingScopeFromContext(ctx); scope != nil && scope.Invocation != nil {
		declarations = scope.Invocation.Bindings
	}
	if scopes := engine.ToolScopesFromContext(ctx); scopes != nil {
		if err := internalplanner.ValidateScopedTypedBoundFlow(nodes, declarations, scopes, parent.RootScopeID); err != nil {
			return nil, err
		}
	} else if err := internalplanner.ValidateTypedBoundFlow(nodes, declarations, engine.PlanToolsFromContext(ctx)); err != nil {
		return nil, err
	}
	transferBindings := func(vars map[string]any) {
		if parent.InvocationState == nil || parent.RunbookInvocation == nil {
			return
		}
		parent.InvocationState.Bindings = make(map[string]any, len(parent.RunbookInvocation.Bindings))
		for _, binding := range parent.RunbookInvocation.Bindings {
			if value, exists := vars[binding.Name]; exists {
				parent.InvocationState.Bindings[binding.Name] = value
			}
		}
	}
	callPath := engine.DebugCallPathFromContext(ctx)
	childCallPath := append(append([]engine.DebugCallFrame(nil), callPath...), engine.DebugCallFrame{
		StepID: parent.ID, RunbookPath: parent.RunbookPath,
	})
	startIndex := 0
	workingVars := vars
	bindingScope := engine.CloneBindingScope(engine.BindingScopeFromContext(ctx))
	var results []*engine.StepResult
	var frameBinding engine.ExecutionFrameBinding
	if committer := engine.ExecutionFrameCommitterFromContext(ctx); committer != nil {
		definitionDigest, err := engine.ExecutionFrameDefinitionDigest(steps)
		if err != nil {
			return nil, err
		}
		parentFrameID := ""
		if binding, ok := engine.ExecutionFrameBindingFromContext(ctx); ok {
			parentFrameID = binding.FrameID
		}
		frame, err := committer.BeginExecutionFrame(ctx, engine.ExecutionFrameRequest{
			ParentFrameID: parentFrameID, ParentQualifiedNodeID: engine.DebugNodeID(callPath, parent.ID),
			ParentStepID: parent.ID, Kind: parent.Kind, CallPath: childCallPath,
			BranchLabel: parent.BranchLabel, IterationIndex: parent.IterationIndex,
			DefinitionDigest: definitionDigest, StepCount: len(steps),
			StepIDs: engine.ExecutionFrameStepIDs(steps), InitialVars: vars,
			RunbookInvocation: parent.RunbookInvocation, BindingScope: bindingScope,
		})
		if err != nil {
			return nil, err
		}
		if frame.NextStepIndex < 0 || frame.NextStepIndex > len(steps) {
			return nil, fmt.Errorf("execution frame %s has invalid next step %d", frame.FrameID, frame.NextStepIndex)
		}
		startIndex = frame.NextStepIndex
		workingVars = frame.WorkingVars
		bindingScope = frame.BindingScope
		results = make([]*engine.StepResult, 0, len(steps))
		for index := 0; index < startIndex; index++ {
			result := frame.Results[strconv.Itoa(index)]
			if result == nil {
				return nil, fmt.Errorf("execution frame %s is missing result %d", frame.FrameID, index)
			}
			results = append(results, result)
		}
		frameBinding = engine.ExecutionFrameBinding{
			FrameID: frame.FrameID, StepOffset: startIndex, Invocation: frame.Invocation,
		}
		if startIndex == len(steps) {
			transferBindings(workingVars)
			return results, nil
		}
	}
	remainingSteps := steps[startIndex:]

	plan := &engine.ExecutionPlan{
		RunbookPath: "substeps",
		Steps:       remainingSteps,
		Tools:       engine.PlanToolsFromContext(ctx),
		ToolScopes:  engine.ToolScopesFromContext(ctx),
		RootScopeID: parent.RootScopeID,
		Metadata: engine.PlanMetadata{
			RunbookID:   "substeps",
			RunbookName: "substeps",
			PlannedAt:   time.Now(),
		},
	}
	if plan.ToolScopes != nil {
		plan.Tools = nil
		plan.Metadata.CatalogDigest = plan.ToolScopes.Export().CatalogDigest
		plan.Metadata.Profile = engine.PlanProfileFromContext(ctx)
		plan.ScopeBoundary = &engine.ToolScopeBoundary{
			ParentID: parent.ID, ParentKind: parent.Kind, ScopeID: parent.RootScopeID,
		}
	}
	if err := internalplanner.ValidateExecutionPlan(plan); err != nil {
		return nil, err
	}
	// Reuse the parent run's ID for the sub-engine. The engine sets
	// WithRunID(ctx, plan.RunID) when it dispatches a step, and prompt
	// providers (e.g. the HTTP broker) key their queues by run ID. If
	// the sub-engine generated its own UUID, every interactive step
	// inside an include/branch/iterate body would fail with
	// "interaction: run not registered". The sub-engine has Store=nil
	// so reusing the ID does not collide with the parent's plan
	// registration; trace events tagged with the parent run ID are
	// also correct, since these sub-steps are logically part of the
	// same run.
	if parentRunID := engine.RunIDFromContext(ctx); parentRunID != "" {
		plan.RunID = parentRunID
	}
	evidenceHook := engine.EvidenceHook(internalevidence.NewDefaultCollector())
	if inheritedEvidenceHook, bound := internalexecutor.RunEvidenceHookFromContext(ctx); bound {
		evidenceHook = inheritedEvidenceHook
	}
	var onEvent func(engine.Event)
	if len(onEvents) > 0 {
		onEvent = onEvents[0]
	}
	eng := internalengine.New(engine.EngineConfig{
		Executors:           registry,
		Dispatcher:          dispatcher,
		TraceWriter:         subEngineTraceSink{},
		Platform:            plat,
		InputProvider:       inputProvider,
		PromptProvider:      promptProvider,
		ToolRuntime:         toolRuntime,
		ApprovalGate:        approvalGate,
		Evaluator:           evaluator,
		ConditionEvaluator:  conditionEvaluator,
		EventBus:            eventBus,
		Store:               nil,
		EvidenceHook:        evidenceHook,
		GovernanceEvaluator: internalgovernance.BuildEvaluator(approvalGate),
		OnEvent:             onEvent,
	})
	// Forward events from this sub-engine back to the parent run so
	// nested step lifecycle reaches the parent's event consumers (CLI
	// tail, SSE bridge, run store). Without this, sub-step events are
	// emitted only on the sub-engine's events channel, which nobody
	// reads — causing branch/iterate children to look frozen in the UI.
	fwd := engine.EventForwarderFromContext(ctx)
	runOpts := engine.RunOptions{
		Mode:        mode,
		Vars:        varsToString(workingVars),
		RuntimeVars: workingVars,
		Debugger:    engine.DebugControllerFromContext(ctx),
		RouteTest:   engine.RouteTestControllerFromContext(ctx),
	}
	if fwd != nil {
		runOpts.OnEvent = func(ev engine.Event) { fwd(ev) }
	}
	childCtx := engine.WithDebugCallPath(ctx, childCallPath)
	childCtx = engine.WithBindingScope(childCtx, bindingScope)
	structuralPath := engine.DynamicIncludeStructuralPathFromContext(ctx)
	frameInvocation := frameBinding.Invocation
	if frameInvocation == 0 {
		frameInvocation = 1
	}
	structuralPath = append(structuralPath, schema.DynamicIncludeFrameIdentity{
		QualifiedNodeID: engine.DebugNodeID(callPath, parent.ID), Kind: parent.Kind,
		BranchLabel: parent.BranchLabel, IterationIndex: parent.IterationIndex,
		Invocation: frameInvocation,
	})
	childCtx = engine.WithDynamicIncludeStructuralPath(childCtx, structuralPath)
	childCtx = engine.WithExecutionFrameBinding(childCtx, frameBinding)
	handle, err := eng.Start(childCtx, plan, runOpts)
	if err != nil {
		return nil, err
	}

	for {
		result, err := handle.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			return results, err
		}
		results = append(results, result)
	}
	if engine.RouteTestTargetReachedFromContext(ctx) {
		return results, engine.ErrRouteTestTargetReached
	}
	transferBindings(handle.State().Vars)
	// If the sub-engine reached a terminal failed state (on_error=stop fired),
	// propagate that as a sentinel error so callers (include/branch executors)
	// can distinguish "tolerated failures" from "terminal failure" and return
	// a failed StepResult rather than an infrastructure error.
	if st := handle.State(); st.Status == engine.RunStatusFailed {
		var cause error
		for index := len(results) - 1; index >= 0; index-- {
			if results[index] != nil && results[index].Status == engine.StepStatusFailed && results[index].Error != nil {
				cause = results[index].Error
				break
			}
		}
		if cause != nil {
			return results, fmt.Errorf("%w: step %s: %w", internalexecutor.ErrSubRunFailed, st.CurrentStep, cause)
		}
		return results, fmt.Errorf("%w: step %s", internalexecutor.ErrSubRunFailed, st.CurrentStep)
	}
	return results, nil
}

func ResolveFlowNode(node schema.FlowNode) (engine.ResolvedStep, bool) {
	return resolveFlowNode(node)
}

func resolveFlowNode(node schema.FlowNode) (engine.ResolvedStep, bool) {
	if node.Step != nil {
		step := node.Step
		return engine.ResolvedStep{
			ID:              step.ID,
			Kind:            string(step.Type),
			Spec:            stepSpecForStep(step),
			Capture:         step.Capture,
			CaptureDefaults: step.CaptureDefaults,
			When:            step.When,
			Delay:           step.Delay,
			OnError:         step.OnError,
			LexicalScopeID:  step.LexicalScopeID,
			ToolBindingID:   step.ToolBindingID,
		}, true
	}
	if node.Iterate != nil {
		return engine.ResolvedStep{
			ID:   node.Iterate.ID,
			Kind: "iterate",
			Spec: node.Iterate,
		}, true
	}
	if node.Parallel != nil {
		return engine.ResolvedStep{
			ID:   node.Parallel.ID,
			Kind: "parallel",
			Spec: node.Parallel,
		}, true
	}
	return engine.ResolvedStep{}, false
}

func stepSpecForStep(step *schema.Step) engine.StepSpec {
	switch step.Type {
	case schema.StepTypeCLI:
		return step.CLI
	case schema.StepTypeTool:
		return step.ToolCall
	case schema.StepTypeInclude:
		return step.IncludeSpec
	case schema.StepTypeChoice:
		return step.ChoiceSpec
	case schema.StepTypeDecision:
		return step.DecisionSpec
	case schema.StepTypeCollector:
		return step.CollectorSpec
	case schema.StepTypeHostAction:
		return step.HostActionSpec
	case schema.StepTypeBranch:
		return step.BranchSpec
	case schema.StepTypeApprove:
		return step.ApproveSpec
	case schema.StepTypeAssert:
		return step.AssertSpec
	case schema.StepTypeCompensate:
		return step.CompensateSpec
	case schema.StepTypeWaitForEvent:
		return step.WaitForEventSpec
	case schema.StepTypeEnd:
		return step.EndSpec
	case schema.StepTypeNoop:
		if step.NoopSpec != nil {
			return step.NoopSpec
		}
		return &schema.NoopSpec{}
	case schema.StepTypeDisplay:
		return step.DisplaySpec
	case schema.StepTypeHandoff:
		return step.HandoffSpec
	case schema.StepTypeAssign:
		return step.AssignSpec
	case schema.StepTypeResults:
		return step.ResultsSpec
	}
	return nil
}

func modeFromString(mode string) engine.RunMode {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case string(engine.RunModeDryRun):
		return engine.RunModeDryRun
	case string(engine.RunModeReplay):
		return engine.RunModeReplay
	case string(engine.RunModeRouteTest):
		return engine.RunModeRouteTest
	default:
		return engine.RunModeReal
	}
}

func varsToString(vars map[string]any) map[string]string {
	if vars == nil {
		return nil
	}
	out := make(map[string]string, len(vars))
	for k, v := range vars {
		out[k] = fmt.Sprint(v)
	}
	return out
}
