package main

import (
	"context"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/eventbus"
	internaltrace "github.com/ormasoftchile/yawr/runtime/internal/trace"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
)

func TestServeMain_WiresRealComponents(t *testing.T) {
	cfg, _, err := buildServerConfig(context.Background(), platform.Real(), "")
	if err != nil {
		t.Fatalf("buildServerConfig: %v", err)
	}
	if _, ok := cfg.EngineConfig.Dispatcher.(*eventbus.Dispatcher); !ok {
		t.Fatalf("expected real dispatcher, got %T", cfg.EngineConfig.Dispatcher)
	}
	if _, ok := cfg.EngineConfig.TraceWriter.(*internaltrace.JSONLWriter); !ok {
		t.Fatalf("expected JSONLWriter, got %T", cfg.EngineConfig.TraceWriter)
	}
	if cfg.EngineConfig.Executors == nil {
		t.Fatalf("expected executors wired")
	}
}

func TestServeMain_ConfigDefaults(t *testing.T) {
	cfg, _, err := buildServerConfig(context.Background(), platform.Real(), "")
	if err != nil {
		t.Fatalf("buildServerConfig: %v", err)
	}
	if cfg.Addr != defaultAddr {
		t.Fatalf("expected default addr %q, got %q", defaultAddr, cfg.Addr)
	}
	if cfg.ReadTimeout != defaultReadTimeout {
		t.Fatalf("expected default read timeout %v, got %v", defaultReadTimeout, cfg.ReadTimeout)
	}
	if cfg.WriteTimeout != defaultWriteTimeout {
		t.Fatalf("expected default write timeout %v, got %v", defaultWriteTimeout, cfg.WriteTimeout)
	}
}
