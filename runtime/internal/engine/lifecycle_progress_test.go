package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	enginepkg "github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestLifecycleProgressPublishedBeforeExecutorWait(t *testing.T) {
	for _, parallel := range []bool{false, true} {
		name := "serial"
		if parallel {
			name = "parallel"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			entered := make(chan struct{})
			release := make(chan struct{})
			var mu sync.Mutex
			var events []enginepkg.Event
			registry := newFakeExecutorRegistry()
			registry.Register("noop", &passThroughExecutor{})
			registry.Register("wait", stepExecutorFunc(func(ctx context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return &enginepkg.StepResult{StepID: step.ID, Status: enginepkg.StepStatusCompleted, Outcome: enginepkg.StepOutcomeSuccess}, nil
			}))
			cfg := makeTestConfig()
			cfg.Executors = registry
			var handle enginepkg.RunHandle
			cfg.OnEvent = func(event enginepkg.Event) {
				_ = handle.State() // Callbacks must not hold the run mutex.
				mu.Lock()
				events = append(events, event)
				mu.Unlock()
			}
			steps := []enginepkg.ResolvedStep{{ID: "done", Kind: "noop", Spec: &schema.NoopSpec{}}, {ID: "waiting", Kind: "wait", Spec: &cliStepSpec{}}}
			if parallel {
				steps = []enginepkg.ResolvedStep{{ID: "container", Kind: "parallel", Spec: &testParallelSpec{branches: []enginepkg.BranchSpec{
					{Label: "serial-child", Steps: steps},
				}}}}
			}
			var err error
			handle, err = New(cfg).Start(ctx, enginepkg.ValidatedForTest(makeTestPlan(steps...)), enginepkg.RunOptions{})
			if err != nil {
				t.Fatal(err)
			}
			finished := make(chan error, 1)
			go func() {
				if !parallel {
					if _, err := handle.Next(ctx); err != nil {
						finished <- err
						return
					}
				}
				_, err := handle.Next(ctx)
				finished <- err
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("executor never entered")
			}
			mu.Lock()
			states := map[string]string{}
			for _, event := range events {
				if event.Kind == "step/started" || event.Kind == "step/completed" {
					states[event.Payload["step_id"].(string)] = event.Kind
				}
			}
			mu.Unlock()
			if states["done"] != "step/completed" || states["waiting"] != "step/started" {
				t.Errorf("live prefix must expose completed sibling and current wait: %v", states)
			}
			if parallel && states["container"] != "step/started" {
				t.Errorf("active parent missing: %v", states)
			}
			close(release)
			select {
			case err := <-finished:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("executor did not finish")
			}
			mu.Lock()
			defer mu.Unlock()
			started, terminal := map[string]int{}, map[string]int{}
			for _, event := range events {
				id, _ := event.Payload["node_id"].(string)
				switch event.Kind {
				case "step/started":
					started[id]++
					if terminal[id] != 0 {
						t.Errorf("late start for %s", id)
					}
				case "step/completed":
					terminal[id]++
					if started[id] != 1 || terminal[id] != 1 {
						t.Errorf("noncanonical lifecycle %s: %d starts %d terminals", id, started[id], terminal[id])
					}
				}
			}
		})
	}
}
