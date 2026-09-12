package perf

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/ormasoftchile/yawr/runtime/internal/adapter"
	internalengine "github.com/ormasoftchile/yawr/runtime/internal/engine"
	internalparser "github.com/ormasoftchile/yawr/runtime/internal/parser"
	internalplanner "github.com/ormasoftchile/yawr/runtime/internal/planner"
	internaltool "github.com/ormasoftchile/yawr/runtime/internal/tool"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/expand"
	pkgparser "github.com/ormasoftchile/yawr/runtime/pkg/parser"
	plannerpkg "github.com/ormasoftchile/yawr/runtime/pkg/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// FixtureSuite is the placeholder P8 fixture suite used by benchmarks and soak.
var FixtureSuite = []string{
	filepath.Join("examples", "collect-health", "collect-health.runbook.yaml"),
}

// RepoRoot returns the repository root for benchmark and soak callers.
func RepoRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("perf: resolve caller path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..")), nil
}

// FixturePaths returns absolute fixture paths for the placeholder suite.
func FixturePaths(repoRoot string) []string {
	out := make([]string, len(FixtureSuite))
	for i, rel := range FixtureSuite {
		out[i] = filepath.Join(repoRoot, rel)
	}
	return out
}

// RunFixtureDryRun parses, validates, starts, and fully drains one dry-run fixture.
func RunFixtureDryRun(ctx context.Context, repoRoot, runbookPath, workDir string) (int, error) {
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return 0, err
	}
	plat := platform.NewFakePlatform()
	plat.TempDirPath = filepath.Join(repoRoot, ".perf-temp")
	parserImpl, err := internalparser.New(plat)
	if err != nil {
		return 0, err
	}
	parsed, err := parserImpl.Parse(ctx, runbookPath)
	if err != nil {
		return 0, err
	}
	runVars := map[string]string{}
	applyInputDefaults(parsed, runVars)

	registry, err := newToolRegistry(repoRoot)
	if err != nil {
		return 0, err
	}
	for _, ref := range parsed.Runbook.ToolRefs {
		if ref.Path == "" {
			continue
		}
		absPath := filepath.Join(filepath.Dir(runbookPath), ref.Path)
		schemaDef, err := internaltool.ParseToolFile(absPath)
		if err != nil {
			return 0, err
		}
		for action := range schemaDef.Actions {
			registry.tools[schemaDef.Name+"/"+action] = schemaDef
		}
	}

	plannerImpl := internalplanner.New(plannerpkg.Config{
		Loader:       &fileLoader{parser: parserImpl},
		Tools:        registry,
		BaseDir:      filepath.Dir(runbookPath),
		ExpandPolicy: expand.Policy{Default: expand.ModeEager},
	})
	plan, err := plannerImpl.Plan(ctx, parsed)
	if err != nil {
		return 0, err
	}

	ecfg, shutdown, err := adapter.BuildEngineConfig(ctx, adapter.WireOptions{
		Mode:              string(engine.RunModeDryRun),
		TraceFile:         filepath.Join(workDir, "trace.jsonl"),
		RunDir:            filepath.Join(workDir, "runs"),
		ToolScanDir:       repoRoot,
		TTYOutput:         false,
		LazyRunbookLoader: adapter.NewParserLazyLoader(parserImpl),
	})
	if err != nil {
		return 0, err
	}
	defer shutdown()

	eng := internalengine.New(ecfg)
	handle, err := eng.Start(ctx, plan, engine.RunOptions{Mode: engine.RunModeDryRun, Vars: runVars, Store: ecfg.Store, Client: "perf"})
	if err != nil {
		return 0, err
	}
	steps := 0
	for {
		result, err := handle.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return steps, err
		}
		if result != nil {
			steps++
		}
	}
	return steps, nil
}

// PlanFixture parses and validates one fixture without dispatching executors.
func PlanFixture(ctx context.Context, repoRoot, runbookPath string) (int, error) {
	plat := platform.NewFakePlatform()
	plat.TempDirPath = filepath.Join(repoRoot, ".perf-temp")
	parserImpl, err := internalparser.New(plat)
	if err != nil {
		return 0, err
	}
	parsed, err := parserImpl.Parse(ctx, runbookPath)
	if err != nil {
		return 0, err
	}
	registry, err := newToolRegistry(repoRoot)
	if err != nil {
		return 0, err
	}
	plannerImpl := internalplanner.New(plannerpkg.Config{
		Loader:       &fileLoader{parser: parserImpl},
		Tools:        registry,
		BaseDir:      filepath.Dir(runbookPath),
		ExpandPolicy: expand.Policy{Default: expand.ModeEager},
	})
	plan, err := plannerImpl.Plan(ctx, parsed)
	if err != nil {
		return 0, err
	}
	return len(plan.Steps), nil
}

type fileLoader struct{ parser pkgparser.Parser }

func (l *fileLoader) Load(ctx context.Context, path string) (*pkgparser.ParsedRunbook, error) {
	return l.parser.Parse(ctx, path)
}

type toolRegistry struct{ tools map[string]*schema.ToolDef }

func newToolRegistry(scanDir string) (*toolRegistry, error) {
	toolDefs, err := internaltool.ScanSchemaDir(scanDir)
	if err != nil {
		return nil, err
	}
	tools := map[string]*schema.ToolDef{}
	for _, def := range toolDefs {
		for action := range def.Actions {
			tools[def.Name+"/"+action] = def
		}
	}
	return &toolRegistry{tools: tools}, nil
}

func (r *toolRegistry) Lookup(_ context.Context, name, action string) (*schema.ToolDef, error) {
	if tool, ok := r.tools[name+"/"+action]; ok {
		return tool, nil
	}
	prefix := name + "/"
	for key := range r.tools {
		if strings.HasPrefix(key, prefix) {
			return nil, plannerpkg.ErrActionNotFound
		}
	}
	return nil, plannerpkg.ErrToolNotFound
}

func applyInputDefaults(parsed *pkgparser.ParsedRunbook, vars map[string]string) {
	if parsed == nil || parsed.Runbook == nil {
		return
	}
	for k, v := range parsed.Runbook.Vars {
		if _, ok := vars[k]; ok {
			continue
		}
		vars[k] = fmt.Sprint(v)
	}
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
}

// SoakStats summarizes one placeholder soak run.
type SoakStats struct {
	Iterations int64
	Steps      int64
	Duration   time.Duration
	StartAlloc uint64
	EndAlloc   uint64
	PeakAlloc  uint64
	PanicFree  bool
	LastError  error
}

// VectorsPerSecond returns dry-run fixture iterations per second.
func (s SoakStats) VectorsPerSecond() float64 {
	if s.Duration <= 0 {
		return 0
	}
	return float64(s.Iterations) / s.Duration.Seconds()
}
