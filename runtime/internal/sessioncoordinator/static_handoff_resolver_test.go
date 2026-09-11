package sessioncoordinator_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/ormasoftchile/yawr/runtime/internal/adapter"
	internalengine "github.com/ormasoftchile/yawr/runtime/internal/engine"
	"github.com/ormasoftchile/yawr/runtime/internal/eventbus"
	"github.com/ormasoftchile/yawr/runtime/internal/executor"
	internalexpr "github.com/ormasoftchile/yawr/runtime/internal/expr"
	internalparser "github.com/ormasoftchile/yawr/runtime/internal/parser"
	internalplanner "github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/internal/runstore"
	"github.com/ormasoftchile/yawr/runtime/internal/sessioncoordinator"
	"github.com/ormasoftchile/yawr/runtime/internal/sessionstore"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/expand"
	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

type staticResolverLoader struct {
	parser parser.Parser
}

func (loader staticResolverLoader) Load(ctx context.Context, path string) (*parser.ParsedRunbook, error) {
	return loader.parser.Parse(ctx, path)
}

type staticResolverTools struct{}

func (staticResolverTools) Lookup(context.Context, string, string) (*schema.ToolDef, error) {
	return nil, planner.ErrToolNotFound
}

func TestStaticHandoffResolverPlansAndRendersTargetFromFilesystem(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "source.runbook.yaml")
	targetPath := filepath.Join(root, "target.runbook.yaml")
	if err := os.WriteFile(targetPath, []byte(`apiVersion: yawr.runbook/v1
id: target
name: Target
inputs:
  server:
    type: string
    required: true
flow:
  - step:
      id: inspect
      type: noop
`), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	parserImpl, err := internalparser.New(platform.NewFakePlatform())
	if err != nil {
		t.Fatalf("new parser: %v", err)
	}
	loader := staticResolverLoader{parser: parserImpl}
	plannerImpl := internalplanner.New(planner.Config{Loader: loader, Tools: staticResolverTools{}})
	resolver, err := sessioncoordinator.NewStaticHandoffResolver(sessioncoordinator.StaticHandoffResolverConfig{
		Parser: parserImpl, Planner: plannerImpl, Loader: loader,
	})
	if err != nil {
		t.Fatalf("NewStaticHandoffResolver: %v", err)
	}
	target, err := resolver.ResolveHandoff(context.Background(), sessioncoordinator.HandoffResolveRequest{
		SourcePlan: &engine.ExecutionPlan{RunbookPath: sourcePath},
		Handoff:    engine.HandoffRequest{TargetRunbook: "target.runbook.yaml"},
	})
	if err != nil {
		t.Fatalf("ResolveHandoff: %v", err)
	}
	if target.Plan == nil || target.Plan.Validation == nil || filepath.Clean(target.Plan.RunbookPath) != targetPath ||
		target.Plan.Metadata.RunbookID != "target" || len(target.Graph) == 0 {
		t.Fatalf("resolved target = %#v graph=%s", target.Plan, target.Graph)
	}
	if _, err := resolver.ResolveHandoff(context.Background(), sessioncoordinator.HandoffResolveRequest{
		SourcePlan: &engine.ExecutionPlan{RunbookPath: sourcePath},
		Handoff:    engine.HandoffRequest{TargetRunbook: "${dynamic}.runbook.yaml"},
	}); err == nil {
		t.Fatal("resolver accepted a dynamic target")
	}
}

func TestStaticHandoffResolverRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	sourcePath := filepath.Join(root, "source.runbook.yaml")
	outsideTarget := filepath.Join(outside, "target.runbook.yaml")
	if err := os.WriteFile(outsideTarget, []byte(`apiVersion: yawr.runbook/v1
id: target
name: Target
flow:
  - step:
      id: inspect
      type: noop
`), 0o600); err != nil {
		t.Fatalf("write outside target: %v", err)
	}
	linkPath := filepath.Join(root, "linked")
	if err := os.Symlink(outside, linkPath); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	parserImpl, err := internalparser.New(platform.NewFakePlatform())
	if err != nil {
		t.Fatalf("new parser: %v", err)
	}
	loader := staticResolverLoader{parser: parserImpl}
	plannerImpl := internalplanner.New(planner.Config{Loader: loader, Tools: staticResolverTools{}})
	resolver, err := sessioncoordinator.NewStaticHandoffResolver(sessioncoordinator.StaticHandoffResolverConfig{
		Parser: parserImpl, Planner: plannerImpl, Loader: loader,
	})
	if err != nil {
		t.Fatalf("NewStaticHandoffResolver: %v", err)
	}
	if _, err := resolver.ResolveHandoff(context.Background(), sessioncoordinator.HandoffResolveRequest{
		SourcePlan: &engine.ExecutionPlan{RunbookPath: sourcePath},
		Handoff:    engine.HandoffRequest{TargetRunbook: "linked/target.runbook.yaml"},
	}); err == nil {
		t.Fatal("resolver followed a handoff target symlink outside the declaring directory")
	}
}

func TestStaticHandoffResolverGraphMatchesDefaultLazyPlan(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "source.runbook.yaml")
	targetPath := filepath.Join(root, "target.runbook.yaml")
	childPath := filepath.Join(root, "child.runbook.yaml")
	if err := os.WriteFile(childPath, []byte(`apiVersion: yawr.runbook/v1
id: child
name: Child
flow:
  - step:
      id: child_work
      type: noop
`), 0o600); err != nil {
		t.Fatalf("write child: %v", err)
	}
	if err := os.WriteFile(targetPath, []byte(`apiVersion: yawr.runbook/v1
id: target
name: Target
flow:
  - step:
      id: child
      type: include
      include:
        runbook: child.runbook.yaml
`), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	parserImpl, err := internalparser.New(platform.NewFakePlatform())
	if err != nil {
		t.Fatalf("new parser: %v", err)
	}
	loader := staticResolverLoader{parser: parserImpl}
	plannerImpl := internalplanner.New(planner.Config{
		Loader: loader, Tools: staticResolverTools{},
		ExpandPolicy: expand.Policy{Default: expand.ModeLazy},
	})
	resolver, err := sessioncoordinator.NewStaticHandoffResolver(sessioncoordinator.StaticHandoffResolverConfig{
		Parser: parserImpl, Planner: plannerImpl, Loader: loader,
	})
	if err != nil {
		t.Fatalf("NewStaticHandoffResolver: %v", err)
	}
	target, err := resolver.ResolveHandoff(context.Background(), sessioncoordinator.HandoffResolveRequest{
		SourcePlan: &engine.ExecutionPlan{RunbookPath: sourcePath},
		Handoff:    engine.HandoffRequest{TargetRunbook: "target.runbook.yaml"},
	})
	if err != nil {
		t.Fatalf("ResolveHandoff lazy target: %v", err)
	}
	if len(target.Plan.Steps) != 1 || target.Plan.Steps[0].ID != "child" {
		t.Fatalf("lazy target plan steps = %#v", target.Plan.Steps)
	}
	var graph struct {
		Nodes []struct {
			Data struct {
				StepID string `json:"step_id"`
			} `json:"data"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(target.Graph, &graph); err != nil {
		t.Fatalf("decode lazy target graph: %v", err)
	}
	if len(graph.Nodes) != 2 || graph.Nodes[0].Data.StepID != "child" || graph.Nodes[1].Data.StepID != "child_work" {
		t.Fatalf("lazy target graph nodes = %#v, want materialized child", graph.Nodes)
	}
}

func TestStaticHandoffResolverLazyTargetCommitsThroughCoordinator(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "source.runbook.yaml")
	targetPath := filepath.Join(root, "target.runbook.yaml")
	childPath := filepath.Join(root, "child.runbook.yaml")
	if err := os.WriteFile(childPath, []byte(`apiVersion: yawr.runbook/v1
id: child
name: Child
flow:
  - step:
      id: child_work
      type: noop
`), 0o600); err != nil {
		t.Fatalf("write child: %v", err)
	}
	if err := os.WriteFile(targetPath, []byte(`apiVersion: yawr.runbook/v1
id: target
name: Target
flow:
  - step:
      id: child
      type: include
      include:
        runbook: child.runbook.yaml
`), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	parserImpl, err := internalparser.New(platform.NewFakePlatform())
	if err != nil {
		t.Fatalf("new parser: %v", err)
	}
	loader := staticResolverLoader{parser: parserImpl}
	plannerImpl := internalplanner.New(planner.Config{
		Loader: loader, Tools: staticResolverTools{},
		ExpandPolicy: expand.Policy{Default: expand.ModeLazy},
	})
	resolver, err := sessioncoordinator.NewStaticHandoffResolver(sessioncoordinator.StaticHandoffResolverConfig{
		Parser: parserImpl, Planner: plannerImpl, Loader: loader,
	})
	if err != nil {
		t.Fatalf("NewStaticHandoffResolver: %v", err)
	}
	registry := executor.NewMapRegistry()
	registry.Register("handoff", executor.NewHandoffExecutor(&internalexpr.TemplateEvaluator{}))
	registry.Register("include", executor.NewIncludeExecutor(
		&internalexpr.TemplateEvaluator{}, nil, adapter.NewParserLazyLoader(parserImpl),
	))
	runs := runstore.NewDirRunStore(filepath.Join(root, "runs"))
	sessions := sessionstore.NewDirStore(filepath.Join(root, "sessions"))
	t.Cleanup(func() {
		_ = runs.Close()
		_ = sessions.Close()
	})
	runtime := internalengine.New(engine.EngineConfig{
		Executors: registry, Dispatcher: eventbus.NewDispatcher(), TraceWriter: discardTraceWriter{},
		Platform: platform.NewFakePlatform(), Store: runs,
	})
	coordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
		Sessions: sessions, Runs: runs, Engine: runtime, HandoffResolver: resolver,
	})
	if err != nil {
		t.Fatalf("New coordinator: %v", err)
	}
	sourcePlan := &engine.ExecutionPlan{
		RunbookPath: sourcePath,
		Steps: []engine.ResolvedStep{{
			ID: "continue", Kind: "handoff", Spec: &schema.HandoffSpec{Handoff: schema.HandoffConfig{
				Runbook: "target.runbook.yaml",
				Reason:  schema.HandoffReason{Code: "continue", Summary: "Continue"},
			}},
		}},
		Metadata: engine.PlanMetadata{RunbookID: "source", RunbookName: "Source"},
	}
	if err := internalplanner.ValidateExecutionPlan(sourcePlan); err != nil {
		t.Fatalf("ValidateExecutionPlan source: %v", err)
	}
	handle, err := coordinator.Start(context.Background(), sessioncoordinator.StartRequest{
		SessionID: uuid.NewString(), CommandID: uuid.NewString(), SegmentID: uuid.NewString(), RunID: uuid.NewString(),
		Plan: sourcePlan, Graph: targetGraphJSON(t, sourcePlan),
		RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result, err := handle.Next(context.Background()); err != nil || result != nil {
		t.Fatalf("handoff Next = %#v, %v", result, err)
	}
	manifest := handle.Manifest()
	if len(manifest.Transitions) != 1 || len(manifest.Segments) != 2 ||
		manifest.Session.ActiveRunID == sourcePlan.RunID {
		t.Fatalf("lazy target handoff manifest = %#v", manifest)
	}

	staleTarget, err := resolver.ResolveHandoff(context.Background(), sessioncoordinator.HandoffResolveRequest{
		SourcePlan: sourcePlan, Handoff: engine.HandoffRequest{TargetRunbook: "target.runbook.yaml"},
	})
	if err != nil {
		t.Fatalf("capture stale target: %v", err)
	}
	if err := os.WriteFile(childPath, []byte(`apiVersion: yawr.runbook/v1
id: child
name: Child
flow:
  - step:
      id: child_work
      type: noop
      title: Changed after graph capture
`), 0o600); err != nil {
		t.Fatalf("change child definition: %v", err)
	}
	driftRuns := runstore.NewDirRunStore(filepath.Join(root, "drift-runs"))
	driftSessions := sessionstore.NewDirStore(filepath.Join(root, "drift-sessions"))
	t.Cleanup(func() {
		_ = driftRuns.Close()
		_ = driftSessions.Close()
	})
	driftRuntime := internalengine.New(engine.EngineConfig{
		Executors: registry, Dispatcher: eventbus.NewDispatcher(), TraceWriter: discardTraceWriter{},
		Platform: platform.NewFakePlatform(), Store: driftRuns,
	})
	driftCoordinator, err := sessioncoordinator.New(sessioncoordinator.Config{
		Sessions: driftSessions, Runs: driftRuns, Engine: driftRuntime,
		HandoffResolver: &staticHandoffResolver{target: staleTarget},
	})
	if err != nil {
		t.Fatalf("New drift coordinator: %v", err)
	}
	driftSessionID := uuid.NewString()
	driftHandle, err := driftCoordinator.Start(context.Background(), sessioncoordinator.StartRequest{
		SessionID: driftSessionID, CommandID: uuid.NewString(), SegmentID: uuid.NewString(), RunID: uuid.NewString(),
		Plan: sourcePlan, Graph: targetGraphJSON(t, sourcePlan),
		RunOptions: engine.RunOptions{Mode: engine.RunModeReal},
	})
	if err != nil {
		t.Fatalf("Start drift source: %v", err)
	}
	if _, err := driftHandle.Next(context.Background()); err == nil {
		t.Fatal("stale target graph was committed after child definition drift")
	}
	driftManifest := driftHandle.Manifest()
	if len(driftManifest.Transitions) != 0 || len(driftManifest.Segments) != 1 {
		t.Fatalf("definition drift published transition: %#v", driftManifest)
	}
}
