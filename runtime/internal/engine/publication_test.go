package engine

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	enginepkg "github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/eventbus"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func publicationParallelPlan() *enginepkg.ExecutionPlan {
	return makeTestPlan(enginepkg.ResolvedStep{ID: "parallel", Kind: "parallel", Spec: &testParallelSpec{branches: []enginepkg.BranchSpec{
		{Label: "left", Steps: []enginepkg.ResolvedStep{{ID: "left", Kind: "probe", Spec: &cliStepSpec{}}}},
		{Label: "right", Steps: []enginepkg.ResolvedStep{{ID: "right", Kind: "probe", Spec: &cliStepSpec{}}}},
	}}})
}

func TestPublicationCallbackSurfacesOrderedAndExecutorsConcurrent(t *testing.T) {
	for _, surface := range []string{"option", "config", "both"} {
		t.Run(surface, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			gate, entered, releaseWork := make(chan struct{}), make(chan struct{}), make(chan struct{})
			work := make(chan string, 2)
			var once sync.Once
			var inside atomic.Int32
			var mu sync.Mutex
			delivered, seen := map[string][]string{}, map[string]bool{}
			var handle enginepkg.RunHandle
			callback := func(label string) func(enginepkg.Event) {
				return func(event enginepkg.Event) {
					if inside.Add(1) != 1 {
						t.Error("callbacks overlapped")
					}
					defer inside.Add(-1)
					_ = handle.State()
					id, _ := event.Payload["step_id"].(string)
					if event.Kind == "step/started" && id != "parallel" {
						once.Do(func() {
							close(entered)
							select {
							case <-gate:
							case <-ctx.Done():
							}
						})
					}
					mu.Lock()
					delivered[event.EventID] = append(delivered[event.EventID], label)
					if event.Kind == "step/started" && (label == "config" || surface == "option") {
						seen[id] = true
					}
					mu.Unlock()
				}
			}
			cfg := makeTestConfig()
			opts := enginepkg.RunOptions{}
			if surface != "config" {
				opts.OnEvent = callback("option")
			}
			if surface != "option" {
				cfg.OnEvent = callback("config")
			}
			registry := newFakeExecutorRegistry()
			registry.Register("probe", stepExecutorFunc(func(ctx context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
				mu.Lock()
				if !seen[step.ID] {
					t.Error("executor entered before both start callbacks returned")
				}
				mu.Unlock()
				work <- step.ID
				select {
				case <-releaseWork:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return &enginepkg.StepResult{StepID: step.ID, Status: enginepkg.StepStatusCompleted}, nil
			}))
			cfg.Executors = registry
			var err error
			handle, err = New(cfg).Start(ctx, enginepkg.ValidatedForTest(publicationParallelPlan()), opts)
			if err != nil {
				t.Fatal(err)
			}
			finished := make(chan error, 1)
			go func() { _, err := handle.Next(ctx); finished <- err }()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("callback not entered")
			}
			select {
			case <-work:
				t.Fatal("work entered while observer gate held")
			case <-time.After(50 * time.Millisecond):
			}
			close(gate)
			for range 2 {
				select {
				case <-work:
				case <-ctx.Done():
					t.Fatal("branch execution was serialized")
				}
			}
			close(releaseWork)
			if err := <-finished; err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			for id, labels := range delivered {
				want := []string{surface}
				if surface == "both" {
					want = []string{"option", "config"}
				}
				if len(labels) != len(want) {
					t.Fatalf("event %s delivered %v, want %v", id, labels, want)
				}
				for i := range want {
					if labels[i] != want[i] {
						t.Fatalf("event %s callback order %v", id, labels)
					}
				}
			}
		})
	}
}

func TestPublicationExternalCancelDoesNotWaitForPausedOwner(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "caller-deadline"}[deadline], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stepCtx, stopStep := context.WithCancel(ctx)
			defer stopStep()
			gate, entered := make(chan struct{}), make(chan struct{})
			var once sync.Once
			var calls atomic.Int32
			var mu sync.Mutex
			counts := map[string]int{}
			cfg := makeTestConfig()
			registry := newFakeExecutorRegistry()
			registry.Register("probe", stepExecutorFunc(func(_ context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
				calls.Add(1)
				return &enginepkg.StepResult{StepID: step.ID, Status: enginepkg.StepStatusCompleted}, nil
			}))
			cfg.Executors = registry
			handle, err := New(cfg).Start(ctx, enginepkg.ValidatedForTest(publicationParallelPlan()), enginepkg.RunOptions{OnEvent: func(event enginepkg.Event) {
				if event.Kind == "step/started" && event.Payload["step_id"] != "parallel" {
					once.Do(func() { close(entered); <-gate })
				}
				mu.Lock()
				counts[event.EventID]++
				mu.Unlock()
			}})
			if err != nil {
				t.Fatal(err)
			}
			h := handle.(*runHandle)
			finished := make(chan error, 1)
			go func() { _, err := h.Next(stepCtx); finished <- err }()
			<-entered
			// Require the second start to be queued behind the held owner.
			for {
				h.callbackMu.Lock()
				queued := len(h.callbackPending)
				h.callbackMu.Unlock()
				if queued >= 2 {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("second branch did not reach its publication barrier")
				case <-time.After(time.Millisecond):
				}
			}
			cancelled := make(chan error, 1)
			go func() {
				_ = h.State()
				if deadline {
					stopStep()
				}
				cancelled <- h.Cancel(ctx, "paused owner")
			}()
			select {
			case err := <-cancelled:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("Cancel waited for observer completion")
			}
			if err := h.Cancel(ctx, "again"); err != nil {
				t.Fatal(err)
			}
			close(gate)
			select {
			case err := <-finished:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("stranded execution waiter")
			}
			if calls.Load() != 0 {
				t.Fatal("cancelled pending start entered cancellation-oblivious executor")
			}
			h.callbackMu.Lock()
			defer h.callbackMu.Unlock()
			if len(h.callbackPending) != 0 || len(h.callbackQueue) != 0 || h.callbacksRunning {
				t.Fatal("stranded callback ticket")
			}
			mu.Lock()
			defer mu.Unlock()
			for _, count := range counts {
				if count != 1 {
					t.Fatal("duplicate callback")
				}
			}
		})
	}
}

type publicationDispatcher struct{ calls atomic.Int32 }

func (*publicationDispatcher) Dispatch(eventbus.InboundEvent) error { return nil }
func (*publicationDispatcher) Cancel(string, string)                {}
func (d *publicationDispatcher) Wait(context.Context, string, eventbus.EventFilter, time.Duration) (*eventbus.InboundEvent, error) {
	d.calls.Add(1)
	return &eventbus.InboundEvent{}, nil
}

func TestPublicationCancelAtEveryStartBoundary(t *testing.T) {
	for _, surface := range []string{"option", "config"} {
		for _, kind := range []string{"probe", "missing", "parallel", "wait_for_event", "delay"} {
			t.Run(surface+"/"+kind, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				cfg := makeTestConfig()
				registry := newFakeExecutorRegistry()
				var calls atomic.Int32
				registry.Register("probe", stepExecutorFunc(func(_ context.Context, step enginepkg.ResolvedStep, _ map[string]any) (*enginepkg.StepResult, error) {
					calls.Add(1)
					return &enginepkg.StepResult{StepID: step.ID, Status: enginepkg.StepStatusCompleted}, nil
				}))
				cfg.Executors = registry
				dispatcher := &publicationDispatcher{}
				cfg.Dispatcher = dispatcher
				step := enginepkg.ResolvedStep{ID: "step", Kind: kind, Spec: &cliStepSpec{}}
				if kind == "delay" {
					step.Kind, step.Delay = "probe", "1h"
				}
				if kind == "wait_for_event" {
					step.Spec = &schema.WaitForEventSpec{}
				}
				plan := makeTestPlan(step)
				if kind == "parallel" {
					plan = publicationParallelPlan()
				}
				var handle enginepkg.RunHandle
				callback := func(event enginepkg.Event) {
					if event.Kind == "step/started" || event.Kind == "step/delaying" {
						_ = handle.State()
						if err := handle.Cancel(ctx, "observable boundary"); err != nil {
							t.Error(err)
						}
						if err := handle.Cancel(ctx, "repeat"); err != nil {
							t.Error(err)
						}
					}
				}
				opts := enginepkg.RunOptions{}
				if surface == "config" {
					cfg.OnEvent = callback
				} else {
					opts.OnEvent = callback
				}
				var err error
				handle, err = New(cfg).Start(ctx, enginepkg.ValidatedForTest(plan), opts)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := handle.Next(ctx); err != nil {
					t.Fatal(err)
				}
				if handle.State().Status != enginepkg.RunStatusCancelled || calls.Load() != 0 || dispatcher.calls.Load() != 0 {
					t.Fatal("start cancellation dispatched work or fabricated a result")
				}
			})
		}
	}
}
