package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/ormasoftchile/yawr/runtime/internal/adapter"
	internaldebugprotect "github.com/ormasoftchile/yawr/runtime/internal/debugprotect"
	internalengine "github.com/ormasoftchile/yawr/runtime/internal/engine"
	internalexecutor "github.com/ormasoftchile/yawr/runtime/internal/executor"
	internalparser "github.com/ormasoftchile/yawr/runtime/internal/parser"
	internalplanner "github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/internal/resultsdelivery"
	internalroutetest "github.com/ormasoftchile/yawr/runtime/internal/routetest"
	internalserve "github.com/ormasoftchile/yawr/runtime/internal/serve"
	internaltool "github.com/ormasoftchile/yawr/runtime/internal/tool"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/expand"
	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/pkgcatalog"
	plannerpkg "github.com/ormasoftchile/yawr/runtime/pkg/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/runstate"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

const (
	outputText  = "text"
	outputJSON  = "json"
	outputQuiet = "quiet"

	exitSuccess    = 0
	exitFailure    = 1
	exitValidation = 2
	exitRuntime    = 3
)

type varFlags []string

func (v *varFlags) String() string {
	return strings.Join(*v, ",")
}

func (v *varFlags) Set(value string) error {
	*v = append(*v, value)
	return nil
}

func runRun(args []string) int {
	return runWithMode(args, engine.RunModeReal)
}

var defaultRunStoreDir = filepath.Join(".runbook", "runs")

func runWithMode(args []string, mode engine.RunMode) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var vars varFlags
	requiredCapabilities := fs.String("require-capabilities", "", "Comma-separated required public capabilities; reject unsupported capabilities before execution")
	tracePath := fs.String("trace", "", "Trace file output path")
	runDir := fs.String("run-dir", defaultRunStoreDir, "Durable run store directory")
	outputFormat := fs.String("output", outputText, "Output format: text, json, quiet")
	resumeID := fs.String("resume", "", "Resume a run by ID")
	otelEndpoint := fs.String("otel-endpoint", "", "OTLP gRPC endpoint for span export (e.g. localhost:4317)")
	otelService := fs.String("otel-service", "yawr", "service.name for OTel spans")
	otelStdout := fs.Bool("otel-stdout", false, "Write OTel spans to stderr as JSON (for debugging)")
	expandMode := fs.String("expand", "", "Default expansion mode for include sites: eager, lazy, or auto (overridden by per-runbook and per-include `expand:`)")
	packageMapPath := fs.String("package-map", "", "Path to a package-map file (yawr.config/v1 shape: requires:/tool-paths:) overriding the project's .yawr/config.yaml package bindings")
	profileID := fs.String("profile", "", "Runtime profile ID to load from the profiles directory (yawr.runtime-profile/v1). Composes with --package-map: --package-map selects which tool definition binds; --profile parameterizes execution after selection.")
	allowPackageDrift := fs.Bool("allow-package-drift", false, "Allow resuming a run whose resolved package/catalog digests differ from the ones recorded at plan time (records a governance/packageDriftAccepted trace event)")
	acknowledgeIndeterminate := fs.Bool("acknowledge-indeterminate", false, "Required to resume a run that halted with status INDETERMINATE. Confirms the operator has verified external state before resuming.")
	stdioMode := fs.Bool("stdio", false, "Use the yawr.stdio/v1 JSON-lines interaction protocol on stdin/stdout")
	debugMode := fs.Bool("debug", false, "Enable debugger configuration over the yawr.stdio/v1 protocol")
	configureMode := fs.Bool("configure", false, "Read private startup inputs from run.configure over yawr.stdio/v1")
	routeTestPath := fs.String("route-test", "", "Run a reviewed yawr.route-test/v1 artifact without external dispatch")
	fs.Var(&vars, "var", "Variable override (repeatable)")

	flagArgs, runbookPath, extraArgs := splitRunArgs(args)
	if err := fs.Parse(flagArgs); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitValidation
	}
	if *requiredCapabilities != "" {
		for _, capability := range strings.Split(*requiredCapabilities, ",") {
			switch capability {
			case "yawr.typed-results/v1", "yawr.run-results-chunks/v1", "yawr.run-get-results/v1", "yawr.terminal-results/v1", "yawr.terminal-outcome-gis/v1":
			case "yawr.file-only-subprocess/v1":
				if !toolpkg.FileOnlySubprocessAvailable() {
					fmt.Fprintln(os.Stderr, "run: unsupported-capability:", capability)
					return exitValidation
				}
			default:
				fmt.Fprintln(os.Stderr, "run: unsupported-capability:", capability)
				return exitValidation
			}
		}
	}
	if (runbookPath == "" && *resumeID == "") || len(extraArgs) > 0 {
		fmt.Fprintln(os.Stderr, "usage: yawr run <runbook-path> [flags]")
		return exitValidation
	}
	if *outputFormat != outputText && *outputFormat != outputJSON && *outputFormat != outputQuiet {
		fmt.Fprintln(os.Stderr, "invalid output format")
		return exitValidation
	}
	if (*debugMode || *configureMode) && !*stdioMode {
		fmt.Fprintln(os.Stderr, "--debug and --configure require --stdio")
		return exitValidation
	}
	if (*debugMode || *configureMode) && *resumeID != "" {
		fmt.Fprintln(os.Stderr, "configured stdio resume is not supported; start a new run")
		return exitValidation
	}
	var routeScenario *internalroutetest.Scenario
	var routeScheduler *internalroutetest.Scheduler
	if *routeTestPath != "" {
		if mode != engine.RunModeReal || *resumeID != "" || *debugMode || *configureMode {
			fmt.Fprintln(os.Stderr, "--route-test cannot be combined with dry-run, resume, debug, or configure")
			return exitValidation
		}
		if len(vars) > 0 {
			fmt.Fprintln(os.Stderr, "--var cannot override saved route-test inputs")
			return exitValidation
		}
		loaded, loadErr := internalroutetest.LoadScenario(*routeTestPath)
		if loadErr != nil {
			fmt.Fprintln(os.Stderr, loadErr)
			return exitValidation
		}
		scheduler, schedulerErr := internalroutetest.NewScheduler(loaded)
		if schedulerErr != nil {
			fmt.Fprintln(os.Stderr, schedulerErr)
			return exitValidation
		}
		routeScenario = &loaded
		routeScheduler = scheduler
		mode = engine.RunModeRouteTest
	}

	expandDefault, err := expand.Parse(*expandMode)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitValidation
	}
	if routeScenario != nil {
		if expandDefault != expand.ModeUnset && expandDefault != expand.ModeEager {
			fmt.Fprintln(os.Stderr, "route test: include expansion must be eager")
			return exitValidation
		}
		expandDefault = expand.ModeEager
	}

	// Load and validate the runtime profile if --profile was supplied.
	// Profiles compose with --package-map: --package-map controls WHICH
	// tool definition/package binds (YAML selection); --profile
	// parameterizes execution AFTER selection — context, attendance,
	// approval scope, per-tool parameters. They do not compete.
	var runtimeProfile *schema.RuntimeProfile
	if *profileID != "" {
		profilePath := *profileID
		if loaded, profErr := schema.ParseProfileFile(profilePath); profErr != nil {
			fmt.Fprintln(os.Stderr, profErr)
			return exitValidation
		} else {
			runtimeProfile = loaded
		}
	}

	// Profileless non-interactive fail-fast (Item 5).
	// Scoped to real execution only. In dry-run mode (RunModeDryRun) the
	// adapter wraps all executors with DryRunExecutorRegistry
	// (internal/adapter/wire.go) so no step is actually executed, and
	// in a non-TTY context buildApprovalGate installs NoOpApprovalGate
	// which never blocks — making the gate inapplicable.
	// For real execution: without a profile the engine normally derives
	// attendance solely from TTYOutput. Structured stdio is the one exception:
	// it installs PromptBroker as ApprovalGateOverride and requires an explicit
	// operator identity for every governed approval instead of auto-approving.
	// Other non-TTY callers would receive NoOpApprovalGate without a declared
	// approval scope, context, or operator identity, which is a governance gap.
	// Fail immediately with a clear, actionable error.
	if mode == engine.RunModeReal && runtimeProfile == nil && !isInteractiveTTY() && !*stdioMode {
		fmt.Fprintln(os.Stderr, "error: non-interactive execution requires an unattended runtime profile")
		fmt.Fprintln(os.Stderr, "fix: pass --profile <profile>")
		return exitValidation
	}

	traceFile := *tracePath
	if traceFile == "" {
		if *resumeID != "" {
			traceFile = filepath.Join(*runDir, *resumeID, "trace.jsonl")
		} else {
			traceFile = defaultTracePath(runbookPath)
		}
	}

	plat := platform.Real()
	parserImpl, err := internalparser.New(plat)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitRuntime
	}

	registry, err := newToolRegistry(".")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitRuntime
	}
	plannerImpl := internalplanner.New(plannerpkg.Config{
		Loader:       &fileRunbookLoader{parser: parserImpl},
		Tools:        registry,
		ExpandPolicy: expand.Policy{Default: expandDefault},
		Profile:      runtimeProfile, // Tier 0 preflight checks (PLAN-010/011/012) read from this.
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	var interactionBroker *internalserve.PromptBroker
	var protocol *stdioProtocol
	if *stdioMode {
		interactionBroker = internalserve.NewPromptBroker(64)
		protocol = newStdioProtocol(os.Stdin, os.Stdout)
		ctx = internaldebugprotect.WithSink(ctx, func(protection engine.DebugProtection) {
			protocol.handleOutputFailure(protocol.extendRedaction(protection))
		})
	}

	// plan is declared here (before BuildEngineConfig) so the pinRecorder
	// closure can capture it by reference; it is assigned during planning below.
	var plan *engine.ExecutionPlan
	resolverProxy := adapter.NewResolverProxy()
	pinRecorder := internalexecutor.PinRecorder(func(pin internalexecutor.DynamicIncludePin) {
		if plan == nil {
			return
		}
		plan.Metadata.DynamicIncludes = append(plan.Metadata.DynamicIncludes, schema.LockedDynamicInclude{
			StepID:             pin.StepID,
			QualifiedNodeID:    pin.QualifiedNodeID,
			Invocation:         pin.Invocation,
			Revision:           pin.Revision,
			StructuralPath:     append([]schema.DynamicIncludeFrameIdentity(nil), pin.StructuralPath...),
			RenderedRef:        pin.RenderedRef,
			QualifiedID:        pin.QualifiedID,
			RunbookID:          pin.RunbookID,
			RunbookName:        pin.RunbookName,
			RunbookContentHash: pin.ContentHash,
			AbsPath:            pin.AbsPath,
			PackageName:        pin.PackageName,
			PackageVersion:     pin.PackageVersion,
			FileDigest:         pin.FileDigest,
			PackageDigest:      pin.PackageDigest,
			ExecutableClosure:  pin.ExecutableClosure,
			ResolvedInputs:     pin.ResolvedInputs,
			ResolvedBindings:   pin.ResolvedBindings,
			ResolvedOutputs:    pin.ResolvedOutputs,
			ResolvedGovernance: pin.ResolvedGovernance,
		})
	})

	wireOptions := adapter.WireOptions{
		Mode:                   string(mode),
		RunDir:                 *runDir,
		TraceFile:              traceFile,
		TTYOutput:              isInteractiveTTY() && !*stdioMode,
		Output:                 protocolOutput(*stdioMode),
		Attended:               attendedFromProfile(runtimeProfile),
		Profile:                runtimeProfile,
		ToolScanDir:            ".",
		OTelEndpoint:           *otelEndpoint,
		OTelServiceName:        *otelService,
		OTelStdout:             *otelStdout,
		LazyRunbookLoader:      adapter.NewParserLazyLoader(parserImpl),
		SubstitutionParser:     parserImpl,
		DynamicIncludeResolver: resolverProxy,
		PinRecorder:            pinRecorder,
		RouteTestScheduler:     routeScheduler,
	}
	if interactionBroker != nil {
		wireOptions.PromptProviderOverride = interactionBroker
		wireOptions.HostActionProvider = interactionBroker
		wireOptions.ApprovalGateOverride = interactionBroker
	}
	ecfg, shutdown, err := adapter.BuildEngineConfig(ctx, wireOptions)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitRuntime
	}
	defer shutdown()

	runVars, err := parseVarFlags(vars)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitValidation
	}
	if routeScenario != nil {
		for name, value := range routeScenario.Inputs {
			runVars[name] = value
		}
	}
	var startupConfig stdioRunConfig
	if *debugMode || *configureMode {
		startupConfig, err = protocol.readRunConfig(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return exitValidation
		}
		if *debugMode && (startupConfig.Debug == nil || !startupConfig.Debug.Enabled) {
			fmt.Fprintln(os.Stderr, "stdio protocol: enabled debug configuration is required")
			return exitValidation
		}
		for name, value := range startupConfig.Inputs {
			if _, exists := runVars[name]; exists {
				fmt.Fprintf(os.Stderr, "stdio protocol: private input %q duplicates --var\n", name)
				return exitValidation
			}
			runVars[name] = value
		}
	}

	eng := internalengine.New(ecfg)
	var handle engine.RunHandle
	var debugger engine.DebugController
	var routeTestController engine.RouteTestController
	if routeScheduler != nil {
		routeTestController = routeScheduler
	}
	var plannedRunID string
	preStart := &preStartTrace{writer: ecfg.TraceWriter}
	var registeredRunID string
	defer func() {
		if interactionBroker != nil && registeredRunID != "" {
			interactionBroker.Unregister(registeredRunID)
		}
	}()
	if *resumeID != "" {
		if interactionBroker != nil {
			interactionBroker.Register(*resumeID)
			registeredRunID = *resumeID
		}
		handle, err = eng.Resume(ctx, *resumeID, engine.RunOptions{
			Mode:                     mode,
			Vars:                     runVars,
			Store:                    ecfg.Store,
			Client:                   "cli",
			AcknowledgeIndeterminate: *acknowledgeIndeterminate,
		})
		if err == nil {
			// Package resumption contract:
			// 13-evidence-tracing-resumption.tex §Package Resumption
			// Contract): recompute the current package/catalog digests
			// the same way a fresh run would and compare them against
			// the ones frozen into the plan at Start time. A mismatch is
			// a hard PKG-009 refusal on resume unless
			// --allow-package-drift is passed, in which case the drift is
			// still recorded (never silently bypassed) via a
			// governance/packageDriftAccepted trace event.
			if driftErr := checkResumePackageDrift(ctx, ecfg, parserImpl, handle, *resumeID, *packageMapPath, *allowPackageDrift); driftErr != nil {
				fmt.Fprintln(os.Stderr, driftErr)
				return exitValidation
			}
		}
	} else {
		parsed, parseErr := parserImpl.Parse(ctx, runbookPath)
		if parseErr != nil {
			fmt.Fprintln(os.Stderr, parseErr)
			return exitValidation
		}
		var routeGraphHash string
		if routeScenario != nil {
			document, hashErr := (&graphdoc.Builder{Loader: &cliLoader{p: parserImpl}, Recurse: true}).Build(ctx, parsed)
			if hashErr != nil {
				fmt.Fprintln(os.Stderr, hashErr)
				return exitValidation
			}
			routeGraphHash = document.Hash
		}
		if err := validateStdioPrivateInputs(parsed.Runbook.Inputs, startupConfig.Inputs); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return exitValidation
		}
		if routeScenario != nil {
			for name := range routeScenario.Inputs {
				if declaration := parsed.Runbook.Inputs[name]; declaration != nil && declaration.Type == "secret" {
					fmt.Fprintf(os.Stderr, "route test: secret input %q cannot be stored in the artifact\n", name)
					return exitValidation
				}
			}
		}
		// ENUM-W001 (AR-ENUM-12, barbara-enum-mvp-implementation-gate.md
		// R4): a warning nothing prints is not a warning. Surface every
		// non-fatal parse warning (S3/S4 declarations included) to
		// stderr; the run still proceeds -- these MUST NOT fail it.
		for _, w := range parsed.Warnings {
			fmt.Fprintf(os.Stderr, "yawr: warning: %s: %s\n", w.Field, w.Message)
		}
		if err := applyInputDefaults(parsed, runVars); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return exitValidation
		}

		// The include closure's global package
		// set and per-file lexical tool binding is not implemented in
		// this revision; fail closed (PKG-017) rather than silently
		// dynamic-scope an included file's own requires:/toolRefs:
		// against the root's frozen catalog and registry.
		if lexErrs := checkIncludeClosureLexicalScoping(ctx, parserImpl, parsed); len(lexErrs) > 0 {
			for _, e := range lexErrs {
				fmt.Fprintln(os.Stderr, e)
			}
			return exitValidation
		}

		var builtCatalog *pkgcatalog.Catalog

		// Resolve requires:/toolRefs: against the Tool Packages MVP catalog
		// (pkg/pkgcatalog) and register the results into both registries.
		// The project's real .yawr/config.yaml bindings are used unless
		// --package-map overrides them, so the exact same runbook binds
		// its real project tools by default and mock/test bindings under
		// --package-map, with no runbook edits either way.
		if parsed.Runbook != nil {
			workspaceRoot, wderr := os.Getwd()
			if wderr != nil {
				fmt.Fprintln(os.Stderr, wderr)
				return exitRuntime
			}
			projCfg, cfgErr := loadProjectConfig(workspaceRoot)
			if cfgErr != nil {
				fmt.Fprintln(os.Stderr, cfgErr)
				return exitValidation
			}
			var projectRequires []*schema.PackageRequirement
			var projectToolPaths []string
			if projCfg != nil {
				projectRequires = projCfg.Requires
				projectToolPaths = projCfg.ToolPaths
			}
			var overrideRequires []*schema.PackageRequirement
			var overrideToolPaths []string
			if *packageMapPath != "" {
				pmCfg, pmErr := loadPackageMap(*packageMapPath)
				if pmErr != nil {
					fmt.Fprintln(os.Stderr, pmErr)
					return exitValidation
				}
				if pmCfg != nil {
					overrideRequires = pmCfg.Requires
					overrideToolPaths = pmCfg.ToolPaths
				}
			}
			mergedRequires, provenance := mergePackageBindings(projectRequires, overrideRequires)
			mergedToolPaths := mergeToolPaths(projectToolPaths, overrideToolPaths)

			catOpts := adapter.PackageCatalogOptions{
				WorkspaceRoot:    workspaceRoot,
				Builtins:         internaltool.NewBuiltinRegistry().All(),
				ProjectRequires:  mergedRequires,
				ProjectToolPaths: mergedToolPaths,
			}
			cat, catErrs := adapter.BuildPackageCatalog(catOpts, runbookPath, parsed.Runbook.Requires)
			fatalCatErrs, warnCatErrs := errkit.SplitWarnings(catErrs)
			if len(fatalCatErrs) > 0 {
				for _, e := range fatalCatErrs {
					fmt.Fprintln(os.Stderr, e)
				}
				return exitValidation
			}
			for _, e := range warnCatErrs {
				fmt.Fprintln(os.Stderr, e)
			}
			builtCatalog = cat
			// Wire the frozen catalog into the dynamic include resolver now
			// that Phase C is complete. Must happen before eng.Start so the
			// proxy is ready when the first dynamic include executes.
			resolverProxy.Set(adapter.NewCatalogIncludeResolver(builtCatalog, parserImpl))
			// Emit package/resolved (per resolved package) and
			// catalog/frozen (once) at the actual runtime boundary where
			// Phase C (catalog construction) completes, before Phase B
			// (binding) and before the engine's own run/started event —
			// using the same runID the run is about to Start with and the
			// same TraceWriter the engine writes every other event to.
			plannedRunID = uuid.New().String()
			var packageOrigin map[string]string
			if *packageMapPath != "" {
				packageOrigin = make(map[string]string, len(provenance))
				for _, p := range provenance {
					packageOrigin[p.Package] = p.Origin
				}
			}
			if emitErr := adapter.EmitPackageCatalogTraceEvents(preStart, plannedRunID, cat, packageOrigin); emitErr != nil {
				fmt.Fprintln(os.Stderr, emitErr)
				return exitRuntime
			}
			if *packageMapPath != "" {
				for _, p := range provenance {
					fmt.Fprintf(os.Stderr, "yawr: package %q bound from %s\n", p.Package, p.Origin)
				}
			}

			if len(parsed.Runbook.ToolRefs) > 0 {
				var warnErrs []error
				bindErrs := func() []error {
					defs, errs := adapter.ResolveToolRefsViaCatalog(cat, runbookPath, parsed.Runbook.ToolRefs)
					// ResolveToolRefsViaCatalog already separates fatal
					// errors from PKG-W* advisories: a non-nil defs return
					// alongside a non-empty errs means errs is
					// warnings-only:
					// PKG-W003 workspace-escape reports MUST NOT abort
					// binding). A nil defs return with non-empty errs is
					// the fatal case.
					if defs == nil && len(errs) > 0 {
						return errs
					}
					warnErrs = errs
					// Force-register into the runtime tool registry via
					// Override so a catalog-bound name always wins
					// invocation, even when the generic workspace
					// directory scan (adapter.BuildEngineConfig's
					// ToolScanDir walk, done once at wiring time) already
					// registered a same-named tool from a different
					// package root — see internal/tool.OverlayRegistry.
					reg := ecfg.ToolRuntime.(*internaltool.DefaultToolRuntime).Registry()
					overlay, isOverlay := reg.(*internaltool.OverlayRegistry)
					for _, def := range defs {
						if isOverlay {
							overlay.Override(def)
							continue
						}
						if err := reg.Register(def); err != nil {
							return []error{fmt.Errorf("failed to register tool %s: %w", def.Name, err)}
						}
					}
					// Register into planner's schema registry so Plan()'s
					// tool/action lookups succeed for package- and
					// bare-name-resolved refs the same way they already do
					// for path-resolved ones.
					for _, def := range defs {
						schemaDef := schemaToolDefFromRuntime(def)
						for action := range schemaDef.Actions {
							registry.tools[schemaDef.Name+"/"+action] = schemaDef
						}
					}
					return nil
				}()
				if len(bindErrs) > 0 {
					for _, e := range bindErrs {
						fmt.Fprintln(os.Stderr, e)
					}
					return exitValidation
				}
				for _, e := range warnErrs {
					fmt.Fprintln(os.Stderr, e)
				}
			}
		}

		// Substitution contracts MUST be
		// validated statically at plan time, for every reachable action
		// (dry-run and unreachable-by-condition steps included), not
		// lazily the first time a substituted step executes. Runs
		// unconditionally here -- before Plan and before Start -- so it
		// also covers --mode=dry-run, which never itself reaches
		// internal/executor/tool.go's runtime check.
		//
		// This check and Plan() (which runs ENUM-006/007) are
		// independent, orthogonal validations over the same runbook; a
		// genuine finding from one MUST NOT mask a genuine finding from
		// the other (a substitution-contract defect on one action, e.g.
		// a PKG-013 output-name mismatch, is unrelated to an enum
		// default/literal defect on a different declaration, and both
		// belong in the same validation report). Both therefore run
		// unconditionally and their errors are aggregated before
		// deciding whether to abort.
		var subErrs []error
		if reg := ecfg.ToolRuntime.(*internaltool.DefaultToolRuntime).Registry(); reg != nil {
			toolDefs := make(map[string]toolpkg.ToolDef)
			for _, def := range reg.All() {
				toolDefs[def.Name] = def
			}
			subErrs = validateSubstitutionsPlanTime(ctx, parserImpl, parsed, toolDefs)
		}

		plan, err = plannerImpl.Plan(ctx, parsed)
		if err != nil || len(subErrs) > 0 {
			for _, e := range subErrs {
				fmt.Fprintln(os.Stderr, e)
			}
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
			return exitValidation
		}
		if routeScenario != nil {
			routePlanHash, hashErr := routeTestPlanHash(routeGraphHash, plan, builtCatalog, runtimeProfile)
			if hashErr != nil {
				fmt.Fprintln(os.Stderr, hashErr)
				return exitValidation
			}
			expectedRunbook, pathErr := filepath.Abs(routeScenario.Runbook)
			if pathErr != nil || !sameFilePath(expectedRunbook, runbookPath) {
				fmt.Fprintln(os.Stderr, "route test: runbook subject does not match the requested runbook")
				return exitValidation
			}
			if routeScenario.PlanHash != routePlanHash {
				fmt.Fprintln(os.Stderr, "route test: plan changed; review and save the route test again")
				return exitValidation
			}
			if compileErr := internalroutetest.ValidateScenarioPlan(*routeScenario, plan); compileErr != nil {
				fmt.Fprintln(os.Stderr, compileErr)
				return exitValidation
			}
		}
		if plannedRunID != "" {
			// Reuse the runID the package/resolved and catalog/frozen
			// trace events were already stamped with, so all three
			// pre-run and in-run events correlate under the same run_id
			// in trace.jsonl even though the catalog events had to be
			// written before the plan/run object existed.
			plan.RunID = plannedRunID
		}
		if builtCatalog != nil {
			// Freeze the package/catalog digests into the plan's metadata
			// so they are persisted as part of RunState.Plan (RunStore.
			// SaveState marshals the whole state, plan included) and can
			// be compared against a freshly recomputed catalog on resume
			// (checkResumePackageDrift below) via pkg/pkgdrift.Evaluate.
			plan.Metadata.CatalogDigest = builtCatalog.CatalogDigest()
			pkgDigests := make(map[string]string, len(builtCatalog.Packages))
			for _, p := range builtCatalog.Packages {
				pkgDigests[p.Name] = p.Digest
			}
			plan.Metadata.PackageDigests = pkgDigests
		}
		// Seed the entry runbook's governance into ctx so executeDynamic can
		// compose child governance against the root policy rather than nil
		// (DEF-006). This must happen after Plan() so parsed.Runbook is final.
		if parsed != nil && parsed.Runbook != nil {
			ctx = internalexecutor.WithEntryGovernance(ctx, parsed.Runbook.Governance)
		}
		// Carry the runtime profile onto the plan so approval and preflight
		// checks can consume it without a separate lookup.
		// The profile MUST be consumed from plan.Tools (post-catalog,
		// post-package-map), never used to re-resolve toolRefs.
		if runtimeProfile != nil {
			plan.Metadata.Profile = runtimeProfile
		}
		if interactionBroker != nil {
			if plan.RunID == "" {
				plan.RunID = uuid.NewString()
			}
			interactionBroker.Register(plan.RunID)
			registeredRunID = plan.RunID
		}
		if *debugMode {
			if configErr := interactionBroker.ConfigureDebug(plan.RunID, *startupConfig.Debug); configErr != nil {
				fmt.Fprintln(os.Stderr, configErr)
				return exitValidation
			}
			debugger = interactionBroker
		}
		handle, err = eng.Start(ctx, plan, engine.RunOptions{
			Mode:           mode,
			Vars:           runVars,
			Store:          ecfg.Store,
			Client:         "cli",
			Debugger:       debugger,
			RouteTest:      routeTestController,
			PreStartEvents: preStart.events,
		})
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitRuntime
	}
	if protocol != nil {
		state := handle.State()
		protection, complete := stdioDeclaredProtection(ctx, ecfg.Executors, state.Plan, state.Vars)
		if !complete && !internaldebugprotect.HasSink(ctx) {
			fmt.Fprintln(os.Stderr, "stdio protocol: incomplete secret protection requires runtime redaction")
			_ = handle.Cancel(ctx, "stdio runtime redaction unavailable")
			return exitRuntime
		}
		if err := protocol.configureRedaction(protection.SecretValues, protection.RedactionPatterns); err != nil {
			fmt.Fprintln(os.Stderr, err)
			_ = handle.Cancel(ctx, "stdio redaction configuration failed")
			return exitRuntime
		}
		runID := state.RunID
		if runID == "" {
			runID = registeredRunID
		}
		if err := protocol.send(map[string]any{
			"type":   "run.started",
			"runID":  runID,
			"status": handle.State().Status,
		}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			_ = handle.Cancel(ctx, "stdio run.started write failed")
			return exitRuntime
		}
		if err := protocol.attach(ctx, runID, interactionBroker, handle); err != nil {
			fmt.Fprintln(os.Stderr, err)
			_ = handle.Cancel(ctx, "stdio protocol failed to attach")
			return exitRuntime
		}
	}

	stepKinds := mapStepKinds(plan)
	var results []stepSummary
	hadFailure := false
	var runFailure string

	for {
		result, err := handle.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			runFailure = err.Error()
			hadFailure = true
			break
		}
		if result == nil {
			continue
		}
		if result.Status == engine.StepStatusFailed || result.Status == engine.StepStatusDenied {
			hadFailure = true
			if result.Error != nil {
				runFailure = result.Error.Error()
			}
		}
		summary := summarizeStep(result, stepKinds[result.StepID])
		if protocol != nil {
			summary = summarizeStdioStep(result, stepKinds[result.StepID])
		}
		results = append(results, summary)
		if *outputFormat == outputText && protocol == nil {
			renderStepText(os.Stdout, result, stepKinds[result.StepID], mode == engine.RunModeDryRun)
		}
	}

	state := handle.State()
	reportedStatus := state.Status
	routeTestPassed := false
	routeTestStatus := ""
	routeTestMessage := ""
	if routeScheduler != nil {
		if verifyErr := routeScheduler.Verify(); verifyErr != nil {
			fmt.Fprintln(os.Stderr, verifyErr)
			routeTestMessage = verifyErr.Error()
			hadFailure = true
		} else if routeScheduler.ExternalDispatchCount() != 0 {
			fmt.Fprintln(os.Stderr, "route test verification failed: external dispatch count was not zero")
			hadFailure = true
		} else if state.Status == engine.RunStatusPausedAtBoundary {
			routeTestPassed = true
			routeTestStatus = "reached"
			hadFailure = false
			reportedStatus = engine.RunStatusPausedAtBoundary
		} else {
			hadFailure = true
		}
		if !routeTestPassed {
			switch {
			case strings.Contains(strings.ToLower(runFailure), "route test safety failure"):
				routeTestStatus = "safety-failed"
				routeTestMessage = runFailure
			case state.Status == engine.RunStatusCancelled:
				routeTestStatus = "stopped"
			case runFailure != "":
				routeTestStatus = "runtime-failed"
				routeTestMessage = runFailure
			case state.Status == engine.RunStatusCompleted:
				routeTestStatus = "route-changed"
			default:
				routeTestStatus = "runtime-failed"
				if runFailure != "" {
					routeTestMessage = runFailure
				}
			}
		}
	}
	exitCode := exitCodeForStatus(reportedStatus)
	if routeTestPassed {
		exitCode = exitSuccess
	}
	if hadFailure {
		exitCode = exitFailure
	}
	if protocol != nil {
		forwardingCtx := context.WithoutCancel(ctx)
		if interactionBroker != nil && registeredRunID != "" {
			interactionBroker.Unregister(registeredRunID)
			registeredRunID = ""
		}
		if err := protocol.waitForInteractions(forwardingCtx); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return exitRuntime
		}
		if err := protocol.waitForEvents(forwardingCtx); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return exitRuntime
		}
		boundedSteps, stepsOmitted := boundedStdioStepSummaries(results)
		finished := map[string]any{
			"type":   "run.finished",
			"runID":  state.RunID,
			"status": reportedStatus,
			"steps":  boundedSteps,
		}
		if routeScheduler != nil {
			finished["routeTest"] = map[string]any{
				"passed": routeTestPassed, "targetReached": routeScheduler.TargetReached(),
				"externalDispatches": routeScheduler.ExternalDispatchCount(),
				"status":             routeTestStatus, "message": routeTestMessage,
			}
		}
		if stepsOmitted > 0 {
			finished["stepsTruncated"] = true
			finished["stepsOmitted"] = stepsOmitted
		}
		if runFailure != "" {
			finished["error"] = stdioExecutionError(runFailure)
		}
		if err := protocol.sendFinished(finished, state); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return exitRuntime
		}
		return exitCode
	}

	switch *outputFormat {
	case outputJSON:
		renderJSONSummary(os.Stdout, reportedStatus, results, state)
	case outputText, outputQuiet:
		if routeTestPassed && *outputFormat == outputText {
			fmt.Fprintln(os.Stdout, "Step reached - command not run")
		}
		renderSummary(os.Stdout, reportedStatus, len(results), mode == engine.RunModeDryRun)
	}
	return exitCode
}

func validateStdioPrivateInputs(declarations map[string]*schema.Input, inputs map[string]string) error {
	for name := range inputs {
		declaration := declarations[name]
		if declaration == nil || declaration.Type != "secret" {
			return fmt.Errorf("stdio protocol: private input %q is not declared as secret", name)
		}
	}
	return nil
}

type stdioProtectionProvider interface {
	ResolveDebugProtection(context.Context, engine.ResolvedStep, map[string]any) (engine.DebugProtection, bool)
}

func stdioDeclaredProtection(ctx context.Context, registry engine.ExecutorRegistry, plan *engine.ExecutionPlan, vars map[string]any) (engine.DebugProtection, bool) {
	if plan == nil {
		return engine.DebugProtection{}, true
	}
	protection := engine.ExtendDebugProtection(engine.DebugProtection{}, vars, plan.Inputs, plan.Governance)
	complete := true
	for _, step := range plan.Steps {
		if step.Kind != "include" {
			continue
		}
		if registry == nil {
			complete = false
			continue
		}
		provider, ok := registry.Lookup(step.Kind).(stdioProtectionProvider)
		if !ok {
			complete = false
			continue
		}
		additional, resolved := provider.ResolveDebugProtection(ctx, step, vars)
		protection = engine.MergeDebugProtection(protection, additional)
		complete = complete && resolved
	}
	return protection, complete
}
func splitRunArgs(args []string) ([]string, string, []string) {
	flagArgs := make([]string, 0, len(args))
	var runbook string
	var extra []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			flagArgs = append(flagArgs, arg)
			if arg == "--trace" || arg == "--run-dir" || arg == "--output" || arg == "--var" || arg == "--resume" ||
				arg == "--otel-endpoint" || arg == "--otel-service" || arg == "--package-map" || arg == "--profile" || arg == "--route-test" || arg == "--require-capabilities" {
				if i+1 < len(args) {
					flagArgs = append(flagArgs, args[i+1])
					i++
				}
			}
			continue
		}
		if runbook == "" {
			runbook = arg
			continue
		}
		extra = append(extra, arg)
	}
	return flagArgs, runbook, extra
}

func defaultTracePath(runbookPath string) string {
	base := filepath.Base(runbookPath)
	trimmed := strings.TrimSuffix(base, filepath.Ext(base))
	ts := time.Now().UTC().Format("20060102-150405")
	return fmt.Sprintf("%s-%s.jsonl", trimmed, ts)
}

func sameFilePath(left, right string) bool {
	leftInfo, leftErr := os.Stat(left)
	rightInfo, rightErr := os.Stat(right)
	if leftErr == nil && rightErr == nil {
		return os.SameFile(leftInfo, rightInfo)
	}
	return sameFilePathForOS(runtime.GOOS, left, right)
}

func sameFilePathForOS(goos, left, right string) bool {
	leftAbs, leftErr := filepath.Abs(left)
	rightAbs, rightErr := filepath.Abs(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	leftAbs = filepath.Clean(leftAbs)
	rightAbs = filepath.Clean(rightAbs)
	if goos == "windows" {
		return strings.EqualFold(leftAbs, rightAbs)
	}
	return leftAbs == rightAbs
}

func parseVarFlags(vars varFlags) (map[string]string, error) {
	out := make(map[string]string, len(vars))
	for _, pair := range vars {
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) != 2 || parts[0] == "" {
			return nil, fmt.Errorf("invalid --var %q", pair)
		}
		out[parts[0]] = parts[1]
	}
	return out, nil
}

func applyInputDefaults(parsed *parser.ParsedRunbook, vars map[string]string) error {
	if parsed == nil || parsed.Runbook == nil {
		return nil
	}
	// ENUM-008 (AR-ENUM-7, barbara-enum-mvp-implementation-gate.md R1):
	// vars at this point contains only caller-supplied --var overrides --
	// defaults are seeded below -- so every present key is a genuine
	// caller-binding. This is the live product entry point (`yawr run`)
	// that pkg/run.Start's own ENUM-008 check never reaches, since
	// cmd/yawr/run.go builds its plan/vars independently and calls
	// internal/engine directly.
	if err := schema.CheckCallerInputBindings(parsed.Runbook.Inputs, vars); err != nil {
		return err
	}
	// Seed runbook-level vars so templates referencing them resolve.
	// Caller-supplied --var flags win over runbook defaults.
	for k, v := range parsed.Runbook.Vars {
		if _, ok := vars[k]; ok {
			continue
		}
		if s, ok := v.(string); ok {
			vars[k] = s
		} else {
			vars[k] = fmt.Sprint(v)
		}
	}
	// Seed declared inputs. Priority: caller-supplied > input default >
	// "" (for non-required inputs). Required inputs without a default
	// remain unset and will surface a clear template error if used.
	for name, input := range parsed.Runbook.Inputs {
		if input == nil {
			continue
		}
		if _, ok := vars[name]; ok {
			continue
		}
		if input.Default != nil {
			vars[name] = fmt.Sprint(input.Default)
		} else if !input.Required {
			vars[name] = ""
		}
	}
	return nil
}

type fileRunbookLoader struct {
	parser parser.Parser
}

func (l *fileRunbookLoader) Load(ctx context.Context, path string) (*parser.ParsedRunbook, error) {
	return l.parser.Parse(ctx, path)
}

type plannerToolRegistry struct {
	tools map[string]*schema.ToolDef
}

func newToolRegistry(dir string) (*plannerToolRegistry, error) {
	defs, err := internaltool.ScanSchemaDir(dir)
	if err != nil {
		return nil, err
	}
	reg := &plannerToolRegistry{tools: make(map[string]*schema.ToolDef)}
	for _, def := range defs {
		if def == nil {
			continue
		}
		if len(def.Actions) == 0 {
			continue
		}
		for action := range def.Actions {
			reg.tools[def.Name+"/"+action] = def
		}
	}
	return reg, nil
}

func (r *plannerToolRegistry) Lookup(ctx context.Context, name string, action string) (*schema.ToolDef, error) {
	_ = ctx
	key := name + "/" + action
	if tool, ok := r.tools[key]; ok {
		return tool, nil
	}
	for k := range r.tools {
		if strings.HasPrefix(k, name+"/") {
			return nil, plannerpkg.ErrActionNotFound
		}
	}
	return nil, plannerpkg.ErrToolNotFound
}

type stepSummary struct {
	StepID      string         `json:"step_id"`
	NodeID      string         `json:"node_id,omitempty"`
	Kind        string         `json:"kind"`
	Status      string         `json:"status"`
	DurationMs  int64          `json:"duration_ms"`
	Output      map[string]any `json:"output,omitempty"`
	Error       string         `json:"error,omitempty"`
	StartedAt   time.Time      `json:"started_at"`
	CompletedAt time.Time      `json:"completed_at"`
}

func summarizeStep(result *engine.StepResult, kind string) stepSummary {
	summary := stepSummary{
		StepID:      result.StepID,
		Kind:        kind,
		Status:      string(result.Status),
		DurationMs:  result.DurationMs,
		Output:      result.Output,
		StartedAt:   result.StartedAt,
		CompletedAt: result.CompletedAt,
	}
	if result.Error != nil {
		summary.Error = result.Error.Error()
	}
	return summary
}

func summarizeStdioStep(result *engine.StepResult, kind string) stepSummary {
	summary := summarizeStep(result, kind)
	summary.NodeID = engine.DebugNodeID(nil, result.StepID)
	summary.Output = runstate.PreviewOutput(result.Output)
	if result.Error != nil {
		bounded := runstate.PreviewEventPayload(map[string]any{"error": result.Error.Error()})
		summary.Error, _ = bounded["error"].(string)
	}
	return summary
}

func boundedStdioStepSummaries(results []stepSummary) ([]stepSummary, int) {
	const stepsBudget = maxStdioFrameBytes * 3 / 4
	bounded := make([]stepSummary, 0, len(results))
	used := 2
	for index, result := range results {
		encoded, err := json.Marshal(result)
		if err != nil || used+len(encoded)+1 > stepsBudget {
			return bounded, len(results) - index
		}
		bounded = append(bounded, result)
		used += len(encoded) + 1
	}
	return bounded, 0
}

func mapStepKinds(plan *engine.ExecutionPlan) map[string]string {
	out := make(map[string]string)
	if plan == nil {
		return out
	}
	for _, step := range plan.Steps {
		out[step.ID] = step.Kind
	}
	return out
}

func renderStepText(w io.Writer, result *engine.StepResult, kind string, dryRun bool) {
	if result == nil {
		return
	}
	status := "•"
	switch result.Status {
	case engine.StepStatusCompleted:
		status = "✓"
	case engine.StepStatusSkipped:
		status = "⊘ skipped"
	case engine.StepStatusFailed:
		status = "✗"
	}
	duration := result.DurationMs
	prefix := ""
	if dryRun {
		prefix = "[DRY-RUN] "
	}
	if result.Status == engine.StepStatusFailed && result.Error != nil {
		fmt.Fprintf(w, "%s%s %s (%s) — %dms: %v\n", prefix, status, result.StepID, kind, duration, result.Error)
		return
	}
	if result.Status == engine.StepStatusSkipped {
		if warning, ok := result.Output["warning"].(string); ok && warning != "" {
			fmt.Fprintf(w, "%s%s %s (%s) — %dms: %s\n", prefix, status, result.StepID, kind, duration, warning)
			return
		}
	}
	fmt.Fprintf(w, "%s%s %s (%s) — %dms\n", prefix, status, result.StepID, kind, duration)
}

func renderSummary(w io.Writer, status engine.RunStatus, steps int, dryRun bool) {
	prefix := ""
	if dryRun {
		prefix = "dry-run "
	}
	fmt.Fprintf(w, "%scomplete: status=%s steps=%d\n", prefix, status, steps)
}

func renderJSONSummary(w io.Writer, status engine.RunStatus, steps []stepSummary, state engine.RunState) {
	record, cloneErr := state.CloneResults()
	body, unavailable := resultsdelivery.Prepare(record, cloneErr, status, engine.DebugProtection{})
	payload := struct {
		RunID              string                       `json:"run_id"`
		Status             string                       `json:"status"`
		Steps              []stepSummary                `json:"steps"`
		Results            json.RawMessage              `json:"results"`
		ResultsUnavailable *resultsdelivery.Unavailable `json:"results_unavailable,omitempty"`
	}{
		RunID: state.RunID, Status: string(status), Steps: steps,
		Results: body, ResultsUnavailable: unavailable,
	}
	if body == nil {
		payload.Results = json.RawMessage("null")
	}
	enc := json.NewEncoder(w)
	_ = enc.Encode(payload)
}

func exitCodeForStatus(status engine.RunStatus) int {
	switch status {
	case engine.RunStatusCompleted:
		return exitSuccess
	case engine.RunStatusFailed, engine.RunStatusCancelled:
		return exitFailure
	default:
		return exitRuntime
	}
}

// attendedFromProfile derives the approval-attendance setting from a RuntimeProfile.
// Returns nil when no profile is present (fallback to TTYOutput behavior).
// This decouples approval attendance from TTY input/prompt configuration:
// only buildApprovalGate uses the returned value; buildInputProvider and
// buildPromptProvider continue to use TTYOutput.
func attendedFromProfile(profile *schema.RuntimeProfile) *bool {
	if profile == nil {
		return nil
	}
	switch profile.Attendance {
	case schema.ProfileAttendanceAttended:
		t := true
		return &t
	case schema.ProfileAttendanceUnattended:
		f := false
		return &f
	default:
		return nil
	}
}

// isInteractiveTTY reports whether stdin is connected to a character device
// (interactive terminal). Uses stdlib only — no external isatty dependency.
// Returns false when stdin has been redirected (pipe, /dev/null, CI runner,
// SSH session, etc.) and true for a real terminal session.
//
// The implementation is delegated to interactiveTTYDetect, which tests may
// replace to avoid per-test profile setup for tests that don't exercise the
// fail-fast path. The default implementation reads os.Stdin.Stat().
func isInteractiveTTY() bool {
	return interactiveTTYDetect()
}

// interactiveTTYDetect is the TTY detection implementation, overridable in
// tests. Tests that run runRun() without a profile must set this to return
// true (simulating an attended terminal) so the profileless fail-fast check
// does not fire. The production default reads os.Stdin.Stat().
var interactiveTTYDetect = defaultInteractiveTTYDetect

func defaultInteractiveTTYDetect() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}
