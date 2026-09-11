package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	internaldebugprotect "github.com/ormasoftchile/yawr/runtime/internal/debugprotect"
	enginepkg "github.com/ormasoftchile/yawr/runtime/pkg/engine"
	gcpparser "github.com/ormasoftchile/yawr/runtime/pkg/gcp/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/governance"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/testutil"
	"github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

type debugControllerFunc func(context.Context, enginepkg.DebugSnapshot) (enginepkg.DebugDecision, error)

func (f debugControllerFunc) Pause(ctx context.Context, snapshot enginepkg.DebugSnapshot) (enginepkg.DebugDecision, error) {
	return f(ctx, snapshot)
}

func TestDebugController_BeforePatchChangesExecutorVariables(t *testing.T) {
	registry := newFakeExecutorRegistry()
	registry.Register("cli", stepExecutorFunc(func(_ context.Context, step enginepkg.ResolvedStep, vars map[string]any) (*enginepkg.StepResult, error) {
		return &enginepkg.StepResult{
			StepID:  step.ID,
			Status:  enginepkg.StepStatusCompleted,
			Outcome: enginepkg.StepOutcomeSuccess,
			Output:  map[string]any{"observed_status": vars["incident_status"]},
		}, nil
	}))

	cfg := makeTestConfig()
	cfg.Executors = registry
	debugger := debugControllerFunc(func(_ context.Context, snapshot enginepkg.DebugSnapshot) (enginepkg.DebugDecision, error) {
		if snapshot.Phase != enginepkg.DebugPhaseBefore {
			return enginepkg.DebugDecision{}, nil
		}
		if got := snapshot.Vars["incident_status"]; got != "Mitigated" {
			t.Fatalf("before snapshot incident_status = %v, want Mitigated", got)
		}
		if snapshot.Actual != nil {
			t.Fatal("before snapshot must not contain an actual result")
		}
		return enginepkg.DebugDecision{
			Action: enginepkg.DebugActionContinue,
			Vars:   map[string]any{"incident_status": "Active"},
		}, nil
	})

	handle, err := New(cfg).Start(
		context.Background(),
		enginepkg.ValidatedForTest(makeTestPlan(enginepkg.ResolvedStep{ID: "get_icm", Kind: "cli", Spec: &cliStepSpec{}})),
		enginepkg.RunOptions{Vars: map[string]string{"incident_status": "Mitigated"}, Debugger: debugger},
	)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	result, err := handle.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if got := result.Output["observed_status"]; got != "Active" {
		t.Fatalf("executor observed incident_status = %v, want Active", got)
	}
	if got := handle.State().Vars["incident_status"]; got != "Active" {
		t.Fatalf("committed incident_status = %v, want Active", got)
	}
}

func TestDebugController_ContextDoesNotExposeEngineValues(t *testing.T) {
	type sensitiveContextKey struct{}
	registry := newFakeExecutorRegistry()
	registry.Register("cli", stepExecutorFunc(func(_ context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
		return &enginepkg.StepResult{StepID: step.ID, Status: enginepkg.StepStatusCompleted, Outcome: enginepkg.StepOutcomeSuccess}, nil
	}))

	var observedRunID string
	var observedCallerValue any
	var observedProtection enginepkg.DebugProtection
	debugger := debugControllerFunc(func(ctx context.Context, _ enginepkg.DebugSnapshot) (enginepkg.DebugDecision, error) {
		observedRunID = enginepkg.RunIDFromContext(ctx)
		observedCallerValue = ctx.Value(sensitiveContextKey{})
		observedProtection = internaldebugprotect.ProtectionFromContext(ctx)
		return enginepkg.DebugDecision{Action: enginepkg.DebugActionContinue}, nil
	})
	ctx := context.WithValue(context.Background(), sensitiveContextKey{}, "caller-secret")
	ctx = internaldebugprotect.WithProtection(ctx, enginepkg.DebugProtection{
		ProtectedVars: []string{"api_secret"}, SecretValues: []string{"secret-value"},
	})
	handle, err := New(makeTestConfig()).Start(
		ctx,
		enginepkg.ValidatedForTest(makeTestPlan(enginepkg.ResolvedStep{ID: "get_icm", Kind: "cli", Spec: &cliStepSpec{}})),
		enginepkg.RunOptions{Debugger: debugger},
	)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := handle.Next(context.Background()); err != nil {
		t.Fatalf("Next: %v", err)
	}
	if observedRunID == "" {
		t.Fatal("debugger context did not preserve the run ID")
	}
	if observedCallerValue != nil {
		t.Fatalf("debugger context exposed caller value: %v", observedCallerValue)
	}
	if len(observedProtection.SecretValues) != 0 || len(observedProtection.ProtectedVars) != 0 {
		t.Fatalf("debugger context exposed protection internals: %#v", observedProtection)
	}
}

func TestDebugController_PreservesStartAndNextDeadlines(t *testing.T) {
	registry := newFakeExecutorRegistry()
	registry.Register("cli", stepExecutorFunc(func(_ context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
		return &enginepkg.StepResult{StepID: step.ID, Status: enginepkg.StepStatusCompleted, Outcome: enginepkg.StepOutcomeSuccess}, nil
	}))
	cfg := makeTestConfig()
	cfg.Executors = registry

	t.Run("start deadline", func(t *testing.T) {
		startDeadline := time.Now().Add(time.Minute)
		startCtx, cancel := context.WithDeadline(context.Background(), startDeadline)
		defer cancel()
		debugger := debugControllerFunc(func(ctx context.Context, _ enginepkg.DebugSnapshot) (enginepkg.DebugDecision, error) {
			observed, ok := ctx.Deadline()
			if !ok || !observed.Equal(startDeadline) {
				return enginepkg.DebugDecision{}, fmt.Errorf("start deadline = %v/%v, want %v", observed, ok, startDeadline)
			}
			return enginepkg.DebugDecision{Action: enginepkg.DebugActionContinue}, nil
		})
		handle, err := New(cfg).Start(
			startCtx,
			enginepkg.ValidatedForTest(makeTestPlan(enginepkg.ResolvedStep{ID: "get_icm", Kind: "cli", Spec: &cliStepSpec{}})),
			enginepkg.RunOptions{Debugger: debugger},
		)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		if _, err := handle.Next(context.Background()); err != nil {
			t.Fatalf("Next: %v", err)
		}
	})

	t.Run("next deadline", func(t *testing.T) {
		debugger := debugControllerFunc(func(ctx context.Context, _ enginepkg.DebugSnapshot) (enginepkg.DebugDecision, error) {
			<-ctx.Done()
			return enginepkg.DebugDecision{}, ctx.Err()
		})
		handle, err := New(cfg).Start(
			context.Background(),
			enginepkg.ValidatedForTest(makeTestPlan(enginepkg.ResolvedStep{ID: "get_icm", Kind: "cli", Spec: &cliStepSpec{}})),
			enginepkg.RunOptions{Debugger: debugger},
		)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		nextCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		if _, err := handle.Next(nextCtx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Next error = %v, want deadline exceeded", err)
		}
	})
}

func TestDebugController_PauseCancellationTerminalizesRun(t *testing.T) {
	for _, phase := range []enginepkg.DebugPhase{enginepkg.DebugPhaseBefore, enginepkg.DebugPhaseAfter} {
		t.Run(string(phase), func(t *testing.T) {
			executions := 0
			registry := newFakeExecutorRegistry()
			registry.Register("cli", stepExecutorFunc(func(_ context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
				executions++
				return &enginepkg.StepResult{StepID: step.ID, Status: enginepkg.StepStatusCompleted, Outcome: enginepkg.StepOutcomeSuccess}, nil
			}))
			debugger := debugControllerFunc(func(ctx context.Context, snapshot enginepkg.DebugSnapshot) (enginepkg.DebugDecision, error) {
				if snapshot.Phase != phase {
					return enginepkg.DebugDecision{Action: enginepkg.DebugActionContinue}, nil
				}
				<-ctx.Done()
				return enginepkg.DebugDecision{}, ctx.Err()
			})
			cfg := makeTestConfig()
			cfg.Executors = registry
			handle, err := New(cfg).Start(
				context.Background(),
				enginepkg.ValidatedForTest(makeTestPlan(enginepkg.ResolvedStep{ID: "get_icm", Kind: "cli", Spec: &cliStepSpec{}})),
				enginepkg.RunOptions{Debugger: debugger},
			)
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			nextCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			if _, err := handle.Next(nextCtx); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Next error = %v, want deadline exceeded", err)
			}
			wantExecutions := 0
			if phase == enginepkg.DebugPhaseAfter {
				wantExecutions = 1
			}
			if executions != wantExecutions {
				t.Fatalf("executions = %d, want %d", executions, wantExecutions)
			}
			if status := handle.State().Status; status != enginepkg.RunStatusCancelled {
				t.Fatalf("run status = %s, want cancelled", status)
			}
			if _, err := handle.Next(context.Background()); err != io.EOF {
				t.Fatalf("second Next error = %v, want EOF", err)
			}
			if executions != wantExecutions {
				t.Fatalf("second Next changed executions to %d", executions)
			}
		})
	}
}

func TestDebugController_ContinueAfterPauseExpiryCancelsRun(t *testing.T) {
	for _, phase := range []enginepkg.DebugPhase{enginepkg.DebugPhaseBefore, enginepkg.DebugPhaseAfter} {
		t.Run(string(phase), func(t *testing.T) {
			executions := 0
			registry := newFakeExecutorRegistry()
			registry.Register("cli", stepExecutorFunc(func(_ context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
				executions++
				return &enginepkg.StepResult{StepID: step.ID, Status: enginepkg.StepStatusCompleted, Outcome: enginepkg.StepOutcomeSuccess}, nil
			}))
			debugger := debugControllerFunc(func(ctx context.Context, snapshot enginepkg.DebugSnapshot) (enginepkg.DebugDecision, error) {
				if snapshot.Phase == phase {
					<-ctx.Done()
				}
				return enginepkg.DebugDecision{Action: enginepkg.DebugActionContinue}, nil
			})
			cfg := makeTestConfig()
			cfg.Executors = registry
			handle, err := New(cfg).Start(
				context.Background(),
				enginepkg.ValidatedForTest(makeTestPlan(enginepkg.ResolvedStep{ID: "get_icm", Kind: "cli", Spec: &cliStepSpec{}})),
				enginepkg.RunOptions{Debugger: debugger},
			)
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			nextCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			if _, err := handle.Next(nextCtx); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Next error = %v, want deadline exceeded", err)
			}
			wantExecutions := 0
			if phase == enginepkg.DebugPhaseAfter {
				wantExecutions = 1
			}
			if executions != wantExecutions {
				t.Fatalf("executions = %d, want %d", executions, wantExecutions)
			}
			if status := handle.State().Status; status != enginepkg.RunStatusCancelled {
				t.Fatalf("run status = %s, want cancelled", status)
			}
		})
	}
}

func TestDebugController_ResumeFailsClosed(t *testing.T) {
	_, err := New(makeTestConfig()).Resume(
		context.Background(), "run-1",
		enginepkg.RunOptions{Debugger: debugControllerFunc(func(context.Context, enginepkg.DebugSnapshot) (enginepkg.DebugDecision, error) {
			return enginepkg.DebugDecision{Action: enginepkg.DebugActionContinue}, nil
		})},
	)
	if !errors.Is(err, enginepkg.ErrDebugResumeUnsupported) {
		t.Fatalf("Resume error = %v, want ErrDebugResumeUnsupported", err)
	}
}

func TestDebugController_DynamicIncludeProtectionIsRetained(t *testing.T) {
	registry := newFakeExecutorRegistry()
	registry.Register("cli", stepExecutorFunc(func(_ context.Context, step enginepkg.ResolvedStep, vars map[string]any) (*enginepkg.StepResult, error) {
		return &enginepkg.StepResult{
			StepID: step.ID, Status: enginepkg.StepStatusCompleted, Outcome: enginepkg.StepOutcomeSuccess,
			Output: map[string]any{"echo": vars["api_secret"]},
		}, nil
	}))
	registry.Register("include", stepExecutorFunc(func(ctx context.Context, step enginepkg.ResolvedStep, vars map[string]any) (*enginepkg.StepResult, error) {
		internaldebugprotect.PublishProtection(ctx, enginepkg.DebugProtection{
			ProtectedVars: []string{"api_secret"}, SecretValues: []string{"secret-value"},
			RedactionPatterns: []*governance.RedactionPattern{{Pattern: "token-[a-z]+", Replacement: "<redacted>"}},
		})
		return &enginepkg.StepResult{
			StepID: step.ID, Status: enginepkg.StepStatusCompleted, Outcome: enginepkg.StepOutcomeSuccess,
			Output: map[string]any{"secret": vars["api_secret"], "token": "token-abc"},
			Vars:   map[string]any{"api_secret": "rotated-secret", "dynamic_value": "dynamic-secret"},
		}, nil
	}))

	var snapshots []enginepkg.DebugSnapshot
	debugger := debugControllerFunc(func(_ context.Context, snapshot enginepkg.DebugSnapshot) (enginepkg.DebugDecision, error) {
		snapshots = append(snapshots, snapshot)
		return enginepkg.DebugDecision{Action: enginepkg.DebugActionContinue}, nil
	})
	step := enginepkg.ResolvedStep{
		ID: "dynamic_child", Kind: "include",
		Spec: &schema.IncludeSpec{Include: schema.IncludeConfig{
			RunbookRef: "${child_ref}", ResolveFrom: schema.ResolveFromCatalog,
		}},
	}
	cfg := makeTestConfig()
	cfg.Executors = registry
	handle, err := New(cfg).Start(
		context.Background(), enginepkg.ValidatedForTest(makeTestPlan(
			enginepkg.ResolvedStep{ID: "prelude", Kind: "cli", Spec: &cliStepSpec{}}, step,
		)),
		enginepkg.RunOptions{Vars: map[string]string{"api_secret": "secret-value", "child_ref": "pkg/child"}, Debugger: debugger},
	)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := handle.Next(context.Background()); err != nil {
		t.Fatalf("prelude Next: %v", err)
	}
	if _, err := handle.Next(context.Background()); err != nil {
		t.Fatalf("include Next: %v", err)
	}
	if len(snapshots) != 4 {
		t.Fatalf("snapshots = %d, want prelude and include before/after", len(snapshots))
	}
	for _, snapshot := range snapshots {
		encoded, _ := json.Marshal(snapshot)
		if strings.Contains(string(encoded), "secret-value") || strings.Contains(string(encoded), "rotated-secret") || strings.Contains(string(encoded), "dynamic-secret") || strings.Contains(string(encoded), "token-abc") || snapshot.Vars["api_secret"] != "<redacted>" {
			t.Fatalf("dynamic include snapshot leaked resolved child protection: %s", encoded)
		}
	}
	last := snapshots[len(snapshots)-1]
	if !last.OutputProtected || last.Actual.Output["secret"] != "<redacted>" || last.Actual.Output["token"] != "<redacted>" {
		t.Fatalf("after snapshot was not protected: %#v", last)
	}
}

func TestDebugController_PendingProtectedCaptureRedactsActualOutput(t *testing.T) {
	registry := newFakeExecutorRegistry()
	registry.Register("cli", stepExecutorFunc(func(_ context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
		return &enginepkg.StepResult{
			StepID: step.ID, Status: enginepkg.StepStatusCompleted, Outcome: enginepkg.StepOutcomeSuccess,
			Output: map[string]any{"credential": "future-secret"}, Vars: map[string]any{},
		}, nil
	}))
	step := enginepkg.ResolvedStep{
		ID: "produce_secret", Kind: "cli", Spec: &cliStepSpec{},
		Capture: map[string]string{"root_token": "outputs.credential"},
	}
	var after enginepkg.DebugSnapshot
	debugger := debugControllerFunc(func(_ context.Context, snapshot enginepkg.DebugSnapshot) (enginepkg.DebugDecision, error) {
		if snapshot.Phase == enginepkg.DebugPhaseAfter {
			after = snapshot
		}
		return enginepkg.DebugDecision{Action: enginepkg.DebugActionContinue}, nil
	})
	ctx := internaldebugprotect.WithProtection(context.Background(), enginepkg.DebugProtection{ProtectedVars: []string{"root_token"}})
	cfg := makeTestConfig()
	cfg.Executors = registry
	handle, err := New(cfg).Start(
		ctx, enginepkg.ValidatedForTest(makeTestPlan(step)), enginepkg.RunOptions{Debugger: debugger},
	)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := handle.Next(context.Background()); err != nil {
		t.Fatalf("Next: %v", err)
	}
	if after.Actual == nil || after.Actual.Output["credential"] != "<redacted>" || !after.OutputProtected {
		t.Fatalf("pending protected capture leaked output: %#v", after)
	}
}

func TestDebugController_StructuredProtectedCapturePromotesProtectAll(t *testing.T) {
	registry := newFakeExecutorRegistry()
	registry.Register("cli", stepExecutorFunc(func(_ context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
		result := &enginepkg.StepResult{
			StepID: step.ID, Status: enginepkg.StepStatusCompleted, Outcome: enginepkg.StepOutcomeSuccess,
		}
		if step.ID == "produce_secret" {
			result.Output = map[string]any{"credential": map[string]any{"plaintext": "structured-secret"}}
			result.Vars = map[string]any{"root_token": map[string]any{"plaintext": "structured-secret"}}
		}
		return result, nil
	}))
	steps := []enginepkg.ResolvedStep{
		{ID: "produce_secret", Kind: "cli", Spec: &cliStepSpec{}, Capture: map[string]string{"root_token": "outputs.credential"}},
		{ID: "after_secret", Kind: "cli", Spec: &cliStepSpec{}},
	}
	var snapshots []enginepkg.DebugSnapshot
	debugger := debugControllerFunc(func(_ context.Context, snapshot enginepkg.DebugSnapshot) (enginepkg.DebugDecision, error) {
		snapshots = append(snapshots, snapshot)
		return enginepkg.DebugDecision{Action: enginepkg.DebugActionContinue}, nil
	})
	ctx := internaldebugprotect.WithProtection(context.Background(), enginepkg.DebugProtection{ProtectedVars: []string{"root_token"}})
	cfg := makeTestConfig()
	cfg.Executors = registry
	handle, err := New(cfg).Start(
		ctx, enginepkg.ValidatedForTest(makeTestPlan(steps...)),
		enginepkg.RunOptions{Vars: map[string]string{"public": "visible"}, Debugger: debugger},
	)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := handle.Next(context.Background()); err != nil {
		t.Fatalf("produce Next: %v", err)
	}
	if _, err := handle.Next(context.Background()); err != nil {
		t.Fatalf("after Next: %v", err)
	}
	if len(snapshots) != 4 {
		t.Fatalf("snapshots = %d, want 4", len(snapshots))
	}
	for index, snapshot := range snapshots[1:] {
		encoded, _ := json.Marshal(snapshot)
		if strings.Contains(string(encoded), "structured-secret") || snapshot.Vars["public"] != "<redacted>" {
			t.Fatalf("snapshot %d did not retain protect-all mode: %s", index+1, encoded)
		}
	}
}

func TestDebugController_FailClosedProtectionPersistsIntoAudit(t *testing.T) {
	traceWriter := &fakeTraceWriter{}
	registry := newFakeExecutorRegistry()
	registry.Register("cli", stepExecutorFunc(func(_ context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
		return &enginepkg.StepResult{
			StepID: step.ID, Status: enginepkg.StepStatusCompleted, Outcome: enginepkg.StepOutcomeSuccess,
			Output: map[string]any{"credential": map[string]any{"plaintext": "audit-secret"}}, Vars: map[string]any{},
		}, nil
	}))
	step := enginepkg.ResolvedStep{
		ID: "produce_secret", Kind: "cli", Spec: &cliStepSpec{},
		Capture: map[string]string{"root_token": "outputs.credential"},
	}
	debugger := debugControllerFunc(func(_ context.Context, snapshot enginepkg.DebugSnapshot) (enginepkg.DebugDecision, error) {
		if snapshot.Phase != enginepkg.DebugPhaseAfter {
			return enginepkg.DebugDecision{Action: enginepkg.DebugActionContinue}, nil
		}
		return enginepkg.DebugDecision{
			Action: enginepkg.DebugActionContinue,
			Result: &enginepkg.DebugResultOverride{Status: enginepkg.StepStatusSkipped},
		}, nil
	})
	ctx := internaldebugprotect.WithProtection(context.Background(), enginepkg.DebugProtection{ProtectedVars: []string{"root_token"}})
	cfg := makeTestConfig()
	cfg.Executors = registry
	cfg.TraceWriter = traceWriter
	handle, err := New(cfg).Start(
		ctx, enginepkg.ValidatedForTest(makeTestPlan(step)), enginepkg.RunOptions{Debugger: debugger},
	)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := handle.Next(context.Background()); err != nil {
		t.Fatalf("Next: %v", err)
	}
	for _, event := range traceWriter.collect() {
		if event.Kind != trace.EventKind("debug/override_applied") {
			continue
		}
		payload := string(event.Payload)
		if strings.Contains(payload, "audit-secret") || strings.Contains(payload, "plaintext") || !strings.Contains(payload, "_debug_protected") {
			t.Fatalf("audit did not preserve fail-closed protection: %s", payload)
		}
		return
	}
	t.Fatal("missing debug/override_applied audit event")
}

func TestDebugController_CannotMutateSnapshotToBypassProtection(t *testing.T) {
	registry := newFakeExecutorRegistry()
	registry.Register("cli", stepExecutorFunc(func(_ context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
		return &enginepkg.StepResult{StepID: step.ID, Status: enginepkg.StepStatusCompleted, Outcome: enginepkg.StepOutcomeSuccess}, nil
	}))
	plan := enginepkg.ValidatedForTest(makeTestPlan(enginepkg.ResolvedStep{ID: "get_icm", Kind: "cli", Spec: &cliStepSpec{}}))
	plan.Inputs = map[string]*schema.Input{"api_secret": {Type: "secret"}}
	debugger := debugControllerFunc(func(_ context.Context, snapshot enginepkg.DebugSnapshot) (enginepkg.DebugDecision, error) {
		for index := range snapshot.ProtectedVars {
			snapshot.ProtectedVars[index] = "unprotected"
		}
		return enginepkg.DebugDecision{
			Action: enginepkg.DebugActionContinue, Vars: map[string]any{"api_secret": "replacement"},
		}, nil
	})
	handle, err := New(makeTestConfig()).Start(
		context.Background(), plan,
		enginepkg.RunOptions{Vars: map[string]string{"api_secret": "secret-value"}, Debugger: debugger},
	)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := handle.Next(context.Background()); err == nil || !strings.Contains(err.Error(), `protected variable "api_secret"`) {
		t.Fatalf("Next error = %v, want protected-variable rejection", err)
	}
}

func TestDebugController_CannotMutateActualToBypassImmutableStatusOrAudit(t *testing.T) {
	traceWriter := &fakeTraceWriter{}
	registry := newFakeExecutorRegistry()
	registry.Register("cli", stepExecutorFunc(func(_ context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
		return &enginepkg.StepResult{
			StepID: step.ID, Status: enginepkg.StepStatusDenied, Outcome: enginepkg.StepOutcomeDenied,
			Output: map[string]any{"reason": "original-denial"}, Error: errors.New("approval denied"),
		}, nil
	}))
	debugger := debugControllerFunc(func(_ context.Context, snapshot enginepkg.DebugSnapshot) (enginepkg.DebugDecision, error) {
		if snapshot.Phase != enginepkg.DebugPhaseAfter {
			return enginepkg.DebugDecision{Action: enginepkg.DebugActionContinue}, nil
		}
		snapshot.Actual.Status = enginepkg.StepStatusCompleted
		snapshot.Actual.Output["reason"] = "tampered"
		return enginepkg.DebugDecision{
			Action: enginepkg.DebugActionContinue,
			Result: &enginepkg.DebugResultOverride{Status: enginepkg.StepStatusCompleted},
		}, nil
	})
	cfg := makeTestConfig()
	cfg.Executors = registry
	cfg.TraceWriter = traceWriter
	handle, err := New(cfg).Start(
		context.Background(),
		enginepkg.ValidatedForTest(makeTestPlan(enginepkg.ResolvedStep{ID: "approve", Kind: "cli", Spec: &cliStepSpec{}})),
		enginepkg.RunOptions{Debugger: debugger},
	)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := handle.Next(context.Background()); err == nil || !strings.Contains(err.Error(), `actual status "denied" is immutable`) {
		t.Fatalf("Next error = %v, want immutable denied rejection", err)
	}
	for _, event := range traceWriter.collect() {
		if strings.Contains(string(event.Payload), "tampered") {
			t.Fatalf("controller mutation reached audit/trace: %s", event.Payload)
		}
	}
}

func TestDebugController_AfterOverrideChangesEffectiveFailureRouting(t *testing.T) {
	registry := newFakeExecutorRegistry()
	registry.Register("cli", stepExecutorFunc(func(_ context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
		return &enginepkg.StepResult{
			StepID:  step.ID,
			Status:  enginepkg.StepStatusFailed,
			Outcome: enginepkg.StepOutcomeFailed,
			Error:   errors.New("actual ICM lookup failure"),
			Output: map[string]any{
				"incident": map[string]any{"status": "Mitigated"},
				"typed":    map[string]string{"status": "Mitigated"},
				"opaque":   "preserve-me",
			},
		}, nil
	}))

	var actual *enginepkg.StepResult
	cfg := makeTestConfig()
	cfg.Executors = registry
	debugger := debugControllerFunc(func(_ context.Context, snapshot enginepkg.DebugSnapshot) (enginepkg.DebugDecision, error) {
		if snapshot.Phase != enginepkg.DebugPhaseAfter {
			return enginepkg.DebugDecision{}, nil
		}
		actual = snapshot.Actual
		return enginepkg.DebugDecision{
			Action: enginepkg.DebugActionContinue,
			Result: &enginepkg.DebugResultOverride{
				Status: enginepkg.StepStatusCompleted,
				OutputPatch: map[string]any{
					"incident": map[string]any{"status": "Active"},
				},
			},
		}, nil
	})

	handle, err := New(cfg).Start(
		context.Background(),
		enginepkg.ValidatedForTest(makeTestPlan(enginepkg.ResolvedStep{ID: "get_icm", Kind: "cli", Spec: &cliStepSpec{}})),
		enginepkg.RunOptions{Debugger: debugger},
	)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	result, err := handle.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if result.Status != enginepkg.StepStatusCompleted || result.Outcome != enginepkg.StepOutcomeSuccess {
		t.Fatalf("effective state = %s/%s, want completed/success", result.Status, result.Outcome)
	}
	incident := result.Output["incident"].(map[string]any)
	if incident["status"] != "Active" {
		t.Fatalf("effective incident status = %v, want Active", incident["status"])
	}
	if result.Output["opaque"] != "preserve-me" {
		t.Fatalf("merge patch discarded unseen output: %#v", result.Output)
	}
	if result.Error != nil {
		t.Fatalf("effective completed result retained actual error: %v", result.Error)
	}
	if actual == nil || actual.Status != enginepkg.StepStatusFailed || actual.Error == nil {
		t.Fatalf("actual result not preserved: %#v", actual)
	}
	actualIncident := actual.Output["incident"].(map[string]any)
	if actualIncident["status"] != "Mitigated" {
		t.Fatalf("actual incident status = %v, want Mitigated", actualIncident["status"])
	}

	_, err = handle.Next(context.Background())
	if !errors.Is(err, io.EOF) {
		t.Fatalf("second Next = %v, want EOF after successful run", err)
	}
	if state := handle.State().Status; state != enginepkg.RunStatusCompleted {
		t.Fatalf("run status = %s, want completed", state)
	}
}

func TestDebugController_ActualSnapshotIsIsolated(t *testing.T) {
	registry := newFakeExecutorRegistry()
	registry.Register("cli", stepExecutorFunc(func(_ context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
		return &enginepkg.StepResult{
			StepID:  step.ID,
			Status:  enginepkg.StepStatusCompleted,
			Outcome: enginepkg.StepOutcomeSuccess,
			Output: map[string]any{
				"incident": map[string]any{"status": "Mitigated"},
				"typed":    map[string]string{"status": "Mitigated"},
			},
		}, nil
	}))

	cfg := makeTestConfig()
	cfg.Executors = registry
	debugger := debugControllerFunc(func(_ context.Context, snapshot enginepkg.DebugSnapshot) (enginepkg.DebugDecision, error) {
		if snapshot.Phase == enginepkg.DebugPhaseAfter {
			snapshot.Actual.Output["incident"].(map[string]any)["status"] = "corrupted"
			snapshot.Actual.Output["typed"].(map[string]any)["status"] = "corrupted"
		}
		return enginepkg.DebugDecision{Action: enginepkg.DebugActionContinue}, nil
	})

	handle, err := New(cfg).Start(
		context.Background(),
		enginepkg.ValidatedForTest(makeTestPlan(enginepkg.ResolvedStep{ID: "get_icm", Kind: "cli", Spec: &cliStepSpec{}})),
		enginepkg.RunOptions{Debugger: debugger},
	)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	result, err := handle.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	incident := result.Output["incident"].(map[string]any)
	if incident["status"] != "Mitigated" {
		t.Fatalf("debugger mutated executor result through snapshot: %#v", result.Output)
	}
	if result.Output["typed"].(map[string]string)["status"] != "Mitigated" {
		t.Fatalf("debugger mutated typed executor output through snapshot: %#v", result.Output)
	}
}

func TestDebugController_AfterOverrideRecomputesCaptures(t *testing.T) {
	registry := newFakeExecutorRegistry()
	registry.Register("cli", stepExecutorFunc(func(_ context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
		return &enginepkg.StepResult{
			StepID:  step.ID,
			Status:  enginepkg.StepStatusCompleted,
			Outcome: enginepkg.StepOutcomeSuccess,
			Output: map[string]any{
				"incident": map[string]any{"status": "Mitigated"},
			},
			Vars: map[string]any{},
		}, nil
	}))

	capturePath, err := gcpparser.Parse("outputs.incident.status")
	if err != nil {
		t.Fatalf("parse capture: %v", err)
	}
	step := enginepkg.ResolvedStep{
		ID:      "get_icm",
		Kind:    "cli",
		Spec:    &cliStepSpec{},
		Capture: map[string]string{"incident_status": "outputs.incident.status"},
	}
	plan := enginepkg.ValidatedForTest(makeTestPlan(step))
	plan.Validation.GCPPaths[enginepkg.StepRef{StepID: step.ID, FieldPath: "capture.incident_status"}] = capturePath

	cfg := makeTestConfig()
	cfg.Executors = registry
	debugger := debugControllerFunc(func(_ context.Context, snapshot enginepkg.DebugSnapshot) (enginepkg.DebugDecision, error) {
		if snapshot.Phase != enginepkg.DebugPhaseAfter {
			return enginepkg.DebugDecision{}, nil
		}
		return enginepkg.DebugDecision{
			Action: enginepkg.DebugActionContinue,
			Result: &enginepkg.DebugResultOverride{OutputPatch: map[string]any{
				"incident": map[string]any{"status": "Active"},
			}},
		}, nil
	})

	handle, err := New(cfg).Start(context.Background(), plan, enginepkg.RunOptions{Debugger: debugger})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	result, err := handle.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if got := result.Vars["incident_status"]; got != "Active" {
		t.Fatalf("captured incident_status = %v, want Active", got)
	}
	if got := handle.State().Vars["incident_status"]; got != "Active" {
		t.Fatalf("committed incident_status = %v, want Active", got)
	}
}

func TestDebugController_StopBeforeExecutionCancelsWithoutDispatch(t *testing.T) {
	dispatched := false
	registry := newFakeExecutorRegistry()
	registry.Register("cli", stepExecutorFunc(func(_ context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
		dispatched = true
		return &enginepkg.StepResult{StepID: step.ID, Status: enginepkg.StepStatusCompleted}, nil
	}))
	cfg := makeTestConfig()
	cfg.Executors = registry
	debugger := debugControllerFunc(func(_ context.Context, snapshot enginepkg.DebugSnapshot) (enginepkg.DebugDecision, error) {
		if snapshot.Phase == enginepkg.DebugPhaseBefore {
			return enginepkg.DebugDecision{Action: enginepkg.DebugActionStop}, nil
		}
		return enginepkg.DebugDecision{}, nil
	})
	handle, err := New(cfg).Start(
		context.Background(),
		enginepkg.ValidatedForTest(makeTestPlan(enginepkg.ResolvedStep{ID: "get_icm", Kind: "cli", Spec: &cliStepSpec{}})),
		enginepkg.RunOptions{Debugger: debugger},
	)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := handle.Next(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Next error = %v, want context.Canceled", err)
	}
	if dispatched {
		t.Fatal("executor dispatched after debugger stop")
	}
	if status := handle.State().Status; status != enginepkg.RunStatusCancelled {
		t.Fatalf("run status = %s, want cancelled", status)
	}
}

func TestDebugController_RejectsDeniedStatusOverride(t *testing.T) {
	registry := newFakeExecutorRegistry()
	registry.Register("cli", &passThroughExecutor{})
	cfg := makeTestConfig()
	cfg.Executors = registry
	debugger := debugControllerFunc(func(_ context.Context, snapshot enginepkg.DebugSnapshot) (enginepkg.DebugDecision, error) {
		if snapshot.Phase == enginepkg.DebugPhaseAfter {
			return enginepkg.DebugDecision{
				Action: enginepkg.DebugActionContinue,
				Vars:   map[string]any{"should_not_commit": true},
				Result: &enginepkg.DebugResultOverride{Status: enginepkg.StepStatusDenied},
			}, nil
		}
		return enginepkg.DebugDecision{}, nil
	})
	handle, err := New(cfg).Start(
		context.Background(),
		enginepkg.ValidatedForTest(makeTestPlan(enginepkg.ResolvedStep{ID: "get_icm", Kind: "cli", Spec: &cliStepSpec{}})),
		enginepkg.RunOptions{Debugger: debugger},
	)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	_, err = handle.Next(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cannot be overridden") {
		t.Fatalf("Next error = %v, want invalid-override error", err)
	}
	if status := handle.State().Status; status != enginepkg.RunStatusFailed {
		t.Fatalf("run status = %s, want failed", status)
	}
	if _, exists := handle.State().Vars["should_not_commit"]; exists {
		t.Fatal("invalid debugger decision partially committed variables")
	}
}

func TestDebugController_RedactsSecretsBeforeSnapshotAndAudit(t *testing.T) {
	traceWriter := &fakeTraceWriter{}
	registry := newFakeExecutorRegistry()
	registry.Register("cli", stepExecutorFunc(func(_ context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
		return &enginepkg.StepResult{
			StepID: step.ID, Status: enginepkg.StepStatusFailed, Outcome: enginepkg.StepOutcomeFailed,
			Output: map[string]any{
				"status":     "Mitigated",
				"credential": "secret-value",
				"typed":      map[string]string{"nested": "prefix secret-value suffix", "token": "token-abc"},
			},
			Error: errors.New("request failed with secret-value"), Vars: map[string]any{},
		}, nil
	}))
	policy := &testutil.FakeGovernancePolicy{Redactions: []*governance.RedactionPattern{{
		Pattern: "token-[a-z]+", Replacement: "<redacted>",
	}}}
	plan := enginepkg.ValidatedForTest(makeTestPlan(enginepkg.ResolvedStep{ID: "get_icm", Kind: "cli", Spec: &cliStepSpec{}}))
	plan.Inputs = map[string]*schema.Input{
		"api_secret":    {Type: "secret"},
		"absent_secret": {Type: "secret"},
	}
	plan.Governance = policy

	cfg := makeTestConfig()
	cfg.Executors = registry
	cfg.TraceWriter = traceWriter
	debugger := debugControllerFunc(func(_ context.Context, snapshot enginepkg.DebugSnapshot) (enginepkg.DebugDecision, error) {
		if snapshot.Vars["api_secret"] != "<redacted>" || snapshot.Vars["note"] != "<redacted>" {
			t.Fatalf("snapshot vars were not redacted: %#v", snapshot.Vars)
		}
		if !containsString(snapshot.ProtectedVars, "api_secret") || !containsString(snapshot.ProtectedVars, "absent_secret") || !containsString(snapshot.ProtectedVars, "note") {
			t.Fatalf("protected vars = %#v", snapshot.ProtectedVars)
		}
		if snapshot.Phase == enginepkg.DebugPhaseAfter {
			if !snapshot.OutputProtected || snapshot.Actual.Output["credential"] != "<redacted>" {
				t.Fatalf("actual result was not protected: %#v", snapshot)
			}
			typed := snapshot.Actual.Output["typed"].(map[string]any)
			if typed["nested"] != "prefix <redacted> suffix" || typed["token"] != "<redacted>" || strings.Contains(snapshot.Actual.Error.Error(), "secret-value") {
				t.Fatalf("nested/error secrets were not redacted: %#v", snapshot.Actual)
			}
			return enginepkg.DebugDecision{
				Action: enginepkg.DebugActionContinue,
				Result: &enginepkg.DebugResultOverride{Status: enginepkg.StepStatusSkipped},
			}, nil
		}
		return enginepkg.DebugDecision{Action: enginepkg.DebugActionContinue}, nil
	})
	handle, err := New(cfg).Start(context.Background(), plan, enginepkg.RunOptions{
		Vars: map[string]string{"api_secret": "secret-value", "note": "token-abc"}, Debugger: debugger,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := handle.Next(context.Background()); err != nil {
		t.Fatalf("Next: %v", err)
	}
	for _, event := range traceWriter.collect() {
		if event.Kind != trace.EventKind("debug/override_applied") {
			continue
		}
		payload := string(event.Payload)
		if strings.Contains(payload, "token-abc") || strings.Contains(payload, "secret-value") {
			t.Fatalf("debug audit leaked a secret: %s", payload)
		}
		return
	}
	t.Fatal("missing debug/override_applied audit event")
}

func TestDebugController_ActualDeniedResultIsImmutable(t *testing.T) {
	registry := newFakeExecutorRegistry()
	registry.Register("cli", stepExecutorFunc(func(_ context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
		return &enginepkg.StepResult{
			StepID: step.ID, Status: enginepkg.StepStatusDenied, Outcome: enginepkg.StepOutcomeDenied,
			Error: errors.New("approval denied"), Output: map[string]any{"reason": "approval denied"},
		}, nil
	}))
	cfg := makeTestConfig()
	cfg.Executors = registry
	debugger := debugControllerFunc(func(_ context.Context, snapshot enginepkg.DebugSnapshot) (enginepkg.DebugDecision, error) {
		if snapshot.Phase != enginepkg.DebugPhaseAfter {
			return enginepkg.DebugDecision{}, nil
		}
		return enginepkg.DebugDecision{
			Action: enginepkg.DebugActionContinue,
			Vars:   map[string]any{"approved": true},
			Result: &enginepkg.DebugResultOverride{Status: enginepkg.StepStatusCompleted},
		}, nil
	})
	handle, err := New(cfg).Start(
		context.Background(),
		enginepkg.ValidatedForTest(makeTestPlan(enginepkg.ResolvedStep{ID: "approve", Kind: "cli", Spec: &cliStepSpec{}})),
		enginepkg.RunOptions{Debugger: debugger},
	)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	_, err = handle.Next(context.Background())
	if err == nil || !strings.Contains(err.Error(), `actual status "denied" is immutable`) {
		t.Fatalf("Next error = %v, want immutable denied error", err)
	}
	if _, exists := handle.State().Vars["approved"]; exists {
		t.Fatal("denied result variable override was committed")
	}
}

func TestDebugController_CancelDuringPauseRemainsCancelled(t *testing.T) {
	traceWriter := &fakeTraceWriter{}
	registry := newFakeExecutorRegistry()
	registry.Register("cli", &passThroughExecutor{})
	paused := make(chan struct{})
	debugger := debugControllerFunc(func(ctx context.Context, snapshot enginepkg.DebugSnapshot) (enginepkg.DebugDecision, error) {
		if snapshot.Phase == enginepkg.DebugPhaseBefore {
			close(paused)
			<-ctx.Done()
			return enginepkg.DebugDecision{}, ctx.Err()
		}
		return enginepkg.DebugDecision{Action: enginepkg.DebugActionContinue}, nil
	})
	cfg := makeTestConfig()
	cfg.Executors = registry
	cfg.TraceWriter = traceWriter
	handle, err := New(cfg).Start(
		context.Background(),
		enginepkg.ValidatedForTest(makeTestPlan(enginepkg.ResolvedStep{ID: "get_icm", Kind: "cli", Spec: &cliStepSpec{}})),
		enginepkg.RunOptions{Debugger: debugger},
	)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	nextErr := make(chan error, 1)
	go func() {
		_, err := handle.Next(context.Background())
		nextErr <- err
	}()
	select {
	case <-paused:
	case <-time.After(time.Second):
		t.Fatal("debugger did not pause")
	}
	if err := handle.Cancel(context.Background(), "test cancellation"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	select {
	case err := <-nextErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Next error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Next did not unblock after cancellation")
	}
	if status := handle.State().Status; status != enginepkg.RunStatusCancelled {
		t.Fatalf("run status = %s, want cancelled", status)
	}
	for _, event := range traceWriter.collect() {
		if event.Kind == trace.EventKindRunFailed {
			t.Fatal("cancellation emitted run/failed")
		}
	}
}

func TestDebugController_OversizedOutputIsBoundedBeforeController(t *testing.T) {
	registry := newFakeExecutorRegistry()
	registry.Register("cli", stepExecutorFunc(func(_ context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
		output := make(map[string]any, 1000)
		for index := 0; index < 1000; index++ {
			output[fmt.Sprintf("field_%04d", index)] = strings.Repeat("x", 8192)
		}
		return &enginepkg.StepResult{StepID: step.ID, Status: enginepkg.StepStatusCompleted, Output: output}, nil
	}))
	cfg := makeTestConfig()
	cfg.Executors = registry
	debugger := debugControllerFunc(func(_ context.Context, snapshot enginepkg.DebugSnapshot) (enginepkg.DebugDecision, error) {
		if snapshot.Phase == enginepkg.DebugPhaseAfter {
			encoded, err := json.Marshal(snapshot.Actual.Output)
			if err != nil {
				t.Fatalf("Marshal snapshot: %v", err)
			}
			if len(encoded) > 70*1024 {
				t.Fatalf("debug snapshot output is unbounded: %d bytes", len(encoded))
			}
			if snapshot.Actual.Output["_debug_truncated"] != true {
				t.Fatalf("missing truncation marker: %#v", snapshot.Actual.Output)
			}
		}
		return enginepkg.DebugDecision{Action: enginepkg.DebugActionContinue}, nil
	})
	handle, err := New(cfg).Start(
		context.Background(),
		enginepkg.ValidatedForTest(makeTestPlan(enginepkg.ResolvedStep{ID: "large", Kind: "cli", Spec: &cliStepSpec{}})),
		enginepkg.RunOptions{Debugger: debugger},
	)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := handle.Next(context.Background()); err != nil {
		t.Fatalf("Next: %v", err)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
