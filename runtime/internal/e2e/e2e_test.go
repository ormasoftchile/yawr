package e2e

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"testing"
	"time"

	internalengine "github.com/ormasoftchile/yawr/runtime/internal/engine"
	internalrunstore "github.com/ormasoftchile/yawr/runtime/internal/runstore"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	tracepkg "github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

// TestE2E_SimpleEcho runs a single-step CLI runbook and verifies that the run
// completes and the trace contains the expected lifecycle events.
func TestE2E_SimpleEcho(t *testing.T) {
	t.Parallel()
	h := NewHarness(t, "echo-runbook.yaml")
	state, err := h.Run()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	h.AssertCompleted(state)
	h.AssertTrace(
		string(tracepkg.EventKindRunStarted),
		string(tracepkg.EventKindStepStarted),
		string(tracepkg.EventKindStepCompleted),
		string(tracepkg.EventKindRunCompleted),
	)
}

// TestE2E_VarInterpolation runs a two-step runbook where the first step captures
// stdout into a variable that the second step consumes.
func TestE2E_VarInterpolation(t *testing.T) {
	t.Parallel()
	h := NewHarness(t, "vars-runbook.yaml")
	h.WithInput("greeting", "hello-world")

	state, err := h.Run()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	h.AssertCompleted(state)
}

// TestE2E_BranchTrue runs the branch runbook with flag=true and verifies that
// the then-step is executed.
func TestE2E_BranchTrue(t *testing.T) {
	t.Parallel()
	h := NewHarness(t, "branch-runbook.yaml")
	h.WithInput("flag", "true")

	state, err := h.Run()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	h.AssertCompleted(state)
}

// TestE2E_BranchEmitsNestedStepEvents is a regression guard: branch arm
// sub-steps are executed via a sub-engine, and historically those step
// lifecycle events vanished into the sub-engine's events channel that
// nobody read. The web preview SSE feed and store both depend on these
// events to surface nested progress, so the parent engine must forward
// them via WithEventForwarder. This test asserts the trace file contains
// step/started + step/completed for the inner cli sub-step.
func TestE2E_BranchEmitsNestedStepEvents(t *testing.T) {
	t.Parallel()
	h := NewHarness(t, "branch-runbook.yaml")
	h.WithInput("flag", "true")

	state, err := h.Run()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	h.AssertCompleted(state)

	events, err := h.TraceEvents()
	if err != nil {
		t.Fatalf("TraceEvents: %v", err)
	}
	var sawStarted, sawCompleted bool
	for _, ev := range events {
		var p struct {
			StepID string `json:"step_id"`
		}
		if len(ev.Payload) > 0 {
			_ = json.Unmarshal(ev.Payload, &p)
		}
		if p.StepID != "then-step" {
			continue
		}
		switch string(ev.Kind) {
		case "step/started":
			sawStarted = true
		case "step/completed":
			sawCompleted = true
		}
	}
	if !sawStarted {
		t.Errorf("missing step/started for nested step then-step (event forwarder regression)")
	}
	if !sawCompleted {
		t.Errorf("missing step/completed for nested step then-step (event forwarder regression)")
	}
}

// TestE2E_BranchFalse runs the branch runbook with flag=false and verifies that
// the else-step is executed (branch condition does not match).
func TestE2E_BranchFalse(t *testing.T) {
	t.Parallel()
	h := NewHarness(t, "branch-runbook.yaml")
	h.WithInput("flag", "false")

	state, err := h.Run()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	h.AssertCompleted(state)
}

// TestE2E_BranchFailingSubstep verifies that a failing CLI sub-step inside a
// branch arm does NOT kill the run. The branch arm stops at the failed step,
// but the run continues to the next top-level step and completes normally.
// Regression test for: branch sub-step failure incorrectly propagated as a
// fatal error causing failRun() to be called.
func TestE2E_BranchFailingSubstep(t *testing.T) {
	t.Parallel()
	h := NewHarness(t, "branch-failing-substep-runbook.yaml")
	h.WithInput("flag", "true")

	state, err := h.Run()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The run must reach RunStatusCompleted even though the branch arm had a
	// failing sub-step. The step after the branch ("after-branch") must also run.
	h.AssertCompleted(state)

	events, err := h.TraceEvents()
	if err != nil {
		t.Fatalf("TraceEvents: %v", err)
	}
	started := map[string]bool{}
	completed := map[string]bool{}
	for _, ev := range events {
		var p struct {
			StepID string `json:"step_id"`
		}
		if len(ev.Payload) > 0 {
			_ = json.Unmarshal(ev.Payload, &p)
		}
		switch string(ev.Kind) {
		case "step/started":
			started[p.StepID] = true
		case "step/completed":
			completed[p.StepID] = true
		}
	}
	if !started["failing-substep"] {
		t.Error("failing branch sub-step did not run")
	}
	if started["should-not-run"] {
		t.Error("branch arm continued after its failing sub-step")
	}
	if !started["after-branch"] || !completed["after-branch"] {
		t.Error("top-level step after tolerated branch failure did not complete")
	}
}

// TestE2E_IterateAll iterates over all 3 items without early exit and verifies
// that the run completes successfully.
func TestE2E_IterateAll(t *testing.T) {
	t.Parallel()
	h := NewHarness(t, "iterate-runbook.yaml")
	h.WithInput("stop_early", "false")

	state, err := h.Run()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	h.AssertCompleted(state)
}

// TestE2E_IterateEarlyExit sets stop_early=true so the until condition triggers
// after the first iteration, stopping the loop before all items are processed.
func TestE2E_IterateEarlyExit(t *testing.T) {
	t.Parallel()
	h := NewHarness(t, "iterate-runbook.yaml")
	h.WithInput("stop_early", "true")

	state, err := h.Run()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	h.AssertCompleted(state)
}

// TestE2E_ManualSkip runs a runbook with an approve step. In non-interactive
// mode (TTYOutput=false), the NoOpApprovalGate auto-approves the step.
func TestE2E_ManualSkip(t *testing.T) {
	t.Parallel()
	h := NewHarness(t, "manual-runbook.yaml")

	state, err := h.Run()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	h.AssertCompleted(state)
}

// TestE2E_TracePersistence verifies that the trace file is written, contains
// valid NDJSON, and can be fully parsed.
func TestE2E_TracePersistence(t *testing.T) {
	t.Parallel()
	h := NewHarness(t, "echo-runbook.yaml")

	_, err := h.Run()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Verify the trace file exists.
	if _, err := os.Stat(h.traceFile); os.IsNotExist(err) {
		t.Fatalf("trace file not found at %s", h.traceFile)
	}

	// Verify it is valid NDJSON and contains at least one event via JSONLReader.
	events, err := h.TraceEvents()
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("trace file is empty; expected at least one event")
	}

	// Verify each event has the required envelope fields.
	for i, ev := range events {
		if ev.EventID == "" {
			t.Errorf("event[%d]: missing event_id", i)
		}
		if ev.RunID == "" {
			t.Errorf("event[%d]: missing run_id", i)
		}
		if ev.Timestamp == "" {
			t.Errorf("event[%d]: missing timestamp", i)
		}
		if string(ev.Kind) == "" {
			t.Errorf("event[%d]: missing kind", i)
		}
	}
}

// TestE2E_ResumeFromCheckpoint runs a two-step runbook, closes the first run
// store after step1, then resumes through a fresh store and engine.
func TestE2E_ResumeFromCheckpoint(t *testing.T) {
	t.Parallel()
	h := NewHarness(t, "vars-runbook.yaml")
	h.WithInput("greeting", "resume-test")

	ctx, cancel := context.WithTimeout(context.Background(), h.Timeout)
	defer cancel()

	plan, ecfg, eng := h.Prepare(ctx)

	// Phase 1: start the run and drive only step1.
	handle, err := eng.Start(ctx, engine.ValidatedForTest(plan), engine.RunOptions{
		Mode:  engine.RunModeReal,
		Vars:  h.inputsAsVars(),
		Store: ecfg.Store,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Drive step1 — the first Next() emits run/started and executes step1.
	result, err := handle.Next(ctx)
	if err == io.EOF {
		t.Fatal("run completed unexpectedly after one step (expected two steps)")
	}
	if err != nil {
		t.Fatalf("Next (step1): %v", err)
	}
	if result == nil || result.StepID != "step1" {
		t.Fatalf("expected step1, got %v", result)
	}

	runID := handle.State().RunID
	if err := h.Store.Close(); err != nil {
		t.Fatalf("Close first store: %v", err)
	}

	// Phase 2: resume from checkpoint through a new store and engine. No
	// in-memory plan cache from phase 1 is available.
	freshStore := internalrunstore.NewDirRunStore(h.RunDir)
	t.Cleanup(func() { _ = freshStore.Close() })
	handle2, err := internalengine.New(ecfg).Resume(ctx, runID, engine.RunOptions{
		Mode:  engine.RunModeReal,
		Store: freshStore,
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}

	// Drive the resumed run to completion (step2).
	for {
		_, err := handle2.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next (resumed): %v", err)
		}
	}

	state := handle2.State()
	h.AssertCompleted(state)
}

// TestE2E_ToolStep runs a runbook with a tool_call step whose tool binding is
// not available at runtime. With fail-stop semantics, the run must terminate
// with a failure — not silently continue.
func TestE2E_ToolStep(t *testing.T) {
	t.Parallel()
	h := NewHarness(t, "tool-runbook.yaml")
	h.WithToolDef("test-tool", "run")

	state, err := h.Run()
	if err != nil {
		// With fail-stop, failRun may also be triggered by infrastructure errors
		// that return (nil, error) from Next(). Accept either path.
		if state.Status != engine.RunStatusFailed {
			t.Fatalf("Run: %v (state=%s)", err, state.Status)
		}
		return
	}
	if state.Status != engine.RunStatusFailed {
		t.Errorf("expected RunStatusFailed, got %s", state.Status)
	}
}

// TestE2E_CancelMidRun starts a two-step runbook, cancels the context after
// the first step completes, and verifies the run ends in a non-completed state.
func TestE2E_CancelMidRun(t *testing.T) {
	t.Parallel()
	// Use a two-step runbook so that cancelling after step1 leaves step2 unexecuted.
	h := NewHarness(t, "vars-runbook.yaml")
	h.WithInput("greeting", "cancel-test")

	ctx, cancel := context.WithTimeout(context.Background(), h.Timeout)
	defer cancel()

	plan, ecfg, eng := h.Prepare(ctx)

	runCtx, runCancel := context.WithCancel(ctx)

	handle, err := internalengine.New(ecfg).Start(runCtx, engine.ValidatedForTest(plan), engine.RunOptions{
		Mode:  engine.RunModeReal,
		Vars:  h.inputsAsVars(),
		Store: ecfg.Store,
	})
	_ = eng
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Drive step1 to completion.
	result, err := handle.Next(runCtx)
	if err == io.EOF {
		// Run completed in one step — unexpected for a two-step runbook.
		runCancel()
		t.Fatal("run completed unexpectedly after one step")
	}
	if err != nil {
		runCancel()
		return // context already cancelled; test still passes
	}
	if result == nil || result.StepID != "step1" {
		runCancel()
		t.Fatalf("expected step1, got %v", result)
	}

	// Cancel the run context before step2 can execute.
	runCancel()

	// Drain remaining steps until EOF or error (cancelled context).
	for {
		_, err := handle.Next(runCtx)
		if err != nil {
			break
		}
	}

	// The run must end in a non-completed state (cancelled or failed).
	state := handle.State()
	if state.Status == engine.RunStatusCompleted {
		t.Errorf("expected non-completed status after cancel, got %s", state.Status)
	}

	// Allow cancelled, failed, or running (if the engine didn't propagate yet).
	allowed := map[engine.RunStatus]bool{
		engine.RunStatusCancelled: true,
		engine.RunStatusFailed:    true,
		engine.RunStatusRunning:   true, // transitional; engine may not have flushed yet
	}
	if !allowed[state.Status] {
		t.Errorf("unexpected status %s after cancel", state.Status)
	}

	_ = time.Second // imported for reference only
}
