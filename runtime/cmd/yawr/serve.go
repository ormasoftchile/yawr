package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/ormasoftchile/yawr/runtime/internal/adapter"
	internalengine "github.com/ormasoftchile/yawr/runtime/internal/engine"
	internalparser "github.com/ormasoftchile/yawr/runtime/internal/parser"
	internalplanner "github.com/ormasoftchile/yawr/runtime/internal/planner"
	internalserve "github.com/ormasoftchile/yawr/runtime/internal/serve"
	internaltool "github.com/ormasoftchile/yawr/runtime/internal/tool"
	"github.com/ormasoftchile/yawr/runtime/pkg/expand"
	plannerpkg "github.com/ormasoftchile/yawr/runtime/pkg/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	servepkg "github.com/ormasoftchile/yawr/runtime/pkg/serve"
)

// repeatedStringFlag implements flag.Value for a repeatable string flag.
type repeatedStringFlag []string

func (f *repeatedStringFlag) String() string { return strings.Join(*f, ",") }
func (f *repeatedStringFlag) Set(v string) error {
	*f = append(*f, v)
	return nil
}

func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	addr := fs.String("addr", "127.0.0.1:7778", "TCP address to listen on")
	var corsOrigins repeatedStringFlag
	fs.Var(&corsOrigins, "cors-origin", "Allowed CORS origin; may be repeated (empty = allow all)")
	authToken := fs.String("auth-token", "", "Bearer token for auth (empty = no auth; development use only)")
	authTokenExpiry := fs.Duration("auth-token-expiry", 0, "Max JWT token age from iat (e.g. 24h); 0 = no expiry check")
	authJWTSecret := fs.String("auth-jwt-secret", "", "Base64-encoded HMAC-SHA256 secret for JWT signature verification (mutually exclusive with --auth-token)")
	runDir := fs.String("run-dir", ".runbook/runs", "Base directory for run state")
	rateLimit := fs.Int("rate-limit", 0, "requests per second per IP (0 = disabled)")
	trustProxyHeaders := fs.Bool("trust-proxy-headers", false,
		"Trust X-Forwarded-For for rate-limit IP extraction (only safe behind a reverse proxy)")
	expandMode := fs.String("expand", "", "Default expansion mode for include sites: eager, lazy, or auto (overridden by per-runbook and per-include `expand:`)")
	packageMapPath := fs.String("package-map", "", "Path to a package-map file (yawr.config/v1 shape: tool-paths:) overriding the project's .yawr/config.yaml tool-path bindings. Only tool-paths: is honoured by serve; requires: is not supported (serve builds one engine at startup and cannot resolve per-runbook package requirements — use yawr run or yawr plan for requires:-based resolution)")

	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return exitValidation
	}

	expandDefault, err := expand.Parse(*expandMode)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitValidation
	}

	if *authToken != "" && *authJWTSecret != "" {
		fmt.Fprintln(os.Stderr, "serve: --auth-token and --auth-jwt-secret are mutually exclusive")
		return exitValidation
	}

	// Safety check: refuse to bind a non-loopback address without auth.
	// A bare ":port" or "0.0.0.0:port" binds all interfaces and is
	// reachable from the local network. Without auth that exposes every
	// runbook in the working directory to any host on the network.
	// Operators who genuinely need a public bind must configure auth
	// (--auth-token or --auth-jwt-secret). The default addr (127.0.0.1)
	// is always loopback, so local development and the VS Code extension
	// are unaffected.
	hasAuth := *authToken != "" || *authJWTSecret != ""
	if !hasAuth {
		if nonLoopback, checkErr := isNonLoopbackAddr(*addr); checkErr != nil {
			fmt.Fprintf(os.Stderr, "serve: invalid --addr %q: %v\n", *addr, checkErr)
			return exitValidation
		} else if nonLoopback {
			fmt.Fprintf(os.Stderr, "serve: refusing to bind non-loopback address %q without authentication\n", *addr)
			fmt.Fprintln(os.Stderr, "fix: add --auth-token or --auth-jwt-secret, or bind to 127.0.0.1 (loopback only)")
			return exitValidation
		}
	}

	var jwtSecretBytes []byte
	if *authJWTSecret != "" {
		var err error
		jwtSecretBytes, err = base64.StdEncoding.DecodeString(*authJWTSecret)
		if err != nil {
			fmt.Fprintf(os.Stderr, "serve: --auth-jwt-secret: invalid base64: %v\n", err)
			return exitValidation
		}
		if len(jwtSecretBytes) < 32 {
			fmt.Fprintln(os.Stderr, "serve: --auth-jwt-secret: decoded secret must be at least 32 bytes")
			return exitValidation
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

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

	// Load package-map at startup (fail-stop). Both requires: and tool-paths:
	// are retained: tool paths still influence the singleton tool registry,
	// while requires participate in per-run catalog construction once the
	// posted runbook is known.
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
	mergedRequires, _ := mergePackageBindings(projectRequires, overrideRequires)
	pmExtraToolPaths := mergeToolPaths(projectToolPaths, overrideToolPaths)

	plat := platform.Real()
	parserImpl, err := internalparser.New(plat)
	if err != nil {
		fmt.Fprintf(os.Stderr, "serve: parser: %v\n", err)
		return exitFailure
	}

	toolRegistry, err := newServingToolRegistry(".", pmExtraToolPaths...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "serve: tool registry: %v\n", err)
		return exitFailure
	}

	plannerImpl := internalplanner.New(plannerpkg.Config{
		Loader:       &fileRunbookLoader{parser: parserImpl},
		Tools:        toolRegistry,
		ExpandPolicy: expand.Policy{Default: expandDefault},
	})

	// Build the HTTP PromptBroker first so it can be wired in as the
	// PromptProvider before executors are constructed. This is what makes
	// choice/decision/collector steps reach the web client over SSE.
	broker := internalserve.NewPromptBroker(0)

	wireOpts := adapter.WireOptions{
		RunDir:                 *runDir,
		TTYOutput:              false,
		ToolScanDir:            ".",
		ExcludeTestTools:       true,
		ExtraToolScanPaths:     pmExtraToolPaths,
		PromptProviderOverride: broker,
		HostActionProvider:     broker,
		LazyRunbookLoader:      adapter.NewParserLazyLoader(parserImpl),
		SubstitutionParser:     parserImpl,
	}
	engineCfg, shutdown, err := adapter.BuildEngineConfig(ctx, wireOpts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "serve: build engine config: %v\n", err)
		return exitFailure
	}
	defer shutdown()

	cfg := servepkg.ServerConfig{
		Addr:              *addr,
		Engine:            internalengine.New(engineCfg),
		EngineConfig:      engineCfg,
		Parser:            parserImpl,
		Planner:           plannerImpl,
		WorkspaceRoot:     workspaceRoot,
		PackageMapPath:    *packageMapPath,
		ProjectRequires:   mergedRequires,
		ProjectToolPaths:  pmExtraToolPaths,
		AllowedOrigins:    []string(corsOrigins),
		BearerToken:       *authToken,
		BearerTokenExpiry: *authTokenExpiry,
		JWTSecret:         jwtSecretBytes,
		RateLimit:         *rateLimit,
		TrustProxyHeaders: *trustProxyHeaders,
	}

	srv, err := internalserve.NewServerWithBroker(cfg, broker)
	if err != nil {
		fmt.Fprintf(os.Stderr, "serve: create server: %v\n", err)
		return exitFailure
	}

	fmt.Fprintf(os.Stderr, "yawr serve: listening on %s\n", *addr)
	if err := srv.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		return exitFailure
	}
	return exitSuccess
}

// newServingToolRegistry builds the planner's schema-level tool registry by
// scanning dir for .tool.yaml files (excluding test fixture trees). When
// extraPaths are supplied (from --package-map tool-paths:), each is scanned
// after the base dir and its tools overwrite any same-named tool from the
// base scan, so the explicit package-map binding wins independent of
// filesystem walk order.
func newServingToolRegistry(dir string, extraPaths ...string) (*plannerToolRegistry, error) {
	defs, err := internaltool.ScanSchemaDirWithoutTests(dir)
	if err != nil {
		return nil, err
	}
	reg := &plannerToolRegistry{tools: make(map[string]*schema.ToolDef)}
	for _, def := range defs {
		if def == nil || len(def.Actions) == 0 {
			continue
		}
		for action := range def.Actions {
			reg.tools[def.Name+"/"+action] = def
		}
	}
	// Apply extra paths last so package-map bindings override base scan.
	for _, extraPath := range extraPaths {
		extraDefs, extraErr := internaltool.ScanSchemaDirWithoutTests(extraPath)
		if extraErr != nil {
			continue // non-existent or unreadable paths are silently skipped
		}
		for _, def := range extraDefs {
			if def == nil || len(def.Actions) == 0 {
				continue
			}
			for action := range def.Actions {
				reg.tools[def.Name+"/"+action] = def
			}
		}
	}
	return reg, nil
}

// isNonLoopbackAddr returns true when addr binds a non-loopback interface.
// A host of "" or "0.0.0.0" or "::" binds all interfaces (non-loopback).
// Returns an error only when addr cannot be parsed by net.SplitHostPort.
func isNonLoopbackAddr(addr string) (bool, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false, err
	}
	// Empty host (bare ":port") binds all interfaces.
	if host == "" {
		return true, nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// Unresolvable hostname — treat as potentially non-loopback
		// (fail closed: operator must add auth or use an IP address).
		return true, nil
	}
	return !ip.IsLoopback(), nil
}
