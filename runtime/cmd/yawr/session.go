package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ormasoftchile/yawr/runtime/internal/adapter"
	internalengine "github.com/ormasoftchile/yawr/runtime/internal/engine"
	internalexecutor "github.com/ormasoftchile/yawr/runtime/internal/executor"
	internalparser "github.com/ormasoftchile/yawr/runtime/internal/parser"
	internalplanner "github.com/ormasoftchile/yawr/runtime/internal/planner"
	internalserve "github.com/ormasoftchile/yawr/runtime/internal/serve"
	"github.com/ormasoftchile/yawr/runtime/internal/sessioncoordinator"
	"github.com/ormasoftchile/yawr/runtime/internal/sessionstdio"
	"github.com/ormasoftchile/yawr/runtime/internal/sessionstore"
	internaltool "github.com/ormasoftchile/yawr/runtime/internal/tool"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/expand"
	parserpkg "github.com/ormasoftchile/yawr/runtime/pkg/parser"
	plannerpkg "github.com/ormasoftchile/yawr/runtime/pkg/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/session"
	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

func runSession(args []string) int {
	return sessionMain(context.Background(), args, os.Stdin, os.Stdout, os.Stderr)
}

func sessionMain(ctx context.Context, args []string, input io.ReadCloser, output io.Writer, errorOutput io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(errorOutput, "usage: yawr session <start|attach|graph> ...")
		return exitValidation
	}
	switch args[0] {
	case "attach":
		return sessionAttachMain(ctx, args[1:], input, output, errorOutput)
	case "start":
		return sessionStartMain(ctx, args[1:], input, output, errorOutput)
	case "graph":
		return sessionGraphMain(ctx, args[1:], output, errorOutput)
	default:
		fmt.Fprintln(errorOutput, "usage: yawr session <start|attach|graph> ...")
		return exitValidation
	}
}

func sessionGraphMain(ctx context.Context, args []string, output io.Writer, errorOutput io.Writer) int {
	fs := flag.NewFlagSet("session graph", flag.ContinueOnError)
	fs.SetOutput(errorOutput)
	segmentID := fs.String("segment-id", "", "Session segment UUID")
	revision := fs.Int64("revision", 0, "Immutable graph revision")
	sessionDir := fs.String("session-dir", ".runbook/sessions", "Session storage directory")
	flagArgs, sessionID, extra := splitSessionGraphArgs(args)
	if err := fs.Parse(flagArgs); err != nil {
		return exitValidation
	}
	if len(extra) > 0 || *revision < 1 {
		fmt.Fprintln(errorOutput, "usage: yawr session graph <session-id> --segment-id <segment-id> --revision <n>")
		return exitValidation
	}
	if _, err := uuid.Parse(sessionID); err != nil {
		fmt.Fprintln(errorOutput, "session graph: session-id must be a UUID")
		return exitValidation
	}
	if _, err := uuid.Parse(*segmentID); err != nil {
		fmt.Fprintln(errorOutput, "session graph: segment-id must be a UUID")
		return exitValidation
	}
	store := sessionstore.NewDirStore(*sessionDir)
	defer store.Close()
	segment, graph, err := store.LoadSegmentGraphRevision(ctx, sessionID, *segmentID, *revision)
	if err != nil {
		fmt.Fprintln(errorOutput, err)
		return exitRuntime
	}
	response := struct {
		SchemaVersion string                `json:"schema_version"`
		SessionID     string                `json:"session_id"`
		Segment       session.SegmentRecord `json:"segment"`
		GraphRevision int64                 `json:"graph_revision"`
		GraphHash     string                `json:"graph_hash"`
		Data          []byte                `json:"data"`
	}{
		SchemaVersion: "yawr.session-graph-revision/v1", SessionID: sessionID, Segment: segment,
		GraphRevision: segment.GraphRevision, GraphHash: segment.GraphHash, Data: graph,
	}
	if err := json.NewEncoder(output).Encode(response); err != nil {
		fmt.Fprintln(errorOutput, err)
		return exitRuntime
	}
	return exitSuccess
}

func sessionAttachMain(ctx context.Context, args []string, input io.ReadCloser, output io.Writer, errorOutput io.Writer) int {
	fs := flag.NewFlagSet("session attach", flag.ContinueOnError)
	fs.SetOutput(errorOutput)
	stdio := fs.Bool("stdio", false, "Use the yawr.session-stdio/v1 JSON-lines protocol")
	afterSequence := fs.Int64("after-sequence", 0, "Replay session events after this durable sequence")
	sessionDir := fs.String("session-dir", ".runbook/sessions", "Session storage directory")
	runDir := fs.String("run-dir", ".runbook/runs", "Run projection storage directory")
	toolDir := fs.String("tool-dir", ".", "Tool discovery directory")
	packageMapPath := fs.String("package-map", "", "Path to a package-map file overriding project package bindings")
	profilePath := fs.String("profile", "", "Runtime profile file")
	flagArgs, sessionID, extra := splitSessionAttachArgs(args)
	if err := fs.Parse(flagArgs); err != nil {
		return exitValidation
	}
	if sessionID == "" || len(extra) > 0 || !*stdio || *afterSequence < 0 {
		fmt.Fprintln(errorOutput, "usage: yawr session attach <session-id> --stdio [--after-sequence N]")
		return exitValidation
	}
	if _, err := uuid.Parse(sessionID); err != nil {
		fmt.Fprintln(errorOutput, "session attach: session-id must be a UUID")
		return exitValidation
	}
	store := sessionstore.NewDirStore(*sessionDir)
	defer store.Close()
	profile, err := loadSessionRuntimeProfile(*profilePath)
	if err != nil {
		fmt.Fprintln(errorOutput, err)
		return exitValidation
	}
	stack, err := buildSessionRuntimeStack(ctx, store, *runDir, *toolDir, *packageMapPath, profile)
	if err != nil {
		fmt.Fprintln(errorOutput, err)
		return exitRuntime
	}
	defer stack.shutdown()
	attachment, err := sessionstdio.AttachReconciled(
		ctx, store, sessionID, *afterSequence, output,
		func(attachment *sessionstdio.Attachment) error {
			attachment.WithCommandHandler(stack.runtime)
			return stack.runtime.ReconcileAttached(ctx, attachment)
		},
	)
	if err != nil {
		var protocolErr *sessionstdio.ProtocolError
		if errors.As(err, &protocolErr) {
			payload, _ := json.Marshal(protocolErr)
			frame := session.StdioFrame{
				Version: session.StdioProtocolV1, Type: session.FrameProtocolError,
				FrameID: session.DigestJSON(protocolErr), SessionID: sessionID,
				SessionSequence: *afterSequence, SequenceIndex: 0, SequenceCount: 1,
				WriterEpoch: 0, Payload: payload,
			}
			_ = sessionstdio.WriteFrame(output, frame)
		} else {
			fmt.Fprintln(errorOutput, err)
		}
		return exitRuntime
	}
	defer attachment.Release()
	return serveSessionAttachment(ctx, store, attachment, stack.runtime, input, errorOutput)
}

func serveSessionAttachment(
	ctx context.Context,
	store *sessionstore.DirStore,
	attachment *sessionstdio.Attachment,
	runtime interface{ Done() <-chan error },
	input io.ReadCloser,
	errorOutput io.Writer,
) int {
	type serveOutcome struct {
		result sessionstdio.ServeResult
		err    error
	}
	serveCtx, cancelServe := context.WithCancel(ctx)
	defer cancelServe()
	serveDone := make(chan serveOutcome, 1)
	go func() {
		result, err := attachment.Serve(serveCtx, input)
		serveDone <- serveOutcome{result: result, err: err}
	}()
	runtimeDone := runtime.Done()
	for {
		select {
		case outcome := <-serveDone:
			return finishSessionAttachment(
				ctx, store, attachment, outcome.result, outcome.err, errorOutput,
			)
		case runtimeErr := <-runtimeDone:
			if runtimeErr == nil {
				runtimeDone = nil
				continue
			}
			cancelServe()
			_ = input.Close()
			<-serveDone
			fmt.Fprintln(errorOutput, runtimeErr)
			return exitRuntime
		case <-ctx.Done():
			cancelServe()
			_ = input.Close()
			outcome := <-serveDone
			return finishSessionAttachment(
				ctx, store, attachment, outcome.result, outcome.err, errorOutput,
			)
		}
	}
}

func finishSessionAttachment(
	ctx context.Context,
	store *sessionstore.DirStore,
	attachment *sessionstdio.Attachment,
	serveResult sessionstdio.ServeResult,
	err error,
	errorOutput io.Writer,
) int {
	sessionID := attachment.SessionID()
	if serveResult.Detached {
		return exitSuccess
	}
	reason := "stdio input closed"
	if err != nil && !errors.Is(err, context.Canceled) {
		reason = "stdio transport lost"
		var protocolErr *sessionstdio.ProtocolError
		if errors.As(err, &protocolErr) {
			_ = attachment.SendProtocolError(context.WithoutCancel(ctx), protocolErr)
		}
	}
	detachCommandID := uuid.NewSHA1(
		uuid.MustParse(sessionID), []byte(fmt.Sprintf("%s:%d", reason, attachment.WriterEpoch())),
	).String()
	var detachErr error
	for attempt := 0; attempt < 128; attempt++ {
		manifest, manifestErr := store.LoadManifest(context.WithoutCancel(ctx), sessionID)
		if manifestErr != nil {
			detachErr = manifestErr
			break
		}
		if manifest.Session.ActiveRunID == "" {
			detachErr = nil
			break
		}
		_, detachErr = attachment.HandleCommand(context.WithoutCancel(ctx), session.StdioCommand{
			Version: session.StdioProtocolV1, Type: session.CommandSessionDetach,
			CommandID: detachCommandID, SessionID: sessionID, WriterEpoch: attachment.WriterEpoch(),
			ExpectedSequence: manifest.Session.Sequence,
		})
		if !errors.Is(detachErr, session.ErrSequenceConflict) {
			break
		}
	}
	if detachErr != nil {
		fmt.Fprintln(errorOutput, detachErr)
		return exitRuntime
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(errorOutput, err)
		return exitRuntime
	}
	return exitSuccess
}

func sessionStartMain(ctx context.Context, args []string, input io.ReadCloser, output io.Writer, errorOutput io.Writer) int {
	fs := flag.NewFlagSet("session start", flag.ContinueOnError)
	fs.SetOutput(errorOutput)
	var vars varFlags
	stdio := fs.Bool("stdio", false, "Use the yawr.session-stdio/v1 JSON-lines protocol")
	sessionID := fs.String("session-id", "", "Client-generated investigation session UUID")
	commandID := fs.String("command-id", "", "Client-generated idempotent creation command UUID")
	sessionDir := fs.String("session-dir", ".runbook/sessions", "Session storage directory")
	runDir := fs.String("run-dir", ".runbook/runs", "Run projection storage directory")
	toolDir := fs.String("tool-dir", ".", "Tool discovery directory")
	packageMapPath := fs.String("package-map", "", "Path to a package-map file overriding project package bindings")
	profilePath := fs.String("profile", "", "Runtime profile file")
	fs.Var(&vars, "var", "Variable override (repeatable)")
	flagArgs, runbookPath, extra := splitSessionStartArgs(args)
	if err := fs.Parse(flagArgs); err != nil {
		return exitValidation
	}
	parsedSessionID, sessionErr := uuid.Parse(*sessionID)
	if runbookPath == "" || len(extra) > 0 || !*stdio || sessionErr != nil {
		fmt.Fprintln(errorOutput, "usage: yawr session start <runbook> --session-id <uuid> --command-id <uuid> --stdio")
		return exitValidation
	}
	if _, err := uuid.Parse(*commandID); err != nil {
		fmt.Fprintln(errorOutput, "session start: command-id must be a UUID")
		return exitValidation
	}
	runVars, err := parseVarFlags(vars)
	if err != nil {
		fmt.Fprintln(errorOutput, err)
		return exitValidation
	}
	store := sessionstore.NewDirStore(*sessionDir)
	defer store.Close()
	profile, err := loadSessionRuntimeProfile(*profilePath)
	if err != nil {
		fmt.Fprintln(errorOutput, err)
		return exitValidation
	}
	stack, err := buildSessionRuntimeStack(ctx, store, *runDir, *toolDir, *packageMapPath, profile)
	if err != nil {
		fmt.Fprintln(errorOutput, err)
		return exitRuntime
	}
	defer stack.shutdown()
	parsed, err := stack.parser.Parse(ctx, runbookPath)
	if err != nil {
		fmt.Fprintln(errorOutput, err)
		return exitValidation
	}
	for _, warning := range parsed.Warnings {
		fmt.Fprintf(errorOutput, "yawr: warning: %s: %s\n", warning.Field, warning.Message)
	}
	for name := range runVars {
		if declaration := parsed.Runbook.Inputs[name]; declaration != nil && declaration.Type == "secret" {
			fmt.Fprintf(errorOutput, "session start: secret input %q must be sent with session.configure\n", name)
			return exitValidation
		}
	}
	if err := applyInputDefaults(parsed, runVars); err != nil {
		fmt.Fprintln(errorOutput, err)
		return exitValidation
	}
	if lexicalErrors := checkIncludeClosureLexicalScoping(ctx, stack.parser, parsed); len(lexicalErrors) > 0 {
		for _, lexicalErr := range lexicalErrors {
			fmt.Fprintln(errorOutput, lexicalErr)
		}
		return exitValidation
	}
	plan, planningWarnings, err := stack.plan(ctx, runbookPath, parsed)
	for _, warning := range planningWarnings {
		fmt.Fprintln(errorOutput, warning)
	}
	if err != nil {
		fmt.Fprintln(errorOutput, err)
		return exitValidation
	}
	ctx = internalexecutor.WithEntryGovernance(ctx, parsed.Runbook.Governance)
	runID := uuid.NewSHA1(parsedSessionID, []byte("run:"+*commandID)).String()
	segmentID := uuid.NewSHA1(parsedSessionID, []byte("segment:"+*commandID)).String()
	plan.RunID = runID
	plan.Metadata.PlannedAt = time.Time{}
	if err := stack.preparePlan(ctx, plan); err != nil {
		fmt.Fprintln(errorOutput, err)
		return exitValidation
	}
	graph, err := sessioncoordinator.BuildExecutionPlanGraph(plan)
	if err != nil {
		fmt.Fprintln(errorOutput, err)
		return exitValidation
	}
	stack.broker.Register(runID)
	handle, err := stack.coordinator.Start(ctx, sessioncoordinator.StartRequest{
		SessionID: *sessionID, CommandID: *commandID, SegmentID: segmentID, RunID: runID,
		Plan: plan, Graph: graph,
		RunOptions: engine.RunOptions{
			Mode: engine.RunModeReal, Client: "cli-session-stdio", Vars: runVars,
		},
	})
	if err != nil {
		stack.broker.Unregister(runID)
		fmt.Fprintln(errorOutput, err)
		return exitRuntime
	}
	attachment, err := sessionstdio.Adopt(ctx, store, *sessionID, 0, output, handle)
	if err != nil {
		stack.broker.Unregister(runID)
		fmt.Fprintln(errorOutput, cleanupFailedSessionStart(
			ctx, err, handle, attachment, "stdio startup write failed",
		))
		return exitRuntime
	}
	defer attachment.Release()
	attachment.WithCommandHandler(stack.runtime)
	if err := stack.runtime.StartAttached(ctx, attachment, handle); err != nil {
		fmt.Fprintln(errorOutput, cleanupFailedSessionStart(
			ctx, err, handle, attachment, "stdio runtime start failed",
		))
		return exitRuntime
	}
	return serveSessionAttachment(ctx, store, attachment, stack.runtime, input, errorOutput)
}

type sessionStartDetacher interface {
	Detach(context.Context, string, string) (session.Manifest, error)
}

type sessionStartReleaser interface {
	Release() error
}

func cleanupFailedSessionStart(
	ctx context.Context,
	primaryErr error,
	detacher sessionStartDetacher,
	releaser sessionStartReleaser,
	reason string,
) error {
	var detachErr error
	if detacher != nil {
		_, detachErr = detacher.Detach(context.WithoutCancel(ctx), uuid.NewString(), reason)
	}
	var releaseErr error
	if releaser != nil {
		releaseErr = releaser.Release()
	}
	return errors.Join(primaryErr, detachErr, releaseErr)
}

func splitSessionStartArgs(args []string) (flagArgs []string, runbookPath string, extra []string) {
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if strings.HasPrefix(argument, "-") {
			flagArgs = append(flagArgs, argument)
			if (argument == "--session-id" || argument == "--command-id" || argument == "--session-dir" ||
				argument == "--run-dir" || argument == "--tool-dir" || argument == "--package-map" ||
				argument == "--profile") && index+1 < len(args) {
				index++
				flagArgs = append(flagArgs, args[index])
			} else if argument == "--var" && index+1 < len(args) {
				index++
				flagArgs = append(flagArgs, args[index])
			}
			continue
		}
		if runbookPath == "" {
			runbookPath = argument
			continue
		}
		extra = append(extra, argument)
	}
	return flagArgs, runbookPath, extra
}

func splitSessionAttachArgs(args []string) (flagArgs []string, sessionID string, extra []string) {
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if strings.HasPrefix(argument, "-") {
			flagArgs = append(flagArgs, argument)
			if (argument == "--after-sequence" || argument == "--session-dir" ||
				argument == "--run-dir" || argument == "--tool-dir" || argument == "--package-map" ||
				argument == "--profile") && index+1 < len(args) {
				index++
				flagArgs = append(flagArgs, args[index])
			}
			continue
		}
		if sessionID == "" {
			sessionID = argument
			continue
		}
		extra = append(extra, argument)
	}
	return flagArgs, sessionID, extra
}

func splitSessionGraphArgs(args []string) (flagArgs []string, sessionID string, extra []string) {
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if strings.HasPrefix(argument, "-") {
			flagArgs = append(flagArgs, argument)
			if (argument == "--segment-id" || argument == "--revision" || argument == "--session-dir") &&
				index+1 < len(args) {
				index++
				flagArgs = append(flagArgs, args[index])
			}
			continue
		}
		if sessionID == "" {
			sessionID = argument
			continue
		}
		extra = append(extra, argument)
	}
	return flagArgs, sessionID, extra
}

type sessionRuntimeStack struct {
	preparePlan      func(context.Context, *engine.ExecutionPlan) error
	parser           parserpkg.Parser
	planner          plannerpkg.Planner
	plannerTools     *plannerToolRegistry
	runtimeTools     *internaltool.OverlayRegistry
	resolverProxy    *adapter.ResolverProxy
	workspaceRoot    string
	packageRequires  []*schema.PackageRequirement
	packageToolPaths []string
	profile          *schema.RuntimeProfile
	broker           *internalserve.PromptBroker
	coordinator      *sessioncoordinator.Coordinator
	runtime          *sessionstdio.CoordinatorRuntime
	shutdown         func()
}

func loadSessionRuntimeProfile(path string) (*schema.RuntimeProfile, error) {
	if path == "" {
		return nil, nil
	}
	return schema.ParseProfileFile(path)
}

func buildSessionRuntimeStack(
	ctx context.Context,
	sessions *sessionstore.DirStore,
	runDir string,
	toolDir string,
	packageMapPath string,
	profile *schema.RuntimeProfile,
) (*sessionRuntimeStack, error) {
	workspaceRoot, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	projectConfig, err := loadProjectConfig(workspaceRoot)
	if err != nil {
		return nil, err
	}
	var projectRequires []*schema.PackageRequirement
	var projectToolPaths []string
	if projectConfig != nil {
		projectRequires = projectConfig.Requires
		projectToolPaths = projectConfig.ToolPaths
	}
	var overrideRequires []*schema.PackageRequirement
	var overrideToolPaths []string
	if packageMapPath != "" {
		packageMap, loadErr := loadPackageMap(packageMapPath)
		if loadErr != nil {
			return nil, loadErr
		}
		if packageMap != nil {
			overrideRequires = packageMap.Requires
			overrideToolPaths = packageMap.ToolPaths
		}
	}
	packageRequires, _ := mergePackageBindings(projectRequires, overrideRequires)
	packageToolPaths := mergeToolPaths(projectToolPaths, overrideToolPaths)
	parserImpl, err := internalparser.New(platform.Real())
	if err != nil {
		return nil, err
	}
	toolRegistry, err := newToolRegistry(toolDir)
	if err != nil {
		return nil, err
	}
	loader := &fileRunbookLoader{parser: parserImpl}
	resolverProxy := adapter.NewResolverProxy()
	plannerImpl := internalplanner.New(plannerpkg.Config{
		Loader: loader, Tools: toolRegistry, BaseDir: toolDir,
		ExpandPolicy: expand.Policy{Default: expand.ModeEager},
		Profile:      profile,
	})
	broker := internalserve.NewPromptBroker(64)
	tracePath := filepath.Join(runDir, "session-stdio.trace.jsonl")
	config, shutdown, err := adapter.BuildEngineConfig(ctx, adapter.WireOptions{
		Mode: string(engine.RunModeReal), RunDir: runDir, TraceFile: tracePath,
		ToolScanDir: toolDir, TTYOutput: false, Output: io.Discard,
		PromptProviderOverride: broker, ApprovalGateOverride: broker, HostActionProvider: broker,
		LazyRunbookLoader: adapter.NewParserLazyLoader(parserImpl), SubstitutionParser: parserImpl,
		DynamicIncludeResolver: resolverProxy, ExtraToolScanPaths: packageToolPaths, Profile: profile,
	})
	if err != nil {
		return nil, err
	}
	durableRuns, ok := config.Store.(engine.DurableRunStore)
	if !ok {
		shutdown()
		return nil, errors.New("session runtime requires durable run storage")
	}
	defaultRuntime, ok := config.ToolRuntime.(*internaltool.DefaultToolRuntime)
	if !ok {
		shutdown()
		return nil, errors.New("session runtime requires the default tool runtime")
	}
	runtimeTools, ok := defaultRuntime.Registry().(*internaltool.OverlayRegistry)
	if !ok {
		shutdown()
		return nil, errors.New("session runtime requires an overlay tool registry")
	}
	resolver, err := sessioncoordinator.NewStaticHandoffResolver(sessioncoordinator.StaticHandoffResolverConfig{
		Parser: parserImpl, Planner: plannerImpl, Loader: loader,
	})
	if err != nil {
		shutdown()
		return nil, err
	}
	runtimeEngine := internalengine.New(config)
	coordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
		Sessions: sessions, Runs: durableRuns, Engine: runtimeEngine, HandoffResolver: resolver,
	})
	if err != nil {
		shutdown()
		return nil, err
	}
	runtime, err := sessionstdio.NewCoordinatorRuntime(
		coordinator, broker, engine.RunOptions{Mode: engine.RunModeReal, Client: "cli-session-stdio"},
	)
	if err != nil {
		shutdown()
		return nil, err
	}
	runtime.RequireStartupConfiguration()
	return &sessionRuntimeStack{
		preparePlan: runtimeEngine.(interface {
			PrepareDurablePlan(context.Context, *engine.ExecutionPlan) error
		}).PrepareDurablePlan,
		parser: parserImpl, planner: plannerImpl, plannerTools: toolRegistry,
		runtimeTools: runtimeTools, resolverProxy: resolverProxy, workspaceRoot: workspaceRoot,
		packageRequires: packageRequires, packageToolPaths: packageToolPaths, profile: profile,
		broker: broker, coordinator: coordinator, runtime: runtime, shutdown: shutdown,
	}, nil
}

func (stack *sessionRuntimeStack) plan(
	ctx context.Context,
	runbookPath string,
	parsed *parserpkg.ParsedRunbook,
) (*engine.ExecutionPlan, []error, error) {
	if parsed == nil || parsed.Runbook == nil {
		return nil, nil, errors.New("session runtime requires a parsed runbook")
	}
	catalog, catalogErrors := adapter.BuildPackageCatalog(adapter.PackageCatalogOptions{
		WorkspaceRoot:    stack.workspaceRoot,
		Builtins:         internaltool.NewBuiltinRegistry().All(),
		ProjectRequires:  stack.packageRequires,
		ProjectToolPaths: stack.packageToolPaths,
	}, runbookPath, parsed.Runbook.Requires)
	fatalCatalogErrors, warnings := errkit.SplitWarnings(catalogErrors)
	if len(fatalCatalogErrors) > 0 {
		return nil, warnings, errors.Join(fatalCatalogErrors...)
	}
	stack.resolverProxy.Set(adapter.NewCatalogIncludeResolver(catalog, stack.parser))
	definitions, bindingErrors := adapter.ResolveToolRefsViaCatalog(catalog, runbookPath, parsed.Runbook.ToolRefs)
	fatalBindingErrors, bindingWarnings := errkit.SplitWarnings(bindingErrors)
	warnings = append(warnings, bindingWarnings...)
	if len(fatalBindingErrors) > 0 {
		return nil, warnings, errors.Join(fatalBindingErrors...)
	}
	for _, definition := range definitions {
		stack.runtimeTools.Override(definition)
		schemaDefinition := schemaToolDefFromRuntime(definition)
		for action := range schemaDefinition.Actions {
			stack.plannerTools.tools[schemaDefinition.Name+"/"+action] = schemaDefinition
		}
	}
	toolDefinitions := make(map[string]toolpkg.ToolDef)
	for _, definition := range stack.runtimeTools.All() {
		toolDefinitions[definition.Name] = definition
	}
	substitutionErrors := validateSubstitutionsPlanTime(ctx, stack.parser, parsed, toolDefinitions)
	plan, planErr := stack.planner.Plan(ctx, parsed)
	if planErr != nil || len(substitutionErrors) > 0 {
		if planErr != nil {
			substitutionErrors = append(substitutionErrors, planErr)
		}
		return nil, warnings, errors.Join(substitutionErrors...)
	}
	plan.Metadata.CatalogDigest = catalog.CatalogDigest()
	plan.Metadata.PackageDigests = make(map[string]string, len(catalog.Packages))
	for _, resolvedPackage := range catalog.Packages {
		plan.Metadata.PackageDigests[resolvedPackage.Name] = resolvedPackage.Digest
	}
	if stack.profile != nil {
		plan.Metadata.Profile = stack.profile
	}
	return plan, warnings, nil
}
