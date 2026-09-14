package run

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	internaladapter "github.com/ormasoftchile/yawr/runtime/internal/adapter"
	internalengine "github.com/ormasoftchile/yawr/runtime/internal/engine"
	internaleventbus "github.com/ormasoftchile/yawr/runtime/internal/eventbus"
	internalevidence "github.com/ormasoftchile/yawr/runtime/internal/evidence"
	internalexecutor "github.com/ormasoftchile/yawr/runtime/internal/executor"
	internalexpr "github.com/ormasoftchile/yawr/runtime/internal/expr"
	internalgovernance "github.com/ormasoftchile/yawr/runtime/internal/governance"
	internalinput "github.com/ormasoftchile/yawr/runtime/internal/input"
	internalparser "github.com/ormasoftchile/yawr/runtime/internal/parser"
	internalplanner "github.com/ormasoftchile/yawr/runtime/internal/planner"
	internaltool "github.com/ormasoftchile/yawr/runtime/internal/tool"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/governance"
	inputpkg "github.com/ormasoftchile/yawr/runtime/pkg/input"
	parserpkg "github.com/ormasoftchile/yawr/runtime/pkg/parser"
	plannerpkg "github.com/ormasoftchile/yawr/runtime/pkg/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
	tracepkg "github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

// Config holds everything an external client needs to start a run.
// Mirrors EngineConfig but hides internal wiring details.
type Config struct {
	// RunbookPath is the path to the runbook YAML file, OR
	// RunbookFS + RunbookName for embedded FS loading.
	RunbookPath string
	RunbookFS   fs.FS  // optional — use with RunbookName
	RunbookName string // filename within RunbookFS

	// PromptProvider handles interactive prompts.
	// If nil, a no-op provider is used (auto-approve defaults).
	PromptProvider inputpkg.PromptProvider

	// ApprovalGate handles governance approval requests.
	// If nil, approvals are auto-approved (no-op).
	ApprovalGate governance.ApprovalGate

	// Client identifies the caller (e.g., "tui", "cli", "batch").
	Client string

	// Variables are initial variable bindings passed to the runbook.
	Variables map[string]string

	// KitFS optionally provides a kit (tools, scripts).
	// If nil, no kit is loaded.
	KitFS fs.FS

	// Output is the writer for display step content (plain text / CLI mode).
	// If nil, display steps write to os.Stdout.
	// Pass io.Discard when running inside a TUI so that direct writes do not
	// corrupt the terminal — the TUI receives display content via engine events.
	Output io.Writer

	// TraceWriter writes trace events. If nil, a no-op writer is used.
	TraceWriter tracepkg.TraceWriter

	// OnEvent is an optional callback invoked for each runtime event.
	OnEvent func(engine.Event)

	// OnSubEvent is an optional callback invoked for events emitted by
	// sub-engines that execute branch/iterate arm steps. These events are
	// not visible via RunHandle.Events() because the sub-engine has its own
	// event channel. Callers that need to observe sub-step output (e.g. for
	// TUI pre-population) should set this.
	// Like OnEvent, delivery completes before the emitting sub-step returns.
	// Parallel sub-steps may invoke the callback concurrently.
	OnSubEvent func(engine.Event)
}

// Result is returned by StartWithWarnings: the RunHandle plus any
// non-fatal parse warnings collected while loading the runbook (D-4,
// T-RUN-WARNINGS, AR-CE-4 §6: "ENUM-W001 MUST reach a human on every
// surface that parses a runbook"). Start (below) discards Warnings for
// source compatibility with existing callers; new callers that want
// warning visibility (e.g. a TUI surface) should call StartWithWarnings
// directly.
type Result struct {
	Handle   engine.RunHandle
	Warnings []parserpkg.ParseWarning
}

// Start parses the runbook, plans execution, and returns a RunHandle.
// The caller drives execution by calling handle.Next() in a loop.
// Start is non-blocking — it returns immediately.
//
// Start discards any non-fatal parse warnings (e.g. ENUM-W001); callers
// that need them (T-RUN-WARNINGS) should use StartWithWarnings instead.
// Start's signature is preserved unchanged so existing callers (including
// yawr-tui's `replace ../yawr` build) keep compiling without a source
// change; StartWithWarnings is purely additive.
func Start(ctx context.Context, cfg Config) (engine.RunHandle, error) {
	res, err := StartWithWarnings(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return res.Handle, nil
}

// StartWithWarnings is identical to Start except it also returns the
// non-fatal parse warnings collected while loading the runbook (D-4,
// T-RUN-WARNINGS): AR-ENUM-12 makes ENUM-W001 non-fatal, not invisible,
// and pkg/run.Start previously dropped parsed.Warnings entirely, making
// ENUM-W001 unreachable by any caller of this library entry point (the
// TUI, notably) even though `yawr run`'s own CLI path already surfaces
// them (cmd/yawr/run.go).
func StartWithWarnings(ctx context.Context, cfg Config) (Result, error) {
	// 1. Load runbook bytes
	runbookBytes, source, err := loadRunbook(cfg)
	if err != nil {
		return Result{}, fmt.Errorf("run: load runbook: %w", err)
	}

	// 2. Parse with internal parser
	plat := platform.Real()
	parser, err := internalparser.New(plat)
	if err != nil {
		return Result{}, fmt.Errorf("run: create parser: %w", err)
	}

	parsed, err := parser.ParseBytes(ctx, runbookBytes)
	if err != nil {
		return Result{}, fmt.Errorf("run: parse runbook: %w", err)
	}
	parsed.Source = source

	// 3. Build tool registries
	mapRegistry, toolRegistryAdapter := buildToolRegistries(cfg.KitFS)

	// 3a. Resolve toolRefs declared in the runbook and register them before planning
	if parsed.Runbook != nil && len(parsed.Runbook.ToolRefs) > 0 {
		toolDefs, err := internaladapter.ResolveToolRefs(source, parsed.Runbook.ToolRefs)
		if err != nil {
			return Result{}, fmt.Errorf("run: resolve toolRefs: %w", err)
		}
		for _, def := range toolDefs {
			if err := mapRegistry.Register(def); err != nil {
				return Result{}, fmt.Errorf("run: register tool %s: %w", def.Name, err)
			}
		}
	}

	loaderInstance := &fsRunbookLoader{
		parser:       parser,
		fsys:         cfg.RunbookFS,
		toolRegistry: mapRegistry,
	}

	// 4. Plan with internal planner
	planner := internalplanner.New(plannerpkg.Config{
		Loader: loaderInstance,
		Tools:  toolRegistryAdapter,
	})

	plan, err := planner.Plan(ctx, parsed)
	if err != nil {
		return Result{}, fmt.Errorf("run: plan runbook: %w", err)
	}

	// 5. Build EngineConfig
	baseDir := filepath.Dir(source)
	var parentImports map[string]string
	if parsed.Runbook != nil {
		parentImports = parsed.Runbook.Imports
	}
	engineCfg, err := buildEngineConfig(cfg, plat, mapRegistry, loaderInstance, baseDir, parentImports)
	if err != nil {
		return Result{}, fmt.Errorf("run: build engine config: %w", err)
	}

	// 6. Create engine and start — merge runbook default vars with caller-supplied vars
	// (caller vars win over runbook defaults)
	runVars := make(map[string]string)
	if parsed.Runbook != nil {
		for k, v := range parsed.Runbook.Vars {
			if s, ok := v.(string); ok {
				runVars[k] = s
			} else {
				runVars[k] = fmt.Sprint(v)
			}
		}
		// Seed declared inputs into runVars so templates referencing them don't fail.
		// Priority: caller-supplied > input default > "" (for non-required inputs).
		for name, input := range parsed.Runbook.Inputs {
			if input == nil {
				continue
			}
			if _, already := runVars[name]; already {
				continue
			}
			if input.Default != nil {
				runVars[name] = fmt.Sprint(input.Default)
			} else if !input.Required {
				runVars[name] = ""
			}
		}
	}
	for k, v := range cfg.Variables {
		runVars[k] = v
	}

	// ENUM-008 (AR-ENUM-7, barbara-enum-mvp-implementation-gate.md R1): a
	// caller-supplied (runtime-bound) value for an enum-constrained
	// input must be one of its declared members. Default values were
	// already validated as part of plan-time ENUM-006; only the
	// caller-supplied override is re-checked here via the shared helper
	// used identically by cmd/yawr/run.go and internal/serve, since it is
	// the only genuinely runtime-produced binding at this call site.
	if parsed.Runbook != nil {
		if err := schema.CheckCallerInputBindings(parsed.Runbook.Inputs, cfg.Variables); err != nil {
			return Result{}, err
		}
	}

	eng := internalengine.New(engineCfg)
	handle, err := eng.Start(ctx, plan, engine.RunOptions{
		Mode:   engine.RunModeReal,
		Vars:   runVars,
		Client: cfg.Client,
	})
	if err != nil {
		return Result{}, fmt.Errorf("run: start engine: %w", err)
	}

	return Result{Handle: handle, Warnings: parsed.Warnings}, nil
}

// loadRunbook loads runbook bytes from either RunbookPath or RunbookFS+RunbookName.
func loadRunbook(cfg Config) ([]byte, string, error) {
	if cfg.RunbookPath != "" {
		data, err := os.ReadFile(cfg.RunbookPath)
		if err != nil {
			return nil, "", err
		}
		return data, cfg.RunbookPath, nil
	}

	if cfg.RunbookFS != nil && cfg.RunbookName != "" {
		data, err := fs.ReadFile(cfg.RunbookFS, cfg.RunbookName)
		if err != nil {
			return nil, "", err
		}
		return data, cfg.RunbookName, nil
	}

	return nil, "", fmt.Errorf("either RunbookPath or (RunbookFS + RunbookName) must be set")
}

// buildEngineConfig constructs an EngineConfig from the public Config.
func buildEngineConfig(cfg Config, plat platform.Platform, mapRegistry *internaltool.MapRegistry, loader *fsRunbookLoader, baseDir string, parentImports map[string]string) (engine.EngineConfig, error) {
	// Trace writer
	traceWriter := cfg.TraceWriter
	if traceWriter == nil {
		traceWriter = &noopTraceWriter{}
	}

	// Tool runtime
	toolRuntime := internaltool.NewDefaultToolRuntime(mapRegistry)

	// Expression evaluators
	evaluator := &internalexpr.TemplateEvaluator{}
	conditionEvaluator := internalexpr.NewSimpleConditionEvaluator(evaluator)

	// Input providers
	promptProvider := cfg.PromptProvider
	if promptProvider == nil {
		// No-op provider for non-interactive mode
		promptProvider = &noopPromptProvider{}
	}

	inputProvider := internalinput.NewChainProvider(
		internalinput.NewEnvProvider(),
		internalinput.NewVaultProvider(),
	)

	// Approval gate
	approvalGate := cfg.ApprovalGate
	if approvalGate == nil {
		approvalGate = internalgovernance.NewNoOpApprovalGate()
	}

	// Event bus and dispatcher
	dispatcher := internaleventbus.NewDispatcher()
	eventBus := internaleventbus.NewBus()

	// Wire the SubStepRunner after the registry is built so branch/iterate
	// steps can execute their nested steps recursively.
	var baseRegistry engine.ExecutorRegistry

	// runSubSteps is the shared helper that creates a sub-engine and drains it.
	runSubSteps := func(ctx context.Context, steps []engine.ResolvedStep, vars map[string]any) ([]*engine.StepResult, error) {
		if len(steps) == 0 {
			return nil, nil
		}
		plan := engine.ValidatedForTest(&engine.ExecutionPlan{
			RunbookPath: "substeps",
			Steps:       steps,
			Metadata: engine.PlanMetadata{
				RunbookID:   "substeps",
				RunbookName: "substeps",
				PlannedAt:   time.Now(),
			},
		})
		subEng := internalengine.New(engine.EngineConfig{
			Executors:           baseRegistry,
			Dispatcher:          dispatcher,
			TraceWriter:         traceWriter,
			Platform:            plat,
			EventBus:            eventBus,
			Store:               nil,
			EvidenceHook:        internalevidence.NewDefaultCollector(),
			Evaluator:           evaluator,
			ConditionEvaluator:  conditionEvaluator,
			PromptProvider:      promptProvider,
			InputProvider:       inputProvider,
			ToolRuntime:         toolRuntime,
			ApprovalGate:        approvalGate,
			GovernanceEvaluator: internalgovernance.BuildEvaluator(approvalGate),
			OnEvent:             cfg.OnSubEvent,
		})
		handle, err := subEng.Start(ctx, plan, engine.RunOptions{
			Mode: engine.RunModeReal,
			Vars: varsToStringMap(vars),
		})
		if err != nil {
			return nil, err
		}
		var results []*engine.StepResult
		for {
			result, nextErr := handle.Next(ctx)
			if nextErr == io.EOF {
				break
			}
			if nextErr != nil {
				return results, nextErr
			}
			results = append(results, result)
		}
		return results, nil
	}

	// subRunner is the single SubStepRunner shared by every composite
	// executor (branch / parallel / iterate / include). It expands flow
	// nodes via expandSubNodes, which produces a single ResolvedStep of
	// kind "include" for any include node — the IncludeExecutor then
	// recurses through this same runner.
	subRunner := func(ctx context.Context, parent internalexecutor.SubStepParent, nodes []schema.FlowNode, vars map[string]any) ([]*engine.StepResult, error) {
		steps, err := expandSubNodes(ctx, parent, nodes, baseDir, loader, parentImports)
		if err != nil {
			return nil, err
		}
		return runSubSteps(ctx, steps, vars)
	}

	baseRegistry = internalexecutor.NewDefaultRegistry(internalexecutor.RegistryConfig{
		Platform:           plat,
		Evaluator:          evaluator,
		ConditionEvaluator: conditionEvaluator,
		PromptProvider:     promptProvider,
		InputProvider:      inputProvider,
		ApprovalGate:       approvalGate,
		SubStepRunner:      subRunner,
		ToolRuntime:        toolRuntime,
		Output:             cfg.Output,
		// Required so includes deferred by the planner (expand=lazy, or
		// auto resolved to lazy) can be materialized at execution time.
		// Without this, the IncludeExecutor fails with "no LazyRunbookLoader
		// is configured" and the run aborts on the first lazy include.
		LazyRunbookLoader: internaladapter.NewParserLazyLoader(loader.parser),
	})

	return engine.EngineConfig{
		Executors:           baseRegistry,
		Dispatcher:          dispatcher,
		TraceWriter:         traceWriter,
		Platform:            plat,
		EventBus:            eventBus,
		Store:               nil, // No checkpoint/resume for external clients
		EvidenceHook:        internalevidence.NewDefaultCollector(),
		Evaluator:           evaluator,
		ConditionEvaluator:  conditionEvaluator,
		PromptProvider:      promptProvider,
		InputProvider:       inputProvider,
		ToolRuntime:         toolRuntime,
		ApprovalGate:        approvalGate,
		GovernanceEvaluator: internalgovernance.BuildEvaluator(approvalGate),
		OnEvent:             cfg.OnEvent,
	}, nil
}

// buildToolRegistries creates both registries: internal MapRegistry and planner adapter.
func buildToolRegistries(kitFS fs.FS) (*internaltool.MapRegistry, *toolRegistryAdapter) {
	// Build internal tool registry
	var toolDefs []toolpkg.ToolDef

	// If KitFS is provided, scan it
	if kitFS != nil {
		// For now, we just use built-in tools
		// In the future, we can scan kitFS for .tool.yaml files
	}

	mapRegistry := internaltool.NewMapRegistry(toolDefs)

	// Add built-in tools
	builtin := internaltool.NewBuiltinRegistry()
	for _, def := range builtin.All() {
		if _, ok := mapRegistry.Lookup(def.Name); ok {
			continue
		}
		_ = mapRegistry.Register(def)
	}

	return mapRegistry, &toolRegistryAdapter{inner: mapRegistry}
}

// toolRegistryAdapter adapts internal MapRegistry to planner.ToolRegistry.
type toolRegistryAdapter struct {
	inner *internaltool.MapRegistry
}

func (a *toolRegistryAdapter) Lookup(ctx context.Context, name string, action string) (*schema.ToolDef, error) {
	def, ok := a.inner.Lookup(name)
	if !ok {
		return nil, plannerpkg.ErrToolNotFound
	}
	// Convert pkg/tool.ToolDef to schema.ToolDef, including actions
	actions := make(map[string]*schema.ToolAction, len(def.Actions))
	for k, a := range def.Actions {
		actions[k] = &schema.ToolAction{
			Description: a.Description,
			Argv:        a.Argv,
		}
	}
	return &schema.ToolDef{
		Name: def.Name,
		Transport: schema.TransportConfig{
			Type:    schema.Transport(def.Transport),
			Command: def.Command,
			Args:    def.Args,
		},
		Actions: actions,
	}, nil
}

// fsRunbookLoader loads runbooks from an fs.FS (for include support).
// It also resolves and registers any toolRefs declared by each loaded runbook.
type fsRunbookLoader struct {
	parser       parserpkg.Parser
	fsys         fs.FS
	toolRegistry *internaltool.MapRegistry
}

func (l *fsRunbookLoader) Load(ctx context.Context, path string) (*parserpkg.ParsedRunbook, error) {
	var parsed *parserpkg.ParsedRunbook
	var err error

	if l.fsys != nil {
		data, err2 := fs.ReadFile(l.fsys, path)
		if err2 != nil {
			return nil, fmt.Errorf("loader: read %s: %w", path, err2)
		}
		parsed, err = l.parser.ParseBytes(ctx, data)
		if err != nil {
			return nil, err
		}
		parsed.Source = path
	} else {
		// Fall back to filesystem
		parsed, err = l.parser.Parse(ctx, path)
		if err != nil {
			return nil, err
		}
	}

	// Resolve and register toolRefs declared by this child runbook.
	if l.toolRegistry != nil && parsed.Runbook != nil && len(parsed.Runbook.ToolRefs) > 0 {
		toolDefs, err := internaladapter.ResolveToolRefs(parsed.Source, parsed.Runbook.ToolRefs)
		if err != nil {
			return nil, fmt.Errorf("loader: resolve toolRefs for %s: %w", path, err)
		}
		for _, def := range toolDefs {
			if err := l.toolRegistry.Register(def); err != nil {
				return nil, fmt.Errorf("loader: register tool %s: %w", def.Name, err)
			}
		}
	}

	return parsed, nil
}

// noopTraceWriter discards all trace events.
type noopTraceWriter struct{}

func (n *noopTraceWriter) Append(_ tracepkg.TraceEvent) error { return nil }
func (n *noopTraceWriter) Close() error                       { return nil }

func varsToStringMap(vars map[string]any) map[string]string {
	if vars == nil {
		return nil
	}
	out := make(map[string]string, len(vars))
	for k, v := range vars {
		out[k] = fmt.Sprint(v)
	}
	return out
}

// expandSubNodes converts FlowNodes to ResolvedSteps for the sub-engine.
// Include steps are expanded inline: their child runbook steps are loaded and
// inlined, with with-binding injection and capture-propagation noop steps added
// around them so var scoping works correctly inside iterate/branch bodies.
//
// parent carries structural-parent context for trace events: every direct
// child node gets ParentID/ParentKind/BranchLabel from parent. Include
// steps are emitted as a single ResolvedStep of kind "include" with the
// loaded child runbook's flow nodes attached to the spec; the
// IncludeExecutor invokes SubStepRunner with those nodes (matching how
// branch / iterate / parallel composites work).
func expandSubNodes(ctx context.Context, parent internalexecutor.SubStepParent, nodes []schema.FlowNode, baseDir string, loader *fsRunbookLoader, imports map[string]string) ([]engine.ResolvedStep, error) {
	var steps []engine.ResolvedStep
	for _, node := range nodes {
		if node.Step == nil {
			continue
		}
		s := node.Step
		if s.Type == schema.StepTypeInclude && s.IncludeSpec != nil {
			rs, err := buildIncludeResolvedStep(ctx, parent, s, baseDir, loader, imports)
			if err != nil {
				return nil, err
			}
			steps = append(steps, rs)
		} else {
			steps = append(steps, engine.ResolvedStep{
				ID:              s.ID,
				Kind:            string(s.Type),
				Spec:            stepSpecForSchema(s),
				Capture:         s.Capture,
				CaptureDefaults: s.CaptureDefaults,
				When:            s.When,
				OnError:         s.OnError,
				Delay:           s.Delay,
				NestDepth:       parent.NestDepth,
				ParentID:        parent.ID,
				ParentKind:      parent.Kind,
				BranchLabel:     parent.BranchLabel,
			})
		}
	}
	return steps, nil
}

// buildIncludeResolvedStep loads the included child runbook and produces a
// single ResolvedStep of kind "include" carrying the child's flow nodes on
// its spec. The IncludeExecutor consumes spec.ResolvedSteps via
// SubStepRunner; there is no inline expansion or synthetic noop machinery.
func buildIncludeResolvedStep(ctx context.Context, parent internalexecutor.SubStepParent, s *schema.Step, baseDir string, loader *fsRunbookLoader, imports map[string]string) (engine.ResolvedStep, error) {
	inclCfg := s.IncludeSpec.Include
	inclPath := inclCfg.Runbook
	if !filepath.IsAbs(inclPath) {
		inclPath = filepath.Join(baseDir, inclPath)
	}
	childRb, err := loader.Load(ctx, inclPath)
	if err != nil {
		return engine.ResolvedStep{}, fmt.Errorf("include %s: load child runbook: %w", s.ID, err)
	}
	var childFlow []schema.FlowNode
	if childRb != nil && childRb.Runbook != nil {
		childFlow = childRb.Runbook.Flow
	}
	includeAlias := lookupIncludeAliasForImports(imports, inclCfg.Runbook)
	return engine.ResolvedStep{
		ID:              s.ID,
		Kind:            string(schema.StepTypeInclude),
		Spec:            &schema.IncludeSpec{Include: inclCfg, ResolvedSteps: childFlow},
		Capture:         s.Capture,
		CaptureDefaults: s.CaptureDefaults,
		When:            s.When,
		OnError:         s.OnError,
		Delay:           s.Delay,
		NestDepth:       parent.NestDepth,
		ParentID:        parent.ID,
		ParentKind:      parent.Kind,
		BranchLabel:     parent.BranchLabel,
		IncludeAlias:    includeAlias,
	}, nil
}

// lookupIncludeAliasForImports reverse-looks-up the alias for an include path
// in an imports map. Returns "" when no alias matches the path.
func lookupIncludeAliasForImports(imports map[string]string, includePath string) string {
	for alias, path := range imports {
		if path == includePath {
			return alias
		}
	}
	return ""
}

// stepSpecForSchema extracts the type-specific spec from a schema.Step.
func stepSpecForSchema(step *schema.Step) engine.StepSpec {
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
	case schema.StepTypeDisplay:
		return step.DisplaySpec
	case schema.StepTypeEnd:
		return step.EndSpec
	case schema.StepTypeNoop:
		if step.NoopSpec != nil {
			return step.NoopSpec
		}
		return &schema.NoopSpec{}
	}
	return nil
}

type noopPromptProvider struct{}

func (n *noopPromptProvider) PromptChoice(_ context.Context, req inputpkg.ChoiceRequest) (*inputpkg.ChoiceResponse, error) {
	// Auto-select default or first option
	selected := req.Default
	if selected == "" && len(req.Options) > 0 {
		selected = req.Options[0].Value
	}
	return &inputpkg.ChoiceResponse{Selected: []string{selected}}, nil
}

func (n *noopPromptProvider) PromptDecision(_ context.Context, req inputpkg.DecisionRequest) (*inputpkg.DecisionResponse, error) {
	// Auto-select first route
	if len(req.Routes) > 0 {
		return &inputpkg.DecisionResponse{Label: req.Routes[0].Label}, nil
	}
	return nil, fmt.Errorf("no routes available")
}

func (n *noopPromptProvider) PromptForm(_ context.Context, req inputpkg.FormRequest) (*inputpkg.FormResponse, error) {
	// Auto-fill with defaults
	values := make(map[string]any)
	for _, field := range req.Fields {
		if field.Default != nil {
			values[field.Name] = field.Default
		}
	}
	return &inputpkg.FormResponse{Values: values}, nil
}
