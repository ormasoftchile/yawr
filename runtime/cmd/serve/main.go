package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ormasoftchile/yawr/runtime/internal/adapter"
	internalengine "github.com/ormasoftchile/yawr/runtime/internal/engine"
	internalparser "github.com/ormasoftchile/yawr/runtime/internal/parser"
	internalplanner "github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/internal/serve"
	internaltool "github.com/ormasoftchile/yawr/runtime/internal/tool"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	plannerpkg "github.com/ormasoftchile/yawr/runtime/pkg/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	servepkg "github.com/ormasoftchile/yawr/runtime/pkg/serve"
)

const (
	defaultAddr         = ":7778"
	defaultReadTimeout  = 30 * time.Second
	defaultWriteTimeout = 60 * time.Second
)

func main() {
	addr := flag.String("addr", defaultAddr, "listen address")
	flag.Parse()

	plat := platform.Real()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, broker, err := buildServerConfig(ctx, plat, *addr)
	if err != nil {
		log.Fatalf("yawr serve: %v", err)
	}

	srv, err := serve.NewServerWithBroker(cfg, broker)
	if err != nil {
		log.Fatalf("yawr serve: %v", err)
	}

	fmt.Fprintf(os.Stderr, "yawr serve listening on %s\n", cfg.Addr)
	if err := srv.Start(ctx); err != nil {
		log.Fatalf("yawr serve: %v", err)
	}
}

func buildServerConfig(ctx context.Context, plat platform.Platform, addr string) (servepkg.ServerConfig, *serve.PromptBroker, error) {
	if addr == "" {
		addr = defaultAddr
	}
	parserImpl, err := internalparser.New(plat)
	if err != nil {
		return servepkg.ServerConfig{}, nil, err
	}
	registry, err := newToolRegistry(".")
	if err != nil {
		return servepkg.ServerConfig{}, nil, err
	}
	plannerImpl := internalplanner.New(plannerpkg.Config{
		Loader: &fileRunbookLoader{parser: parserImpl},
		Tools:  registry,
	})

	// Build the HTTP PromptBroker first so it can be wired into the
	// executor registry as the PromptProvider before the engine is
	// constructed. Otherwise choice/decision/collector executors will be
	// bound to the default terminal-input provider.
	broker := serve.NewPromptBroker(0)

	ecfg, _, err := adapter.BuildEngineConfig(ctx, adapter.WireOptions{
		Mode:                   "real",
		TTYOutput:              false,
		ToolScanDir:            ".",
		ExcludeTestTools:       true,
		PromptProviderOverride: broker,
		HostActionProvider:     broker,
		SubstitutionParser:     parserImpl,
	})
	if err != nil {
		return servepkg.ServerConfig{}, nil, err
	}

	return servepkg.ServerConfig{
		Addr:          addr,
		ReadTimeout:   defaultReadTimeout,
		WriteTimeout:  defaultWriteTimeout,
		EngineConfig:  ecfg,
		EngineFactory: func(cfg engine.EngineConfig) engine.Engine { return internalengine.New(cfg) },
		Parser:        parserImpl,
		Planner:       plannerImpl,
	}, broker, nil
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
	defs, err := internaltool.ScanSchemaDirWithoutTests(dir)
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
