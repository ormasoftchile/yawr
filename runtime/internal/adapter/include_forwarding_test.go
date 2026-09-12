package adapter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	internalengine "github.com/ormasoftchile/yawr/runtime/internal/engine"
	"github.com/ormasoftchile/yawr/runtime/internal/executor"
	internalparser "github.com/ormasoftchile/yawr/runtime/internal/parser"
	internalplanner "github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/internal/runstore"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

type forwardingCheckpointFailure struct {
	*runstore.DirRunStore
	point       string
	fired       bool
	publication *engine.RunResults
}

type forwardingCounter struct{ directory string }

func (counter forwardingCounter) Execute(ctx context.Context, step engine.ResolvedStep, vars map[string]any) (*engine.StepResult, error) {
	name := "counter"
	if step.ID == "downstream" {
		name = "downstream-counter"
	}
	return (&typedCounterExecutor{path: filepath.Join(counter.directory, name)}).Execute(ctx, step, vars)
}

func (store *forwardingCheckpointFailure) SaveState(ctx context.Context, state engine.RunState) error {
	match := store.point == "parent-return" && state.StepResults["invoke"] != nil
	for _, frame := range state.ExecutionFrames {
		for _, result := range frame.Results {
			if result != nil && (result.StepID == "publish" && result.Results != nil || result.StepID == "return" && result.Status == engine.StepStatusCompleted) {
				match = match || store.point == "child-return"
				if result.Results != nil {
					store.publication, _ = engine.CloneRunResults(result.Results)
				}
			}
		}
	}
	if !store.fired && match {
		store.fired = true
		if err := store.DirRunStore.SaveState(ctx, state); err != nil {
			return err
		}
		return errors.New("injected forwarding committed-return failure")
	}
	return store.DirRunStore.SaveState(ctx, state)
}

func TestIncludeForwardingDurableGate(t *testing.T) {
	for _, expansion := range []string{"eager", "lazy", "dynamic", "nested"} {
		for _, blocked := range []bool{false, true} {
			for _, point := range []string{"child-return", "parent-return"} {
				t.Run(fmt.Sprintf("%s/blocked-%v/%s", expansion, blocked, point), func(t *testing.T) {
					ctx := context.Background()
					dir := typedEngineDirectory(t)
					childPath := filepath.Join(dir, "child.yaml")
					source := `apiVersion: yawr.runbook/v1
id: child
name: Child
inputs:
  configured: {type: string, required: true}
bindings:
  - {name: entry, type: string, value: '${configured}'}
  - {name: count, type: integer, mutable: true, value: 0}
outputs:
  result:
    type: object
    value_tree:
      entry: '${entry}'
      count: '${count}'
      flag: false
      zero: 0
      nullable: null
      ordered: [false, 0, null]
      object: {}
flow:
  - step: {id: produce, type: cli, cli: {command: never-launched}}
`
					if blocked {
						source += "  - step: {id: return, type: end, outcome: {category: blocked, code: missing-target}}\n"
					}

					source += "  - step: {id: publish, type: results}\n"
					if err := os.WriteFile(childPath, []byte(source), 0600); err != nil {
						t.Fatal(err)
					}
					parser, err := internalparser.New(platform.Real())
					if err != nil {
						t.Fatal(err)
					}
					loader := NewParserLazyLoader(parser)
					resolver := &typedDynamicLoader{loader: loader, path: childPath}
					loaded, err := loader.Load(ctx, childPath)
					if err != nil {
						t.Fatal(err)
					}
					include := &schema.IncludeSpec{Include: schema.IncludeConfig{Runbook: "child.yaml", Gate: &schema.GateSpec{StopIf: []string{"blocked"}}},
						ResolvedRunbookPath: childPath, ResolvedSteps: loaded.Flow, ResolvedInputs: loaded.Inputs, ResolvedBindings: loaded.Bindings, ResolvedOutputs: loaded.Outputs}
					if expansion == "lazy" {
						include.ResolvedSteps, include.ResolvedBindings, include.ResolvedOutputs = nil, nil, nil
						include.LazyRunbookPath = childPath
					}
					if expansion == "dynamic" {
						include = &schema.IncludeSpec{Include: schema.IncludeConfig{RunbookRef: "synthetic/child", Gate: &schema.GateSpec{StopIf: []string{"blocked"}}}}
					}
					if expansion == "nested" {
						include = &schema.IncludeSpec{Include: schema.IncludeConfig{Runbook: "wrapper.yaml", Gate: &schema.GateSpec{StopIf: []string{"blocked"}}},
							ResolvedRunbookPath: filepath.Join(dir, "wrapper.yaml"),
							ResolvedOutputs:     map[string]*schema.Output{"result": {Type: "object", ValueExpr: "nested_result"}},
							ResolvedSteps: []schema.FlowNode{
								{Step: &schema.Step{ID: "nested", Type: schema.StepTypeInclude, IncludeSpec: include, Capture: map[string]string{"nested_result": "outputs.result"}}},
								{Step: &schema.Step{ID: "wrapper_results", Type: schema.StepTypeResults, ResultsSpec: &schema.ResultsSpec{}}},
							}}
					}
					plan := &engine.ExecutionPlan{RunID: "forwarding-restart", RunbookPath: filepath.Join(dir, "root.yaml"), Metadata: engine.PlanMetadata{RunbookID: "forwarding-restart"},
						Outputs: map[string]*schema.Output{"result": {Type: "object", Optional: true, ValueExpr: "forwarded"}},
						Steps: []engine.ResolvedStep{
							{ID: "invoke", Kind: "include", Spec: include, Capture: map[string]string{"forwarded": "outputs.result", "alias": "outputs.result"}},
							{ID: "downstream", Kind: "cli", Spec: &schema.CLISpec{Command: "never-launched"}},
							{ID: "results", Kind: "results", Spec: &schema.ResultsSpec{}},
						}}
					if err := internalplanner.ValidateExecutionPlan(plan); err != nil {
						t.Fatal(err)
					}
					configure := func(trace string) (engine.EngineConfig, func()) {
						cfg, stop, err := BuildEngineConfig(ctx, WireOptions{Mode: "real", RunDir: filepath.Join(dir, "runs"), TraceFile: filepath.Join(dir, trace), ToolScanDir: dir, LazyRunbookLoader: loader, DynamicIncludeResolver: resolver})
						if err != nil {
							t.Fatal(err)
						}
						cfg.Executors.(*executor.MapRegistry).Register("cli", forwardingCounter{directory: dir})
						return cfg, stop
					}
					first, stop := configure("first.jsonl")
					fault := &forwardingCheckpointFailure{DirRunStore: first.Store.(*runstore.DirRunStore), point: point}
					first.Store = fault
					handle, err := internalengine.New(first).Start(ctx, plan, engine.RunOptions{Store: fault, RuntimeVars: map[string]any{"configured": "original", "alias": "unchanged"}})
					if err != nil {
						t.Fatal(err)
					}
					for {
						_, err = handle.Next(ctx)
						if err != nil {
							break
						}
					}
					if !fault.fired || !errors.Is(err, engine.ErrCheckpointCommit) {
						t.Fatalf("fault=%v err=%v", fault.fired, err)
					}
					before, err := fault.LoadState(ctx, plan.RunID)
					if err != nil {
						t.Fatal(err)
					}
					if !blocked && fault.publication == nil {
						t.Fatal("child had no authoritative committed publication")
					}
					if blocked && before.Results != nil {
						t.Fatal("blocked child published root result")
					}
					stop()
					if err := os.Remove(childPath); err != nil {
						t.Fatal(err)
					}
					second, stopSecond := configure("second.jsonl")
					defer stopSecond()
					resumed, err := internalengine.New(second).Resume(ctx, plan.RunID, engine.RunOptions{Store: second.Store, AcknowledgeIndeterminate: true, RuntimeVars: map[string]any{"configured": "changed"}})
					if blocked && point == "parent-return" {
						if err == nil || !strings.Contains(err.Error(), "run already completed") {
							t.Fatalf("terminal resume: %v", err)
						}
					} else if err != nil {
						t.Fatal(err)
					}
					for resumed != nil {
						result, err := resumed.Next(ctx)
						if err == io.EOF {
							break
						}
						if err != nil {
							t.Fatal(err)
						}
						if result != nil && result.Error != nil {
							t.Fatal(result.Error)
						}
					}
					state, err := second.Store.LoadState(ctx, plan.RunID)
					if err != nil {
						t.Fatal(err)
					}
					if state.Status != engine.RunStatusCompleted {
						t.Fatalf("status %s", state.Status)
					}
					counter, err := os.ReadFile(filepath.Join(dir, "counter"))
					if err != nil || string(counter) != "1" {
						t.Fatalf("producer replay count %q: %v", counter, err)
					}
					if expansion == "dynamic" && resolver.calls != 1 {
						t.Fatalf("resolver replayed %d times", resolver.calls)
					}
					if blocked {
						if _, err := os.Stat(filepath.Join(dir, "downstream-counter")); !os.IsNotExist(err) {
							t.Fatal("gate dispatched downstream action")
						}
						if state.Results != nil || state.StepResults["results"] != nil && state.StepResults["results"].Status != engine.StepStatusSkipped {
							t.Fatal("gate ran parent Results")
						}
						if _, exists := state.Vars["forwarded"]; exists {
							t.Fatal("invented forwarded value")
						}
						if state.Vars["alias"] != "unchanged" {
							t.Fatal("gate committed partial aliases")
						}
						if state.StepResults["invoke"].Output["terminal"] != true || state.StepResults["invoke"].Output["outcome_category"] != "blocked" {
							t.Fatal("lost durable blocked outcome")
						}
					} else {
						downstream, err := os.ReadFile(filepath.Join(dir, "downstream-counter"))
						if err != nil || string(downstream) != "1" {
							t.Fatal("ordinary continuation did not dispatch exactly once")
						}
						if state.Results == nil || !reflect.DeepEqual(state.Results.Outputs["result"].Value, fault.publication.Outputs["result"].Value) {
							t.Fatal("forwarded value differs from committed native child result")
						}
						found := false
						for _, frame := range state.ExecutionFrames {
							if frame.RunResults != nil && frame.RunResults.PublicationID == fault.publication.PublicationID {
								found = reflect.DeepEqual(frame.RunResults, fault.publication)
							}
						}
						if !found {
							t.Fatal("original child identity/digest/sequence changed")
						}
					}
					for _, name := range []string{"first.jsonl", "second.jsonl"} {
						data, err := os.ReadFile(filepath.Join(dir, name))
						if err != nil {
							t.Fatal(err)
						}
						if strings.Contains(string(data), "was not published") {
							t.Fatal("capture failure after return")
						}
					}
					stopSecond()
					third, stopThird := configure("third.jsonl")
					defer stopThird()
					terminal, err := internalengine.New(third).Resume(ctx, plan.RunID, engine.RunOptions{Store: third.Store})
					if blocked {
						if err == nil || !strings.Contains(err.Error(), "run already completed") {
							t.Fatalf("terminal resume continued: %v", err)
						}
					} else {
						if err != nil {
							t.Fatal(err)
						}
						if _, err = terminal.Next(ctx); err != io.EOF {
							t.Fatalf("published terminal resume continued: %v", err)
						}
					}
					after, err := third.Store.LoadState(ctx, plan.RunID)
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(after.Results, state.Results) || after.Vars["__run_outcome_category"] != state.Vars["__run_outcome_category"] {
						t.Fatal("terminal reopen changed publication/outcome")
					}
					counter, _ = os.ReadFile(filepath.Join(dir, "counter"))
					if string(counter) != "1" {
						t.Fatal("terminal resume redispatched producer")
					}
				})
			}
		}
	}
}
