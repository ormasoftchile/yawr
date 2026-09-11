package e2e

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ormasoftchile/yawr/runtime/internal/adapter"
	internalengine "github.com/ormasoftchile/yawr/runtime/internal/engine"
	internalparser "github.com/ormasoftchile/yawr/runtime/internal/parser"
	internalplanner "github.com/ormasoftchile/yawr/runtime/internal/planner"
	internalrunstore "github.com/ormasoftchile/yawr/runtime/internal/runstore"
	internaltool "github.com/ormasoftchile/yawr/runtime/internal/tool"
	internaltrace "github.com/ormasoftchile/yawr/runtime/internal/trace"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	parserpkg "github.com/ormasoftchile/yawr/runtime/pkg/parser"
	plannerpkg "github.com/ormasoftchile/yawr/runtime/pkg/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
	tracepkg "github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

// mockToolRuntime is a test double for tool.ToolRuntime.
// It echoes the invocation parameters as structured output.
type mockToolRuntime struct{}

func (m *mockToolRuntime) Invoke(_ context.Context, name, action string, args map[string]any) (*toolpkg.ToolResult, error) {
	return &toolpkg.ToolResult{
		Stdout:   fmt.Sprintf("tool=%s action=%s args=%v", name, action, args),
		ExitCode: 0,
	}, nil
}

// E2EHarness configures and runs full engine executions.
// It wires the real stack: parser → planner → engine → executors → trace → runstore.
type E2EHarness struct {
	t           *testing.T
	WorkDir     string
	RunbookPath string
	Inputs      map[string]any
	Timeout     time.Duration

	// RunDir and TraceDir are temp dirs created by the harness.
	RunDir   string
	TraceDir string

	// traceFile is the JSONL trace path used by BuildEngineConfig.
	traceFile string

	// Store is the run store; exposed for resume tests.
	Store *internalrunstore.DirRunStore

	// extraToolDefs holds additional tool definitions to register in the planner
	// tool registry. Required for runbooks that contain tool_call steps.
	extraToolDefs map[string]*schema.ToolDef
}

// NewHarness constructs an E2EHarness for the runbook at the relative path
// under testdata/. The runbook path is resolved relative to this package's
// source directory via the test binary's working directory.
func NewHarness(t *testing.T, runbookRelPath string) *E2EHarness {
	t.Helper()
	if runtime.GOOS == "windows" && runbookRelPath != "tool-runbook.yaml" && runbookRelPath != "production-composites.runbook.yaml" {
		t.Skip("requires POSIX shell utilities (echo, false, /bin/sh)")
	}
	workDir := t.TempDir()
	runDir := filepath.Join(workDir, "runs")
	traceDir := filepath.Join(workDir, "traces")

	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("e2e: mkdir runDir: %v", err)
	}
	if err := os.MkdirAll(traceDir, 0o755); err != nil {
		t.Fatalf("e2e: mkdir traceDir: %v", err)
	}

	return &E2EHarness{
		t:             t,
		WorkDir:       workDir,
		RunbookPath:   filepath.Join("testdata", runbookRelPath),
		Inputs:        make(map[string]any),
		Timeout:       60 * time.Second,
		RunDir:        runDir,
		TraceDir:      traceDir,
		traceFile:     filepath.Join(traceDir, "trace.jsonl"),
		extraToolDefs: make(map[string]*schema.ToolDef),
	}
}

// WithInput sets an input variable for the run.
func (h *E2EHarness) WithInput(key string, val any) *E2EHarness {
	h.Inputs[key] = val
	return h
}

// WithToolDef registers a dummy tool definition for use in tool_call steps.
// The planner requires a tool to be registered before planning can succeed.
func (h *E2EHarness) WithToolDef(name string, actions ...string) *E2EHarness {
	if len(actions) == 0 {
		actions = []string{"run"}
	}
	def := &schema.ToolDef{
		Name:    name,
		Actions: make(map[string]*schema.ToolAction, len(actions)),
	}
	for _, a := range actions {
		def.Actions[a] = &schema.ToolAction{}
	}
	h.extraToolDefs[name] = def
	return h
}

// inputsAsVars converts the Inputs map to the map[string]string form required
// by engine.RunOptions.Vars. Values are formatted with fmt.Sprint.
func (h *E2EHarness) inputsAsVars() map[string]string {
	if len(h.Inputs) == 0 {
		return nil
	}
	out := make(map[string]string, len(h.Inputs))
	for k, v := range h.Inputs {
		out[k] = fmt.Sprint(v)
	}
	return out
}

// Prepare parses the runbook, creates the planner, builds engine config, and
// returns the execution plan, engine config, and a ready-to-drive engine.
// It registers the test cleanup for the OTel shutdown function.
// Exported for use by individual tests that need direct engine control (resume, cancel).
func (h *E2EHarness) Prepare(ctx context.Context) (*engine.ExecutionPlan, engine.EngineConfig, engine.Engine) {
	h.t.Helper()

	plat := platform.Real()
	p, err := internalparser.New(plat)
	if err != nil {
		h.t.Fatalf("e2e: parser.New: %v", err)
	}

	parsed, err := p.Parse(ctx, h.RunbookPath)
	if err != nil {
		h.t.Fatalf("e2e: parse %s: %v", h.RunbookPath, err)
	}

	// Build a minimal tool registry (no external tools; only built-ins).
	toolRegistry, err := buildE2EToolRegistry(h.WorkDir, h.extraToolDefs)
	if err != nil {
		h.t.Fatalf("e2e: build tool registry: %v", err)
	}

	plannerImpl := internalplanner.New(plannerpkg.Config{
		Loader: &e2eRunbookLoader{parser: p},
		Tools:  toolRegistry,
	})

	plan, err := plannerImpl.Plan(ctx, parsed)
	if err != nil {
		h.t.Fatalf("e2e: plan %s: %v", h.RunbookPath, err)
	}

	ecfg, shutdown, err := adapter.BuildEngineConfig(ctx, adapter.WireOptions{
		Mode:        "real",
		TraceFile:   h.traceFile,
		RunDir:      h.RunDir,
		TTYOutput:   false, // non-interactive: NoOpApprovalGate auto-approves
		ToolScanDir: h.WorkDir,
	})
	if err != nil {
		h.t.Fatalf("e2e: BuildEngineConfig: %v", err)
	}
	h.t.Cleanup(shutdown)

	// Inject mock tool runtime so tool_call steps can execute without real transports.
	ecfg.ToolRuntime = &mockToolRuntime{}

	// Cache the store for resume tests.
	if store, ok := ecfg.Store.(*internalrunstore.DirRunStore); ok {
		h.Store = store
	}

	eng := internalengine.New(ecfg)
	return plan, ecfg, eng
}

// Run executes the full runbook from start to completion and returns the
// final RunState. It fails the test if the engine cannot start or errors out.
func (h *E2EHarness) Run() (engine.RunState, error) {
	h.t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), h.Timeout)
	defer cancel()

	plan, ecfg, eng := h.Prepare(ctx)

	handle, err := eng.Start(ctx, engine.ValidatedForTest(plan), engine.RunOptions{
		Mode:  engine.RunModeReal,
		Vars:  h.inputsAsVars(),
		Store: ecfg.Store,
	})
	if err != nil {
		return engine.RunState{}, fmt.Errorf("e2e: Start: %w", err)
	}

	for {
		_, err := handle.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			return handle.State(), fmt.Errorf("e2e: Next: %w", err)
		}
	}

	return handle.State(), nil
}

// AssertCompleted fails the test if state is not RunStatusCompleted.
func (h *E2EHarness) AssertCompleted(state engine.RunState) {
	h.t.Helper()
	if state.Status != engine.RunStatusCompleted {
		h.t.Errorf("expected status=completed, got %s", state.Status)
	}
}

// AssertTrace reads the trace JSONL file and fails if any of the given event
// kinds are absent. The kinds are matched as exact strings against TraceEvent.Kind.
func (h *E2EHarness) AssertTrace(kinds ...string) {
	h.t.Helper()

	events, err := h.readTraceEvents()
	if err != nil {
		h.t.Fatalf("e2e: read trace %s: %v", h.traceFile, err)
	}

	present := make(map[string]bool, len(events))
	for _, ev := range events {
		present[string(ev.Kind)] = true
	}

	for _, want := range kinds {
		if !present[want] {
			h.t.Errorf("trace missing event kind %q; found: %v", want, traceKindList(events))
		}
	}
}

// TraceEvents returns all events from the trace file.
func (h *E2EHarness) TraceEvents() ([]tracepkg.TraceEvent, error) {
	return h.readTraceEvents()
}

// readTraceEvents reads all trace events from the JSONL trace file.
// It uses the internal JSONLReader which handles the wire-format field name
// differences (e.g. "ts" → Timestamp, "seq" → Sequence).
func (h *E2EHarness) readTraceEvents() ([]tracepkg.TraceEvent, error) {
	reader := internaltrace.NewJSONLReader(h.traceFile)
	return reader.ReadAll(context.Background())
}

func traceKindList(events []tracepkg.TraceEvent) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, string(ev.Kind))
	}
	return out
}

// --- helpers ---

// e2eRunbookLoader wraps the real parser for use as a planner.RunbookLoader.
type e2eRunbookLoader struct {
	parser parserpkg.Parser
}

func (l *e2eRunbookLoader) Load(ctx context.Context, path string) (*parserpkg.ParsedRunbook, error) {
	return l.parser.Parse(ctx, path)
}

// e2ePlannerToolRegistry adapts schema tool defs to the planner.ToolRegistry
// interface required by internalplanner.New.
type e2ePlannerToolRegistry struct {
	tools map[string]*schema.ToolDef
}

var _ plannerpkg.ToolRegistry = (*e2ePlannerToolRegistry)(nil)

func (r *e2ePlannerToolRegistry) Lookup(_ context.Context, name, action string) (*schema.ToolDef, error) {
	key := name + "/" + action
	if def, ok := r.tools[key]; ok {
		return def, nil
	}
	for k := range r.tools {
		if strings.HasPrefix(k, name+"/") {
			return nil, plannerpkg.ErrActionNotFound
		}
	}
	return nil, plannerpkg.ErrToolNotFound
}

// buildE2EToolRegistry creates a planner-compatible tool registry by scanning
// scanDir for .tool.yaml files. Since e2e runbooks use only built-in executors
// (cli, branch, iterate, approve), an empty directory is sufficient.
// extraDefs allows tests to register additional tool definitions for tool_call steps.
func buildE2EToolRegistry(scanDir string, extraDefs map[string]*schema.ToolDef) (*e2ePlannerToolRegistry, error) {
	defs, err := internaltool.ScanSchemaDir(scanDir)
	if err != nil {
		defs = nil // tolerate scan errors on temp dirs
	}
	reg := &e2ePlannerToolRegistry{tools: make(map[string]*schema.ToolDef)}
	for _, def := range defs {
		if def == nil || len(def.Actions) == 0 {
			continue
		}
		for action := range def.Actions {
			reg.tools[def.Name+"/"+action] = def
		}
	}
	for _, def := range extraDefs {
		if def == nil || len(def.Actions) == 0 {
			continue
		}
		for action := range def.Actions {
			reg.tools[def.Name+"/"+action] = def
		}
	}
	return reg, nil
}
