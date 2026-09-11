package engine_test

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/run"
)

func collectHealthPath() string {
	_, thisFile, _, _ := runtime.Caller(0)
	pkgDir := filepath.Dir(thisFile)
	return filepath.Join(pkgDir, "testdata", "examples", "collect-health", "collect-health.runbook.yaml")
}

// TestIterationEvents_CollectHealth asserts that the iterate executor emits
// iterate/iteration_started and iterate/iteration_completed events, one pair
// per iteration, carrying iteration_index, iteration_total, as, and value.
func TestIterationEvents_CollectHealth(t *testing.T) {
	var (
		mu     sync.Mutex
		events []engine.Event
	)
	collect := func(e engine.Event) {
		mu.Lock()
		defer mu.Unlock()
		copyP := make(map[string]any, len(e.Payload))
		for k, v := range e.Payload {
			copyP[k] = v
		}
		events = append(events, engine.Event{Kind: e.Kind, Payload: copyP})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	handle, err := run.Start(ctx, run.Config{
		RunbookPath: collectHealthPath(),
		Client:      "engine-contract-test",
		OnEvent:     collect,
		OnSubEvent:  collect,
	})
	if err != nil {
		t.Fatalf("run.Start: %v", err)
	}
	for {
		_, nextErr := handle.Next(ctx)
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			t.Fatalf("Next: %v", nextErr)
		}
	}

	mu.Lock()
	defer mu.Unlock()

	type iterEvt struct {
		kind  string
		idx   int
		total int
		as    string
		value string
	}
	var got []iterEvt
	for _, e := range events {
		if e.Kind != "iterate/iteration_started" && e.Kind != "iterate/iteration_completed" {
			continue
		}
		idx, _ := e.Payload["iteration_index"].(int)
		total, _ := e.Payload["iteration_total"].(int)
		as, _ := e.Payload["as"].(string)
		val, _ := e.Payload["value"].(string)
		got = append(got, iterEvt{kind: e.Kind, idx: idx, total: total, as: as, value: val})
	}

	want := []iterEvt{
		{kind: "iterate/iteration_started", idx: 1, total: 2, as: "svc", value: "registry.npmjs.org"},
		{kind: "iterate/iteration_completed", idx: 1, total: 2, as: "svc", value: "registry.npmjs.org"},
		{kind: "iterate/iteration_started", idx: 2, total: 2, as: "svc", value: "github.com"},
		{kind: "iterate/iteration_completed", idx: 2, total: 2, as: "svc", value: "github.com"},
	}
	if len(got) != len(want) {
		t.Fatalf("iteration events: got %d, want %d\ngot: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		g := got[i]
		if g.kind != w.kind || g.idx != w.idx || g.total != w.total || g.as != w.as || g.value != w.value {
			t.Errorf("event[%d]: got %+v, want %+v", i, g, w)
		}
	}
}
