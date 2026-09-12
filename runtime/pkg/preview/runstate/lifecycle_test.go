package runstate_test

import (
	"sync"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/runstate"
)

// TestApply_DuplicateCompletedIsIdempotent verifies that re-applying the
// same step/completed event leaves the node in the same status. (Replay
// during SSE reconnect can produce duplicates.)
func TestApply_DuplicateCompletedIsIdempotent(t *testing.T) {
	s := runstate.New()
	s.Apply(ev("step/started", 1, "", map[string]any{"step_id": "a"}))
	s.Apply(ev("step/completed", 2, "", map[string]any{"step_id": "a", "duration_ms": int64(50)}))
	first := s.Snapshot()
	s.Apply(ev("step/completed", 2, "", map[string]any{"step_id": "a", "duration_ms": int64(50)}))
	second := s.Snapshot()
	if first.Get("a").Status != second.Get("a").Status {
		t.Errorf("status changed under duplicate: %v vs %v", first.Get("a"), second.Get("a"))
	}
	if first.Get("a").DurationMs != second.Get("a").DurationMs {
		t.Errorf("duration changed under duplicate: %d vs %d", first.Get("a").DurationMs, second.Get("a").DurationMs)
	}
}

// TestApply_UnknownEventKindIgnored verifies Apply does not panic on an
// event Kind it does not recognise and does not corrupt the node map.
func TestApply_UnknownEventKindIgnored(t *testing.T) {
	s := runstate.New()
	s.Apply(ev("step/started", 1, "", map[string]any{"step_id": "a"}))
	s.Apply(ev("widget/garbled", 2, "", map[string]any{"step_id": "a"}))
	if got := s.Get("a").Status; got != runstate.StatusRunning {
		t.Errorf("unknown event kind affected status: got %q", got)
	}
}

// TestApply_OutOfOrderEventsKeepLatestStatus accepts whatever the engine
// emits — even when sequence numbers go backwards we should not roll
// status backwards (running after completed). The current implementation
// simply applies every event; this test pins that behaviour so future
// tightening is a deliberate choice.
func TestApply_LowerSequenceDoesNotRewindCounter(t *testing.T) {
	s := runstate.New()
	s.Apply(ev("step/started", 5, "", map[string]any{"step_id": "a"}))
	s.Apply(ev("step/completed", 3, "", map[string]any{"step_id": "a"}))
	if got := s.Sequence; got != 5 {
		t.Errorf("Sequence should track max seen, got %d want 5", got)
	}
}

// TestApply_MissingStepIDIgnored verifies events without a step_id
// payload do not panic or mutate the node map.
func TestApply_MissingStepIDIgnored(t *testing.T) {
	s := runstate.New()
	s.Apply(ev("step/started", 1, "", map[string]any{}))
	if len(s.Snapshot().Nodes) != 0 {
		t.Errorf("missing step_id should not create a node")
	}
}

// TestApply_RunLifecycle covers the run/* event branches end to end.
func TestApply_RunLifecycle(t *testing.T) {
	cases := []struct {
		name  string
		final string
		want  runstate.RunStatus
	}{
		{"completed", "run/completed", runstate.RunCompleted},
		{"failed", "run/failed", runstate.RunFailed},
		{"cancelled", "run/cancelled", runstate.RunCancelled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := runstate.New()
			s.Apply(ev("run/started", 1, "2026-05-03T12:00:00Z", nil))
			if s.Status != runstate.RunRunning {
				t.Fatalf("after run/started: got %q want running", s.Status)
			}
			s.Apply(ev(tc.final, 2, "2026-05-03T12:00:01Z", nil))
			if s.Status != tc.want {
				t.Errorf("after %s: got %q want %q", tc.final, s.Status, tc.want)
			}
			if s.EndedAt == nil {
				t.Errorf("EndedAt not set on terminal event")
			}
		})
	}
}

// TestApply_ConcurrentReadersDuringWrites ensures Apply + Snapshot can
// race without panicking under -race. We don't assert on exact values —
// only on absence of data races.
func TestApply_ConcurrentReadersDuringWrites(t *testing.T) {
	s := runstate.New()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			s.Apply(engine.Event{Kind: "step/started", Sequence: int64(i), Payload: map[string]any{"step_id": "n"}})
			s.Apply(engine.Event{Kind: "step/completed", Sequence: int64(i + 1000), Payload: map[string]any{"step_id": "n"}})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = s.Snapshot()
			_ = s.Get("n")
		}
	}()
	wg.Wait()
}
