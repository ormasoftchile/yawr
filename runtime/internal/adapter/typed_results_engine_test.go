package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	internalengine "github.com/ormasoftchile/yawr/runtime/internal/engine"
	"github.com/ormasoftchile/yawr/runtime/internal/executor"
	internalplanner "github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	"github.com/ormasoftchile/yawr/runtime/internal/runstore"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/input"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/testutil"
)

func typedEngineDirectory(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(fmt.Sprintf(".typed-engine-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(path) })
	return path
}

type typedCounterExecutor struct{ path string }

func (counter *typedCounterExecutor) Execute(ctx context.Context, step engine.ResolvedStep, _ map[string]any) (*engine.StepResult, error) {
	if _, err := engine.PrepareExternalDispatch(ctx, engine.DispatchRequest{Classification: "read-only", EndpointIdentity: "typed-synthetic-counter", RenderedRequest: map[string]any{"increment": true}}); err != nil {
		return nil, err
	}
	count := 0
	if data, err := os.ReadFile(counter.path); err == nil {
		_, _ = fmt.Sscan(string(data), &count)
	}
	count++
	if err := os.WriteFile(counter.path, []byte(fmt.Sprint(count)), 0600); err != nil {
		return nil, err
	}
	return &engine.StepResult{StepID: step.ID, Status: engine.StepStatusCompleted, Outcome: engine.StepOutcomeSuccess,
		Output: map[string]any{"stdout": "", "exit_code": 0}, Vars: map[string]any{"count": count}}, nil
}

type typedCheckpointFailure struct {
	*runstore.DirRunStore
	point                 string
	fired                 bool
	committedPublications map[string]*engine.RunResults
	committedEventIDs     []string
}

func (store *typedCheckpointFailure) SaveState(ctx context.Context, state engine.RunState) error {
	hasResults := state.Results != nil
	hasProducer := state.StepResults["produce"] != nil
	for _, frame := range state.ExecutionFrames {
		hasResults = hasResults || frame.RunResults != nil
		for _, result := range frame.Results {
			hasProducer = hasProducer || result != nil && result.StepID == "produce"
		}
	}
	match := (store.point == "before-results" || store.point == "after-results") && hasResults ||
		store.point == "after-entry" && state.BindingScope != nil ||
		store.point == "after-producer" && hasProducer
	if !store.fired && match {
		store.fired = true
		if store.point != "before-results" {
			if err := store.DirRunStore.SaveState(ctx, state); err != nil {
				return err
			}
			store.committedPublications = make(map[string]*engine.RunResults)
			if state.Results != nil {
				store.committedPublications[state.Results.PublicationID], _ = engine.CloneRunResults(state.Results)
			}
			for _, frame := range state.ExecutionFrames {
				if frame.RunResults != nil {
					store.committedPublications[frame.RunResults.PublicationID], _ = engine.CloneRunResults(frame.RunResults)
				}
			}
			for _, event := range state.PendingTraceEvents {
				store.committedEventIDs = append(store.committedEventIDs, event.EventID)
			}
		}
		return errors.New("injected typed checkpoint failure")
	}
	return store.DirRunStore.SaveState(ctx, state)
}

func TestTypedResultsEngineRestartNoDuplicate(t *testing.T) {
	for _, nested := range []bool{false, true} {
		for _, point := range []string{"before-results", "after-results", "after-entry", "after-producer"} {
			t.Run(fmt.Sprintf("nested-%v/%s", nested, point), func(t *testing.T) {
				ctx := context.Background()
				dir := typedEngineDirectory(t)
				configure := func(traceName string) (engine.EngineConfig, func()) {
					cfg, shutdown, err := BuildEngineConfig(ctx, WireOptions{Mode: "real", RunDir: filepath.Join(dir, "runs"), TraceFile: filepath.Join(dir, traceName), ToolScanDir: dir})
					if err != nil {
						t.Fatal(err)
					}
					cfg.Executors.(*executor.MapRegistry).Register("cli", &typedCounterExecutor{path: filepath.Join(dir, "counter")})
					return cfg, shutdown
				}
				bindings := []schema.Binding{{Name: "entry", Type: "string", Value: "${configured}", ValuePresent: true}, {Name: "count", Type: "integer", Mutable: true, Value: 0, ValuePresent: true}}
				outputs := map[string]*schema.Output{"result": {Type: "object", ValueTreePresent: true, ValueTree: map[string]any{"entry": "${entry}", "count": "${count}"}}}
				producer := &schema.Step{ID: "produce", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "never-launched"}}
				flow := []schema.FlowNode{{Step: producer}, {Step: &schema.Step{ID: "results", Type: schema.StepTypeResults, ResultsSpec: &schema.ResultsSpec{}}}}
				plan := &engine.ExecutionPlan{RunID: "typed-restart", RunbookPath: filepath.Join(dir, "root.yaml"), Bindings: bindings, Outputs: outputs,
					Steps:    []engine.ResolvedStep{{ID: "produce", Kind: "cli", Spec: producer.CLI}, {ID: "results", Kind: "results", Spec: &schema.ResultsSpec{}}},
					Metadata: engine.PlanMetadata{RunbookID: "typed-restart"}}
				if nested {
					plan.Bindings = nil
					plan.Outputs = map[string]*schema.Output{"result": {Type: "object", ValueExpr: "child_result"}}
					plan.Steps = []engine.ResolvedStep{{ID: "child", Kind: "include", Spec: &schema.IncludeSpec{Include: schema.IncludeConfig{Runbook: "child.yaml"},
						ResolvedRunbookPath: filepath.Join(dir, "child.yaml"), ResolvedSteps: flow, ResolvedBindings: bindings, ResolvedOutputs: outputs}, Capture: map[string]string{"child_result": "outputs.result"}},
						{ID: "results", Kind: "results", Spec: &schema.ResultsSpec{}}}
				}
				if err := internalplanner.ValidateExecutionPlan(plan); err != nil {
					t.Fatal(err)
				}
				cfg, shutdown := configure("first.jsonl")
				fault := &typedCheckpointFailure{DirRunStore: cfg.Store.(*runstore.DirRunStore), point: point}
				cfg.Store = fault
				handle, err := internalengine.New(cfg).Start(ctx, plan, engine.RunOptions{Store: fault, RuntimeVars: map[string]any{"configured": "original"}})
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
					t.Fatalf("fault fired=%v error=%v", fault.fired, err)
				}
				if point == "before-results" && handle.State().Results != nil {
					t.Fatal("uncommitted Results escaped State")
				}
				shutdown()
				second, stopSecond := configure("second.jsonl")
				defer stopSecond()
				resumed, err := internalengine.New(second).Resume(ctx, plan.RunID, engine.RunOptions{Store: second.Store, AcknowledgeIndeterminate: true, RuntimeVars: map[string]any{"configured": "changed", "entry": "tampered"}})
				if err != nil {
					t.Fatal("resume:", err)
				}
				for {
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
				state := resumed.State()
				if state.Results == nil || state.Status != engine.RunStatusCompleted {
					t.Fatalf("missing resumed publication: status %s", state.Status)
				}
				value := state.Results.Outputs["result"].Value.(map[string]any)
				if value["entry"] != "original" || fmt.Sprint(value["count"]) != "1" {
					t.Fatalf("entry reinitialized or producer duplicated: %#v", value)
				}
				counter, err := os.ReadFile(filepath.Join(dir, "counter"))
				if err != nil || string(counter) != "1" {
					t.Fatalf("external count=%s err=%v", counter, err)
				}
				durable, err := second.Store.LoadState(ctx, plan.RunID)
				if err != nil {
					t.Fatal(err)
				}
				if durable.Results.Digest != state.Results.Digest || durable.Results.PublicationID != state.Results.PublicationID {
					t.Fatal("durable identity differs")
				}
				for id, original := range fault.committedPublications {
					actual := state.Results
					if actual.PublicationID != id {
						actual = nil
						for _, frame := range state.ExecutionFrames {
							if frame.RunResults != nil && frame.RunResults.PublicationID == id {
								actual = frame.RunResults
							}
						}
					}
					if actual == nil || actual.Digest != original.Digest || actual.CheckpointSequence != original.CheckpointSequence {
						t.Fatal("committed publication regenerated after ambiguous save")
					}
				}
				counts := make(map[string]int)
				for _, name := range []string{"first.jsonl", "second.jsonl"} {
					data, err := os.ReadFile(filepath.Join(dir, name))
					if err != nil {
						t.Fatal(err)
					}
					for _, line := range strings.Split(string(data), "\n") {
						if strings.TrimSpace(line) == "" {
							continue
						}
						var event struct {
							EventID string `json:"event_id"`
						}
						if err := json.Unmarshal([]byte(line), &event); err != nil {
							t.Fatal(err)
						}
						counts[event.EventID]++
					}
				}
				for _, id := range fault.committedEventIDs {
					if counts[id] != 1 {
						t.Fatalf("original committed event %s projected %d times", id, counts[id])
					}
				}
			})
		}
	}
}

func TestTypedResultsEngineFrozenToolPublication(t *testing.T) {
	ctx := context.Background()
	dir := typedEngineDirectory(t)
	cfg, shutdown, err := BuildEngineConfig(ctx, WireOptions{Mode: "real", RunDir: filepath.Join(dir, "runs"), TraceFile: filepath.Join(dir, "trace.jsonl"), ToolScanDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown()
	flow := []schema.FlowNode{{Step: &schema.Step{ID: "child_results", Type: schema.StepTypeResults, ResultsSpec: &schema.ResultsSpec{}}}}
	closure, err := plansnapshot.EncodeFlowClosure(flow)
	if err != nil {
		t.Fatal(err)
	}
	outputs := map[string]*schema.Output{"result": {Type: "object", ValueTreePresent: true, ValueTree: map[string]any{"flag": "${flag}", "null": nil, "array": []any{0, false, nil}}}}
	plan := &engine.ExecutionPlan{RunID: "typed-tool", RunbookPath: filepath.Join(dir, "root.yaml"), Metadata: engine.PlanMetadata{RunbookID: "typed-tool"},
		Tools: map[string]*schema.ToolDef{"saved": {Name: "saved", Actions: map[string]*schema.ToolAction{"run": {
			Execute: &schema.ExecuteSpec{Kind: "runbook", Path: "child.yaml"}, Outputs: map[string]*schema.ArgDef{"result": {Type: "object"}},
			FrozenSubstitution: &schema.FrozenToolSubstitution{PackageName: "synthetic", RunbookPath: filepath.Join(dir, "child.yaml"), RunbookID: "child", RunbookContentHash: strings.Repeat("a", 64),
				ExecutableClosure: closure, Outputs: outputs, Bindings: []schema.Binding{{Name: "flag", Type: "boolean", Value: false, ValuePresent: true}}},
		}}}},
		Outputs: map[string]*schema.Output{"result": {Type: "object", ValueExpr: "public.result"}},
		Steps: []engine.ResolvedStep{{ID: "tool", Kind: "tool", Spec: &schema.ToolCallSpec{Tool: schema.ToolInvocation{Name: "saved", Action: "run"}}, Capture: map[string]string{"public": "outputs"}},
			{ID: "results", Kind: "results", Spec: &schema.ResultsSpec{}}},
	}
	if err := internalplanner.ValidateExecutionPlan(plan); err != nil {
		t.Fatal(err)
	}
	handle, err := internalengine.New(cfg).Start(ctx, plan, engine.RunOptions{Store: cfg.Store})
	if err != nil {
		t.Fatal(err)
	}
	for {
		result, err := handle.Next(ctx)
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
	state := handle.State()
	if state.Results == nil {
		t.Fatal("root missing")
	}
	value := state.Results.Outputs["result"].Value.(map[string]any)
	if value["flag"] != false || value["null"] != nil {
		t.Fatalf("typed child values: %#v", value)
	}
	childPublications := 0
	for _, frame := range state.ExecutionFrames {
		if frame.RunResults != nil {
			childPublications++
			if frame.RunResults.Origin.NodeID != "tool/child_results" || frame.Status != engine.ExecutionFrameStatusCompleted {
				t.Fatal("child scope not atomically terminal")
			}
		}
	}
	if childPublications != 1 {
		t.Fatalf("child publications=%d", childPublications)
	}
	if _, exists := state.Vars["flag"]; exists {
		t.Fatal("tool private binding leaked into caller vars")
	}
}

type typedHeldForm struct {
	testutil.FakePromptProvider
	entered chan struct{}
	release chan struct{}
}

func (provider *typedHeldForm) PromptForm(ctx context.Context, _ input.FormRequest) (*input.FormResponse, error) {
	close(provider.entered)
	select {
	case <-provider.release:
		return &input.FormResponse{Values: map[string]any{"answer": "accepted"}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestTypedResultsEnginePendingCollector(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dir := typedEngineDirectory(t)
	provider := &typedHeldForm{entered: make(chan struct{}), release: make(chan struct{})}
	cfg, shutdown, err := BuildEngineConfig(ctx, WireOptions{Mode: "real", RunDir: filepath.Join(dir, "runs"), TraceFile: filepath.Join(dir, "trace.jsonl"), ToolScanDir: dir, PromptProviderOverride: provider})
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown()
	plan := &engine.ExecutionPlan{RunID: "typed-pending", RunbookPath: filepath.Join(dir, "root.yaml"), Metadata: engine.PlanMetadata{RunbookID: "typed-pending"},
		Outputs: map[string]*schema.Output{"answer": {Type: "string", ValueExpr: "answer"}},
		Steps: []engine.ResolvedStep{{ID: "collect", Kind: "collector", Spec: &schema.CollectorSpec{Fields: []schema.CollectorField{{Name: "answer", Type: schema.FieldTypeText, Required: true}}}},
			{ID: "results", Kind: "results", Spec: &schema.ResultsSpec{}}},
	}
	if err := internalplanner.ValidateExecutionPlan(plan); err != nil {
		t.Fatal(err)
	}
	handle, err := internalengine.New(cfg).Start(ctx, plan, engine.RunOptions{Store: cfg.Store})
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { _, err := handle.Next(ctx); finished <- err }()
	select {
	case <-provider.entered:
	case <-ctx.Done():
		t.Fatal("collector not entered")
	}
	state := handle.State()
	if state.Results != nil || state.Status == engine.RunStatusCompleted || len(state.Interactions) == 0 {
		t.Fatalf("fabricated pending completion: %s", state.Status)
	}
	persisted, err := cfg.Store.LoadState(ctx, plan.RunID)
	if err != nil || persisted.Results != nil {
		t.Fatalf("pending durable Results: %v", err)
	}
	close(provider.release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Next(ctx); err != nil {
		t.Fatal(err)
	}
	if handle.State().Results == nil {
		t.Fatal("answered collector did not permit Results")
	}
}

func TestTypedResultsEngineFailureBoundaries(t *testing.T) {
	for _, mode := range []string{"immutable-capture", "atomic-assign", "tolerated-assert", "tolerated-child", "optional-type", "protected-result"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			dir := typedEngineDirectory(t)
			cfg, shutdown, err := BuildEngineConfig(ctx, WireOptions{Mode: "real", RunDir: filepath.Join(dir, "runs"), TraceFile: filepath.Join(dir, "trace.jsonl"), ToolScanDir: dir})
			if err != nil {
				t.Fatal(err)
			}
			defer shutdown()
			plan := &engine.ExecutionPlan{RunID: "typed-failure", RunbookPath: filepath.Join(dir, "root.yaml"), Metadata: engine.PlanMetadata{RunbookID: "typed-failure"},
				Bindings: []schema.Binding{{Name: "count", Type: "integer", Mutable: true, Value: 0, ValuePresent: true}},
				Outputs:  map[string]*schema.Output{"count": {Type: "integer", ValueExpr: "count"}},
				Steps:    []engine.ResolvedStep{{ID: "results", Kind: "results", Spec: &schema.ResultsSpec{}}},
			}
			switch mode {
			case "immutable-capture":
				plan.Bindings[0].Mutable = false
				cfg.Executors.(*executor.MapRegistry).Register("cli", &typedCounterExecutor{path: filepath.Join(dir, "counter")})
				plan.Steps = append([]engine.ResolvedStep{{ID: "produce", Kind: "cli", Spec: &schema.CLISpec{Command: "never-launched"}}}, plan.Steps...)
			case "atomic-assign":
				plan.Steps = append([]engine.ResolvedStep{{ID: "assign", Kind: "assign", Spec: &schema.AssignSpec{Assign: []schema.Assignment{{Name: "count", Value: 1, ValuePresent: true}, {Name: "count", Value: "wrong", ValuePresent: true}}}}}, plan.Steps...)
			case "tolerated-assert":
				plan.Steps = append([]engine.ResolvedStep{{ID: "assert", Kind: "assert", Spec: &schema.AssertSpec{Assert: []schema.Assertion{{Type: "eq", Subject: "actual", Expected: "different"}}}, OnError: "continue"}}, plan.Steps...)
			case "tolerated-child":
				child := &schema.Step{ID: "assert", Type: schema.StepTypeAssert, AssertSpec: &schema.AssertSpec{Assert: []schema.Assertion{{Type: "eq", Subject: "actual", Expected: "different"}}}, OnError: "continue"}
				plan.Steps = append([]engine.ResolvedStep{{ID: "child", Kind: "include", OnError: "continue", Spec: &schema.IncludeSpec{Include: schema.IncludeConfig{Runbook: "child.yaml"}, ResolvedSteps: []schema.FlowNode{{Step: child}}}}}, plan.Steps...)
			case "optional-type":
				plan.Outputs["bad"] = &schema.Output{Type: "string", Optional: true, ValueTreePresent: true, ValueTree: false}
			case "protected-result":
				plan.Inputs = map[string]*schema.Input{"secret_value": {Type: "secret"}}
				plan.Outputs["secret"] = &schema.Output{Type: "string", ValueExpr: "secret_value"}
			}
			if err := internalplanner.ValidateExecutionPlan(plan); err != nil {
				t.Fatal(err)
			}
			handle, err := internalengine.New(cfg).Start(ctx, plan, engine.RunOptions{Store: cfg.Store, RuntimeVars: map[string]any{"secret_value": "synthetic-protected-literal"}})
			if err != nil {
				t.Fatal(err)
			}
			for {
				_, err := handle.Next(ctx)
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			state := handle.State()
			if state.Status != engine.RunStatusFailed || state.Results != nil {
				t.Fatalf("failure published Results: %s", state.Status)
			}
			if fmt.Sprint(state.Vars["count"]) != "0" {
				t.Fatal("failed write mutated binding")
			}
			persisted, err := cfg.Store.LoadState(ctx, plan.RunID)
			if err != nil || persisted.Results != nil {
				t.Fatalf("failed publication durable: %v", err)
			}
		})
	}
}
func TestTypedResultsEngineRootAndIncluded(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(fmt.Sprint(nested), func(t *testing.T) {
			ctx := context.Background()
			dir := typedEngineDirectory(t)
			cfg, shutdown, err := BuildEngineConfig(ctx, WireOptions{
				Mode: "real", RunDir: filepath.Join(dir, "runs"), TraceFile: filepath.Join(dir, "trace.jsonl"), ToolScanDir: dir,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(shutdown)
			bindings := []schema.Binding{
				{Name: "flag", Type: "boolean", Mutable: true, Value: "${configured}", ValuePresent: true},
				{Name: "nothing", Type: "any", Value: nil, ValuePresent: true},
			}
			outputs := map[string]*schema.Output{
				"result": {Type: "object", ValueTreePresent: true, ValueTree: map[string]any{"flag": "${flag}", "zero": 0, "null": "${nothing}", "empty": []any{}}},
				"absent": {Type: "string", Optional: true, ValueExpr: "missing"},
			}
			assign := &schema.Step{ID: "write", Type: schema.StepTypeAssign, AssignSpec: &schema.AssignSpec{Assign: []schema.Assignment{{Name: "flag", Value: false, ValuePresent: true}}}}
			branch := &schema.Step{ID: "branch", Type: schema.StepTypeBranch, BranchSpec: &schema.BranchSpec{Branches: []schema.BranchArm{{Else: true, Steps: []schema.FlowNode{{Step: assign}}}}}}
			flow := []schema.FlowNode{{Step: branch}, {Step: &schema.Step{ID: "publish", Type: schema.StepTypeResults, ResultsSpec: &schema.ResultsSpec{}}}}
			steps := []engine.ResolvedStep{{ID: "branch", Kind: "branch", Spec: branch.BranchSpec}, {ID: "publish", Kind: "results", Spec: &schema.ResultsSpec{}}}
			plan := &engine.ExecutionPlan{RunID: "typed-native", RunbookPath: filepath.Join(dir, "root.yaml"), Bindings: bindings, Outputs: outputs, Steps: steps, Metadata: engine.PlanMetadata{RunbookID: "typed-native"}}
			if nested {
				plan.Bindings = nil
				plan.Outputs = map[string]*schema.Output{"result": {Type: "object", ValueExpr: "child_result"}}
				plan.Steps = []engine.ResolvedStep{
					{ID: "child", Kind: "include", Spec: &schema.IncludeSpec{Include: schema.IncludeConfig{Runbook: "child.yaml"}, ResolvedRunbookPath: filepath.Join(dir, "child.yaml"), ResolvedSteps: flow, ResolvedBindings: bindings, ResolvedOutputs: outputs}, Capture: map[string]string{"child_result": "outputs.result"}},
					{ID: "root_results", Kind: "results", Spec: &schema.ResultsSpec{}},
				}
			}
			if err := internalplanner.ValidateExecutionPlan(plan); err != nil {
				t.Fatal(err)
			}
			handle, err := internalengine.New(cfg).Start(ctx, plan, engine.RunOptions{Store: cfg.Store, RuntimeVars: map[string]any{"configured": true}})
			if err != nil {
				t.Fatal(err)
			}
			if handle.State().Results != nil {
				t.Fatal("published before execution")
			}
			for {
				result, err := handle.Next(ctx)
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if result != nil && result.Error != nil {
					t.Fatalf("step %s: %v", result.StepID, result.Error)
				}
			}
			state := handle.State()
			if state.Status != engine.RunStatusCompleted || state.Results == nil {
				t.Fatalf("status=%s results=%v", state.Status, state.Results)
			}
			actual := state.Results.Outputs["result"].Value.(map[string]any)
			if actual["flag"] != false || actual["null"] != nil || fmt.Sprint(actual["zero"]) != "0" {
				t.Fatalf("native values lost: %#v", actual)
			}
			if _, exists := state.Results.Outputs["absent"]; exists {
				t.Fatal("optional missing fabricated")
			}
			want, err := engine.CanonicalResultsJSON(state.Results, true)
			if err != nil {
				t.Fatal(err)
			}
			state.Results.Outputs["result"] = engine.NamedResultValue{Type: "string", Value: "modified"}
			if err := handle.State().Results.Validate(); err != nil {
				t.Fatal("State exposed mutable record", err)
			}
			reopened := runstore.NewDirRunStore(filepath.Join(dir, "runs"))
			defer reopened.Close()
			persisted, err := reopened.LoadState(ctx, plan.RunID)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := engine.CanonicalResultsJSON(persisted.Results, true)
			if string(got) != string(want) {
				t.Fatalf("reopened result differs: %s != %s", got, want)
			}
		})
	}
}
