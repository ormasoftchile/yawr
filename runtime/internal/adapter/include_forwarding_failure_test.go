package adapter

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	internalengine "github.com/ormasoftchile/yawr/runtime/internal/engine"
	"github.com/ormasoftchile/yawr/runtime/internal/executor"
	internalplanner "github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestIncludeForwardingFailureAndAtomicCapture(t *testing.T) {
	for _, mode := range []string{"no-gate", "unmatched-gate", "failed-child", "tolerated-failure-gate", "immutable-capture", "wrong-type-capture"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			dir := typedEngineDirectory(t)
			cfg, stop, err := BuildEngineConfig(ctx, WireOptions{Mode: "real", RunDir: filepath.Join(dir, "runs"), TraceFile: filepath.Join(dir, "trace.jsonl"), ToolScanDir: dir})
			if err != nil {
				t.Fatal(err)
			}
			defer stop()
			cfg.Executors.(*executor.MapRegistry).Register("cli", forwardingCounter{directory: dir})
			gate := &schema.GateSpec{StopIf: []string{"blocked"}}
			child := []schema.FlowNode{
				{Step: &schema.Step{ID: "return", Type: schema.StepTypeEnd, EndSpec: &schema.EndSpec{Outcome: &schema.OutcomeDeclaration{Category: "blocked", Code: "missing-target"}}}},
				{Step: &schema.Step{ID: "publish", Type: schema.StepTypeResults, ResultsSpec: &schema.ResultsSpec{}}},
			}
			switch mode {
			case "no-gate":
				gate = nil
			case "unmatched-gate":
				gate.StopIf = []string{"resolved"}
			case "failed-child", "tolerated-failure-gate":
				failed := &schema.Step{ID: "source_failure", Type: schema.StepTypeAssert, AssertSpec: &schema.AssertSpec{Assert: []schema.Assertion{{Type: "eq", Subject: "original", Expected: "different"}}}}
				if mode == "tolerated-failure-gate" {
					failed.OnError = "continue"
				}
				child = append([]schema.FlowNode{{Step: failed}}, child...)
			case "immutable-capture", "wrong-type-capture":
				child = child[1:]
			}
			plan := &engine.ExecutionPlan{RunID: "forwarding-negative", RunbookPath: filepath.Join(dir, "root.yaml"), Metadata: engine.PlanMetadata{RunbookID: "forwarding-negative"},
				Outputs: map[string]*schema.Output{"result": {Type: "object", Optional: true, ValueExpr: "forwarded"}},
				Steps: []engine.ResolvedStep{
					{ID: "invoke", Kind: "include", Capture: map[string]string{"forwarded": "outputs.result", "other_alias": "outputs.result"},
						Spec: &schema.IncludeSpec{Include: schema.IncludeConfig{Runbook: "child.yaml", Gate: gate}, ResolvedRunbookPath: filepath.Join(dir, "child.yaml"),
							ResolvedOutputs: map[string]*schema.Output{"result": {Type: "object", ValueTreePresent: true, ValueTree: map[string]any{"native": false}}}, ResolvedSteps: child}},
					{ID: "downstream", Kind: "cli", Spec: &schema.CLISpec{Command: "never-launched"}},
					{ID: "results", Kind: "results", Spec: &schema.ResultsSpec{}},
				}}
			if mode == "immutable-capture" || mode == "wrong-type-capture" {
				plan.Bindings = []schema.Binding{{Name: "forwarded", Type: "string", Mutable: mode == "wrong-type-capture", Value: "unchanged", ValuePresent: true}}
			}
			if err := internalplanner.ValidateExecutionPlan(plan); err != nil {
				t.Fatal(err)
			}
			handle, err := internalengine.New(cfg).Start(ctx, plan, engine.RunOptions{Store: cfg.Store})
			if err != nil {
				t.Fatal(err)
			}
			for {
				_, err := handle.Next(ctx)
				if err == io.EOF {
					break
				}
				if err != nil {
					if (mode == "no-gate" || mode == "unmatched-gate") && strings.Contains(err.Error(), `public output "result" was not published`) {
						break
					}
					t.Fatal(err)
				}
			}
			state, err := cfg.Store.LoadState(ctx, plan.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if state.Status != engine.RunStatusFailed || state.Results != nil {
				t.Fatalf("negative branch published or completed: %s", state.Status)
			}
			if _, err := os.Stat(filepath.Join(dir, "downstream-counter")); !os.IsNotExist(err) {
				t.Fatal("failed include dispatched downstream")
			}
			if _, exists := state.Vars["other_alias"]; exists {
				t.Fatal("failed capture transaction committed partial alias")
			}
			result := state.StepResults["invoke"]
			if result == nil || result.Error == nil || result.Output["terminal"] == true {
				t.Fatalf("failure lost or converted to blocked: %#v", result)
			}
			if mode == "no-gate" || mode == "unmatched-gate" {
				if !strings.Contains(result.Error.Error(), `public output "result" was not published`) {
					t.Fatal(result.Error)
				}
			}
			if mode == "failed-child" || mode == "tolerated-failure-gate" {
				if strings.Contains(result.Error.Error(), "was not published") {
					t.Fatal("capture masked original source failure")
				}
			}
			if len(plan.Bindings) != 0 && state.Vars["forwarded"] != "unchanged" {
				t.Fatal("invalid capture overwrote binding")
			}
		})
	}
}
