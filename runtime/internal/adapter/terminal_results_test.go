package adapter

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	internalengine "github.com/ormasoftchile/yawr/runtime/internal/engine"
	internalparser "github.com/ormasoftchile/yawr/runtime/internal/parser"
	internalplanner "github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/internal/runstore"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func terminalResultsFlow(category string, nested bool) []schema.FlowNode {
	flow := []schema.FlowNode{
		{Step: &schema.Step{ID: "end", Type: schema.StepTypeEnd, EndSpec: &schema.EndSpec{
			PublishResults: true, Outcome: &schema.OutcomeDeclaration{Category: category, Code: "GeoDR-Exact_Code"},
		}}},
		{Step: &schema.Step{ID: "unreachable", Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{}}},
	}
	if nested {
		for _, id := range []string{"inner", "outer"} {
			flow = []schema.FlowNode{{Step: &schema.Step{ID: id, Type: schema.StepTypeBranch,
				BranchSpec: &schema.BranchSpec{Branches: []schema.BranchArm{{Else: true, Steps: flow}}},
			}}, {Step: &schema.Step{ID: "tail_" + id, Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{}}}}
		}
	}
	return flow
}

func TestTerminalResultsOutcomesAndIncludes(t *testing.T) {
	for _, category := range []string{"resolved", "escalated", "no_action", "blocked", "no-data", "boundary", "custom-category"} {
		for _, mode := range []string{"direct", "nested", "iteration", "eager", "lazy", "dynamic"} {
			t.Run(category+"/"+mode, func(t *testing.T) {
				ctx := context.Background()
				dir := typedEngineDirectory(t)
				parser, err := internalparser.New(platform.Real())
				if err != nil {
					t.Fatal(err)
				}
				loader := NewParserLazyLoader(parser)
				childPath := filepath.Join(dir, "child.yaml")
				outputs := map[string]*schema.Output{"result": {Type: "object", ValueTreePresent: true,
					ValueTree: map[string]any{"flag": false, "zero": 0, "null": nil, "status": category}}}
				flow := terminalResultsFlow(category, mode != "direct")
				if mode == "iteration" {
					flow = []schema.FlowNode{{Iterate: &schema.IterateNode{ID: "loop", Over: "items", Steps: flow}}}
				}
				plan := &engine.ExecutionPlan{RunID: "terminal-results", RunbookPath: filepath.Join(dir, "root.yaml"),
					Metadata: engine.PlanMetadata{RunbookID: "terminal-results"}, Outputs: outputs}
				var resolver *typedDynamicLoader
				included := mode == "eager" || mode == "lazy" || mode == "dynamic"
				if included {
					source := fmt.Sprintf(`apiVersion: yawr.runbook/v1
id: child
name: Child
outputs:
  result: {type: object, value_tree: {flag: false, zero: 0, "null": null, status: %s}}
flow:
  - step:
      id: outer
      type: branch
      branches:
        - else: true
          steps:
            - step:
                id: inner
                type: branch
                branches:
                  - else: true
                    steps:
                      - step: {id: end, type: end, publish_results: true, outcome: {category: %s, code: GeoDR-Exact_Code}}
                      - step: {id: unreachable, type: noop}
  - step: {id: tail_child, type: noop}
`, category, category)
					if err := os.WriteFile(childPath, []byte(source), 0600); err != nil {
						t.Fatal(err)
					}
					include := &schema.IncludeSpec{Include: schema.IncludeConfig{Runbook: "child.yaml"},
						ResolvedRunbookPath: childPath, ResolvedSteps: flow, ResolvedOutputs: outputs}
					if mode == "lazy" {
						include.ResolvedSteps, include.ResolvedOutputs = nil, nil
						include.LazyRunbookPath = childPath
					} else if mode == "dynamic" {
						include = &schema.IncludeSpec{Include: schema.IncludeConfig{RunbookRef: "synthetic/child"}}
						resolver = &typedDynamicLoader{loader: loader, path: childPath}
					}
					plan.Steps = append(plan.Steps, engine.ResolvedStep{ID: "invoke", Kind: "include", Spec: include,
						Capture: map[string]string{"child_result": "outputs.result"}})
					plan.Outputs = map[string]*schema.Output{"result": {Type: "object", ValueExpr: "child_result"}}
				}
				for _, node := range flow {
					step, _ := resolveFlowNode(node)
					plan.Steps = append(plan.Steps, step)
				}
				if err := internalplanner.ValidateExecutionPlan(plan); err != nil {
					t.Fatal(err)
				}
				options := WireOptions{Mode: "real", RunDir: filepath.Join(dir, "runs"), TraceFile: filepath.Join(dir, "trace.jsonl"),
					ToolScanDir: dir, LazyRunbookLoader: loader}
				if resolver != nil {
					options.DynamicIncludeResolver = resolver
				}
				cfg, stop, err := BuildEngineConfig(ctx, options)
				if err != nil {
					t.Fatal(err)
				}
				defer stop()
				handle, err := internalengine.New(cfg).Start(ctx, plan, engine.RunOptions{RuntimeVars: map[string]any{"items": []any{1, 2}}})
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
					if result.Error != nil {
						t.Fatal(result.Error)
					}
				}
				state := handle.State()
				if state.Status != engine.RunStatusCompleted || state.Results == nil || state.Results.Validate() != nil {
					t.Fatalf("missing completed publication: status=%s", state.Status)
				}
				if state.Vars["__run_outcome_category"] != category || state.Vars["__run_outcome_code"] != "GeoDR-Exact_Code" {
					t.Fatalf("outcome changed: %#v", state.Vars)
				}
				record := state.Results
				if mode == "iteration" && state.StepResults["loop"].Output["iterations"] != 1 {
					t.Fatal("iteration continued after publishing end")
				}
				wantPublications := 1
				if included {
					wantPublications++
				}
				publications := map[string]bool{record.PublicationID: true}
				check := func(result *engine.StepResult) {
					if result.Status == engine.StepStatusCompleted && (result.StepID == "unreachable" ||
						result.StepID == "tail_inner" || result.StepID == "tail_outer" || result.StepID == "tail_child") {
						t.Fatalf("dispatched terminal tail: %s", result.StepID)
					}
				}
				for _, result := range state.StepResults {
					check(result)
				}
				for _, frame := range state.ExecutionFrames {
					if frame.RunResults != nil {
						publications[frame.RunResults.PublicationID] = true
					}
					for _, result := range frame.Results {
						check(result)
					}
				}
				if len(publications) != wantPublications {
					t.Fatalf("publications=%d want=%d", len(publications), wantPublications)
				}
				value := record.Outputs["result"].Value.(map[string]any)
				if value["status"] != category || value["flag"] != false || value["null"] != nil || fmt.Sprint(value["zero"]) != "0" {
					t.Fatalf("native outputs changed: %#v", value)
				}
				stop()
				reopened := runstore.NewDirRunStore(options.RunDir)
				defer reopened.Close()
				loaded, err := reopened.LoadState(ctx, plan.RunID)
				if err != nil {
					t.Fatal(err)
				}
				if loaded.Results.Digest != record.Digest {
					t.Fatal("durable record changed")
				}
				options.TraceFile = filepath.Join(dir, "resumed.jsonl")
				cfg, stopResumed, err := BuildEngineConfig(ctx, options)
				if err != nil {
					t.Fatal(err)
				}
				defer stopResumed()
				cfg.Store = reopened
				resumed, err := internalengine.New(cfg).Resume(ctx, plan.RunID, engine.RunOptions{Store: reopened})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := resumed.Next(ctx); err != io.EOF {
					t.Fatalf("completed resume dispatched: %v", err)
				}
				if resumed.State().Results.Digest != record.Digest || resumed.State().Results.CheckpointSequence != record.CheckpointSequence {
					t.Fatal("completed resume republished")
				}
			})
		}
	}
}
