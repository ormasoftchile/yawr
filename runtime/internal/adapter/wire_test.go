package adapter

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	internalengine "github.com/ormasoftchile/yawr/runtime/internal/engine"
	internalplanner "github.com/ormasoftchile/yawr/runtime/internal/planner"
	internaltool "github.com/ormasoftchile/yawr/runtime/internal/tool"
	enginepkg "github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/hostaction"
	inputpkg "github.com/ormasoftchile/yawr/runtime/pkg/input"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/testutil"
)

func TestBuildEngineConfig_Real(t *testing.T) {
	dir := makeWorkDir(t)
	tracePath := filepath.Join(dir, "trace.jsonl")

	cfg, _, err := BuildEngineConfig(context.Background(), WireOptions{
		Mode:        "real",
		TraceFile:   tracePath,
		ToolScanDir: dir,
		TTYOutput:   true,
	})
	if err != nil {
		t.Fatalf("BuildEngineConfig: %v", err)
	}
	if cfg.Executors == nil || cfg.Dispatcher == nil || cfg.TraceWriter == nil || cfg.Platform == nil {
		t.Fatalf("expected required components to be wired")
	}
	if cfg.ToolRuntime == nil || cfg.InputProvider == nil || cfg.Evaluator == nil || cfg.ConditionEvaluator == nil {
		t.Fatalf("expected supporting components to be wired")
	}
}

func TestBuildEngineConfig_RealPromptUsesDurableInteractionProtocol(t *testing.T) {
	directory := t.TempDir()
	prompt := &testutil.FakePromptProvider{ChoiceResponses: map[string]*inputpkg.ChoiceResponse{
		"choose": {Selected: []string{"one"}},
	}}
	config, shutdown, err := BuildEngineConfig(context.Background(), WireOptions{
		Mode: "real", RunDir: filepath.Join(directory, "runs"), TraceFile: filepath.Join(directory, "trace.jsonl"),
		ToolScanDir: directory, PromptProviderOverride: prompt,
	})
	if err != nil {
		t.Fatalf("BuildEngineConfig: %v", err)
	}
	t.Cleanup(shutdown)
	plan := &enginepkg.ExecutionPlan{
		RunID: "run-durable-terminal-choice", RunbookPath: "choice.runbook.yaml",
		Steps: []enginepkg.ResolvedStep{{
			ID: "choose", Kind: "choice", Spec: &schema.ChoiceSpec{
				Prompt: "Choose", Variable: "selected",
				Options: []schema.ChoiceOption{{Label: "One", Value: "one"}},
			},
		}},
		Metadata: enginepkg.PlanMetadata{RunbookID: "durable-terminal-choice"},
	}
	if err := internalplanner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	handle, err := internalengine.New(config).Start(context.Background(), plan, enginepkg.RunOptions{Store: config.Store})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = handle.Cancel(context.Background(), "test cleanup") })
	result, err := handle.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	state := handle.State()
	if result.Vars["selected"] != "one" || state.CheckpointSequence != 3 || len(state.Interactions) != 0 ||
		len(state.InteractionInvocationCounts) != 1 || len(prompt.Calls) != 1 {
		t.Fatalf("result=%#v sequence=%d interactions=%#v counts=%#v prompt_calls=%d",
			result, state.CheckpointSequence, state.Interactions, state.InteractionInvocationCounts, len(prompt.Calls))
	}
}

func TestBuildEngineConfig_DryRun(t *testing.T) {
	dir := makeWorkDir(t)
	tracePath := filepath.Join(dir, "trace.jsonl")

	cfg, _, err := BuildEngineConfig(context.Background(), WireOptions{
		Mode:        "dry-run",
		TraceFile:   tracePath,
		ToolScanDir: dir,
	})
	if err != nil {
		t.Fatalf("BuildEngineConfig: %v", err)
	}
	if _, ok := cfg.Executors.(*DryRunExecutorRegistry); !ok {
		t.Fatalf("expected DryRunExecutorRegistry, got %T", cfg.Executors)
	}
}

func TestDryRunExecutor_HostActionProducesTypedStatus(t *testing.T) {
	result, err := (&DryRunExecutor{kind: "host_action"}).Execute(
		context.Background(),
		enginepkg.ResolvedStep{ID: "open_external_view"},
		nil,
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := result.Output["status"]; got != string(hostaction.StatusUnsupported) {
		t.Fatalf("status = %#v, want %q", got, hostaction.StatusUnsupported)
	}
}

func TestBuildEngineConfig_NoVault(t *testing.T) {
	dir := makeWorkDir(t)
	tracePath := filepath.Join(dir, "trace.jsonl")

	cfg, _, err := BuildEngineConfig(context.Background(), WireOptions{
		Mode:        "real",
		TraceFile:   tracePath,
		ToolScanDir: dir,
		VaultAddr:   "",
	})
	if err != nil {
		t.Fatalf("BuildEngineConfig: %v", err)
	}
	t.Setenv("YAWR_VAULT_TEST", "secret")
	resp, err := cfg.InputProvider.Provide(context.Background(), inputpkg.InputRequest{VarName: "test"})
	if err == nil || resp != nil {
		t.Fatalf("expected no vault provider, got response %#v (err=%v)", resp, err)
	}
}

func TestBuildEngineConfig_TraceFile(t *testing.T) {
	dir := makeWorkDir(t)
	tracePath := filepath.Join(dir, "trace.jsonl")

	if _, _, err := BuildEngineConfig(context.Background(), WireOptions{
		Mode:        "real",
		TraceFile:   tracePath,
		ToolScanDir: dir,
	}); err != nil {
		t.Fatalf("BuildEngineConfig: %v", err)
	}
	if _, err := os.Stat(tracePath); err != nil {
		t.Fatalf("expected trace file to exist: %v", err)
	}
}

func TestWireOptions_Defaults(t *testing.T) {
	opts := WireOptions{}.withDefaults()
	if opts.Mode != defaultMode {
		t.Fatalf("expected default mode %q, got %q", defaultMode, opts.Mode)
	}
	if opts.TraceDir != defaultTraceDir {
		t.Fatalf("expected default trace dir %q, got %q", defaultTraceDir, opts.TraceDir)
	}
	if opts.InputEnvPrefix != defaultInputEnvPrefix {
		t.Fatalf("expected default input env prefix %q, got %q", defaultInputEnvPrefix, opts.InputEnvPrefix)
	}
	if opts.ToolScanDir != defaultToolScanDir {
		t.Fatalf("expected default tool scan dir %q, got %q", defaultToolScanDir, opts.ToolScanDir)
	}
	if opts.RunDir != defaultRunDir {
		t.Fatalf("expected default run dir %q, got %q", defaultRunDir, opts.RunDir)
	}
}

func TestBuildEngineConfig_ToolScan(t *testing.T) {
	dir := makeWorkDir(t)
	tracePath := filepath.Join(dir, "trace.jsonl")
	toolPath := filepath.Join(dir, "echo.tool.yaml")
	if err := os.WriteFile(toolPath, []byte(sampleToolYAML()), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, _, err := BuildEngineConfig(context.Background(), WireOptions{
		Mode:        "real",
		TraceFile:   tracePath,
		ToolScanDir: dir,
	})
	if err != nil {
		t.Fatalf("BuildEngineConfig: %v", err)
	}
	runtime, ok := cfg.ToolRuntime.(*internaltool.DefaultToolRuntime)
	if !ok {
		t.Fatalf("expected DefaultToolRuntime, got %T", cfg.ToolRuntime)
	}
	// buildToolRegistry wraps its MapRegistry in an OverlayRegistry so that
	// Tool Packages MVP catalog/--package-map bindings can force-replace a
	// same-named tool the directory scan found first (see
	// internal/tool/overlay_registry.go). Lookup/All still transparently
	// fall through to the underlying scanned registry when there is no
	// override in play, which is what this test exercises.
	registry, ok := runtime.Registry().(*internaltool.OverlayRegistry)
	if !ok {
		t.Fatalf("expected OverlayRegistry, got %T", runtime.Registry())
	}
	if _, ok := registry.Lookup("echo"); !ok {
		t.Fatalf("expected scanned tool to be registered")
	}
}

func sampleToolYAML() string {
	return `apiVersion: yawr.tool/v1
meta: {name: echo}
transport:
  mode: stdio
  command: echo
  args: ["hello"]
actions:
  - name: ping
    description: ping`
}

func makeWorkDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(".", "adapter-test-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
