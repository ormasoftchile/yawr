package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	enginepkg "github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

type resumeSignalStore struct {
	mu        sync.Mutex
	plan      *enginepkg.ExecutionPlan
	state     enginepkg.RunState
	tracePath string
}

func (store *resumeSignalStore) SaveState(_ context.Context, state enginepkg.RunState) error {
	store.mu.Lock()
	store.state = state
	store.mu.Unlock()
	return nil
}

func (store *resumeSignalStore) LoadState(context.Context, string) (enginepkg.RunState, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.state, nil
}

func (*resumeSignalStore) WriteTrace(context.Context, string, enginepkg.Event) error { return nil }
func (*resumeSignalStore) Close() error                                              { return nil }
func (store *resumeSignalStore) SavePlan(_ context.Context, _ string, plan *enginepkg.ExecutionPlan) error {
	store.plan = plan
	return nil
}
func (store *resumeSignalStore) LoadPlan(context.Context, string) (*enginepkg.ExecutionPlan, error) {
	return store.plan, nil
}
func (*resumeSignalStore) PlanDigest(string) (string, bool) { return "", false }
func (store *resumeSignalStore) TracePath(string) string    { return store.tracePath }
func (*resumeSignalStore) AcquireRunLease(context.Context, string) (enginepkg.RunLease, error) {
	return resumeSignalLease{}, nil
}

type resumeSignalLease struct{}

func (resumeSignalLease) Epoch() uint64  { return 1 }
func (resumeSignalLease) Release() error { return nil }

func TestResumeSignalCallbackWaitsForAssignedHandleUse(t *testing.T) {
	tracePath := filepath.Join(t.TempDir(), "trace.jsonl")
	if err := os.WriteFile(tracePath, nil, 0o600); err != nil {
		t.Fatalf("write empty trace: %v", err)
	}
	for attempt := 0; attempt < 100; attempt++ {
		runID := fmt.Sprintf("run-resume-signal-%d", attempt)
		plan := enginepkg.ValidatedForTest(&enginepkg.ExecutionPlan{
			RunID: runID, RunbookPath: "resume-signal.runbook.yaml",
			Steps: []enginepkg.ResolvedStep{
				{ID: "first", Kind: "noop", Spec: &schema.NoopSpec{}},
				{ID: "second", Kind: "noop", Spec: &schema.NoopSpec{}},
			},
			Metadata: enginepkg.PlanMetadata{RunbookID: "resume-signal"},
		})
		store := &resumeSignalStore{
			plan: plan, tracePath: tracePath,
			state: enginepkg.RunState{
				RunID: runID, Status: enginepkg.RunStatusRunning, CheckpointSequence: 1,
				CurrentStep: "first", CurrentStepIndex: 0,
				CursorSet: &enginepkg.ExecutionCursorSet{
					SchemaVersion: enginepkg.ExecutionCursorSchemaV1,
					Cursors: []enginepkg.ExecutionCursor{{
						QualifiedNodeID: "second", StepID: "second", StepIndex: 1,
						Phase: enginepkg.ExecutionPhaseBefore, Invocation: 1, RetryAttempt: 1,
					}},
				},
				Vars: map[string]any{}, StepResults: map[string]*enginepkg.StepResult{
					"first": {StepID: "first", Status: enginepkg.StepStatusCompleted},
				},
			},
		}
		platformFake := platform.NewFakePlatform()
		platformFake.SignalCh = make(chan platform.Signal, 1)
		platformFake.SignalCh <- platform.Signal{Name: "SIGTERM"}
		config := makeTestConfig()
		config.Platform = platformFake
		callbackSawNil := make(chan bool, 1)
		var resumed enginepkg.RunHandle
		var err error
		resumed, err = New(config).Resume(context.Background(), runID, enginepkg.RunOptions{
			Store: store,
			OnEvent: func(event enginepkg.Event) {
				if event.Kind == string(trace.EventKindRunCancelled) {
					callbackSawNil <- resumed == nil
				}
			},
		})
		if err != nil {
			t.Fatalf("Resume attempt %d: %v", attempt, err)
		}
		_ = resumed.State()
		select {
		case sawNil := <-callbackSawNil:
			if sawNil {
				t.Fatalf("signal callback ran before handle assignment on attempt %d", attempt)
			}
		case <-time.After(time.Second):
			t.Fatalf("signal callback was not delivered after handle use on attempt %d", attempt)
		}
	}
}
