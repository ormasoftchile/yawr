package routetest_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/adapter"
	internalengine "github.com/ormasoftchile/yawr/runtime/internal/engine"
	internalparser "github.com/ormasoftchile/yawr/runtime/internal/parser"
	internalplanner "github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/internal/routetest"
	internaltrace "github.com/ormasoftchile/yawr/runtime/internal/trace"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/expand"
	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	plannerpkg "github.com/ormasoftchile/yawr/runtime/pkg/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	tracepkg "github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

type fileLoader struct{ parser parser.Parser }

func (loader fileLoader) Load(ctx context.Context, path string) (*parser.ParsedRunbook, error) {
	return loader.parser.Parse(ctx, path)
}

type emptyToolRegistry struct{}

func (emptyToolRegistry) Lookup(context.Context, string, string) (*schema.ToolDef, error) {
	return nil, plannerpkg.ErrToolNotFound
}

func TestRouteTest_UsesNormalCaptureAndBranchThenStopsBeforeTarget(t *testing.T) {
	review := routetest.Review{State: "reviewed", ReviewedBy: "operator", ReviewedAt: "2026-08-28T12:05:00Z", SensitivityReviewed: true}
	scheduler, err := routetest.NewScheduler(routetest.Scenario{
		Target: routetest.Selector{CallPath: []string{"route_finding"}, Step: "dangerous_command", Phase: "before", Invocation: 1, Attempt: 1},
		HostActionResponses: []routetest.HostActionBinding{{
			At: routetest.Selector{CallPath: []string{"inspect_replication"}, Step: "open_xts_view", Phase: "execute", Invocation: 1, Attempt: 1}, Capability: "xts.open-view",
			Response: routetest.HostActionResponse{Status: "completed", Result: map[string]any{"status": "opened"}}, Review: review,
		}},
		InteractionAnswers: []routetest.InteractionBinding{{
			At: routetest.Selector{CallPath: []string{"inspect_replication", "handle_xts_launch"}, Step: "record_findings", Phase: "execute", Invocation: 1, Attempt: 1}, Kind: "collector",
			Values: map[string]any{"xts_check_primary_health": "unavailable"}, Review: review,
		}},
	})
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}

	dir := t.TempDir()
	runbookPath := filepath.Join(dir, "route-test.runbook.yaml")
	childPath := filepath.Join(dir, "inspect-replication.runbook.yaml")
	childRunbook := strings.ReplaceAll(`apiVersion: yawr.runbook/v1
id: inspect-replication
name: Inspect replication in XTS
kind: composable
flow:
	- step:
			id: open_xts_view
			type: host_action
			host_action:
				capability: xts.open-view
				request: { view_path: replicas.xts }
			capture:
				xts_check_host_status: outputs.status
				xts_check_launch_status: outputs.result.status
	- step:
			id: handle_xts_launch
			type: branch
			branches:
				- condition: xts_check_host_status == "completed" and xts_check_launch_status == "opened"
					steps:
						- step:
								id: record_findings
								type: collector
								prompt: Record findings
								fields:
									- name: xts_check_primary_health
										type: select
										label: Primary health
										required: true
										options:
											- { label: Unavailable, value: unavailable }
											- { label: Healthy, value: healthy }
				- else: true
					steps:
						- step:
								id: xts_blocked
								type: end
								outcome: { category: blocked, code: xts-not-opened }
`, "\t", "  ")
	if err := os.WriteFile(childPath, []byte(childRunbook), 0o600); err != nil {
		t.Fatalf("WriteFile child: %v", err)
	}
	runbook := strings.ReplaceAll(`apiVersion: yawr.runbook/v1
id: route-test
name: Route test
kind: mitigation
flow:
	- step:
			id: inspect_replication
			type: include
			include:
				runbook: inspect-replication.runbook.yaml
			capture:
				replication_primary_health: xts_check_primary_health
	- step:
			id: route_finding
			type: branch
			branches:
				- condition: replication_primary_health == "unavailable"
					steps:
						- step:
								id: dangerous_command
								type: cli
								run: must-not-run
				- else: true
					steps:
						- step:
								id: wrong_route
								type: end
								outcome: { category: blocked, code: wrong-route }
	- step:
			id: must_not_continue
			type: cli
			run: must-not-run-after-target
				`, "\t", "  ")
	if err := os.WriteFile(runbookPath, []byte(runbook), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	tracePath := filepath.Join(dir, "trace.jsonl")
	config, shutdown, err := adapter.BuildEngineConfig(context.Background(), adapter.WireOptions{
		Mode: "route-test", TraceFile: tracePath, RunDir: filepath.Join(dir, "runs"),
		ToolScanDir: dir, Output: io.Discard, RouteTestScheduler: scheduler,
	})
	if err != nil {
		t.Fatalf("BuildEngineConfig: %v", err)
	}
	defer shutdown()

	parserImpl, err := internalparser.New(platform.Real())
	if err != nil {
		t.Fatalf("New parser: %v", err)
	}
	parsed, err := parserImpl.Parse(context.Background(), runbookPath)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	plannerImpl := internalplanner.New(plannerpkg.Config{
		Loader: fileLoader{parser: parserImpl}, Tools: emptyToolRegistry{},
		ExpandPolicy: expand.Policy{Default: expand.ModeEager},
	})
	plan, err := plannerImpl.Plan(context.Background(), parsed)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	handle, err := internalengine.New(config).Start(context.Background(), engine.ValidatedForTest(plan), engine.RunOptions{
		Mode: engine.RunModeRouteTest, RouteTest: scheduler,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	for {
		_, err = handle.Next(context.Background())
		if err == io.EOF || errors.Is(err, context.Canceled) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
	}
	if !scheduler.TargetReached() {
		t.Fatal("target before-boundary was not reached")
	}
	state := handle.State()
	if state.Status != engine.RunStatusPausedAtBoundary {
		t.Fatalf("route-test state = %s, want paused_at_boundary", state.Status)
	}
	if state.CursorSet == nil || len(state.CursorSet.Cursors) != 1 {
		t.Fatalf("route-test cursor = %#v", state.CursorSet)
	}
	cursor := state.CursorSet.Cursors[0]
	if cursor.StepID != "dangerous_command" || cursor.Phase != engine.ExecutionPhaseBefore ||
		len(cursor.CallPath) != 1 || cursor.CallPath[0].StepID != "route_finding" {
		t.Fatalf("route-test target cursor = %#v", cursor)
	}
	traceData, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	if !strings.Contains(string(traceData), `"kind":"route_test/target_reached"`) || strings.Contains(string(traceData), `"kind":"run/cancelled"`) {
		t.Fatalf("route-test trace does not record a clean target reach: %s", traceData)
	}
	if scheduler.ExternalDispatchCount() != 0 {
		t.Fatalf("external dispatches = %d, want 0", scheduler.ExternalDispatchCount())
	}
	if err := scheduler.Verify(); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestRouteTest_NestedPausedBoundaryResumesLiveAfterProcessRestart(t *testing.T) {
	dir := t.TempDir()
	runbookPath := filepath.Join(dir, "nested-pause.runbook.yaml")
	runbook := strings.ReplaceAll(`apiVersion: yawr.runbook/v1
id: nested-pause
name: Nested pause
kind: mitigation
flow:
	- step:
			id: route
			type: branch
			branches:
				- condition: "true"
					steps:
						- step:
								id: target
								type: noop
						- step:
								id: nested_after
								type: noop
	- step:
			id: root_after
			type: noop
`, "\t", "  ")
	if err := os.WriteFile(runbookPath, []byte(runbook), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	parserImpl, err := internalparser.New(platform.Real())
	if err != nil {
		t.Fatalf("New parser: %v", err)
	}
	parsed, err := parserImpl.Parse(context.Background(), runbookPath)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	plannerImpl := internalplanner.New(plannerpkg.Config{
		Loader: fileLoader{parser: parserImpl}, Tools: emptyToolRegistry{},
		ExpandPolicy: expand.Policy{Default: expand.ModeEager},
	})
	plan, err := plannerImpl.Plan(context.Background(), parsed)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	scheduler, err := routetest.NewScheduler(routetest.Scenario{
		Target: routetest.Selector{CallPath: []string{"route"}, Step: "target", Phase: "before", Invocation: 1, Attempt: 1},
	})
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	tracePath := filepath.Join(dir, "trace.jsonl")
	runDir := filepath.Join(dir, "runs")
	firstConfig, firstShutdown, err := adapter.BuildEngineConfig(context.Background(), adapter.WireOptions{
		Mode: "route-test", TraceFile: tracePath, RunDir: runDir,
		ToolScanDir: dir, Output: io.Discard, RouteTestScheduler: scheduler,
	})
	if err != nil {
		t.Fatalf("BuildEngineConfig first: %v", err)
	}
	firstHandle, err := internalengine.New(firstConfig).Start(context.Background(), plan, engine.RunOptions{
		Mode: engine.RunModeRouteTest, RouteTest: scheduler,
	})
	if err != nil {
		firstShutdown()
		t.Fatalf("Start: %v", err)
	}
	if result, err := firstHandle.Next(context.Background()); err != io.EOF {
		state := firstHandle.State()
		firstShutdown()
		t.Fatalf("paused Next = %#v, %v target=%v state=%#v", result, err, scheduler.TargetReached(), state)
	}
	paused := firstHandle.State()
	if paused.Status != engine.RunStatusPausedAtBoundary || paused.CursorSet == nil ||
		len(paused.CursorSet.Cursors) != 1 || paused.CursorSet.Cursors[0].StepID != "target" {
		firstShutdown()
		t.Fatalf("paused state = %#v", paused)
	}
	beforeRestart, err := os.ReadFile(tracePath)
	if err != nil {
		firstShutdown()
		t.Fatalf("read trace before restart: %v", err)
	}
	if strings.Contains(string(beforeRestart), `"kind":"step/completed","run_id":"`+paused.RunID+`"`) &&
		strings.Contains(string(beforeRestart), `"step_id":"target"`) {
		firstShutdown()
		t.Fatal("target completed before the paused boundary")
	}
	firstShutdown()

	secondConfig, secondShutdown, err := adapter.BuildEngineConfig(context.Background(), adapter.WireOptions{
		Mode: "real", TraceFile: tracePath, RunDir: runDir, ToolScanDir: dir, Output: io.Discard,
	})
	if err != nil {
		t.Fatalf("BuildEngineConfig second: %v", err)
	}
	defer secondShutdown()
	resumed, err := internalengine.New(secondConfig).Resume(context.Background(), paused.RunID, engine.RunOptions{Mode: engine.RunModeReal})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	for {
		_, err = resumed.Next(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("resumed Next: %v", err)
		}
	}
	if resumed.State().Status != engine.RunStatusCompleted {
		t.Fatalf("resumed status = %s", resumed.State().Status)
	}
	afterRestart, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("read trace after restart: %v", err)
	}
	if !strings.Contains(string(afterRestart), `"step_id":"target"`) ||
		!strings.Contains(string(afterRestart), `"step_id":"nested_after"`) ||
		!strings.Contains(string(afterRestart), `"step_id":"root_after"`) {
		t.Fatalf("resumed trace does not contain the complete suffix: %s", afterRestart)
	}
}

func TestRouteTest_IncludeBranchPausedBoundaryResumesTargetExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	childPath := filepath.Join(dir, "child.runbook.yaml")
	rootPath := filepath.Join(dir, "root.runbook.yaml")
	child := strings.ReplaceAll(`apiVersion: yawr.runbook/v1
id: child
name: Child
kind: composable
flow:
	- step:
			id: choose
			type: branch
			branches:
				- condition: "true"
					steps:
						- step:
								id: target
								type: noop
						- step:
								id: child_after
								type: noop
`, "\t", "  ")
	root := strings.ReplaceAll(`apiVersion: yawr.runbook/v1
id: root
name: Root
kind: mitigation
flow:
	- step:
			id: inspect
			type: include
			include:
				runbook: child.runbook.yaml
	- step:
			id: root_after
			type: noop
`, "\t", "  ")
	if err := os.WriteFile(childPath, []byte(child), 0o600); err != nil {
		t.Fatalf("WriteFile child: %v", err)
	}
	if err := os.WriteFile(rootPath, []byte(root), 0o600); err != nil {
		t.Fatalf("WriteFile root: %v", err)
	}
	parserImpl, err := internalparser.New(platform.Real())
	if err != nil {
		t.Fatalf("New parser: %v", err)
	}
	parsed, err := parserImpl.Parse(context.Background(), rootPath)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	plan, err := internalplanner.New(plannerpkg.Config{
		Loader: fileLoader{parser: parserImpl}, Tools: emptyToolRegistry{},
		ExpandPolicy: expand.Policy{Default: expand.ModeEager},
	}).Plan(context.Background(), parsed)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	scheduler, err := routetest.NewScheduler(routetest.Scenario{
		Target: routetest.Selector{
			CallPath: []string{"inspect", "choose"}, Step: "target",
			Phase: "before", Invocation: 1, Attempt: 1,
		},
	})
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	tracePath := filepath.Join(dir, "trace.jsonl")
	runDir := filepath.Join(dir, "runs")
	firstConfig, firstShutdown, err := adapter.BuildEngineConfig(context.Background(), adapter.WireOptions{
		Mode: "route-test", TraceFile: tracePath, RunDir: runDir,
		ToolScanDir: dir, Output: io.Discard, RouteTestScheduler: scheduler,
	})
	if err != nil {
		t.Fatalf("BuildEngineConfig first: %v", err)
	}
	firstHandle, err := internalengine.New(firstConfig).Start(context.Background(), plan, engine.RunOptions{
		Mode: engine.RunModeRouteTest, RouteTest: scheduler,
	})
	if err != nil {
		firstShutdown()
		t.Fatalf("Start: %v", err)
	}
	if result, err := firstHandle.Next(context.Background()); err != io.EOF || result != nil {
		firstShutdown()
		t.Fatalf("paused Next = %#v, %v", result, err)
	}
	paused := firstHandle.State()
	if paused.Status != engine.RunStatusPausedAtBoundary || paused.CursorSet == nil ||
		len(paused.CursorSet.Cursors) != 1 {
		firstShutdown()
		t.Fatalf("paused state = %#v", paused)
	}
	cursor := paused.CursorSet.Cursors[0]
	if cursor.StepID != "target" || cursor.Phase != engine.ExecutionPhaseBefore ||
		len(cursor.CallPath) != 2 || cursor.CallPath[0].StepID != "inspect" ||
		cursor.CallPath[1].StepID != "choose" || cursor.FrameID == "" {
		firstShutdown()
		t.Fatalf("include-branch cursor = %#v", cursor)
	}
	if got := countTraceStepCompletions(t, tracePath, "target"); got != 0 {
		firstShutdown()
		t.Fatalf("target completions before restart = %d, want 0", got)
	}
	firstShutdown()

	secondConfig, secondShutdown, err := adapter.BuildEngineConfig(context.Background(), adapter.WireOptions{
		Mode: "real", TraceFile: tracePath, RunDir: runDir, ToolScanDir: dir, Output: io.Discard,
	})
	if err != nil {
		t.Fatalf("BuildEngineConfig second: %v", err)
	}
	defer secondShutdown()
	resumed, err := internalengine.New(secondConfig).Resume(context.Background(), paused.RunID, engine.RunOptions{Mode: engine.RunModeReal})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	for {
		_, err = resumed.Next(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("resumed Next: %v", err)
		}
	}
	if resumed.State().Status != engine.RunStatusCompleted {
		t.Fatalf("resumed status = %s", resumed.State().Status)
	}
	if got := countTraceStepCompletions(t, tracePath, "target"); got != 1 {
		t.Fatalf("target completions after restart = %d, want 1", got)
	}
	for _, stepID := range []string{"child_after", "root_after"} {
		if got := countTraceStepCompletions(t, tracePath, stepID); got != 1 {
			t.Fatalf("%s completions after restart = %d, want 1", stepID, got)
		}
	}
}

func TestRouteTest_MissingNestedFixtureCannotBeTolerated(t *testing.T) {
	dir := t.TempDir()
	childPath := filepath.Join(dir, "child.runbook.yaml")
	rootPath := filepath.Join(dir, "root.runbook.yaml")
	child := `apiVersion: yawr.runbook/v1
id: child
name: Child
kind: composable
flow:
  - step:
      id: unbound
      type: cli
      run: must-not-run
      continue_on_fail: true
`
	root := `apiVersion: yawr.runbook/v1
id: root
name: Root
kind: mitigation
flow:
  - step:
      id: inspect
      type: include
      include: { runbook: child.runbook.yaml }
  - step: { id: target, type: noop }
`
	if err := os.WriteFile(childPath, []byte(child), 0o600); err != nil {
		t.Fatalf("WriteFile child: %v", err)
	}
	if err := os.WriteFile(rootPath, []byte(root), 0o600); err != nil {
		t.Fatalf("WriteFile root: %v", err)
	}
	parserImpl, err := internalparser.New(platform.Real())
	if err != nil {
		t.Fatalf("New parser: %v", err)
	}
	parsed, err := parserImpl.Parse(context.Background(), rootPath)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	plan, err := internalplanner.New(plannerpkg.Config{
		Loader: fileLoader{parser: parserImpl}, Tools: emptyToolRegistry{},
		ExpandPolicy: expand.Policy{Default: expand.ModeEager},
	}).Plan(context.Background(), parsed)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	scheduler, err := routetest.NewScheduler(routetest.Scenario{
		Target: routetest.Selector{Step: "target", Phase: "before", Invocation: 1, Attempt: 1},
	})
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	config, shutdown, err := adapter.BuildEngineConfig(context.Background(), adapter.WireOptions{
		Mode: "route-test", TraceFile: filepath.Join(dir, "trace.jsonl"), RunDir: filepath.Join(dir, "runs"),
		ToolScanDir: dir, Output: io.Discard, RouteTestScheduler: scheduler,
	})
	if err != nil {
		t.Fatalf("BuildEngineConfig: %v", err)
	}
	defer shutdown()
	handle, err := internalengine.New(config).Start(context.Background(), plan, engine.RunOptions{
		Mode: engine.RunModeRouteTest, RouteTest: scheduler,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := handle.Next(context.Background()); !engine.IsRouteTestBoundaryError(err) {
		t.Fatalf("Next error = %v, want route-test boundary error", err)
	}
	if scheduler.TargetReached() || handle.State().Status != engine.RunStatusFailed {
		t.Fatalf("target=%v status=%s", scheduler.TargetReached(), handle.State().Status)
	}
}

func TestRouteTest_SecondIterationPausePreservesExactInvocationAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	runbookPath := filepath.Join(dir, "iterate-pause.runbook.yaml")
	runbook := strings.ReplaceAll(`apiVersion: yawr.runbook/v1
id: iterate-pause
name: Iterate pause
kind: mitigation
vars:
  items: "one,two"
flow:
	- iterate:
			id: loop
			over: items
			as: item
			steps:
				- step:
						id: target
						type: noop
	- step:
			id: after
			type: noop
`, "\t", "  ")
	if err := os.WriteFile(runbookPath, []byte(runbook), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	parserImpl, err := internalparser.New(platform.Real())
	if err != nil {
		t.Fatalf("New parser: %v", err)
	}
	parsed, err := parserImpl.Parse(context.Background(), runbookPath)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	plan, err := internalplanner.New(plannerpkg.Config{
		Loader: fileLoader{parser: parserImpl}, Tools: emptyToolRegistry{},
		ExpandPolicy: expand.Policy{Default: expand.ModeEager},
	}).Plan(context.Background(), parsed)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	scheduler, err := routetest.NewScheduler(routetest.Scenario{
		Target: routetest.Selector{CallPath: []string{"loop"}, Step: "target", Phase: "before", Invocation: 2, Attempt: 1},
	})
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	tracePath := filepath.Join(dir, "trace.jsonl")
	runDir := filepath.Join(dir, "runs")
	firstConfig, firstShutdown, err := adapter.BuildEngineConfig(context.Background(), adapter.WireOptions{
		Mode: "route-test", TraceFile: tracePath, RunDir: runDir,
		ToolScanDir: dir, Output: io.Discard, RouteTestScheduler: scheduler,
	})
	if err != nil {
		t.Fatalf("BuildEngineConfig first: %v", err)
	}
	firstHandle, err := internalengine.New(firstConfig).Start(context.Background(), plan, engine.RunOptions{
		Mode: engine.RunModeRouteTest, RouteTest: scheduler,
		RuntimeVars: map[string]any{"items": []any{"one", "two"}},
	})
	if err != nil {
		firstShutdown()
		t.Fatalf("Start: %v", err)
	}
	if result, err := firstHandle.Next(context.Background()); err != io.EOF {
		state := firstHandle.State()
		firstShutdown()
		t.Fatalf("paused Next = %#v, %v target=%v state=%#v", result, err, scheduler.TargetReached(), state)
	}
	paused := firstHandle.State()
	if paused.CursorSet == nil || len(paused.CursorSet.Cursors) != 1 {
		firstShutdown()
		t.Fatalf("paused cursor set = %#v", paused.CursorSet)
	}
	cursor := paused.CursorSet.Cursors[0]
	if paused.Status != engine.RunStatusPausedAtBoundary || cursor.StepID != "target" ||
		cursor.Invocation != 2 || cursor.IterationIndex != 2 || cursor.FrameID == "" {
		firstShutdown()
		t.Fatalf("second-iteration cursor = %#v", cursor)
	}
	if got := countTraceStepCompletions(t, tracePath, "target"); got != 1 {
		firstShutdown()
		t.Fatalf("target completions before restart = %d, want 1", got)
	}
	firstShutdown()

	secondConfig, secondShutdown, err := adapter.BuildEngineConfig(context.Background(), adapter.WireOptions{
		Mode: "real", TraceFile: tracePath, RunDir: runDir, ToolScanDir: dir, Output: io.Discard,
	})
	if err != nil {
		t.Fatalf("BuildEngineConfig second: %v", err)
	}
	defer secondShutdown()
	resumed, err := internalengine.New(secondConfig).Resume(context.Background(), paused.RunID, engine.RunOptions{Mode: engine.RunModeReal})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	for {
		_, err = resumed.Next(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("resumed Next: %v", err)
		}
	}
	if got := countTraceStepCompletions(t, tracePath, "target"); got != 2 {
		t.Fatalf("target completions after restart = %d, want 2", got)
	}
}

func countTraceStepCompletions(t *testing.T, path, stepID string) int {
	t.Helper()
	events, err := internaltrace.NewJSONLReader(path).ReadAll(context.Background())
	if err != nil {
		t.Fatalf("ReadAll trace: %v", err)
	}
	count := 0
	for _, event := range events {
		if event.Kind != tracepkg.EventKindStepCompleted {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("decode trace payload: %v", err)
		}
		if payload["step_id"] == stepID {
			count++
		}
	}
	return count
}

func TestRouteTest_MissingHostResponseCannotReachTarget(t *testing.T) {
	scheduler, err := routetest.NewScheduler(routetest.Scenario{
		Target: routetest.Selector{Step: "target", Phase: "before", Invocation: 1, Attempt: 1},
	})
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	dir := t.TempDir()
	config, shutdown, err := adapter.BuildEngineConfig(context.Background(), adapter.WireOptions{
		Mode: "route-test", TraceFile: filepath.Join(dir, "trace.jsonl"), RunDir: filepath.Join(dir, "runs"),
		ToolScanDir: dir, Output: io.Discard, RouteTestScheduler: scheduler,
	})
	if err != nil {
		t.Fatalf("BuildEngineConfig: %v", err)
	}
	defer shutdown()
	plan := &engine.ExecutionPlan{RunID: "missing-host", Steps: []engine.ResolvedStep{
		{ID: "host", Kind: "host_action", Spec: &schema.HostActionSpec{HostAction: schema.HostActionConfig{Capability: "test.host", Request: map[string]any{}}}},
		{ID: "target", Kind: "noop", Spec: &schema.NoopSpec{}},
	}, Metadata: engine.PlanMetadata{RunbookID: "missing-host"}}
	if err := internalplanner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	handle, err := internalengine.New(config).Start(context.Background(), plan, engine.RunOptions{Mode: engine.RunModeRouteTest, RouteTest: scheduler})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := handle.Next(context.Background()); err == nil || !strings.Contains(err.Error(), "no host action response") {
		t.Fatalf("Next error = %v, want missing response", err)
	}
	if scheduler.TargetReached() || handle.State().Status != engine.RunStatusFailed {
		t.Fatalf("target=%v status=%s", scheduler.TargetReached(), handle.State().Status)
	}
}

func TestRouteTest_InvalidHostBindingsCannotReachTarget(t *testing.T) {
	tests := []struct {
		name       string
		capability string
		hostSteps  int
		want       string
	}{
		{name: "capability mismatch", capability: "actual.capability", hostSteps: 1, want: "capability"},
		{name: "response reuse", capability: "expected.capability", hostSteps: 2, want: "no host action response"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			review := routetest.Review{State: "reviewed", ReviewedBy: "operator", ReviewedAt: "2026-08-28T12:05:00Z", SensitivityReviewed: true}
			scheduler, err := routetest.NewScheduler(routetest.Scenario{
				Target: routetest.Selector{Step: "target", Phase: "before", Invocation: 1, Attempt: 1},
				HostActionResponses: []routetest.HostActionBinding{{
					At:         routetest.Selector{Step: "host", Phase: "execute", Invocation: 1, Attempt: 1},
					Capability: "expected.capability", Response: routetest.HostActionResponse{Status: "completed", Result: map[string]any{"status": "ok"}}, Review: review,
				}},
			})
			if err != nil {
				t.Fatalf("NewScheduler: %v", err)
			}
			dir := t.TempDir()
			config, shutdown, err := adapter.BuildEngineConfig(context.Background(), adapter.WireOptions{
				Mode: "route-test", TraceFile: filepath.Join(dir, "trace.jsonl"), RunDir: filepath.Join(dir, "runs"), ToolScanDir: dir, Output: io.Discard, RouteTestScheduler: scheduler,
			})
			if err != nil {
				t.Fatalf("BuildEngineConfig: %v", err)
			}
			defer shutdown()
			steps := make([]engine.ResolvedStep, 0, test.hostSteps+1)
			for index := 0; index < test.hostSteps; index++ {
				steps = append(steps, engine.ResolvedStep{ID: "host", Kind: "host_action", Spec: &schema.HostActionSpec{HostAction: schema.HostActionConfig{Capability: test.capability, Request: map[string]any{}}}})
			}
			steps = append(steps, engine.ResolvedStep{ID: "target", Kind: "noop", Spec: &schema.NoopSpec{}})
			plan := &engine.ExecutionPlan{RunID: "bad-host", Steps: steps, Metadata: engine.PlanMetadata{RunbookID: "bad-host"}}
			if err := internalplanner.ValidateExecutionPlan(plan); err != nil {
				t.Fatalf("ValidateExecutionPlan: %v", err)
			}
			handle, err := internalengine.New(config).Start(context.Background(), plan, engine.RunOptions{Mode: engine.RunModeRouteTest, RouteTest: scheduler})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			var runErr error
			for runErr == nil {
				_, runErr = handle.Next(context.Background())
			}
			if !strings.Contains(runErr.Error(), test.want) {
				t.Fatalf("error = %v, want %q", runErr, test.want)
			}
			if scheduler.TargetReached() || handle.State().Status != engine.RunStatusFailed {
				t.Fatalf("target=%v status=%s", scheduler.TargetReached(), handle.State().Status)
			}
		})
	}
}
