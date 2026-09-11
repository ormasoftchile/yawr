package adapter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	internalengine "github.com/ormasoftchile/yawr/runtime/internal/engine"
	internalplanner "github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestTypedResultsEnginePreEntrySessionRestart(t *testing.T) {
	ctx := context.Background()
	dir := typedEngineDirectory(t)
	configure := func(name string) (engine.EngineConfig, func()) {
		cfg, shutdown, err := BuildEngineConfig(ctx, WireOptions{Mode: "real", RunDir: filepath.Join(dir, "runs"), TraceFile: filepath.Join(dir, name), ToolScanDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		return cfg, shutdown
	}
	plan := &engine.ExecutionPlan{RunID: "typed-preentry", RunbookPath: filepath.Join(dir, "root.yaml"), Metadata: engine.PlanMetadata{RunbookID: "typed-preentry"},
		Bindings: []schema.Binding{{Name: "entry", Type: "string", Value: "${configured}", ValuePresent: true}},
		Outputs:  map[string]*schema.Output{"result": {Type: "string", ValueExpr: "entry"}},
		Steps:    []engine.ResolvedStep{{ID: "results", Kind: "results", Spec: &schema.ResultsSpec{}}},
	}
	if err := internalplanner.ValidateExecutionPlan(plan); err != nil {
		t.Fatal(err)
	}
	cfg, shutdown := configure("first.jsonl")
	handle, err := internalengine.New(cfg).Start(ctx, plan, engine.RunOptions{Store: cfg.Store, RuntimeVars: map[string]any{"configured": "original"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Store.SaveState(ctx, handle.State()); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(dir, "runs", plan.RunID, "snapshots", "*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("initial checkpoint: %v", err)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "run-state/v3") {
		t.Fatal("feature-bearing pre-entry checkpoint downgraded")
	}
	shutdown()
	second, stop := configure("second.jsonl")
	defer stop()
	resumed, err := internalengine.New(second).Resume(ctx, plan.RunID, engine.RunOptions{Store: second.Store, RuntimeVars: map[string]any{"configured": "changed"}})
	if err != nil {
		t.Fatal(err)
	}
	for {
		_, err := resumed.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	state := resumed.State()
	if state.Results == nil || state.Results.Outputs["result"].Value != "original" || state.StartedAt.IsZero() {
		t.Fatal("pre-entry persisted input or initialization lost")
	}
}

func TestTypedResultsEngineEntryDiagnosticOrigin(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(fmt.Sprint(nested), func(t *testing.T) {
			ctx := context.Background()
			dir := typedEngineDirectory(t)
			cfg, shutdown, err := BuildEngineConfig(ctx, WireOptions{Mode: "real", RunDir: filepath.Join(dir, "runs"), TraceFile: filepath.Join(dir, "trace.jsonl"), ToolScanDir: dir})
			if err != nil {
				t.Fatal(err)
			}
			defer shutdown()
			bindings := []schema.Binding{{Name: "entry", Type: "integer", Value: "redaction-proof-wrong", ValuePresent: true}}
			outputs := map[string]*schema.Output{"result": {Type: "integer", ValueExpr: "entry"}}
			plan := &engine.ExecutionPlan{RunID: "typed-entry-error", RunbookPath: filepath.Join(dir, "root.yaml"), Metadata: engine.PlanMetadata{RunbookID: "typed-entry-error"},
				Bindings: bindings, Outputs: outputs, Steps: []engine.ResolvedStep{{ID: "results", Kind: "results", Spec: &schema.ResultsSpec{}}},
			}
			origin := "bindings"
			if nested {
				origin = "child/bindings"
				plan.Bindings = nil
				plan.Steps = []engine.ResolvedStep{{ID: "child", Kind: "include", Spec: &schema.IncludeSpec{Include: schema.IncludeConfig{Runbook: "child.yaml"}, ResolvedBindings: bindings, ResolvedOutputs: outputs,
					ResolvedSteps: []schema.FlowNode{{Step: &schema.Step{ID: "results", Type: schema.StepTypeResults, ResultsSpec: &schema.ResultsSpec{}}}}},
				}}
			}
			if err := internalplanner.ValidateExecutionPlan(plan); err != nil {
				t.Fatal(err)
			}
			handle, err := internalengine.New(cfg).Start(ctx, plan, engine.RunOptions{Store: cfg.Store})
			if err != nil {
				t.Fatal(err)
			}
			_, err = handle.Next(ctx)
			var diagnostic *engine.TypedDiagnostic
			if !errors.As(err, &diagnostic) {
				t.Fatalf("typed diagnostic missing: %v", err)
			}
			if diagnostic.Origin.NodeID != origin || diagnostic.Preview != "<redacted>" || diagnostic.ActualType != "string" || strings.Contains(err.Error(), "redaction-proof-wrong") {
				t.Fatalf("unsafe/incomplete typed origin: %#v", diagnostic)
			}
			if handle.State().Results != nil {
				t.Fatal("failed entry published Results")
			}
		})
	}
}
