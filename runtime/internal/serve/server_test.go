package serve

import (
	"context"
	"testing"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	servepkg "github.com/ormasoftchile/yawr/runtime/pkg/serve"
)

func TestServer_StartStop_Graceful(t *testing.T) {
	parserFake := &fakeParser{result: &parser.ParsedRunbook{Source: "test"}}
	plannerFake := &fakePlanner{plan: &engine.ExecutionPlan{RunID: "run-1"}}
	engineFake := &fakeEngine{
		startFunc: func(_ context.Context, _ *engine.ExecutionPlan, _ engine.RunOptions) (engine.RunHandle, error) {
			return newFakeRunHandle("run-1"), nil
		},
	}

	cfg := servepkg.ServerConfig{
		Addr:    "127.0.0.1:0",
		Engine:  engineFake,
		Parser:  parserFake,
		Planner: plannerFake,
	}
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- srv.Start(ctx)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not stop")
	}
}

func TestServer_CancelActiveRuns_OnStop(t *testing.T) {
	h := newTestServerHarness(t)
	entry := &RunEntry{
		ID:     "run-1",
		Handle: h.handle,
		State:  engine.RunStatusRunning,
	}
	h.server.registry.Add(entry)

	if err := h.server.Stop(context.Background()); err != nil {
		t.Fatalf("Stop error: %v", err)
	}
	if h.handle.State().Status != engine.RunStatusCancelled {
		t.Fatalf("expected cancelled state, got %s", h.handle.State().Status)
	}
}
