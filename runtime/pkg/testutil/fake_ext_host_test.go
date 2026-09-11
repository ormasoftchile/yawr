package testutil

import (
	"context"
	"testing"

	engineimpl "github.com/ormasoftchile/yawr/runtime/internal/engine"
	executorpkg "github.com/ormasoftchile/yawr/runtime/internal/executor"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/extension"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

func TestEngine_WithExtensionHost_ToolContribution(t *testing.T) {
	host := &FakeExtensionHost{Tools: []*schema.ToolDef{{Name: "hello"}}}
	eng := newTestEngine(t, host)
	plan := minimalPlan()
	if _, err := eng.Start(context.Background(), engine.ValidatedForTest(plan), engine.RunOptions{}); err != nil {
		t.Fatalf("expected start success: %v", err)
	}
	if host.LoadCallCount != 1 {
		t.Fatalf("expected Load to be called")
	}
}

func TestEngine_WithNilExtensionHost_NoError(t *testing.T) {
	eng := newTestEngine(t, nil)
	plan := minimalPlan()
	if _, err := eng.Start(context.Background(), engine.ValidatedForTest(plan), engine.RunOptions{}); err != nil {
		t.Fatalf("expected start success: %v", err)
	}
}

func TestEngine_ExtensionHost_LoadCalledOnRun(t *testing.T) {
	host := &FakeExtensionHost{}
	eng := newTestEngine(t, host)
	plan := minimalPlan()
	if _, err := eng.Start(context.Background(), engine.ValidatedForTest(plan), engine.RunOptions{}); err != nil {
		t.Fatalf("expected start success: %v", err)
	}
	if host.LoadCallCount != 1 {
		t.Fatalf("expected Load to be called once, got %d", host.LoadCallCount)
	}
}

func newTestEngine(t *testing.T, host extension.ExtensionHost) engine.Engine {
	t.Helper()
	reg := executorpkg.NewMapRegistry()
	dispatcher := NewFakeEventDispatcher()
	traceWriter := &fakeTraceWriter{}
	platform := platform.NewFakePlatform()
	cfg := engine.EngineConfig{
		Executors:     reg,
		Dispatcher:    dispatcher,
		TraceWriter:   traceWriter,
		Platform:      platform,
		ExtensionHost: host,
	}
	return engineimpl.New(cfg)
}

func minimalPlan() *engine.ExecutionPlan {
	return &engine.ExecutionPlan{
		Steps:     []engine.ResolvedStep{},
		Tools:     map[string]*schema.ToolDef{},
		Providers: map[string]*schema.ProviderDef{},
	}
}

type fakeTraceWriter struct{}

func (w *fakeTraceWriter) Append(ev trace.TraceEvent) error {
	_ = ev
	return nil
}

func (w *fakeTraceWriter) Close() error {
	return nil
}
