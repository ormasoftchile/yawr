package adapter

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

type typedDynamicLoader struct {
	loader executor.LazyRunbookLoader
	path   string
	calls  int
}

func (resolver *typedDynamicLoader) Resolve(ctx context.Context, _ string) (*executor.DynamicIncludeResult, error) {
	resolver.calls++
	loaded, err := resolver.loader.Load(ctx, resolver.path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(resolver.path)
	if err != nil {
		return nil, err
	}
	return &executor.DynamicIncludeResult{Flow: loaded.Flow, ChildInputs: loaded.Inputs, ChildBindings: loaded.Bindings, ChildOutputs: loaded.Outputs,
		RunbookID: loaded.ID, RunbookName: loaded.Name, ContentHash: loaded.ContentHash, AbsPath: resolver.path,
		QualifiedID: "synthetic/child", PackageName: "synthetic", PackageVersion: "1.0.0",
		FileDigest: fmt.Sprintf("sha256:%x", sha256.Sum256(data)), PackageDigest: "sha256:" + strings.Repeat("a", 64)}, nil
}

type typedChildEntryFailure struct {
	*runstore.DirRunStore
	fired bool
}

func (store *typedChildEntryFailure) SaveState(ctx context.Context, state engine.RunState) error {
	for _, frame := range state.ExecutionFrames {
		if !store.fired && frame.BindingScope != nil && frame.BindingScope.FrameID == frame.FrameID && len(frame.Results) == 0 {
			store.fired = true
			if err := store.DirRunStore.SaveState(ctx, state); err != nil {
				return err
			}
			return fmt.Errorf("injected committed child entry failure")
		}
	}
	return store.DirRunStore.SaveState(ctx, state)
}

func TestTypedResultsEngineDynamicAndLazyRestart(t *testing.T) {
	for _, dynamic := range []bool{false, true} {
		t.Run(fmt.Sprint(dynamic), func(t *testing.T) {
			ctx := context.Background()
			dir := typedEngineDirectory(t)
			path := filepath.Join(dir, "child.yaml")
			source := `apiVersion: yawr.runbook/v1
id: child
name: Child
bindings:
  - name: entry
    type: boolean
    value: false
outputs:
  result:
    type: object
    value_tree:
      flag: "${entry}"
      zero: 0
      nothing: null
      array: [false, 0, null]
flow:
  - step:
      id: publish
      type: results
`
			if err := os.WriteFile(path, []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			parser, err := internalparser.New(platform.Real())
			if err != nil {
				t.Fatal(err)
			}
			loader := NewParserLazyLoader(parser)
			resolver := &typedDynamicLoader{loader: loader, path: path}
			configure := func(traceName string) (engine.EngineConfig, func()) {
				cfg, shutdown, err := BuildEngineConfig(ctx, WireOptions{Mode: "real", RunDir: filepath.Join(dir, "runs"), TraceFile: filepath.Join(dir, traceName), ToolScanDir: dir,
					LazyRunbookLoader: loader, DynamicIncludeResolver: resolver})
				if err != nil {
					t.Fatal(err)
				}
				return cfg, shutdown
			}
			include := &schema.IncludeSpec{Include: schema.IncludeConfig{Runbook: "child.yaml"}, LazyRunbookPath: path}
			if dynamic {
				include = &schema.IncludeSpec{Include: schema.IncludeConfig{RunbookRef: "synthetic/child"}}
			}
			plan := &engine.ExecutionPlan{RunID: "typed-closure", RunbookPath: filepath.Join(dir, "root.yaml"), Metadata: engine.PlanMetadata{RunbookID: "typed-closure"},
				Outputs: map[string]*schema.Output{"result": {Type: "object", ValueExpr: "child_result"}},
				Steps: []engine.ResolvedStep{{ID: "child", Kind: "include", Spec: include, Capture: map[string]string{"child_result": "outputs.result"}},
					{ID: "results", Kind: "results", Spec: &schema.ResultsSpec{}}},
			}
			if err := internalplanner.ValidateExecutionPlan(plan); err != nil {
				t.Fatal(err)
			}
			cfg, shutdown := configure("first.jsonl")
			fault := &typedChildEntryFailure{DirRunStore: cfg.Store.(*runstore.DirRunStore)}
			cfg.Store = fault
			handle, err := internalengine.New(cfg).Start(ctx, plan, engine.RunOptions{Store: fault})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = handle.Next(ctx); err == nil || !fault.fired {
				t.Fatalf("child entry failure not reached: %v", err)
			}
			if handle.State().Results != nil {
				t.Fatal("entry published Results")
			}
			shutdown()
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			second, stop := configure("second.jsonl")
			defer stop()
			resumed, err := internalengine.New(second).Resume(ctx, plan.RunID, engine.RunOptions{Store: second.Store, AcknowledgeIndeterminate: true, RuntimeVars: map[string]any{"entry": true}})
			if err != nil {
				t.Fatal(err)
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
			state, err := second.Store.LoadState(ctx, plan.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if state.Results == nil {
				t.Fatal("missing frozen child capture")
			}
			value := state.Results.Outputs["result"].Value.(map[string]any)
			if value["flag"] != false || value["nothing"] != nil || fmt.Sprint(value["zero"]) != "0" {
				t.Fatalf("frozen values changed: %#v", value)
			}
			if dynamic && resolver.calls != 1 {
				t.Fatalf("dynamic resolver dispatched %d times", resolver.calls)
			}
		})
	}
}
