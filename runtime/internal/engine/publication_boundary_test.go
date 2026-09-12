package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	enginepkg "github.com/ormasoftchile/yawr/runtime/pkg/engine"
)

func TestPublicationBoundaryDoesNotWaitForLaterObserver(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cfg := makeTestConfig()
	gate, second := make(chan struct{}), make(chan struct{})
	work := make(chan string, 2)
	var observed int
	first := ""
	cfg.OnEvent = func(event enginepkg.Event) {
		if event.Kind != "step/started" || event.Payload["step_id"] == "parallel" {
			return
		}
		observed++
		if observed == 1 {
			first = event.Payload["step_id"].(string)
		} else {
			close(second)
			select {
			case <-gate:
			case <-ctx.Done():
			}
		}
	}
	registry := newFakeExecutorRegistry()
	registry.Register("probe", stepExecutorFunc(func(_ context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
		work <- step.ID
		return &enginepkg.StepResult{StepID: step.ID, Status: enginepkg.StepStatusCompleted}, nil
	}))
	cfg.Executors = registry
	handle, err := New(cfg).Start(ctx, enginepkg.ValidatedForTest(publicationParallelPlan()), enginepkg.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := handle.Next(ctx); done <- err }()
	select {
	case <-second:
	case <-ctx.Done():
		t.Fatal("second observer was never reached")
	}
	select {
	case id := <-work:
		if id != first {
			t.Errorf("work %s escaped its own blocked observer", id)
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("first start waited for a later event, not its own completion")
	}
	close(gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type publicationCancelCommitStore struct {
	*capturingCheckpointStore
	fail bool
}

func (store *publicationCancelCommitStore) SaveState(ctx context.Context, state enginepkg.RunState) error {
	if store.fail {
		return errors.New("injected cancellation checkpoint failure")
	}
	return store.capturingCheckpointStore.SaveState(ctx, state)
}

func TestPublicationCancellationCheckpointFailureIsNotLost(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cfg := makeTestConfig()
	store := &publicationCancelCommitStore{capturingCheckpointStore: &capturingCheckpointStore{}}
	cfg.Store = store
	registry := newFakeExecutorRegistry()
	calls := 0
	registry.Register("probe", stepExecutorFunc(func(_ context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
		calls++
		return &enginepkg.StepResult{StepID: step.ID, Status: enginepkg.StepStatusCompleted}, nil
	}))
	cfg.Executors = registry
	var handle enginepkg.RunHandle
	cfg.OnEvent = func(event enginepkg.Event) {
		if event.Kind == "step/started" {
			store.fail = true
			if err := handle.Cancel(ctx, "checkpoint failure"); !errors.Is(err, enginepkg.ErrCheckpointCommit) {
				t.Errorf("Cancel lost commit error: %v", err)
			}
		}
	}
	var err error
	handle, err = New(cfg).Start(ctx, enginepkg.ValidatedForTest(makeTestPlan(enginepkg.ResolvedStep{ID: "probe", Kind: "probe", Spec: &cliStepSpec{}})), enginepkg.RunOptions{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Next(ctx); !errors.Is(err, enginepkg.ErrCheckpointCommit) {
		t.Fatalf("Next lost cancellation durability failure: %v", err)
	}
	if calls != 0 {
		t.Fatal("failed cancellation publication allowed executor entry")
	}
}
