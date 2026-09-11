package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	internaldebugprotect "github.com/ormasoftchile/yawr/runtime/internal/debugprotect"
	internalengine "github.com/ormasoftchile/yawr/runtime/internal/engine"
	"github.com/ormasoftchile/yawr/runtime/internal/executor"
	internalexpr "github.com/ormasoftchile/yawr/runtime/internal/expr"
	internalparser "github.com/ormasoftchile/yawr/runtime/internal/parser"
	internalplanner "github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/expand"
	parserpkg "github.com/ormasoftchile/yawr/runtime/pkg/parser"
	plannerpkg "github.com/ormasoftchile/yawr/runtime/pkg/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

type nestedDebugRecorder struct {
	locations   []engine.DebugLocation
	snapshots   []engine.DebugSnapshot
	mutateAfter bool
}

type nestedDebugControllerFunc func(context.Context, engine.DebugSnapshot) (engine.DebugDecision, error)

type nestedOutputExecutor struct{}

func (nestedOutputExecutor) Execute(_ context.Context, step engine.ResolvedStep, _ map[string]any) (*engine.StepResult, error) {
	return &engine.StepResult{
		StepID:  step.ID,
		Status:  engine.StepStatusCompleted,
		Outcome: engine.StepOutcomeSuccess,
		Output:  map[string]any{"recommendation_status": "suggested"},
		Vars:    map[string]any{},
	}, nil
}

func (controller nestedDebugControllerFunc) Pause(ctx context.Context, snapshot engine.DebugSnapshot) (engine.DebugDecision, error) {
	return controller(ctx, snapshot)
}

func (r *nestedDebugRecorder) Pause(_ context.Context, snapshot engine.DebugSnapshot) (engine.DebugDecision, error) {
	r.snapshots = append(r.snapshots, snapshot)
	if snapshot.Phase == engine.DebugPhaseBefore {
		r.locations = append(r.locations, snapshot.Location)
	}
	if r.mutateAfter && snapshot.Phase == engine.DebugPhaseAfter {
		return engine.DebugDecision{Action: engine.DebugActionContinue, Vars: map[string]any{"debug_marker": true}}, nil
	}
	return engine.DebugDecision{Action: engine.DebugActionContinue}, nil
}

func TestRunSubStepsViaEngine_InheritsParentToolContracts(t *testing.T) {
	registry := &testRegistry{executors: map[string]engine.StepExecutor{
		"tool": nestedOutputExecutor{},
	}}
	ctx := engine.WithPlanTools(context.Background(), map[string]*schema.ToolDef{
		"tsg-recommendation": {
			Name: "tsg-recommendation",
			Actions: map[string]*schema.ToolAction{
				"recommend": {
					Outputs: map[string]*schema.ArgDef{
						"recommendation_status": {Type: "string", Required: true},
					},
				},
			},
		},
	})
	child := []schema.FlowNode{{Step: &schema.Step{
		ID:   "recommend_tsg",
		Type: schema.StepTypeTool,
		ToolCall: &schema.ToolCallSpec{Tool: schema.ToolInvocation{
			Name: "tsg-recommendation", Action: "recommend",
		}},
		Capture: map[string]string{"recommendation_status": "outputs.recommendation_status"},
	}}}

	results, err := runSubStepsViaEngine(
		ctx, registry, executor.SubStepParent{ID: "sql_context_gate", Kind: "branch"}, child,
		map[string]any{"incident_id": "DEMO-INC-000001"}, engine.RunModeReal,
		&noopTraceWriter{}, &noopDispatcher{}, platform.NewFakePlatform(),
		nil, nil, nil, nil, nil, nil, nil,
	)
	if err != nil {
		t.Fatalf("run nested tool: %v", err)
	}
	if len(results) != 1 || results[0].Vars["recommendation_status"] != "suggested" {
		t.Fatalf("nested capture = %#v, want recommendation_status=suggested", results)
	}
}

func TestRootEngine_PropagatesToolContractsToNestedBranch(t *testing.T) {
	registry := &testRegistry{executors: map[string]engine.StepExecutor{
		"tool": nestedOutputExecutor{},
	}}
	runner := func(ctx context.Context, parent executor.SubStepParent, nodes []schema.FlowNode, vars map[string]any) ([]*engine.StepResult, error) {
		return runSubStepsViaEngine(
			ctx, registry, parent, nodes, vars, engine.RunModeReal,
			&noopTraceWriter{}, &noopDispatcher{}, platform.NewFakePlatform(),
			nil, nil, nil, nil, nil, nil, nil,
		)
	}
	registry.Register("branch", executor.NewBranchExecutor(nil, runner))
	plan := &engine.ExecutionPlan{
		RunID: "root-run", RunbookPath: "root.runbook.yaml",
		Tools: map[string]*schema.ToolDef{
			"tsg-recommendation": {
				Name: "tsg-recommendation",
				Actions: map[string]*schema.ToolAction{
					"recommend": {
						Outputs: map[string]*schema.ArgDef{
							"recommendation_status": {Type: "string", Required: true},
						},
					},
				},
			},
		},
		Steps: []engine.ResolvedStep{{
			ID: "sql_context_gate", Kind: "branch",
			Spec: &schema.BranchSpec{Branches: []schema.BranchArm{{
				Else: true,
				Steps: []schema.FlowNode{{Step: &schema.Step{
					ID: "recommend_tsg", Type: schema.StepTypeTool,
					ToolCall: &schema.ToolCallSpec{Tool: schema.ToolInvocation{
						Name: "tsg-recommendation", Action: "recommend",
					}},
					Capture: map[string]string{"recommendation_status": "outputs.recommendation_status"},
				}}},
			}}},
		}},
	}
	engine.ValidatedForTest(plan)
	handle, err := internalengine.New(engine.EngineConfig{
		Executors: registry, Dispatcher: &noopDispatcher{}, TraceWriter: &noopTraceWriter{}, Platform: platform.NewFakePlatform(),
	}).Start(context.Background(), plan, engine.RunOptions{Vars: map[string]string{"incident_id": "DEMO-INC-000001"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	result, err := handle.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if result.Vars["recommendation_status"] != "suggested" {
		t.Fatalf("branch vars = %#v, want recommendation_status=suggested", result.Vars)
	}
}

func TestRunSubStepsViaEngine_DebuggerDistinguishesIncludeCallPaths(t *testing.T) {
	registry := &testRegistry{executors: map[string]engine.StepExecutor{
		"tool": &passExecutor{},
	}}
	recorder := &nestedDebugRecorder{}
	child := []schema.FlowNode{{Step: &schema.Step{ID: "get_incident", Type: schema.StepTypeTool}}}

	for _, parentID := range []string{"inspect_primary_icm", "inspect_secondary_icm"} {
		debugCtx := engine.WithDebugController(context.Background(), recorder)
		_, err := runSubStepsViaEngine(
			debugCtx,
			registry,
			executor.SubStepParent{ID: parentID, Kind: "include"},
			child,
			nil,
			engine.RunModeReal,
			&noopTraceWriter{},
			&noopDispatcher{},
			platform.NewFakePlatform(),
			nil, nil, nil, nil, nil, nil, nil,
		)
		if err != nil {
			t.Fatalf("run child through %s: %v", parentID, err)
		}
	}

	if len(recorder.locations) != 2 {
		t.Fatalf("debug locations = %d, want 2", len(recorder.locations))
	}
	for i, wantParent := range []string{"inspect_primary_icm", "inspect_secondary_icm"} {
		location := recorder.locations[i]
		if location.StepID != "get_incident" {
			t.Fatalf("location %d step = %q, want get_incident", i, location.StepID)
		}
		if len(location.CallPath) != 1 || location.CallPath[0].StepID != wantParent {
			t.Fatalf("location %d call path = %#v, want [%s]", i, location.CallPath, wantParent)
		}
	}
}

func TestRunSubStepsViaEngine_DebuggerInvocationCounterSpansSubEngines(t *testing.T) {
	registry := &testRegistry{executors: map[string]engine.StepExecutor{"tool": &passExecutor{}}}
	recorder := &nestedDebugRecorder{}
	child := []schema.FlowNode{{Step: &schema.Step{ID: "get_incident", Type: schema.StepTypeTool}}}
	tracker := engine.NewDebugInvocationTracker()
	debugCtx := engine.WithDebugController(context.Background(), recorder)
	debugCtx = engine.WithDebugInvocationTracker(debugCtx, tracker)

	for iteration := 0; iteration < 2; iteration++ {
		_, err := runSubStepsViaEngine(
			debugCtx,
			registry,
			executor.SubStepParent{ID: "loop", Kind: "iterate"},
			child,
			nil,
			engine.RunModeReal,
			&noopTraceWriter{},
			&noopDispatcher{},
			platform.NewFakePlatform(),
			nil, nil, nil, nil, nil, nil, nil,
		)
		if err != nil {
			t.Fatalf("iteration %d: %v", iteration+1, err)
		}
	}
	if len(recorder.locations) != 2 || recorder.locations[0].Invocation != 1 || recorder.locations[1].Invocation != 2 {
		t.Fatalf("invocations = %#v, want 1 then 2", recorder.locations)
	}
}

func TestNestedDebuggerStopCancelsRootRun(t *testing.T) {
	registry := &testRegistry{executors: map[string]engine.StepExecutor{"tool": &passExecutor{}}}
	runner := func(ctx context.Context, parent executor.SubStepParent, nodes []schema.FlowNode, vars map[string]any) ([]*engine.StepResult, error) {
		return runSubStepsViaEngine(
			ctx, registry, parent, nodes, vars, engine.RunModeReal,
			&noopTraceWriter{}, &noopDispatcher{}, platform.NewFakePlatform(),
			nil, nil, nil, nil, nil, nil, nil,
		)
	}
	registry.Register("include", executor.NewIncludeExecutor(nil, runner, nil))
	debugger := nestedDebugControllerFunc(func(_ context.Context, snapshot engine.DebugSnapshot) (engine.DebugDecision, error) {
		if snapshot.Phase == engine.DebugPhaseBefore && len(snapshot.Location.CallPath) > 0 {
			return engine.DebugDecision{Action: engine.DebugActionStop}, nil
		}
		return engine.DebugDecision{Action: engine.DebugActionContinue}, nil
	})
	plan := &engine.ExecutionPlan{
		RunID: "root-run", RunbookPath: "root.runbook.yaml",
		Steps: []engine.ResolvedStep{{
			ID: "child", Kind: "include", Spec: &schema.IncludeSpec{
				Include:       schema.IncludeConfig{Runbook: "child.runbook.yaml"},
				ResolvedSteps: []schema.FlowNode{{Step: &schema.Step{ID: "nested", Type: schema.StepTypeTool}}},
			},
		}},
	}
	engine.ValidatedForTest(plan)
	handle, err := internalengine.New(engine.EngineConfig{
		Executors: registry, Dispatcher: &noopDispatcher{}, TraceWriter: &noopTraceWriter{}, Platform: platform.NewFakePlatform(),
	}).Start(context.Background(), plan, engine.RunOptions{Debugger: debugger})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := handle.Next(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Next error = %v, want context.Canceled", err)
	}
	if status := handle.State().Status; status != engine.RunStatusCancelled {
		t.Fatalf("root status = %s, want cancelled", status)
	}
}

func TestRunSubStepsViaEngine_InheritsRootDebugProtection(t *testing.T) {
	registry := &testRegistry{executors: map[string]engine.StepExecutor{
		"tool": debugEchoSecretExecutor{},
	}}
	recorder := &nestedDebugRecorder{mutateAfter: true}
	var auditPayloads []string
	ctx := engine.WithDebugController(context.Background(), recorder)
	ctx = internaldebugprotect.WithProtection(ctx, engine.DebugProtection{
		ProtectedVars: []string{"api_secret"},
		SecretValues:  []string{"secret-value"},
	})
	ctx = engine.WithEventForwarder(ctx, func(event engine.Event) {
		if event.Kind == "debug/override_applied" {
			encoded, _ := json.Marshal(event.Payload)
			auditPayloads = append(auditPayloads, string(encoded))
		}
	})
	child := []schema.FlowNode{{Step: &schema.Step{ID: "get_incident", Type: schema.StepTypeTool}}}
	_, err := runSubStepsViaEngine(
		ctx, registry, executor.SubStepParent{ID: "inspect_primary_icm", Kind: "include"}, child,
		map[string]any{"api_secret": "secret-value"}, engine.RunModeReal,
		&noopTraceWriter{}, &noopDispatcher{}, platform.NewFakePlatform(),
		nil, nil, nil, nil, nil, nil, nil,
	)
	if err != nil {
		t.Fatalf("run child: %v", err)
	}
	if len(recorder.snapshots) != 2 {
		t.Fatalf("snapshots = %d, want before and after", len(recorder.snapshots))
	}
	for _, snapshot := range recorder.snapshots {
		encoded, _ := json.Marshal(snapshot)
		if strings.Contains(string(encoded), "secret-value") || snapshot.Vars["api_secret"] != "<redacted>" {
			t.Fatalf("nested snapshot leaked root secret: %s", encoded)
		}
	}
	if len(auditPayloads) != 1 || strings.Contains(auditPayloads[0], "secret-value") {
		t.Fatalf("nested audit leaked root secret or was missing: %#v", auditPayloads)
	}
}

func TestRunSubStepsViaEngine_ProtectsEagerChildSecrets(t *testing.T) {
	dir := t.TempDir()
	childPath := filepath.Join(dir, "child.runbook.yaml")
	child := &parserpkg.ParsedRunbook{
		Source: childPath,
		Runbook: &schema.Runbook{
			ID: "child", Name: "child",
			Inputs: map[string]*schema.Input{"api_secret": {Type: "secret"}},
			Governance: &schema.GovernanceConfig{Redact: []schema.RedactRule{{
				Pattern: "token-[a-z]+", Replace: "<redacted>",
			}}},
			Flow: []schema.FlowNode{{Step: &schema.Step{
				ID: "get_incident", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "echo"},
			}}},
		},
	}
	parent := &parserpkg.ParsedRunbook{
		Source: filepath.Join(dir, "parent.runbook.yaml"),
		Runbook: &schema.Runbook{
			ID: "parent", Name: "parent",
			Flow: []schema.FlowNode{
				{Step: &schema.Step{ID: "prelude", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "echo"}}},
				{Step: &schema.Step{
					ID: "inspect_primary_icm", Type: schema.StepTypeInclude,
					IncludeSpec: &schema.IncludeSpec{Include: schema.IncludeConfig{
						Runbook: "child.runbook.yaml", With: map[string]string{"api_secret": "Bearer ${root_token}"},
					}},
				}},
			},
		},
	}
	planner := internalplanner.New(plannerpkg.Config{
		Loader: debugRunbookLoader{runbooks: map[string]*parserpkg.ParsedRunbook{childPath: child}},
		Tools:  debugToolRegistry{}, BaseDir: dir,
		ExpandPolicy: expand.Policy{Default: expand.ModeEager},
	})
	plan, err := planner.Plan(context.Background(), parent)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	registry := &testRegistry{executors: map[string]engine.StepExecutor{}}
	runner := func(ctx context.Context, parent executor.SubStepParent, nodes []schema.FlowNode, vars map[string]any) ([]*engine.StepResult, error) {
		return runSubStepsViaEngine(
			ctx, registry, parent, nodes, vars, engine.RunModeReal,
			&noopTraceWriter{}, &noopDispatcher{}, platform.NewFakePlatform(),
			nil, nil, nil, nil, nil, nil, nil,
		)
	}
	registry.Register("include", executor.NewIncludeExecutor(&internalexpr.TemplateEvaluator{}, runner, nil))
	registry.Register("cli", debugEchoSecretExecutor{})
	recorder := &nestedDebugRecorder{}
	eng := internalengine.New(engine.EngineConfig{
		Executors: registry, Dispatcher: &noopDispatcher{}, TraceWriter: &noopTraceWriter{}, Platform: platform.NewFakePlatform(),
	})
	handle, err := eng.Start(context.Background(), plan, engine.RunOptions{
		Debugger: recorder,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	for {
		_, err = handle.Next(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
	}

	for _, snapshot := range recorder.snapshots {
		encoded, _ := json.Marshal(snapshot)
		rootToken, exists := snapshot.Vars["root_token"]
		if strings.Contains(string(encoded), "secret-value") || strings.Contains(string(encoded), "rotated-secret") || strings.Contains(string(encoded), "token-abc") || (exists && rootToken != "<redacted>") {
			t.Fatalf("eager include snapshot leaked its child's declared secret: %s", encoded)
		}
	}
	if len(recorder.snapshots) != 6 {
		t.Fatalf("snapshots = %d, want prelude, include, and child before/after", len(recorder.snapshots))
	}
}

func TestRunSubStepsViaEngine_ProtectsLazyChildSecrets(t *testing.T) {
	dir := t.TempDir()
	childPath := filepath.Join(dir, "child.runbook.yaml")
	child := `apiVersion: yawr.runbook/v1
id: child
name: child
inputs:
  api_secret:
    type: secret
governance:
  redact:
    - pattern: "token-[a-z]+"
      replace: "<redacted>"
flow:
  - step:
      id: get_incident
      type: cli
      command: echo
`
	if err := os.WriteFile(childPath, []byte(child), 0o600); err != nil {
		t.Fatalf("write child: %v", err)
	}
	parent := &parserpkg.ParsedRunbook{
		Source: filepath.Join(dir, "parent.runbook.yaml"),
		Runbook: &schema.Runbook{
			ID: "parent", Name: "parent",
			Flow: []schema.FlowNode{
				{Step: &schema.Step{ID: "prelude", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "echo"}}},
				{Step: &schema.Step{
					ID: "inspect_primary_icm", Type: schema.StepTypeInclude,
					IncludeSpec: &schema.IncludeSpec{Include: schema.IncludeConfig{
						Runbook: "child.runbook.yaml", With: map[string]string{"api_secret": "Bearer ${root_token}"},
					}},
				}},
			},
		},
	}
	planner := internalplanner.New(plannerpkg.Config{
		Loader: debugRunbookLoader{}, Tools: debugToolRegistry{}, BaseDir: dir,
		ExpandPolicy: expand.Policy{Default: expand.ModeLazy},
	})
	plan, err := planner.Plan(context.Background(), parent)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	parser, err := internalparser.New(platform.Real())
	if err != nil {
		t.Fatalf("parser.New: %v", err)
	}

	registry := &testRegistry{executors: map[string]engine.StepExecutor{}}
	runner := func(ctx context.Context, parent executor.SubStepParent, nodes []schema.FlowNode, vars map[string]any) ([]*engine.StepResult, error) {
		return runSubStepsViaEngine(
			ctx, registry, parent, nodes, vars, engine.RunModeReal,
			&noopTraceWriter{}, &noopDispatcher{}, platform.NewFakePlatform(),
			nil, nil, nil, nil, nil, nil, nil,
		)
	}
	registry.Register("include", executor.NewIncludeExecutor(&internalexpr.TemplateEvaluator{}, runner, NewParserLazyLoader(parser)))
	registry.Register("cli", debugEchoSecretExecutor{})
	recorder := &nestedDebugRecorder{}
	eng := internalengine.New(engine.EngineConfig{
		Executors: registry, Dispatcher: &noopDispatcher{}, TraceWriter: &noopTraceWriter{}, Platform: platform.NewFakePlatform(),
	})
	handle, err := eng.Start(context.Background(), plan, engine.RunOptions{
		Debugger: recorder,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	for {
		_, err = handle.Next(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
	}

	for _, snapshot := range recorder.snapshots {
		encoded, _ := json.Marshal(snapshot)
		rootToken, exists := snapshot.Vars["root_token"]
		if strings.Contains(string(encoded), "secret-value") || strings.Contains(string(encoded), "rotated-secret") || strings.Contains(string(encoded), "token-abc") || (exists && rootToken != "<redacted>") {
			t.Fatalf("lazy include snapshot leaked its child's declared secret: %s", encoded)
		}
	}
	if len(recorder.snapshots) != 6 {
		t.Fatalf("snapshots = %d, want prelude, include, and child before/after", len(recorder.snapshots))
	}
}

type debugRunbookLoader struct {
	runbooks map[string]*parserpkg.ParsedRunbook
}

func (l debugRunbookLoader) Load(_ context.Context, path string) (*parserpkg.ParsedRunbook, error) {
	if runbook := l.runbooks[filepath.Clean(path)]; runbook != nil {
		return runbook, nil
	}
	return nil, plannerpkg.ErrRunbookNotFound
}

type debugToolRegistry struct{}

func (debugToolRegistry) Lookup(context.Context, string, string) (*schema.ToolDef, error) {
	return nil, plannerpkg.ErrToolNotFound
}

type debugEchoSecretExecutor struct{}

func (debugEchoSecretExecutor) Execute(_ context.Context, step engine.ResolvedStep, vars map[string]any) (*engine.StepResult, error) {
	if step.ID == "prelude" {
		return &engine.StepResult{
			StepID: step.ID, Status: engine.StepStatusCompleted, Outcome: engine.StepOutcomeSuccess,
			Output: map[string]any{"token": "token-abc", "future": "secret-value"},
			Vars:   map[string]any{"root_token": "secret-value"},
		}, nil
	}
	return &engine.StepResult{
		StepID: step.ID, Status: engine.StepStatusCompleted, Outcome: engine.StepOutcomeSuccess,
		Output: map[string]any{"echo": map[string]string{"secret": vars["api_secret"].(string), "token": "token-abc"}},
		Vars:   map[string]any{"api_secret": "rotated-secret"},
		Error:  errors.New("child failed with secret-value"),
	}, nil
}
